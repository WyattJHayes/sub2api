from __future__ import annotations

import argparse
import asyncio
import hashlib
import json
import logging
import os
from collections.abc import Callable, Mapping
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

from ..chaos.models import canonical_json_bytes, canonical_utc
from ..config import Settings, get_settings
from ..control_plane import ControlPlaneClient
from ..observability import MetricsServer, RadarMetrics
from .models import RecoveryEvidence, RecoveryObjectives, RecoveryObservation, RecoveryStatus

_SHA256_LENGTH = 64
_POSTGRES_INT_MAX = 2_147_483_647
_POSTGRES_BIGINT_MAX = 9_223_372_036_854_775_807
log = logging.getLogger(__name__)


class RecoveryVerifier:
    def __init__(
        self,
        *,
        objectives: RecoveryObjectives | None = None,
        verifier_identity: str,
        clock: Callable[[], datetime] | None = None,
        metrics: RadarMetrics | None = None,
    ) -> None:
        if not verifier_identity.strip():
            raise ValueError("verifier identity is required")
        self._objectives = objectives or RecoveryObjectives()
        self._verifier_identity = verifier_identity
        self._clock = clock or (lambda: datetime.now(UTC))
        self.metrics = metrics or RadarMetrics()

    def verify(
        self,
        observation: RecoveryObservation,
        *,
        verified_by: int | None,
    ) -> RecoveryEvidence:
        verified_at = self._clock()
        if not self._is_utc(verified_at):
            raise ValueError("verifier clock must use UTC")

        reasons: list[str] = []
        self._verify_watermark(observation, reasons)
        rpo_ms = self._calculate_rpo(observation, reasons)
        rto_ms = self._calculate_rto(observation, reasons)
        if rto_ms is not None:
            self.metrics.observe_recovery_duration(rto_ms / 1000)
        recovery_times = (
            observation.failover_declared_at,
            observation.control_plane_recovered_at,
            observation.worker_reregistered_at,
            observation.deterministic_acceptance_completed_at,
            observation.approval_completed_at,
        )
        if any(
            value is not None and self._is_utc(value) and value > verified_at
            for value in recovery_times
        ):
            reasons.append("verification_precedes_recovery_completion")

        if rpo_ms is not None and rpo_ms > self._objectives.rpo_ms:
            reasons.append("rpo_exceeded")
        if rto_ms is not None and rto_ms > self._objectives.rto_ms:
            reasons.append("rto_exceeded")
        if observation.duplicate_score_count < 0:
            reasons.append("duplicate_score_count_invalid")
        elif observation.duplicate_score_count > _POSTGRES_INT_MAX:
            reasons.append("duplicate_score_count_out_of_range")
            reasons.append("duplicate_scores")
        elif observation.duplicate_score_count != 0:
            reasons.append("duplicate_scores")

        self._verify_deterministic_acceptance(observation, reasons)
        self._verify_integrity(observation, reasons)
        if not self._valid_bigint_identity(verified_by):
            reasons.append("verifier_identity_invalid")

        status = RecoveryStatus.VERIFIED if not reasons else RecoveryStatus.REJECTED
        persisted_watermark = (
            observation.source_watermark
            if self._is_sha256(observation.source_watermark)
            else "0" * _SHA256_LENGTH
        )
        persisted_duplicate_count = min(
            _POSTGRES_INT_MAX,
            max(0, observation.duplicate_score_count),
        )
        persisted_verified_by = verified_by if self._valid_bigint_identity(verified_by) else None
        canonical = self._canonical_evidence(
            observation=observation,
            status=status,
            source_watermark=persisted_watermark,
            rpo_ms=rpo_ms,
            rto_ms=rto_ms,
            duplicate_score_count=persisted_duplicate_count,
            verified_by=persisted_verified_by,
            verified_at=verified_at,
            reasons=reasons,
        )
        evidence_hash = hashlib.sha256(canonical).hexdigest()
        return RecoveryEvidence(
            run_id=observation.run_id,
            experiment_id=observation.experiment_id,
            recovery_generation=observation.recovery_generation,
            source_watermark=persisted_watermark,
            status=status,
            rpo_ms=rpo_ms,
            rto_ms=rto_ms,
            duplicate_score_count=persisted_duplicate_count,
            deterministic_run_id=observation.deterministic_run_id,
            verified_by=persisted_verified_by,
            verified_at=verified_at,
            reason_codes=tuple(reasons),
            canonical_evidence_bytes=canonical,
            evidence_hash=evidence_hash,
        )

    def _verify_watermark(
        self, observation: RecoveryObservation, reasons: list[str]
    ) -> None:
        if not self._is_sha256(observation.source_watermark):
            reasons.append("source_watermark_invalid")
        if not self._is_sha256(observation.expected_source_watermark):
            reasons.append("expected_source_watermark_invalid")
        if observation.source_watermark != observation.expected_source_watermark:
            reasons.append("source_watermark_mismatch")

    def _calculate_rpo(
        self, observation: RecoveryObservation, reasons: list[str]
    ) -> int | None:
        failover = observation.failover_declared_at
        transaction = observation.last_persisted_transaction_at
        object_version = observation.last_available_object_version_at
        if transaction is None or object_version is None:
            reasons.append("rpo_source_missing")
            return None
        if not self._all_utc(failover, transaction, object_version):
            reasons.append("recovery_timestamps_not_utc")
            return None
        if transaction > failover or object_version > failover:
            reasons.append("rpo_timestamp_invalid")
            return None
        common_recovery_point = min(transaction, object_version)
        return self._milliseconds(failover - common_recovery_point)

    def _calculate_rto(
        self, observation: RecoveryObservation, reasons: list[str]
    ) -> int | None:
        milestones = (
            observation.control_plane_recovered_at,
            observation.worker_reregistered_at,
            observation.deterministic_acceptance_completed_at,
            observation.approval_completed_at,
        )
        if not self._is_utc(observation.failover_declared_at) or any(
            value is not None and not self._is_utc(value) for value in milestones
        ):
            if "recovery_timestamps_not_utc" not in reasons:
                reasons.append("recovery_timestamps_not_utc")
            return None
        if any(value is None for value in milestones):
            reasons.append("rto_milestone_missing")
            return None
        complete_milestones = tuple(value for value in milestones if value is not None)
        if any(value < observation.failover_declared_at for value in complete_milestones):
            reasons.append("rto_timestamp_invalid")
            return None
        completed_at = max(complete_milestones)
        return self._milliseconds(completed_at - observation.failover_declared_at)

    def _verify_deterministic_acceptance(
        self, observation: RecoveryObservation, reasons: list[str]
    ) -> None:
        if observation.deterministic_run_id is None:
            reasons.append("deterministic_run_missing")
        if observation.deterministic_run_status != "completed":
            reasons.append("deterministic_run_incomplete")
        if observation.deterministic_acceptance_completed_at is None:
            reasons.append("deterministic_acceptance_incomplete")
        if observation.deterministic_pair_count < self._objectives.deterministic_pair_count:
            reasons.append("deterministic_pair_count_insufficient")
        elif observation.deterministic_pair_count > self._objectives.deterministic_pair_count:
            reasons.append("deterministic_pair_count_unexpected")
        hashes = (
            observation.pre_disaster_acceptance_hash,
            observation.recovered_acceptance_hash,
        )
        if not all(self._is_sha256(value) for value in hashes):
            reasons.append("deterministic_hash_invalid")
        elif hashes[0] != hashes[1]:
            reasons.append("deterministic_hash_mismatch")

    @staticmethod
    def _verify_integrity(
        observation: RecoveryObservation, reasons: list[str]
    ) -> None:
        checks = (
            (observation.lease_recovery_ok, "lease_recovery_failed"),
            (observation.evidence_checksums_match, "checksum_mismatch"),
            (observation.ledger_idempotent, "ledger_not_idempotent"),
            (observation.object_references_consistent, "object_reference_mismatch"),
            (observation.policy_version_traceable, "policy_version_untraceable"),
            (observation.backup_evidence_fresh, "backup_evidence_stale"),
            (observation.alert_delivery_ok, "alert_delivery_failed"),
        )
        for passed, reason in checks:
            if passed is not True:
                reasons.append(reason)

    def _canonical_evidence(
        self,
        *,
        observation: RecoveryObservation,
        status: RecoveryStatus,
        source_watermark: str,
        rpo_ms: int | None,
        rto_ms: int | None,
        duplicate_score_count: int,
        verified_by: int | None,
        verified_at: datetime,
        reasons: list[str],
    ) -> bytes:
        milestones = {
            "approval_completed_at": self._optional_time(observation.approval_completed_at),
            "control_plane_recovered_at": self._optional_time(
                observation.control_plane_recovered_at
            ),
            "deterministic_acceptance_completed_at": self._optional_time(
                observation.deterministic_acceptance_completed_at
            ),
            "failover_declared_at": self._optional_time(observation.failover_declared_at),
            "last_available_object_version_at": self._optional_time(
                observation.last_available_object_version_at
            ),
            "last_persisted_transaction_at": self._optional_time(
                observation.last_persisted_transaction_at
            ),
            "worker_reregistered_at": self._optional_time(
                observation.worker_reregistered_at
            ),
        }
        return canonical_json_bytes(
            {
                "deterministic_run_id": (
                    str(observation.deterministic_run_id)
                    if observation.deterministic_run_id is not None
                    else None
                ),
                "duplicate_score_count": duplicate_score_count,
                "experiment_id": str(observation.experiment_id),
                "recovery_generation": observation.recovery_generation,
                "rpo_ms": rpo_ms,
                "rto_ms": rto_ms,
                "run_id": str(observation.run_id),
                "source_watermark": source_watermark,
                "status": status.value,
                "verification": {
                    "alert_delivery_ok": observation.alert_delivery_ok,
                    "backup_evidence_fresh": observation.backup_evidence_fresh,
                    "deterministic_pair_count": observation.deterministic_pair_count,
                    "deterministic_run_status": observation.deterministic_run_status,
                    "evidence_checksums_match": observation.evidence_checksums_match,
                    "expected_source_watermark": observation.expected_source_watermark,
                    "lease_recovery_ok": observation.lease_recovery_ok,
                    "ledger_idempotent": observation.ledger_idempotent,
                    "milestones": milestones,
                    "object_references_consistent": observation.object_references_consistent,
                    "observed_duplicate_score_count": observation.duplicate_score_count,
                    "observed_source_watermark": observation.source_watermark,
                    "policy_version_traceable": observation.policy_version_traceable,
                    "pre_disaster_acceptance_hash": observation.pre_disaster_acceptance_hash,
                    "reason_codes": reasons,
                    "recovered_acceptance_hash": observation.recovered_acceptance_hash,
                    "rpo_objective_ms": self._objectives.rpo_ms,
                    "rto_objective_ms": self._objectives.rto_ms,
                },
                "verified_at": canonical_utc(verified_at),
                "verified_by": verified_by,
                "verifier_identity": self._verifier_identity,
            }
        )

    @classmethod
    def _optional_time(cls, value: datetime | None) -> str | None:
        if value is None:
            return None
        if not cls._is_utc(value):
            return value.isoformat()
        return canonical_utc(value)

    @staticmethod
    def _is_sha256(value: str) -> bool:
        return len(value) == _SHA256_LENGTH and all(
            char in "0123456789abcdef" for char in value
        )

    @staticmethod
    def _valid_bigint_identity(value: int | None) -> bool:
        return (
            isinstance(value, int)
            and not isinstance(value, bool)
            and 0 < value <= _POSTGRES_BIGINT_MAX
        )

    @staticmethod
    def _is_utc(value: datetime) -> bool:
        return (
            value.tzinfo is not None
            and value.utcoffset() is not None
            and value.utcoffset() == timedelta(0)
        )

    @classmethod
    def _all_utc(cls, *values: datetime | None) -> bool:
        return all(value is not None and cls._is_utc(value) for value in values)

    @staticmethod
    def _milliseconds(value: timedelta) -> int:
        whole, remainder = divmod(value, timedelta(milliseconds=1))
        return whole + int(remainder > timedelta(0))


