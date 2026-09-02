from __future__ import annotations

import argparse
import asyncio
import logging
import time
from collections.abc import Sequence
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from decimal import Decimal
from typing import Any, Protocol

from pydantic import ValidationError

from .. import __version__
from ..config import Settings
from ..control_plane import ControlPlaneClient, LeaseFencedError
from ..models import (
    AggregateSubmission,
    AnalysisLease,
    FailureClass,
    PairedScore,
    QualityAnalysisContext,
)
from ..observability import MetricsServer, RadarMetrics, trace_scope
from .bootstrap import bootstrap_delta, weighted_delta
from .classification import count_failures
from .cusum import cusum
from .ewma import ewma
from .pairing import pair_scores
from .quality import (
    FingerprintCandidate,
    ProbeEventClass,
    QualityConclusion,
    QualityDimension,
    QualityDimensionResult,
    QualityEvidenceCode,
    QualityProbeObservation,
    QualityReportPublication,
    SourceAttribution,
    SourceAttributionPolicy,
    classify_overall,
    infer_source,
)

log = logging.getLogger(__name__)


@dataclass(frozen=True)
class AnalysisPolicy:
    seed: int = 20260725
    bootstrap_draws: int = 10_000
    minimum_pairs: int = 30
    maximum_ci_width_pp: Decimal = Decimal("5")
    ewma_lambda: Decimal = Decimal("0.2")
    cusum_drift: Decimal = Decimal("0.5")
    cusum_threshold: Decimal = Decimal("5")


@dataclass(frozen=True)
class AggregatePoint:
    delta_pp: Decimal


@dataclass(frozen=True)
class AggregateAnalysis:
    baseline_score: Decimal
    candidate_score: Decimal
    delta_pp: Decimal
    ci_low_pp: Decimal
    ci_high_pp: Decimal
    effective_pair_count: int
    invalid_counts: dict[FailureClass, int]
    ewma: Decimal | None
    cusum: Decimal
    cusum_signal: bool
    evidence_sufficiency: str
    seed: int


_DIMENSION_EVIDENCE_CODES: dict[QualityDimension, QualityEvidenceCode] = {
    QualityDimension.KNOWLEDGE_FRESHNESS: QualityEvidenceCode.WITHIN_POLICY_BOUNDS,
    QualityDimension.MODEL_FINGERPRINT: QualityEvidenceCode.FINGERPRINT_MISMATCH,
    QualityDimension.REASONING_STABILITY: QualityEvidenceCode.REASONING_VARIANCE,
    QualityDimension.STRUCTURE_COMPLIANCE: QualityEvidenceCode.STRUCTURE_VIOLATION,
    QualityDimension.PARAMETER_FIDELITY: QualityEvidenceCode.PARAMETER_DEVIATION,
    QualityDimension.INSTRUCTION_HIERARCHY: QualityEvidenceCode.INSTRUCTION_VIOLATION,
    QualityDimension.PROTOCOL_SCHEMA: QualityEvidenceCode.PROTOCOL_VIOLATION,
    QualityDimension.STREAM_COMPLETENESS: QualityEvidenceCode.STREAM_INCOMPLETE,
}

_ADULTERATION_DIMENSIONS = frozenset(
    {
        QualityDimension.MODEL_FINGERPRINT,
        QualityDimension.REASONING_STABILITY,
        QualityDimension.STRUCTURE_COMPLIANCE,
    }
)

_DEGRADATION_DIMENSIONS = frozenset(
    {
        QualityDimension.KNOWLEDGE_FRESHNESS,
        QualityDimension.REASONING_STABILITY,
        QualityDimension.INSTRUCTION_HIERARCHY,
    }
)


def _status_for_quality_dimension(
    delta_pp: Decimal,
    sample_count: int,
    context: QualityAnalysisContext,
) -> QualityConclusion:
    policy = context.policy
    if sample_count < policy.minimum_samples_per_dimension:
        return QualityConclusion.INSUFFICIENT_COVERAGE
    magnitude = abs(delta_pp)
    if magnitude < policy.observe_delta_pp:
        return QualityConclusion.NO_SIGNIFICANT_ANOMALY
    if magnitude < policy.suspected_delta_pp:
        return QualityConclusion.OBSERVE
    if magnitude < policy.high_risk_delta_pp:
        return QualityConclusion.SUSPECTED
    return QualityConclusion.HIGH_RISK


