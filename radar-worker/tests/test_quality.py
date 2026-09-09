from __future__ import annotations

from datetime import UTC, datetime, timedelta
from decimal import Decimal
from uuid import UUID

import pytest
from pydantic import ValidationError

from sub2api_radar.models import (
    CaseSpec,
    QualityAnalysisContext,
    QualityAnalysisDimensionInput,
    QualityProbeEventClass,
    QualitySourceCandidate,
)
from sub2api_radar.models import (
    QualityPolicy as LeaseQualityPolicy,
)
from sub2api_radar.statistics.quality import (
    EVIDENCE_CODES,
    REQUIRED_DIMENSIONS,
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
    SourceState,
    classify_overall,
    infer_source,
)
from sub2api_radar.statistics.service import AggregateAnalysis, build_quality_report

NOW = datetime(2026, 8, 11, tzinfo=UTC)
POLICY = SourceAttributionPolicy(
    minimum_coverage=Decimal("0.80"),
    minimum_confidence=Decimal("0.70"),
    minimum_margin=Decimal("0.15"),
)


def dimension_result(
    key: QualityDimension,
    status: QualityConclusion = QualityConclusion.NO_SIGNIFICANT_ANOMALY,
    evidence_code: str = "within_policy_bounds",
) -> QualityDimensionResult:
    return QualityDimensionResult(
        key=key,
        score=Decimal("0.95"),
        status=status,
        sample_count=12,
        confidence=Decimal("0.90"),
        stable_baseline_delta_pp=Decimal("1.20"),
        reference_baseline_delta_pp=Decimal("0.80"),
        checked_at=NOW,
        evidence_code=evidence_code,
    )


def all_dimension_results(
    status: QualityConclusion = QualityConclusion.NO_SIGNIFICANT_ANOMALY,
) -> tuple[QualityDimensionResult, ...]:
    return tuple(dimension_result(key, status=status) for key in REQUIRED_DIMENSIONS)


def insufficient_source() -> SourceAttribution:
    return SourceAttribution.insufficient_evidence()


def confirmed_mismatch() -> SourceAttribution:
    return SourceAttribution.confirmed("Claude-3.7-Sonnet", evidence_code="fingerprint_mismatch")


def quality_report(source: SourceAttribution) -> QualityReportPublication:
    return QualityReportPublication(
        run_id=UUID("11111111-1111-1111-1111-111111111111"),
        model_alias="gpt-4.1",
        policy_version="quality-v1",
        overall_conclusion=QualityConclusion.NO_SIGNIFICANT_ANOMALY,
        adulteration_risk=QualityConclusion.NO_SIGNIFICANT_ANOMALY,
        degradation_risk=QualityConclusion.NO_SIGNIFICANT_ANOMALY,
        generated_at=NOW,
        fresh_until=NOW + timedelta(days=1),
        dimension_results=all_dimension_results(),
        source_attribution=source,
        source_attribution_policy=POLICY,
    )


def inferred_source(
    *,
    coverage: Decimal = Decimal("0.90"),
    confidence: Decimal = Decimal("0.90"),
    alternates: tuple[FingerprintCandidate, ...] = (
        FingerprintCandidate(display_name="Claude-3.7-Sonnet", confidence=Decimal("0.70")),
    ),
) -> SourceAttribution:
    return SourceAttribution(
        state=SourceState.INFERRED,
        display_name="GPT-4.1",
        confidence=confidence,
        coverage=coverage,
        alternate_candidates=alternates,
        evidence_code="source_inferred",
    )


