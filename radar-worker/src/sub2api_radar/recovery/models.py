from __future__ import annotations

import hashlib
from datetime import datetime
from enum import StrEnum
from typing import Literal
from uuid import UUID

from pydantic import Field, model_validator

from ..models import StrictModel

_POSTGRES_INT_MAX = 2_147_483_647
_POSTGRES_BIGINT_MAX = 9_223_372_036_854_775_807


class RecoveryStatus(StrEnum):
    PENDING = "pending"
    VERIFIED = "verified"
    REJECTED = "rejected"


class RecoveryObjectives(StrictModel):
    rpo_ms: int = Field(default=300_000, strict=True, ge=0)
    rto_ms: int = Field(default=1_800_000, strict=True, ge=0)
    deterministic_pair_count: Literal[30] = 30


class RecoveryObservation(StrictModel):
    run_id: UUID
    experiment_id: UUID
    recovery_generation: int = Field(strict=True, ge=0, le=_POSTGRES_INT_MAX)
    source_watermark: str
    expected_source_watermark: str
    failover_declared_at: datetime
    last_persisted_transaction_at: datetime | None = None
    last_available_object_version_at: datetime | None = None
    control_plane_recovered_at: datetime | None = None
    worker_reregistered_at: datetime | None = None
    deterministic_acceptance_completed_at: datetime | None = None
    approval_completed_at: datetime | None = None
    duplicate_score_count: int = Field(strict=True)
    deterministic_run_id: UUID | None = None
    deterministic_run_status: str = Field(min_length=1, max_length=24)
    deterministic_pair_count: int = Field(strict=True)
    pre_disaster_acceptance_hash: str
    recovered_acceptance_hash: str
    lease_recovery_ok: bool = Field(strict=True)
    evidence_checksums_match: bool = Field(strict=True)
    ledger_idempotent: bool = Field(strict=True)
    object_references_consistent: bool = Field(strict=True)
    policy_version_traceable: bool = Field(strict=True)
    backup_evidence_fresh: bool = Field(strict=True)
    alert_delivery_ok: bool = Field(strict=True)


class RecoveryEvidence(StrictModel):
    run_id: UUID
    experiment_id: UUID
    recovery_generation: int = Field(strict=True, ge=0, le=_POSTGRES_INT_MAX)
    source_watermark: str = Field(pattern=r"^[0-9a-f]{64}$")
    status: RecoveryStatus
    rpo_ms: int | None = Field(default=None, ge=0)
    rto_ms: int | None = Field(default=None, ge=0)
    duplicate_score_count: int = Field(strict=True, ge=0, le=_POSTGRES_INT_MAX)
    deterministic_run_id: UUID | None = None
    verified_by: int | None = Field(
        default=None,
        strict=True,
        gt=0,
        le=_POSTGRES_BIGINT_MAX,
    )
    verified_at: datetime
    reason_codes: tuple[str, ...] = ()
    canonical_evidence_bytes: bytes = Field(min_length=1)
    evidence_hash: str = Field(pattern=r"^[0-9a-f]{64}$")

    def canonical_bytes(self) -> bytes:
        return self.canonical_evidence_bytes

    @model_validator(mode="after")
    def evidence_hash_matches_canonical_bytes(self) -> RecoveryEvidence:
        expected = hashlib.sha256(self.canonical_evidence_bytes).hexdigest()
        if self.evidence_hash != expected:
            raise ValueError("evidence_hash does not match canonical evidence bytes")
        if self.status is RecoveryStatus.VERIFIED:
            if self.rpo_ms is None or self.rto_ms is None:
                raise ValueError("verified recovery evidence requires RPO and RTO")
            if self.deterministic_run_id is None or self.verified_by is None:
                raise ValueError("verified recovery evidence requires run and verifier identities")
            if self.reason_codes:
                raise ValueError("verified recovery evidence cannot contain rejection reasons")
        return self