async def run(
    *,
    verifier: RecoveryVerifier | None = None,
    observation: RecoveryObservation | None = None,
    verified_by: int | None = None,
    settings: Settings | None = None,
) -> RecoveryEvidence:
    """Verify and publish recovery evidence through the control plane.

    A local observation file is supported only when explicitly configured and
    produces a durable JSON artifact in ``recovery_evidence_dir``. The normal
    path fetches the observation by ID and publishes the immutable evidence.
    """
    if (verifier is None) != (observation is None):
        raise RuntimeError("recovery verifier and observation must be supplied together")
    if verifier is not None and observation is not None:
        return verifier.verify(observation, verified_by=verified_by)

    try:
        settings = settings or get_settings()
    except Exception as exc:
        raise RuntimeError(
            "recovery verifier and observation must be injected or valid settings supplied"
        ) from exc
    if settings.recovery_evidence_id is None and not settings.recovery_observation_file:
        raise RuntimeError(
            "RADAR_RECOVERY_EVIDENCE_ID or RADAR_RECOVERY_OBSERVATION_FILE is required; "
            "inject an observation for embedded execution"
        )

    loaded_observation: RecoveryObservation
    metrics = RadarMetrics()
    control_plane: ControlPlaneClient | None = None
    if settings.recovery_evidence_id is not None:
        control_plane = ControlPlaneClient(settings, metrics=metrics)
        await control_plane.start()
        try:
            payload = await control_plane.get_recovery_observation(settings.recovery_evidence_id)
        except Exception:
            await control_plane.close()
            raise
        raw_observation: Any = payload.get("observation", payload)
        if not isinstance(raw_observation, Mapping):
            await control_plane.close()
            raise RuntimeError("control plane returned an invalid recovery observation")
        loaded_observation = RecoveryObservation.model_validate(raw_observation)
    else:
        assert settings.recovery_observation_file is not None
        try:
            with open(settings.recovery_observation_file, encoding="utf-8") as source:
                payload = json.load(source)
        except (OSError, json.JSONDecodeError) as exc:
            raise RuntimeError("recovery observation must be a valid JSON file") from exc
        if not isinstance(payload, Mapping):
            raise RuntimeError("recovery observation file must contain a JSON object")
        loaded_observation = RecoveryObservation.model_validate(payload)

    metrics_server = (
        MetricsServer(metrics, host=settings.metrics_host, port=settings.metrics_port)
        if settings.metrics_enabled
        else None
    )
    managed_verifier = verifier or RecoveryVerifier(
        verifier_identity=settings.worker_id, metrics=metrics
    )
    try:
        if metrics_server is not None:
            await metrics_server.start()
        evidence = managed_verifier.verify(loaded_observation, verified_by=verified_by)
        if control_plane is not None:
            assert settings.recovery_evidence_id is not None
            await control_plane.publish_recovery_evidence(
                settings.recovery_evidence_id,
                evidence.model_dump(mode="json"),
            )
        else:
            output_dir = Path(settings.recovery_evidence_dir)
            output_dir.mkdir(parents=True, exist_ok=True)
            output_path = output_dir / f"{evidence.evidence_hash}.json"
            output_path.write_bytes(evidence.canonical_evidence_bytes)
            log.warning(
                "recovery evidence was written to explicit local staging path %s", output_path
            )
        return evidence
    finally:
        if control_plane is not None:
            await control_plane.close()
        if metrics_server is not None:
            await metrics_server.close()


def main(argv: list[str] | None = None) -> None:
    parser = argparse.ArgumentParser(description="Sub2API Radar recovery verifier")
    parser.add_argument("--version", action="version", version="0.1.0")
    parser.parse_args(argv)
    value = os.environ.get("RADAR_RECOVERY_VERIFIER_TOKEN", "").strip()
    if len(value) < 32 or value.lower().startswith(("change-me", "placeholder", "test-")):
        raise SystemExit(
            "credential RADAR_RECOVERY_VERIFIER_TOKEN is required and must be a dedicated secret"
        )
    if not os.environ.get("RADAR_WORKER_TOKEN", "").strip():
        os.environ["RADAR_WORKER_TOKEN"] = value
    settings = get_settings()
    asyncio.run(run(settings=settings))


if __name__ == "__main__":
    main()