def quality_context(
    *,
    sample_count: int = 3,
    candidate_confidences: tuple[Decimal, ...] = (Decimal("0.90"), Decimal("0.70")),
) -> QualityAnalysisContext:
    return QualityAnalysisContext(
        run_id=UUID("11111111-1111-1111-1111-111111111111"),
        model_alias="model-a",
        policy_version="quality-v1",
        policy=LeaseQualityPolicy(
            minimum_coverage=Decimal("0.80"),
            minimum_confidence=Decimal("0.70"),
            minimum_margin=Decimal("0.15"),
            minimum_samples_per_dimension=3,
            observe_delta_pp=Decimal("5"),
            suspected_delta_pp=Decimal("10"),
            high_risk_delta_pp=Decimal("20"),
            freshness_hours=24,
        ),
        dimensions=tuple(
            QualityAnalysisDimensionInput(
                key=key,
                baseline_score=Decimal("0.80"),
                candidate_score=Decimal("0.70"),
                sample_count=sample_count,
                reference_baseline_delta_pp=Decimal("-10"),
                probe_event_class=QualityProbeEventClass.RESPONSE_SHAPE,
                probe_spec_hash="a" * 64,
                observation_hash="b" * 64,
                observed_at=NOW,
            )
            for key in REQUIRED_DIMENSIONS
        ),
        source_candidates=tuple(
            QualitySourceCandidate(
                display_name=f"Candidate {index}",
                confidence=confidence,
                sample_count=sample_count,
                baseline_score=Decimal("0.80"),
                candidate_score=Decimal("0.70"),
                probe_event_class=QualityProbeEventClass.FINGERPRINT,
                probe_spec_hash="abcdef"[(index - 1) % 6] * 64,
                observation_hash="fedcba"[(index - 1) % 6] * 64,
                observed_at=NOW,
            )
            for index, confidence in enumerate(candidate_confidences, start=1)
        ),
    )


def aggregate_analysis() -> AggregateAnalysis:
    return AggregateAnalysis(
        baseline_score=Decimal("0.80"),
        candidate_score=Decimal("0.70"),
        delta_pp=Decimal("-10"),
        ci_low_pp=Decimal("-12"),
        ci_high_pp=Decimal("-8"),
        effective_pair_count=8,
        invalid_counts={},
        ewma=None,
        cusum=Decimal("0"),
        cusum_signal=False,
        evidence_sufficiency="sufficient",
        seed=1,
    )


def test_build_quality_report_uses_complete_context_and_digest_only_observations() -> None:
    report = build_quality_report(quality_context(), aggregate_analysis(), NOW)

    assert report is not None
    assert report.run_id == UUID("11111111-1111-1111-1111-111111111111")
    assert report.fresh_until == NOW + timedelta(hours=24)
    assert {result.key for result in report.dimension_results} == set(REQUIRED_DIMENSIONS)
    assert report.source_attribution.state is SourceState.INFERRED
    assert report.adulteration_risk is QualityConclusion.SUSPECTED
    assert report.degradation_risk is QualityConclusion.SUSPECTED
    assert len(report.probe_observations) == len(REQUIRED_DIMENSIONS)
    assert all(observation.event_digest == "b" * 64 for observation in report.probe_observations)


def test_build_quality_report_marks_insufficient_samples_before_risk_classification() -> None:
    report = build_quality_report(quality_context(sample_count=2), aggregate_analysis(), NOW)

    assert report is not None
    assert report.overall_conclusion is QualityConclusion.INSUFFICIENT_COVERAGE
    assert all(
        result.status is QualityConclusion.INSUFFICIENT_COVERAGE
        for result in report.dimension_results
    )
    assert all(
        result.evidence_code is QualityEvidenceCode.COVERAGE_INSUFFICIENT
        for result in report.dimension_results
    )


def test_build_quality_report_withholds_source_when_only_fingerprint_samples_are_insufficient(
) -> None:
    context = quality_context()
    context = context.model_copy(
        update={
            "dimensions": tuple(
                item.model_copy(update={"sample_count": 2})
                if item.key is QualityDimension.MODEL_FINGERPRINT
                else item
                for item in context.dimensions
            )
        }
    )

    report = build_quality_report(context, aggregate_analysis(), NOW)

    assert report is not None
    assert report.source_attribution == SourceAttribution.insufficient_evidence()
    assert report.source_attribution.display_name is None
    assert report.source_attribution.confidence is None
    assert report.source_attribution.coverage is None
    assert report.source_attribution.alternate_candidates == ()


