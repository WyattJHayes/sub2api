from __future__ import annotations

from collections.abc import Sequence
from datetime import datetime
from decimal import Decimal
from enum import StrEnum
from typing import Annotated, Self
from uuid import UUID

from pydantic import Field, PlainSerializer, field_validator, model_validator

from ..models import QualityDimension as QualityDimension
from ..models import StrictModel

REQUIRED_DIMENSIONS: tuple[QualityDimension, ...] = tuple(QualityDimension)
QualityJSONDecimal = Annotated[
    Decimal,
    PlainSerializer(float, return_type=float, when_used="json"),
]


class QualityConclusion(StrEnum):
    NO_SIGNIFICANT_ANOMALY = "no_significant_anomaly"
    OBSERVE = "observe"
    SUSPECTED = "suspected"
    HIGH_RISK = "high_risk"
    INSUFFICIENT_COVERAGE = "insufficient_coverage"


class SourceState(StrEnum):
    CONFIRMED = "confirmed"
    INFERRED = "inferred"
    INSUFFICIENT_EVIDENCE = "insufficient_evidence"


class QualityEvidenceCode(StrEnum):
    WITHIN_POLICY_BOUNDS = "within_policy_bounds"
    COVERAGE_INSUFFICIENT = "coverage_insufficient"
    FINGERPRINT_MATCHED = "fingerprint_matched"
    FINGERPRINT_MISMATCH = "fingerprint_mismatch"
    REASONING_VARIANCE = "reasoning_variance"
    STRUCTURE_VIOLATION = "structure_violation"
    PARAMETER_DEVIATION = "parameter_deviation"
    INSTRUCTION_VIOLATION = "instruction_violation"
    PROTOCOL_VIOLATION = "protocol_violation"
    STREAM_INCOMPLETE = "stream_incomplete"
    SOURCE_CONFIRMED = "source_confirmed"
    SOURCE_INFERRED = "source_inferred"
    SOURCE_INSUFFICIENT_EVIDENCE = "source_insufficient_evidence"


EVIDENCE_CODES: frozenset[str] = frozenset(code.value for code in QualityEvidenceCode)
SENSITIVE_SOURCE_TOKENS: tuple[str, ...] = (
    "prompt",
    "completion",
    "api key",
    "api_key",
    "sk-",
    "route",
    "account",
    "channel",
    "artifact",
)


class ProbeEventClass(StrEnum):
    REQUEST_SHAPE = "request_shape"
    RESPONSE_SHAPE = "response_shape"
    STREAM_INTEGRITY = "stream_integrity"
    PARAMETER_ECHO = "parameter_echo"
    FINGERPRINT = "fingerprint"


class FingerprintCandidate(StrictModel):
    display_name: str = Field(min_length=1, max_length=200, pattern=r"^[A-Za-z0-9 ._:/-]+$")
    confidence: QualityJSONDecimal = Field(ge=Decimal("0"), le=Decimal("1"))

    @field_validator("display_name")
    @classmethod
    def reject_sensitive_display_name(cls, value: str) -> str:
        if any(token in value.lower() for token in SENSITIVE_SOURCE_TOKENS):
            raise ValueError("display_name contains sensitive content")
        return value


class SourceAttributionPolicy(StrictModel):
    minimum_coverage: QualityJSONDecimal = Field(ge=Decimal("0"), le=Decimal("1"))
    minimum_confidence: QualityJSONDecimal = Field(ge=Decimal("0"), le=Decimal("1"))
    minimum_margin: QualityJSONDecimal = Field(ge=Decimal("0.15"), le=Decimal("1"))


class SourceAttribution(StrictModel):
    state: SourceState
    display_name: str | None = Field(
        default=None,
        min_length=1,
        max_length=200,
        pattern=r"^[A-Za-z0-9 ._:/-]+$",
    )
    confidence: QualityJSONDecimal | None = Field(
        default=None, ge=Decimal("0"), le=Decimal("1")
    )
    coverage: QualityJSONDecimal | None = Field(
        default=None, ge=Decimal("0"), le=Decimal("1")
    )
    alternate_candidates: tuple[FingerprintCandidate, ...] = Field(max_length=2, default=())
    evidence_code: QualityEvidenceCode

    @field_validator("display_name")
    @classmethod
    def reject_sensitive_display_name(cls, value: str | None) -> str | None:
        if value is not None and any(token in value.lower() for token in SENSITIVE_SOURCE_TOKENS):
            raise ValueError("display_name contains sensitive content")
        return value

    @model_validator(mode="after")
    def validate_state_fields(self) -> Self:
        if self.state is SourceState.CONFIRMED:
            if self.display_name is None or self.alternate_candidates:
                raise ValueError("confirmed source requires one display name and no alternates")
        elif self.state is SourceState.INFERRED:
            if (
                self.display_name is None
                or self.confidence is None
                or self.coverage is None
                or not self.alternate_candidates
            ):
                raise ValueError("inferred source requires coverage, confidence, and alternates")
        elif (
            self.display_name is not None
            or self.confidence is not None
            or self.coverage is not None
            or self.alternate_candidates
        ):
            raise ValueError("insufficient evidence must withhold all candidates")
        return self

    @classmethod
    def confirmed(
        cls,
        display_name: str,
        *,
        evidence_code: QualityEvidenceCode | str = QualityEvidenceCode.SOURCE_CONFIRMED,
    ) -> Self:
        return cls(
            state=SourceState.CONFIRMED,
            display_name=display_name,
            evidence_code=QualityEvidenceCode(evidence_code),
        )

    @classmethod
    def insufficient_evidence(cls) -> Self:
        return cls(
            state=SourceState.INSUFFICIENT_EVIDENCE,
            evidence_code=QualityEvidenceCode.SOURCE_INSUFFICIENT_EVIDENCE,
        )


