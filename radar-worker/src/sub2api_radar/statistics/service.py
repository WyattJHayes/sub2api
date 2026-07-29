from __future__ import annotations

import argparse
import asyncio
import logging
from collections.abc import Sequence
from dataclasses import dataclass
from datetime import UTC, datetime
from decimal import Decimal
from typing import Any, Protocol

from ..config import Settings
from ..control_plane import ControlPlaneClient, LeaseFencedError
from ..models import AggregateSubmission, AnalysisLease, FailureClass, PairedScore
from .bootstrap import bootstrap_delta, weighted_delta
from .classification import count_failures
from .cusum import cusum
from .ewma import ewma
from .pairing import pair_scores

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
    lease: AnalysisLease, result: AggregateAnalysis | dict[str, Any]
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
    )


class StatisticsWorker:
    def __init__(
        self,
        settings: Settings,
        client: ControlPlaneClient,
        *,
        capabilities: Sequence[str] = (),
        analyzer: Analyzer | None = None,
    ) -> None:
        self.settings = settings
        self.client = client
        self.capabilities = (
            tuple(capabilities) or settings.analysis_capabilities or settings.capabilities
        )
        self.analyzer: Analyzer = analyzer or default_analyzer
        self._stop = asyncio.Event()

    def stop(self) -> None:
        self._stop.set()

    async def process_once(self) -> bool:
        lease = await self.client.claim_analysis(list(self.capabilities))
        if lease is None:
            return False
        try:
            result = self.analyzer(lease)
            if asyncio.iscoroutine(result):
                result = await result
            await self.client.complete_analysis(
                lease.id, lease.lease_token, aggregate_submission(lease, result), lease.lease_epoch
            )
        except LeaseFencedError:
            log.warning("analysis lease %s fenced", lease.id)
        except Exception:
            log.exception("analysis lease %s failed", lease.id)
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
    async with ControlPlaneClient(settings) as client:
        await StatisticsWorker(settings, client).run_forever()


def main(argv: list[str] | None = None) -> None:
    parser = argparse.ArgumentParser(description="Sub2API Radar statistics worker")
    parser.add_argument("--version", action="version", version="0.1.0")
    parser.parse_args(argv)
    import logging

    logging.basicConfig(level=logging.INFO)
    asyncio.run(run(__import__("sub2api_radar.config", fromlist=["get_settings"]).get_settings()))