def test_build_quality_report_withholds_source_when_candidate_observations_are_incomplete() -> None:
    context = quality_context().model_copy(
        update={
            "source_candidates": tuple(
                candidate.model_copy(update={"observation_hash": None})
                for candidate in quality_context().source_candidates
            )
        }
    )

    report = build_quality_report(context, aggregate_analysis(), NOW)

    assert report is not None
    assert report.source_attribution == SourceAttribution.insufficient_evidence()


def test_build_quality_report_withholds_source_for_nonfingerprint_candidate_observations() -> None:
    context = quality_context().model_copy(
        update={
            "source_candidates": tuple(
                candidate.model_copy(
                    update={"probe_event_class": QualityProbeEventClass.RESPONSE_SHAPE}
                )
                for candidate in quality_context().source_candidates
            )
        }
    )

    report = build_quality_report(context, aggregate_analysis(), NOW)

    assert report is not None
    assert report.source_attribution == SourceAttribution.insufficient_evidence()


def test_build_quality_report_withholds_source_when_any_candidate_is_incomplete() -> None:
    candidate_confidences = (
        Decimal("0.90"),
        Decimal("0.70"),
        Decimal("0.50"),
        Decimal("0.40"),
        Decimal("0.30"),
    )
    complete_context = quality_context(candidate_confidences=candidate_confidences)
    context = complete_context.model_copy(
        update={
            "source_candidates": tuple(
                candidate.model_copy(update={"observation_hash": None})
                if candidate.display_name == "Candidate 5"
                else candidate
                for candidate in complete_context.source_candidates
            )
        }
    )

    report = build_quality_report(context, aggregate_analysis(), NOW)

    assert report is not None
    assert report.source_attribution == SourceAttribution.insufficient_evidence()


@pytest.mark.parametrize(
    "candidate_confidences",
    (
        (Decimal("0.69"), Decimal("0.40")),
        (Decimal("0.84"), Decimal("0.70")),
    ),
    ids=("low_confidence", "insufficient_margin"),
)
def test_build_quality_report_withholds_source_without_threshold_evidence(
    candidate_confidences: tuple[Decimal, ...],
) -> None:
    report = build_quality_report(
        quality_context(candidate_confidences=candidate_confidences), aggregate_analysis(), NOW
    )

    assert report is not None
    assert report.source_attribution == SourceAttribution.insufficient_evidence()


def test_build_quality_report_omits_a_context_that_bypassed_dimension_validation() -> None:
    valid_context = quality_context()
    invalid_context = QualityAnalysisContext.model_construct(
        run_id=valid_context.run_id,
        model_alias=valid_context.model_alias,
        policy_version=valid_context.policy_version,
        policy=valid_context.policy,
        dimensions=(valid_context.dimensions[0],) * len(REQUIRED_DIMENSIONS),
        source_candidates=valid_context.source_candidates,
    )

    assert build_quality_report(invalid_context, aggregate_analysis(), NOW) is None


def test_infer_source_returns_ranked_inference_when_all_thresholds_hold() -> None:
    attribution = infer_source(
        (
            FingerprintCandidate(display_name="GPT-4.1", confidence=Decimal("0.24")),
            FingerprintCandidate(display_name="Claude-3.7-Sonnet", confidence=Decimal("0.88")),
            FingerprintCandidate(display_name="Gemini-2.5-Pro", confidence=Decimal("0.60")),
        ),
        coverage=Decimal("0.90"),
        minimum_coverage=POLICY.minimum_coverage,
        minimum_confidence=POLICY.minimum_confidence,
        minimum_margin=POLICY.minimum_margin,
    )

    assert attribution.state is SourceState.INFERRED
    assert attribution.display_name == "Claude-3.7-Sonnet"
    assert attribution.confidence == Decimal("0.88")
    assert attribution.coverage == Decimal("0.90")
    assert [candidate.display_name for candidate in attribution.alternate_candidates] == [
        "Gemini-2.5-Pro",
        "GPT-4.1",
    ]
    assert attribution.evidence_code == "source_inferred"