def _dimension_evidence_code(
    dimension: QualityDimension, status: QualityConclusion
) -> QualityEvidenceCode:
    if status is QualityConclusion.INSUFFICIENT_COVERAGE:
        return QualityEvidenceCode.COVERAGE_INSUFFICIENT
    if status is QualityConclusion.NO_SIGNIFICANT_ANOMALY:
        if dimension is QualityDimension.MODEL_FINGERPRINT:
            return QualityEvidenceCode.FINGERPRINT_MATCHED
        return QualityEvidenceCode.WITHIN_POLICY_BOUNDS
    return _DIMENSION_EVIDENCE_CODES[dimension]


def _worst_quality_conclusion(
    results: Sequence[QualityDimensionResult],
) -> QualityConclusion:
    statuses = tuple(result.status for result in results)
    if QualityConclusion.INSUFFICIENT_COVERAGE in statuses:
        return QualityConclusion.INSUFFICIENT_COVERAGE
    for status in (
        QualityConclusion.HIGH_RISK,
        QualityConclusion.SUSPECTED,
        QualityConclusion.OBSERVE,
    ):
        if status in statuses:
            return status
    return QualityConclusion.NO_SIGNIFICANT_ANOMALY


def build_quality_report(
    context: QualityAnalysisContext | None,
    aggregate: AggregateAnalysis | dict[str, Any],
    now: datetime,
) -> QualityReportPublication | None:
    if context is None:
        return None
    try:
        context = QualityAnalysisContext.model_validate(context.model_dump())
    except ValidationError:
        return None

    policy = context.policy
    policy_for_source = SourceAttributionPolicy(
        minimum_coverage=policy.minimum_coverage,
        minimum_confidence=policy.minimum_confidence,
        minimum_margin=policy.minimum_margin,
    )
    results: list[QualityDimensionResult] = []
    observations: list[QualityProbeObservation] = []
    for item in context.dimensions:
        delta_pp = (item.candidate_score - item.baseline_score) * Decimal("100")
        status = _status_for_quality_dimension(delta_pp, item.sample_count, context)
        confidence = (
            Decimal("1")
            if status is not QualityConclusion.INSUFFICIENT_COVERAGE
            else Decimal("0")
        )
        results.append(
            QualityDimensionResult(
                key=item.key,
                score=item.candidate_score if confidence else Decimal("0"),
                status=status,
                sample_count=item.sample_count,
                confidence=confidence,
                stable_baseline_delta_pp=item.stable_baseline_delta_pp,
                reference_baseline_delta_pp=(
                    item.reference_baseline_delta_pp
                    if item.reference_baseline_delta_pp is not None
                    else delta_pp
                ),
                checked_at=item.observed_at,
                evidence_code=_dimension_evidence_code(item.key, status),
            )
        )
        observations.append(
            QualityProbeObservation(
                probe_spec_hash=item.probe_spec_hash,
                observation_hash=item.observation_hash,
                event_class=ProbeEventClass(item.probe_event_class),
                event_digest=item.observation_hash,
                observed_at=item.observed_at,
            )
        )

    fingerprint_dimension = next(
        result
        for result in results
        if result.key is QualityDimension.MODEL_FINGERPRINT
    )
    source_candidates_are_valid = all(
        candidate.has_complete_observation
        and candidate.sample_count is not None
        and candidate.sample_count >= policy.minimum_samples_per_dimension
        for candidate in context.source_candidates
    )
    if (
        fingerprint_dimension.status is QualityConclusion.INSUFFICIENT_COVERAGE
        or not source_candidates_are_valid
    ):
        source = SourceAttribution.insufficient_evidence()
    else:
        source = infer_source(
            tuple(
                FingerprintCandidate(
                    display_name=candidate.display_name, confidence=candidate.confidence
                )
                for candidate in context.source_candidates
            ),
            coverage=Decimal("1") if context.source_candidates else Decimal("0"),
            minimum_coverage=policy.minimum_coverage,
            minimum_confidence=policy.minimum_confidence,
            minimum_margin=policy.minimum_margin,
        )
    adulteration = _worst_quality_conclusion(
        tuple(result for result in results if result.key in _ADULTERATION_DIMENSIONS)
    )
    degradation = _worst_quality_conclusion(
        tuple(result for result in results if result.key in _DEGRADATION_DIMENSIONS)
    )
    return QualityReportPublication(
        run_id=context.run_id,
        model_alias=context.model_alias,
        policy_version=context.policy_version,
        overall_conclusion=classify_overall(source, results),
        adulteration_risk=adulteration,
        degradation_risk=degradation,
        generated_at=now,
        fresh_until=now + timedelta(hours=policy.freshness_hours),
        dimension_results=tuple(results),
        source_attribution=source,
        source_attribution_policy=policy_for_source,
        probe_observations=tuple(observations),
    )