class QualityDimensionResult(StrictModel):
    key: QualityDimension
    score: QualityJSONDecimal = Field(ge=Decimal("0"), le=Decimal("1"))
    status: QualityConclusion
    sample_count: int = Field(ge=0)
    confidence: QualityJSONDecimal = Field(ge=Decimal("0"), le=Decimal("1"))
    stable_baseline_delta_pp: QualityJSONDecimal | None = None
    reference_baseline_delta_pp: QualityJSONDecimal | None = None
    checked_at: datetime
    evidence_code: QualityEvidenceCode


class QualityProbeObservation(StrictModel):
    probe_spec_hash: str = Field(pattern=r"^[0-9a-f]{64}$")
    observation_hash: str = Field(pattern=r"^[0-9a-f]{64}$")
    event_class: ProbeEventClass
    event_digest: str = Field(pattern=r"^[0-9a-f]{64}$")
    observed_at: datetime


class QualityReportPublication(StrictModel):
    run_id: UUID
    model_alias: str = Field(min_length=1)
    policy_version: str = Field(min_length=1)
    overall_conclusion: QualityConclusion
    adulteration_risk: QualityConclusion
    degradation_risk: QualityConclusion
    generated_at: datetime
    fresh_until: datetime
    dimension_results: tuple[QualityDimensionResult, ...]
    source_attribution: SourceAttribution
    source_attribution_policy: SourceAttributionPolicy
    probe_observations: tuple[QualityProbeObservation, ...] = ()

    @field_validator("dimension_results")
    @classmethod
    def validate_required_dimensions(
        cls, results: tuple[QualityDimensionResult, ...]
    ) -> tuple[QualityDimensionResult, ...]:
        keys = tuple(result.key for result in results)
        if len(set(keys)) != len(keys):
            raise ValueError("quality dimensions must be unique")
        if set(keys) != set(REQUIRED_DIMENSIONS):
            raise ValueError("quality report must include every required dimension")
        return results

    @model_validator(mode="after")
    def validate_publication(self) -> Self:
        if self.fresh_until < self.generated_at:
            raise ValueError("fresh_until must be after generated_at")
        if self.source_attribution.state is SourceState.INFERRED:
            attribution = self.source_attribution
            policy = self.source_attribution_policy
            if attribution.coverage is None or attribution.confidence is None:
                raise ValueError("inferred source requires coverage and confidence")
            if attribution.coverage < policy.minimum_coverage:
                raise ValueError("source_attribution coverage is below policy")
            if attribution.confidence < policy.minimum_confidence:
                raise ValueError("source_attribution confidence is below policy")
            required_margin = max(policy.minimum_margin, Decimal("0.15"))
            if any(
                attribution.confidence - candidate.confidence < required_margin
                for candidate in attribution.alternate_candidates
            ):
                raise ValueError("source_attribution alternate_candidates are below policy margin")
        return self


def infer_source(
    candidates: Sequence[FingerprintCandidate],
    *,
    coverage: Decimal,
    minimum_coverage: Decimal,
    minimum_confidence: Decimal,
    minimum_margin: Decimal,
) -> SourceAttribution:
    ranked = sorted(candidates, key=lambda candidate: candidate.confidence, reverse=True)
    required_margin = max(minimum_margin, Decimal("0.15"))
    if (
        coverage < minimum_coverage
        or len(ranked) < 2
        or ranked[0].confidence < minimum_confidence
        or ranked[0].confidence - ranked[1].confidence < required_margin
    ):
        return SourceAttribution.insufficient_evidence()

    return SourceAttribution(
        state=SourceState.INFERRED,
        display_name=ranked[0].display_name,
        confidence=ranked[0].confidence,
        coverage=coverage,
        alternate_candidates=tuple(ranked[1:3]),
        evidence_code=QualityEvidenceCode.SOURCE_INFERRED,
    )


def classify_overall(
    source: SourceAttribution,
    dimensions: Sequence[QualityDimensionResult],
) -> QualityConclusion:
    keys = tuple(result.key for result in dimensions)
    if len(keys) != len(REQUIRED_DIMENSIONS) or len(set(keys)) != len(keys):
        return QualityConclusion.INSUFFICIENT_COVERAGE
    if set(keys) != set(REQUIRED_DIMENSIONS):
        return QualityConclusion.INSUFFICIENT_COVERAGE
    statuses = tuple(result.status for result in dimensions)
    if QualityConclusion.INSUFFICIENT_COVERAGE in statuses:
        return QualityConclusion.INSUFFICIENT_COVERAGE
    if (
        source.state is SourceState.CONFIRMED
        and source.evidence_code is QualityEvidenceCode.FINGERPRINT_MISMATCH
    ) or statuses.count(QualityConclusion.HIGH_RISK) >= 2:
        return QualityConclusion.HIGH_RISK
    if (
        statuses.count(QualityConclusion.SUSPECTED) >= 2
        or statuses.count(QualityConclusion.HIGH_RISK) == 1
    ):
        return QualityConclusion.SUSPECTED
    if QualityConclusion.OBSERVE in statuses:
        return QualityConclusion.OBSERVE
    return QualityConclusion.NO_SIGNIFICANT_ANOMALY