@pytest.mark.parametrize(
    ("candidates", "coverage", "minimum_confidence", "minimum_margin"),
    [
        (
            (FingerprintCandidate(display_name="GPT-4.1", confidence=Decimal("0.90")),
             FingerprintCandidate(display_name="Claude-3.7-Sonnet", confidence=Decimal("0.50"))),
            Decimal("0.79"),
            Decimal("0.70"),
            Decimal("0.15"),
        ),
        (
            (FingerprintCandidate(display_name="GPT-4.1", confidence=Decimal("0.69")),
             FingerprintCandidate(display_name="Claude-3.7-Sonnet", confidence=Decimal("0.30"))),
            Decimal("0.90"),
            Decimal("0.70"),
            Decimal("0.15"),
        ),
        (
            (FingerprintCandidate(display_name="GPT-4.1", confidence=Decimal("0.90")),),
            Decimal("0.90"),
            Decimal("0.70"),
            Decimal("0.15"),
        ),
        (
            (FingerprintCandidate(display_name="GPT-4.1", confidence=Decimal("0.84")),
             FingerprintCandidate(display_name="Claude-3.7-Sonnet", confidence=Decimal("0.70"))),
            Decimal("0.90"),
            Decimal("0.70"),
            Decimal("0.15"),
        ),
    ],
    ids=("low_coverage", "low_confidence", "missing_runner_up", "margin_below_015"),
)
def test_infer_source_withholds_candidates_when_any_inference_threshold_fails(
    candidates: tuple[FingerprintCandidate, ...],
    coverage: Decimal,
    minimum_confidence: Decimal,
    minimum_margin: Decimal,
) -> None:
    attribution = infer_source(
        candidates,
        coverage=coverage,
        minimum_coverage=Decimal("0.80"),
        minimum_confidence=minimum_confidence,
        minimum_margin=minimum_margin,
    )

    assert attribution == SourceAttribution.insufficient_evidence()


def test_infer_source_enforces_the_global_minimum_margin() -> None:
    attribution = infer_source(
        (
            FingerprintCandidate(display_name="GPT-4.1", confidence=Decimal("0.82")),
            FingerprintCandidate(display_name="Claude-3.7-Sonnet", confidence=Decimal("0.69")),
        ),
        coverage=Decimal("0.90"),
        minimum_coverage=Decimal("0.80"),
        minimum_confidence=Decimal("0.70"),
        minimum_margin=Decimal("0.10"),
    )

    assert attribution == SourceAttribution.insufficient_evidence()


@pytest.mark.parametrize(
    "source",
    (
        inferred_source(coverage=Decimal("0.79")),
        inferred_source(confidence=Decimal("0.69")),
        inferred_source(
            alternates=(
                FingerprintCandidate(
                    display_name="Claude-3.7-Sonnet", confidence=Decimal("0.76")
                ),
            )
        ),
    ),
    ids=("low_coverage", "low_confidence", "margin_below_policy"),
)
def test_quality_report_rejects_direct_inferred_source_below_policy(
    source: SourceAttribution,
) -> None:
    with pytest.raises(ValidationError, match="source_attribution"):
        quality_report(source)


def test_quality_report_rejects_more_than_two_inferred_alternates() -> None:
    with pytest.raises(ValidationError, match="alternate_candidates"):
        SourceAttribution(
            state=SourceState.INFERRED,
            display_name="GPT-4.1",
            confidence=Decimal("0.90"),
            coverage=Decimal("0.90"),
            alternate_candidates=(
                FingerprintCandidate(
                    display_name="Claude-3.7-Sonnet", confidence=Decimal("0.70")
                ),
                FingerprintCandidate(
                    display_name="Gemini-2.5-Pro", confidence=Decimal("0.50")
                ),
                FingerprintCandidate(display_name="Llama-4", confidence=Decimal("0.30")),
            ),
            evidence_code="source_inferred",
        )