class Analyzer(Protocol):
    def __call__(self, lease: AnalysisLease) -> Any: ...


def _history_points(lease: AnalysisLease) -> tuple[AggregatePoint, ...]:
    points: list[AggregatePoint] = []
    for item in lease.history:
        value = item.get("delta_pp")
        if value is None:
            continue
        points.append(AggregatePoint(Decimal(str(value))))
    return tuple(points)


def default_analyzer(
    lease: AnalysisLease, policy: AnalysisPolicy | None = None
) -> AggregateAnalysis:
    pairs = lease.pairs
    return analyze(
        pairs,
        _history_points(lease),
        policy,
        lease.invalid_failures,
    )


def aggregate_submission(
    lease: AnalysisLease,
    result: AggregateAnalysis | dict[str, Any],
    *,
    quality_report: QualityReportPublication | dict[str, Any] | None = None,
) -> AggregateSubmission:
    if isinstance(result, AggregateAnalysis):
        payload: dict[str, Any] = {
            "baseline_score": result.baseline_score,
            "candidate_score": result.candidate_score,
            "delta_pp": result.delta_pp,
            "ci_low_pp": result.ci_low_pp,
            "ci_high_pp": result.ci_high_pp,
            "effective_pair_count": result.effective_pair_count,
            "invalid_counts": result.invalid_counts,
            "evidence_sufficiency": result.evidence_sufficiency,
            "ewma": result.ewma,
            "cusum": result.cusum,
            "seed": result.seed,
        }
    else:
        payload = dict(result)
    start = lease.window_start or datetime.now(UTC)
    return AggregateSubmission(
        run_id=lease.run_id,
        capability_domain=lease.capability_domain,
        model_route=lease.model_route,
        window=lease.window,
        analysis_version=lease.analysis_version,
        window_start=start,
        baseline_score=Decimal(str(payload.get("baseline_score", "0"))),
        candidate_score=Decimal(str(payload.get("candidate_score", "0"))),
        delta_pp=Decimal(str(payload.get("delta_pp", "0"))),
        ci_low_pp=Decimal(str(payload.get("ci_low_pp", "0"))),
        ci_high_pp=Decimal(str(payload.get("ci_high_pp", "0"))),
        effective_pair_count=int(payload.get("effective_pair_count", 0)),
        invalid_counts=payload.get("invalid_counts", {}),
        evidence_sufficiency=str(payload.get("evidence_sufficiency", "insufficient_evidence")),
        ewma=Decimal(str(payload["ewma"])) if payload.get("ewma") is not None else None,
        cusum=Decimal(str(payload["cusum"])) if payload.get("cusum") is not None else None,
        seed=int(payload.get("seed", 20260725)),
        input_set_hash=lease.input_set_hash,
        score_ids=lease.score_ids,
        score_refs=lease.score_refs,
        snapshot_refs=lease.snapshot_refs,
        aggregate=payload,
        quality_report=quality_report,
    )


