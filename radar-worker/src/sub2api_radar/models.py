from __future__ import annotations

from datetime import datetime
from decimal import Decimal
from enum import StrEnum
from typing import Any
from uuid import UUID

from pydantic import AliasChoices, BaseModel, ConfigDict, Field, field_validator


class StrictModel(BaseModel):
    model_config = ConfigDict(extra="forbid", frozen=True, populate_by_name=True)


class FailureClass(StrEnum):
    CAPABILITY = "capability"
    PROTOCOL = "protocol"
    UPSTREAM = "upstream"
    INFRASTRUCTURE = "infrastructure"
    JUDGE = "judge"
    INVALID_EVIDENCE = "invalid_evidence"


class LeaseStatus(StrEnum):
    CLAIMED = "claimed"
    RUNNING = "running"
    EVIDENCE_UPLOADED = "evidence_uploaded"
    GRADING = "grading"
    COMPLETED = "completed"


class ArtifactReceipt(StrictModel):
    id: UUID
    object_key: str
    sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    bytes: int = Field(ge=0)
    mime_type: str
    scan_status: str
    scan_reason: str | None = None
    scanner: str | None = None
    scanned_at: datetime | None = None
    confirmed_at: datetime | None = None
    deleted_at: datetime | None = None


class ArtifactUpload(StrictModel):
    artifact_id: UUID
    object_key: str = Field(min_length=1)
    upload_url: str = Field(min_length=1)
    upload_headers: dict[str, str] = Field(default_factory=dict)
    sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    bytes: int = Field(gt=0)
    mime_type: str = Field(min_length=1)
    expires_at: datetime


class ArtifactDownload(StrictModel):
    artifact_id: UUID
    object_key: str = Field(min_length=1)
    download_url: str = Field(min_length=1)
    sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    bytes: int = Field(gt=0)
    mime_type: str = Field(min_length=1)
    expires_at: datetime


class CaseSpec(StrictModel):
    case_id: UUID
    case_key: str
    capability_domain: str
    priority: str
    weight: Decimal = Field(gt=0)
    prompt_spec: dict[str, Any] | list[Any] | str | None = None
    expected_spec: dict[str, Any] | list[Any] | str | None = None
    execution_spec: dict[str, Any] = Field(default_factory=dict)
    grader_id: str
    grader_version: str
    content_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    confidentiality: str


class AssignmentLease(StrictModel):
    id: UUID
    sample_id: UUID
    run_id: UUID
    case: CaseSpec
    model_route: str
    route_config: dict[str, Any] = Field(default_factory=dict, alias="model_config")
    model_config_sha256: str = Field(default="", pattern=r"^$|^[0-9a-f]{64}$")
    attempt: int = Field(ge=1)
    lease_token: str = Field(
        min_length=16, validation_alias=AliasChoices("lease_token", "token")
    )
    lease_expires_at: datetime = Field(
        validation_alias=AliasChoices("lease_expires_at", "expires_at")
    )
    lease_epoch: int = Field(default=0, ge=0)
    worker_image_digest: str = ""
    work_origin: str = ""
    gateway_api_key: str = ""
    gateway_evaluation_token: str = Field(min_length=1)
    route_trace_id: str = Field(min_length=1)
    dataset_version_id: UUID | None = None
    dataset_key: str = ""
    dataset_version: str = ""
    dataset_manifest_sha256: str = Field(default="", pattern=r"^$|^[0-9a-f]{64}$")


class EvidenceReceipt(StrictModel):
    assignment_id: UUID
    evidence_manifest_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    artifact_ids: tuple[UUID, ...] = ()
    accepted_at: datetime


class PairedScore(StrictModel):
    case_id: UUID
    model_route: str
    sample_index: int = Field(ge=0)
    weight: Decimal = Field(gt=0)
    baseline_score: Decimal = Field(ge=0, le=1)
    candidate_score: Decimal = Field(ge=0, le=1)


class GradingLease(StrictModel):
    id: UUID
    sample_id: UUID
    run_id: UUID
    case: CaseSpec | None = None
    assignment_id: UUID | None = None
    route_trace_id: str | None = None
    grader_id: str = ""
    grader_version: str = ""
    attempt: int = Field(default=1, ge=1)
    evidence_manifest: dict[str, Any] | None = None
    evidence: tuple[ArtifactReceipt, ...] = ()
    lease_token: str = Field(
        min_length=16, validation_alias=AliasChoices("lease_token", "token")
    )
    lease_expires_at: datetime = Field(
        validation_alias=AliasChoices("lease_expires_at", "expires_at")
    )
    lease_epoch: int = Field(default=0, ge=0)
    revision_batch_id: UUID | None = None
    grading_input_hash: str = Field(default="", pattern=r"^$|^[0-9a-f]{64}$")
    recovery_generation: int = Field(default=0, ge=0)
    worker_image_digest: str = ""
    work_origin: str = ""