def test_classify_overall_prioritizes_insufficient_coverage() -> None:
    dimensions = list(all_dimension_results(status=QualityConclusion.HIGH_RISK))
    dimensions[0] = dimension_result(
        QualityDimension.KNOWLEDGE_FRESHNESS,
        status=QualityConclusion.INSUFFICIENT_COVERAGE,
        evidence_code="coverage_insufficient",
    )

    assert (
        classify_overall(confirmed_mismatch(), dimensions)
        is QualityConclusion.INSUFFICIENT_COVERAGE
    )


@pytest.mark.parametrize(
    "dimensions",
    (
        (),
        all_dimension_results()[1:],
        all_dimension_results()
        + (dimension_result(QualityDimension.KNOWLEDGE_FRESHNESS),),
        all_dimension_results()[1:]
        + (
            dimension_result(
                QualityDimension.KNOWLEDGE_FRESHNESS,
                status=QualityConclusion.HIGH_RISK,
            ),
            dimension_result(
                QualityDimension.MODEL_FINGERPRINT,
                status=QualityConclusion.HIGH_RISK,
            ),
        ),
    ),
    ids=("empty", "missing_dimension", "duplicate_dimension", "missing_dimension_overrides_severe"),
)
def test_classify_overall_returns_insufficient_coverage_for_incomplete_dimensions(
    dimensions: tuple[QualityDimensionResult, ...],
) -> None:
    assert (
        classify_overall(confirmed_mismatch(), dimensions)
        is QualityConclusion.INSUFFICIENT_COVERAGE
    )


def test_classify_overall_returns_high_risk_for_confirmed_source_mismatch() -> None:
    assert (
        classify_overall(confirmed_mismatch(), all_dimension_results())
        is QualityConclusion.HIGH_RISK
    )


def test_classify_overall_returns_high_risk_for_two_severe_dimensions() -> None:
    dimensions = list(all_dimension_results())
    dimensions[0] = dimension_result(
        QualityDimension.KNOWLEDGE_FRESHNESS, QualityConclusion.HIGH_RISK
    )
    dimensions[1] = dimension_result(
        QualityDimension.MODEL_FINGERPRINT, QualityConclusion.HIGH_RISK
    )

    assert classify_overall(insufficient_source(), dimensions) is QualityConclusion.HIGH_RISK


def test_classify_overall_returns_suspected_for_repeated_significant_deviation() -> None:
    dimensions = list(all_dimension_results())
    dimensions[0] = dimension_result(
        QualityDimension.KNOWLEDGE_FRESHNESS, QualityConclusion.SUSPECTED
    )
    dimensions[2] = dimension_result(
        QualityDimension.REASONING_STABILITY, QualityConclusion.SUSPECTED
    )

    assert classify_overall(insufficient_source(), dimensions) is QualityConclusion.SUSPECTED


def test_classify_overall_returns_observe_for_a_single_mild_deviation() -> None:
    dimensions = list(all_dimension_results())
    dimensions[0] = dimension_result(
        QualityDimension.KNOWLEDGE_FRESHNESS, QualityConclusion.OBSERVE
    )

    assert classify_overall(insufficient_source(), dimensions) is QualityConclusion.OBSERVE


def test_classify_overall_returns_no_significant_anomaly_without_deviations() -> None:
    assert (
        classify_overall(insufficient_source(), all_dimension_results())
        is QualityConclusion.NO_SIGNIFICANT_ANOMALY
    )


def test_quality_dimension_result_accepts_every_required_dimension() -> None:
    assert {result.key for result in all_dimension_results()} == set(REQUIRED_DIMENSIONS)