class StatisticsWorker:
    def __init__(
        self,
        settings: Settings,
        client: ControlPlaneClient,
        *,
        capabilities: Sequence[str] = (),
        analyzer: Analyzer | None = None,
        metrics: RadarMetrics | None = None,
    ) -> None:
        self.settings = settings
        self.client = client
        self.capabilities = (
            tuple(capabilities) or settings.analysis_capabilities or settings.capabilities
        )
        self.analyzer: Analyzer = analyzer or default_analyzer
        self.metrics = metrics or RadarMetrics()
        self._stop = asyncio.Event()

    def stop(self) -> None:
        self._stop.set()

    async def process_once(self) -> bool:
        lease = await self.client.claim_analysis(list(self.capabilities))
        if lease is None:
            return False
        with trace_scope(str(lease.id)):
            return await self._process_claimed(lease)

    async def _process_claimed(self, lease: AnalysisLease) -> bool:
        lease_started = time.monotonic()
        try:
            if lease.window_start is not None and lease.window_start.tzinfo is not None:
                self.metrics.observe_analysis_lag(
                    max(
                        0.0,
                        (
                            datetime.now(lease.window_start.tzinfo) - lease.window_start
                        ).total_seconds(),
                    )
                )
            result = self.analyzer(lease)
            if asyncio.iscoroutine(result):
                result = await result
            quality_report = build_quality_report(
                lease.quality_context,
                result,
                datetime.now(UTC),
            )
            await self.client.complete_analysis(
                lease.id,
                lease.lease_token,
                aggregate_submission(lease, result, quality_report=quality_report),
                lease.lease_epoch,
            )
        except LeaseFencedError:
            log.warning("analysis lease %s fenced", lease.id)
        except Exception:
            log.exception("analysis lease %s failed", lease.id)
            try:
                await self.client.fail_analysis(
                    lease.id,
                    lease.lease_token,
                    "statistics_worker_error",
                    lease.lease_epoch,
                )
            except LeaseFencedError:
                log.warning("analysis failure lease %s was fenced", lease.id)
            except Exception:
                log.exception("failed to persist analysis failure for lease %s", lease.id)
        finally:
            self.metrics.observe_lease_age(time.monotonic() - lease_started)
        return True

    async def run_forever(self, stop: asyncio.Event | None = None) -> None:
        stop_event = stop or self._stop
        while not stop_event.is_set():
            claimed = await self.process_once()
            if not claimed:
                try:
                    await asyncio.wait_for(stop_event.wait(), self.settings.poll_interval_seconds)
                except TimeoutError:
                    pass


def analyze(
    pairs: Sequence[PairedScore],
    history: Sequence[AggregatePoint] = (),
    policy: AnalysisPolicy | None = None,
    invalid_failures: Sequence[FailureClass | None] = (),
) -> AggregateAnalysis:
    policy = policy or AnalysisPolicy()
    valid = pair_scores(pairs)
    if valid:
        total_weight = sum((item.weight for item in valid), Decimal("0"))
        baseline = (
            sum((item.baseline_score * item.weight for item in valid), Decimal("0")) / total_weight
        )
        candidate = (
            sum((item.candidate_score * item.weight for item in valid), Decimal("0")) / total_weight
        )
    else:
        baseline = Decimal("0")
        candidate = Decimal("0")
    delta = weighted_delta(valid)
    low, high = bootstrap_delta(valid, seed=policy.seed, draws=policy.bootstrap_draws)
    width_pp = (high - low) * 100
    sufficient = len(valid) >= policy.minimum_pairs and width_pp <= policy.maximum_ci_width_pp
    history_values = [point.delta_pp for point in history] + [delta]
    positive, negative, signal = cusum(
        history_values, drift=policy.cusum_drift, threshold=policy.cusum_threshold
    )
    return AggregateAnalysis(
        baseline_score=baseline,
        candidate_score=candidate,
        delta_pp=delta * 100,
        ci_low_pp=low * 100,
        ci_high_pp=high * 100,
        effective_pair_count=len(valid),
        invalid_counts=count_failures(invalid_failures),
        ewma=ewma(history_values, lam=policy.ewma_lambda),
        cusum=positive if positive >= abs(negative) else negative,
        cusum_signal=signal,
        evidence_sufficiency="sufficient" if sufficient else "insufficient_evidence",
        seed=policy.seed,
    )


async def run(settings: Settings) -> None:
    metrics = RadarMetrics()
    metrics_server = (
        MetricsServer(metrics, host=settings.metrics_host, port=settings.metrics_port)
        if settings.metrics_enabled
        else None
    )
    if metrics_server is not None:
        await metrics_server.start()
    try:
        async with ControlPlaneClient(settings, metrics=metrics) as client:
            await StatisticsWorker(settings, client, metrics=metrics).run_forever()
    finally:
        if metrics_server is not None:
            await metrics_server.close()


def main(argv: list[str] | None = None) -> None:
    parser = argparse.ArgumentParser(description="Sub2API Radar statistics worker")
    parser.add_argument("--version", action="version", version=__version__)
    parser.parse_args(argv)
    import logging

    logging.basicConfig(level=logging.INFO)
    asyncio.run(run(__import__("sub2api_radar.config", fromlist=["get_settings"]).get_settings()))