class ScoreRef(StrictModel):
    id: UUID = Field(
        validation_alias=AliasChoices("score_id", "id"),
        serialization_alias="score_id",
    )
    created_at: datetime = Field(
        validation_alias=AliasChoices("score_created_at", "created_at"),
        serialization_alias="score_created_at",
    )


class SnapshotRef(StrictModel):
    id: UUID = Field(
        validation_alias=AliasChoices("snapshot_id", "id"),
        serialization_alias="snapshot_id",
    )
    window_start: datetime


class AnalysisLease(StrictModel):
    id: UUID
    run_id: UUID
    capability_domain: str = ""
    model_route: str = ""
    window: str = ""
    analysis_version: str = "v1"
    window_start: datetime | None = None
    lease_token: str = Field(
        min_length=16, validation_alias=AliasChoices("lease_token", "token")
    )
    lease_expires_at: datetime = Field(
        validation_alias=AliasChoices("lease_expires_at", "expires_at")
    )
    lease_epoch: int = Field(default=0, ge=0)
    worker_image_digest: str = ""
    work_origin: str = ""
    scope: str = Field(default="cell", pattern=r"^(cell|global)$")
    input_set_hash: str = Field(default="", pattern=r"^$|^[0-9a-f]{64}$")
    aggregate_revision: int = Field(default=0, ge=0)
    revision_batch_id: UUID | None = None
    score_ids: tuple[UUID, ...] = ()
    score_refs: tuple[ScoreRef, ...] = ()
    snapshot_refs: tuple[SnapshotRef, ...] = ()
    pairs: tuple[PairedScore, ...] = ()
    history: tuple[dict[str, Any], ...] = ()
    invalid_failures: tuple[FailureClass, ...] = ()


class ExecutionEvidence(StrictModel):
    assignment_id: UUID
    sample_id: UUID
    case_content_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    execution_image_digest: str = Field(min_length=20)
    request_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    response_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    route_trace_id: str = Field(min_length=1)
    started_at: datetime
    finished_at: datetime
    latency_ms: int = Field(ge=0)
    ttft_ms: int | None = Field(default=None, ge=0)
    input_tokens: int | None = Field(default=None, ge=0)
    output_tokens: int | None = Field(default=None, ge=0)
    transport_status: str
    finish_reason: str | None = None
    response_headers: dict[str, str] = Field(default_factory=dict)
    final_output: str | None = None
    tool_calls: tuple[dict[str, Any], ...] = ()
    protocol_events: tuple[dict[str, Any], ...] = ()
    error_code: str | None = None
    artifacts: tuple[ArtifactReceipt, ...] = ()
    environment_fingerprint: str = Field(min_length=1)


class ScoreSubmission(StrictModel):
    score: Decimal = Field(ge=0, le=1)
    passed: bool | None = None
    failure_class: FailureClass | None = None
    failure_code: str = ""
    explanation: str = Field(default="", max_length=4000)
    evidence_hashes: tuple[str, ...] = ()

    @field_validator("evidence_hashes")
    @classmethod
    def validate_hashes(cls, values: tuple[str, ...]) -> tuple[str, ...]:
        for value in values:
            if len(value) != 64 or any(char not in "0123456789abcdef" for char in value):
                raise ValueError("evidence hashes must be lowercase SHA-256 values")
        return values


class ScoreReceipt(StrictModel):
    score_ref: ScoreRef
    head_version: int = Field(ge=1)


class AggregateSubmission(StrictModel):
    run_id: UUID | None = None
    capability_domain: str
    model_route: str
    window: str
    analysis_version: str
    window_start: datetime
    baseline_score: Decimal = Field(ge=0, le=1)
    candidate_score: Decimal = Field(ge=0, le=1)
    delta_pp: Decimal
    ci_low_pp: Decimal
    ci_high_pp: Decimal
    effective_pair_count: int = Field(ge=0)
    invalid_counts: dict[FailureClass, int] = Field(default_factory=dict)
    evidence_sufficiency: str
    ewma: Decimal | None = None
    cusum: Decimal | None = None
    seed: int
    input_set_hash: str = Field(default="", pattern=r"^$|^[0-9a-f]{64}$")
    score_ids: tuple[UUID, ...] = ()
    score_refs: tuple[ScoreRef, ...] = ()
    snapshot_refs: tuple[SnapshotRef, ...] = ()
    aggregate: dict[str, Any] = Field(default_factory=dict)


class ArtifactPresignRequest(StrictModel):
    mime_type: str
    bytes: int = Field(gt=0)
    sha256: str = Field(pattern=r"^[0-9a-f]{64}$")


class ArtifactConfirmation(StrictModel):
    artifact_id: UUID
    object_key: str
    sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    bytes: int = Field(ge=0)