def test_quality_models_reject_evidence_outside_the_allowlist() -> None:
    with pytest.raises(ValidationError, match="evidence_code"):
        dimension_result(
            QualityDimension.KNOWLEDGE_FRESHNESS,
            evidence_code="the_upstream_answer_says_northstar",
        )


@pytest.mark.parametrize(
    "display_name",
    ("prompt: summarize this request", "sk-secret-token", "route: upstream/model-a"),
)
def test_source_candidates_reject_sensitive_display_names(display_name: str) -> None:
    with pytest.raises(ValidationError, match="display_name"):
        FingerprintCandidate(display_name=display_name, confidence=Decimal("0.90"))


@pytest.mark.parametrize(
    "sensitive_field",
    (
        "prompt",
        "completion",
        "final_output",
        "api_key",
        "route_trace_id",
        "account_ref",
        "channel_ref",
    ),
)
def test_quality_publication_rejects_sensitive_extra_fields(sensitive_field: str) -> None:
    payload: dict[str, object] = {
        "run_id": UUID("11111111-1111-1111-1111-111111111111"),
        "model_alias": "gpt-4.1",
        "policy_version": "quality-v1",
        "overall_conclusion": "no_significant_anomaly",
        "adulteration_risk": "no_significant_anomaly",
        "degradation_risk": "no_significant_anomaly",
        "generated_at": NOW,
        "fresh_until": NOW + timedelta(days=1),
        "dimension_results": all_dimension_results(),
        "source_attribution": insufficient_source(),
        "source_attribution_policy": POLICY,
        sensitive_field: "secret",
    }

    with pytest.raises(ValidationError, match=sensitive_field):
        QualityReportPublication.model_validate(payload)


def test_probe_observation_allows_fixed_event_and_digest_fields_only() -> None:
    observation = QualityProbeObservation(
        probe_spec_hash="a" * 64,
        observation_hash="b" * 64,
        event_class=ProbeEventClass.FINGERPRINT,
        event_digest="c" * 64,
        observed_at=NOW,
    )

    assert observation.event_class is ProbeEventClass.FINGERPRINT
    with pytest.raises(ValidationError, match="completion"):
        QualityProbeObservation.model_validate({**observation.model_dump(), "completion": "raw"})


def test_case_spec_accepts_optional_quality_metadata() -> None:
    case = CaseSpec(
        case_id=UUID("11111111-1111-1111-1111-111111111111"),
        case_key="fingerprint-v1",
        capability_domain="quality",
        priority="high",
        weight=Decimal("1"),
        grader_id="exact",
        grader_version="v1",
        content_sha256="d" * 64,
        confidentiality="internal",
        quality_dimension="model_fingerprint",
        quality_probe_spec={"version": "v1"},
    )

    assert case.quality_dimension == "model_fingerprint"
    assert case.quality_probe_spec == {"version": "v1"}


def test_case_spec_rejects_unknown_quality_dimension() -> None:
    with pytest.raises(ValidationError, match="quality_dimension"):
        CaseSpec(
            case_id=UUID("11111111-1111-1111-1111-111111111111"),
            case_key="unknown-quality-v1",
            capability_domain="quality",
            priority="high",
            weight=Decimal("1"),
            grader_id="exact",
            grader_version="v1",
            content_sha256="d" * 64,
            confidentiality="internal",
            quality_dimension="unknown_dimension",
        )


def test_evidence_allowlist_matches_the_contract() -> None:
    assert EVIDENCE_CODES == frozenset(
        {
            "within_policy_bounds",
            "coverage_insufficient",
            "fingerprint_matched",
            "fingerprint_mismatch",
            "reasoning_variance",
            "structure_violation",
            "parameter_deviation",
            "instruction_violation",
            "protocol_violation",
            "stream_incomplete",
            "source_confirmed",
            "source_inferred",
            "source_insufficient_evidence",
        }
    )
