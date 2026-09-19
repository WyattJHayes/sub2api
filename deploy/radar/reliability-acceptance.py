#!/usr/bin/env python3
"""Fail-closed acceptance gate for a Radar staging reliability evidence bundle."""

from __future__ import annotations

import argparse
import hashlib
import hmac
import json
import os
import re
import sys
from datetime import UTC, datetime
from decimal import Decimal, InvalidOperation
from pathlib import Path
from typing import Any
from uuid import UUID

from reliability_hash import snapshot_hash as compute_snapshot_hash

SCHEMA_VERSION = "radar-staging-reliability-acceptance-v1"
FACT_MANIFEST_SCHEMA_VERSION = "radar-fact-manifest-v1"
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
IMAGE_DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")


class EvidenceValidator:
    def __init__(self, document: object) -> None:
        self.document = document
        self.failures: list[str] = []

    def fail(self, path: str, message: str) -> None:
        self.failures.append(f"{path}: {message}")

    def mapping(self, value: object, path: str) -> dict[str, Any] | None:
        if not isinstance(value, dict):
            self.fail(path, "must be an object")
            return None
        return value

    def required(self, mapping: dict[str, Any], key: str, path: str) -> object | None:
        if key not in mapping or mapping[key] is None:
            self.fail(f"{path}.{key}", "is required")
            return None
        return mapping[key]

    def nonempty_string(self, value: object, path: str) -> str | None:
        if not isinstance(value, str) or not value.strip():
            self.fail(path, "must be a non-empty string")
            return None
        return value

    def integer(self, value: object, path: str, *, minimum: int = 0) -> int | None:
        if isinstance(value, bool) or not isinstance(value, int):
            self.fail(path, "must be an integer")
            return None
        if value < minimum:
            self.fail(path, f"must be at least {minimum}")
            return None
        return value

    def decimal(self, value: object, path: str, *, minimum: Decimal = Decimal(0)) -> Decimal | None:
        if isinstance(value, bool) or not isinstance(value, int | float | str):
            self.fail(path, "must be a decimal number")
            return None
        try:
            parsed = Decimal(str(value))
        except InvalidOperation:
            self.fail(path, "must be a decimal number")
            return None
        if not parsed.is_finite() or parsed < minimum:
            self.fail(path, f"must be finite and at least {minimum}")
            return None
        return parsed

    def true(self, value: object, path: str) -> None:
        if value is not True:
            self.fail(path, "must be true")

    def uuid(self, value: object, path: str) -> None:
        text = self.nonempty_string(value, path)
        if text is None:
            return
        try:
            UUID(text)
        except ValueError:
            self.fail(path, "must be a UUID")

    def sha256(self, value: object, path: str) -> None:
        text = self.nonempty_string(value, path)
        if text is not None and SHA256_RE.fullmatch(text) is None:
            self.fail(path, "must be a lowercase SHA256")

    def image_digest(self, value: object, path: str) -> None:
        text = self.nonempty_string(value, path)
        if text is not None and IMAGE_DIGEST_RE.fullmatch(text) is None:
            self.fail(path, "must be a lowercase sha256 image digest")

    def nonempty_sha256_list(self, value: object, path: str) -> None:
        if not isinstance(value, list) or not value:
            self.fail(path, "must be a non-empty list")
            return
        for index, item in enumerate(value):
            self.sha256(item, f"{path}[{index}]")

    def validate(self) -> list[str]:
        root = self.mapping(self.document, "evidence")
        if root is None:
            return self.failures
        if root.get("schema_version") != SCHEMA_VERSION:
            self.fail("schema_version", f"must equal {SCHEMA_VERSION}")
        self.validate_release(root.get("release"))
        self.validate_snapshots(root.get("reliability_snapshots"))
        self.validate_recovery(root.get("recovery"))
        self.validate_rollback(root.get("rollback"))
        self.validate_fact_manifest(
            root.get("fact_manifest"),
            root.get("release"),
            root.get("reliability_snapshots"),
            root.get("recovery"),
            root.get("rollback"),
        )
        return self.failures

    @staticmethod
    def canonical_json(value: object) -> bytes:
        return json.dumps(
            value, ensure_ascii=False, sort_keys=True, separators=(",", ":")
        ).encode("utf-8")

    def validate_release(self, value: object) -> None:
        release = self.mapping(value, "release")
        if release is None:
            return
        self.uuid(self.required(release, "run_id", "release"), "release.run_id")
        self.uuid(self.required(release, "load_plan_id", "release"), "release.load_plan_id")
        self.image_digest(
            self.required(release, "control_plane_image_digest", "release"),
            "release.control_plane_image_digest",
        )
        self.image_digest(
            self.required(release, "worker_image_digest", "release"),
            "release.worker_image_digest",
        )

    def validate_snapshots(self, value: object) -> None:
        if not isinstance(value, list) or not value:
            self.fail("reliability_snapshots", "must be a non-empty list")
            return
        for index, item in enumerate(value):
            path = f"reliability_snapshots[{index}]"
            snapshot = self.mapping(item, path)
            if snapshot is None:
                continue
            for key in (
                "snapshot_id",
                "run_id",
                "load_plan_id",
                "profile_id",
                "slice_key",
                "window_start",
                "window_end",
                "query_version",
                "source_hash",
                "source_watermark",
                "fresh_until",
                "metrics",
                "snapshot_hash",
            ):
                self.required(snapshot, key, path)
            self.uuid(self.required(snapshot, "snapshot_id", path), f"{path}.snapshot_id")
            self.uuid(self.required(snapshot, "run_id", path), f"{path}.run_id")
            self.uuid(self.required(snapshot, "load_plan_id", path), f"{path}.load_plan_id")
            self.nonempty_string(self.required(snapshot, "slice_key", path), f"{path}.slice_key")
            self.nonempty_string(
                self.required(snapshot, "profile_id", path), f"{path}.profile_id"
            )
            self.nonempty_string(
                self.required(snapshot, "query_version", path), f"{path}.query_version"
            )
            self.sha256(self.required(snapshot, "source_hash", path), f"{path}.source_hash")
            self.sha256(
                self.required(snapshot, "source_watermark", path),
                f"{path}.source_watermark",
            )
            self.sha256(self.required(snapshot, "snapshot_hash", path), f"{path}.snapshot_hash")
            for key in ("window_start", "window_end", "fresh_until"):
                timestamp = self.nonempty_string(
                    self.required(snapshot, key, path), f"{path}.{key}"
                )
                if timestamp is not None:
                    try:
                        datetime.fromisoformat(timestamp.replace("Z", "+00:00"))
                    except ValueError:
                        self.fail(f"{path}.{key}", "must be an ISO 8601 timestamp")
            if snapshot.get("source_hash") != snapshot.get("source_watermark"):
                self.fail(path, "source_hash does not match source_watermark")
            try:
                expected_snapshot_hash = compute_snapshot_hash(snapshot)
            except (KeyError, TypeError, ValueError) as exc:
                self.fail(f"{path}.snapshot_hash", f"cannot recompute snapshot hash: {exc}")
            else:
                if snapshot.get("snapshot_hash") != expected_snapshot_hash:
                    self.fail(
                        f"{path}.snapshot_hash",
                        "does not recompute from immutable fields",
                    )
            metrics = self.mapping(snapshot.get("metrics"), f"{path}.metrics")
            if metrics is not None:
                for key in (
                    "request_count",
                    "success_count",
                    "error_count",
                    "timeout_count",
                    "billing_idempotency_failures",
                    "p99_latency_ms",
                    "error_rate",
                ):
                    if key not in metrics and snapshot.get(key) in (0, "0", "0.0", "0.00", "0.000000"):
                        continue
                    if key == "error_rate":
                        try:
                            matches = Decimal(str(metrics.get(key))) == Decimal(str(snapshot.get(key)))
                        except (InvalidOperation, TypeError):
                            matches = False
                    else:
                        matches = metrics.get(key) == snapshot.get(key)
                    if not matches:
                        self.fail(f"{path}.metrics.{key}", "does not match the snapshot summary")
            request_count = self.integer(
                self.required(snapshot, "request_count", path),
                f"{path}.request_count",
                minimum=1,
            )
            success_count = self.integer(
                self.required(snapshot, "success_count", path), f"{path}.success_count"
            )
            error_count = self.integer(
                self.required(snapshot, "error_count", path), f"{path}.error_count"
            )
            timeout_count = self.integer(
                self.required(snapshot, "timeout_count", path), f"{path}.timeout_count"
            )
            billing_failures = self.integer(
                self.required(snapshot, "billing_idempotency_failures", path),
                f"{path}.billing_idempotency_failures",
            )
            p99 = self.integer(
                self.required(snapshot, "p99_latency_ms", path), f"{path}.p99_latency_ms"
            )
            p99_slo = self.integer(
                self.required(snapshot, "p99_slo_ms", path),
                f"{path}.p99_slo_ms",
                minimum=1,
            )
            error_rate = self.decimal(
                self.required(snapshot, "error_rate", path), f"{path}.error_rate"
            )

            counts = (request_count, success_count, error_count, timeout_count)
            if all(count is not None for count in counts):
                terminal_count = success_count + error_count + timeout_count  # type: ignore[operator]
                if terminal_count != request_count:
                    self.fail(path, "terminal outcomes do not equal request_count")
                expected_rate = Decimal(error_count + timeout_count) / Decimal(request_count)  # type: ignore[arg-type,operator]
                if error_rate is not None and error_rate != expected_rate:
                    self.fail(
                        f"{path}.error_rate",
                        "does not match the full request denominator",
                    )
            if billing_failures is not None and billing_failures != 0:
                self.fail(f"{path}.billing_idempotency_failures", "must be zero")
            if p99 is not None and p99_slo is not None and p99 > p99_slo:
                self.fail(f"{path}.p99_latency_ms", "exceeds p99_slo_ms")

    def validate_recovery(self, value: object) -> None:
        recovery = self.mapping(value, "recovery")
        if recovery is None:
            return
        rpo = self.decimal(
            self.required(recovery, "rpo_seconds", "recovery"), "recovery.rpo_seconds"
        )
        rpo_limit = self.decimal(
            self.required(recovery, "rpo_limit_seconds", "recovery"),
            "recovery.rpo_limit_seconds",
            minimum=Decimal("0.000001"),
        )
        rto = self.decimal(
            self.required(recovery, "rto_seconds", "recovery"), "recovery.rto_seconds"
        )
        rto_limit = self.decimal(
            self.required(recovery, "rto_limit_seconds", "recovery"),
            "recovery.rto_limit_seconds",
            minimum=Decimal("0.000001"),
        )
        if rpo is not None and rpo_limit is not None and rpo > rpo_limit:
            self.fail("recovery.rpo_seconds", "exceeds rpo_limit_seconds")
        if rto is not None and rto_limit is not None and rto > rto_limit:
            self.fail("recovery.rto_seconds", "exceeds rto_limit_seconds")

        for key in (
            "worker_reregistered",
            "leases_recovered",
            "evidence_hash_consistent",
            "ledger_consistent",
        ):
            self.true(self.required(recovery, key, "recovery"), f"recovery.{key}")
        duplicate_scores = self.integer(
            self.required(recovery, "duplicate_score_count", "recovery"),
            "recovery.duplicate_score_count",
        )
        if duplicate_scores is not None and duplicate_scores != 0:
            self.fail("recovery.duplicate_score_count", "must be zero")

        deterministic = self.mapping(
            recovery.get("deterministic_acceptance"), "recovery.deterministic_acceptance"
        )
        if deterministic is None:
            return
        valid_pairs = self.integer(
            self.required(deterministic, "valid_pairs", "recovery.deterministic_acceptance"),
            "recovery.deterministic_acceptance.valid_pairs",
        )
        if valid_pairs is not None and valid_pairs != 30:
            self.fail(
                "recovery.deterministic_acceptance.valid_pairs",
                "must equal 30",
            )
        pre_hash = self.required(
            deterministic, "pre_recovery_hash", "recovery.deterministic_acceptance"
        )
        post_hash = self.required(
            deterministic, "post_recovery_hash", "recovery.deterministic_acceptance"
        )
        self.sha256(pre_hash, "recovery.deterministic_acceptance.pre_recovery_hash")
        self.sha256(post_hash, "recovery.deterministic_acceptance.post_recovery_hash")
        if isinstance(pre_hash, str) and isinstance(post_hash, str) and pre_hash != post_hash:
            self.fail(
                "recovery.deterministic_acceptance",
                "deterministic acceptance hash changed after recovery",
            )

    def validate_fact_manifest(
        self,
        value: object,
        release_value: object,
        snapshots_value: object,
        recovery_value: object,
        rollback_value: object,
    ) -> None:
        manifest = self.mapping(value, "fact_manifest")
        release = self.mapping(release_value, "release")
        recovery = self.mapping(recovery_value, "recovery")
        rollback = self.mapping(rollback_value, "rollback")
        if manifest is None or release is None or recovery is None or rollback is None:
            return
        if manifest.get("schema_version") != FACT_MANIFEST_SCHEMA_VERSION:
            self.fail(
                "fact_manifest.schema_version",
                f"must equal {FACT_MANIFEST_SCHEMA_VERSION}",
            )
        run_id = self.required(manifest, "run_id", "fact_manifest")
        load_plan_id = self.required(manifest, "load_plan_id", "fact_manifest")
        self.uuid(run_id, "fact_manifest.run_id")
        self.uuid(load_plan_id, "fact_manifest.load_plan_id")
        if run_id != release.get("run_id"):
            self.fail("fact_manifest.run_id", "does not match release.run_id")
        if load_plan_id != release.get("load_plan_id"):
            self.fail("fact_manifest.load_plan_id", "does not match release.load_plan_id")
        profile_id = self.nonempty_string(
            self.required(manifest, "profile_id", "fact_manifest"), "fact_manifest.profile_id"
        )
        self.uuid(
            self.required(manifest, "policy_id", "fact_manifest"), "fact_manifest.policy_id"
        )
        self.sha256(
            self.required(manifest, "policy_hash", "fact_manifest"), "fact_manifest.policy_hash"
        )
        self.uuid(
            self.required(manifest, "release_subject_id", "fact_manifest"),
            "fact_manifest.release_subject_id",
        )
        self.sha256(
            self.required(manifest, "release_subject_hash", "fact_manifest"),
            "fact_manifest.release_subject_hash",
        )

        snapshot_refs = manifest.get("snapshot_refs")
        snapshots = snapshots_value if isinstance(snapshots_value, list) else []
        if not isinstance(snapshot_refs, list) or not snapshot_refs:
            self.fail("fact_manifest.snapshot_refs", "must be a non-empty list")
            snapshot_refs = []
        if len(snapshot_refs) != len(snapshots):
            self.fail("fact_manifest.snapshot_refs", "must cover every reliability snapshot")
        by_id: dict[object, dict[str, Any]] = {
            item.get("snapshot_id"): item
            for item in snapshots
            if isinstance(item, dict) and item.get("snapshot_id") is not None
        }
        seen_ids: set[object] = set()
        for index, item in enumerate(snapshot_refs):
            path = f"fact_manifest.snapshot_refs[{index}]"
            ref = self.mapping(item, path)
            if ref is None:
                continue
            snapshot_id = self.required(ref, "snapshot_id", path)
            self.uuid(snapshot_id, f"{path}.snapshot_id")
            self.sha256(self.required(ref, "snapshot_hash", path), f"{path}.snapshot_hash")
            ref_run = self.required(ref, "run_id", path)
            ref_plan = self.required(ref, "load_plan_id", path)
            ref_profile = self.nonempty_string(
                self.required(ref, "profile_id", path), f"{path}.profile_id"
            )
            ref_slice = self.nonempty_string(
                self.required(ref, "slice_key", path), f"{path}.slice_key"
            )
            ref_watermark = self.required(ref, "source_watermark", path)
            self.sha256(ref_watermark, f"{path}.source_watermark")
            if ref_run != run_id:
                self.fail(f"{path}.run_id", "does not match fact manifest run_id")
            if ref_plan != load_plan_id:
                self.fail(f"{path}.load_plan_id", "does not match fact manifest load_plan_id")
            if profile_id is not None and ref_profile != profile_id:
                self.fail(f"{path}.profile_id", "does not match fact manifest profile_id")
            if snapshot_id in seen_ids:
                self.fail(f"{path}.snapshot_id", "is duplicated")
            seen_ids.add(snapshot_id)
            snapshot = by_id.get(snapshot_id)
            if snapshot is None:
                self.fail(f"{path}.snapshot_id", "does not identify a reliability snapshot")
                continue
            for key, expected in (
                ("snapshot_hash", ref.get("snapshot_hash")),
                ("run_id", run_id),
                ("load_plan_id", load_plan_id),
                ("profile_id", profile_id),
                ("slice_key", ref_slice),
                ("source_watermark", ref_watermark),
            ):
                if snapshot.get(key) != expected:
                    self.fail(f"{path}.{key}", "does not match the immutable snapshot")

        recovery_ref = self.mapping(manifest.get("recovery_ref"), "fact_manifest.recovery_ref")
        if recovery_ref is not None:
            evidence_id = self.required(recovery_ref, "evidence_id", "fact_manifest.recovery_ref")
            self.uuid(evidence_id, "fact_manifest.recovery_ref.evidence_id")
            self.sha256(
                self.required(recovery_ref, "evidence_hash", "fact_manifest.recovery_ref"),
                "fact_manifest.recovery_ref.evidence_hash",
            )
            ref_run = self.required(recovery_ref, "run_id", "fact_manifest.recovery_ref")
            self.uuid(ref_run, "fact_manifest.recovery_ref.run_id")
            if ref_run != run_id:
                self.fail("fact_manifest.recovery_ref.run_id", "does not match fact manifest run_id")
            experiment_id = self.required(
                recovery_ref, "experiment_id", "fact_manifest.recovery_ref"
            )
            self.uuid(experiment_id, "fact_manifest.recovery_ref.experiment_id")
            watermark = self.required(recovery_ref, "source_watermark", "fact_manifest.recovery_ref")
            self.sha256(watermark, "fact_manifest.recovery_ref.source_watermark")
            if watermark != recovery.get("source_watermark"):
                self.fail(
                    "fact_manifest.recovery_ref.source_watermark",
                    "does not match recovery source watermark",
                )
            generation = self.integer(
                self.required(recovery_ref, "recovery_generation", "fact_manifest.recovery_ref"),
                "fact_manifest.recovery_ref.recovery_generation",
            )
            if generation is not None and generation != recovery.get("recovery_generation"):
                self.fail(
                    "fact_manifest.recovery_ref.recovery_generation",
                    "does not match recovery generation",
                )
            if evidence_id != recovery.get("evidence_id"):
                self.fail("fact_manifest.recovery_ref.evidence_id", "does not match recovery evidence")
            if recovery_ref.get("evidence_hash") != recovery.get("evidence_hash"):
                self.fail(
                    "fact_manifest.recovery_ref.evidence_hash",
                    "does not match recovery evidence hash",
                )
        else:
            self.fail("fact_manifest.recovery_ref", "is required")

        artifact_hashes = self.required(manifest, "artifact_manifest_hashes", "fact_manifest")
        self.nonempty_sha256_list(artifact_hashes, "fact_manifest.artifact_manifest_hashes")
        if artifact_hashes != rollback.get("artifact_manifest_hashes"):
            self.fail(
                "fact_manifest.artifact_manifest_hashes",
                "does not match rollback artifact manifest hashes",
            )

        unsigned = dict(manifest)
        manifest_hash = unsigned.pop("manifest_sha256", None)
        signature = unsigned.pop("signature", None)
        self.sha256(manifest_hash, "fact_manifest.manifest_sha256")
        expected_hash = hashlib.sha256(self.canonical_json(unsigned)).hexdigest()
        if manifest_hash != expected_hash:
            self.fail("fact_manifest.manifest_sha256", "does not match canonical manifest")
        key = os.environ.get("RADAR_EVIDENCE_MANIFEST_KEY", "")
        if len(key) < 32:
            self.fail("fact_manifest.signature", "verification key is required")
        elif not isinstance(signature, str) or not SHA256_RE.fullmatch(signature):
            self.fail("fact_manifest.signature", "must be a lowercase HMAC-SHA256")
        else:
            signed = dict(unsigned)
            signed["manifest_sha256"] = manifest_hash
            expected_signature = hmac.new(
                key.encode("utf-8"), self.canonical_json(signed), hashlib.sha256
            ).hexdigest()
            if not hmac.compare_digest(signature, expected_signature):
                self.fail("fact_manifest.signature", "does not verify")

    def validate_rollback(self, value: object) -> None:
        rollback = self.mapping(value, "rollback")
        if rollback is None:
            return
        recorded_at = self.nonempty_string(
            self.required(rollback, "recorded_at", "rollback"), "rollback.recorded_at"
        )
        if recorded_at is not None:
            try:
                timestamp = datetime.fromisoformat(recorded_at.replace("Z", "+00:00"))
            except ValueError:
                self.fail("rollback.recorded_at", "must be an ISO 8601 timestamp")
            else:
                if timestamp.tzinfo is None or timestamp.astimezone(UTC).utcoffset() is None:
                    self.fail("rollback.recorded_at", "must include a timezone")

        failed_runs = self.required(rollback, "failed_run_ids", "rollback")
        if not isinstance(failed_runs, list) or not failed_runs:
            self.fail("rollback.failed_run_ids", "must be a non-empty list")
        else:
            for index, item in enumerate(failed_runs):
                self.uuid(item, f"rollback.failed_run_ids[{index}]")

        active_leases = self.required(rollback, "active_lease_ids", "rollback")
        if not isinstance(active_leases, list):
            self.fail("rollback.active_lease_ids", "must be a list")
        else:
            for index, item in enumerate(active_leases):
                self.uuid(item, f"rollback.active_lease_ids[{index}]")

        for key in ("previous_control_plane_image_digest", "previous_worker_image_digest"):
            self.image_digest(self.required(rollback, key, "rollback"), f"rollback.{key}")
        for key in ("control_plane_binary_sha256", "worker_binary_sha256"):
            self.sha256(self.required(rollback, key, "rollback"), f"rollback.{key}")
        for key in (
            "migration_checksums",
            "score_hashes",
            "aggregate_hashes",
            "artifact_manifest_hashes",
        ):
            self.nonempty_sha256_list(
                self.required(rollback, key, "rollback"), f"rollback.{key}"
            )

        ledger_before = self.decimal(
            self.required(rollback, "budget_ledger_total_before", "rollback"),
            "rollback.budget_ledger_total_before",
        )
        ledger_after = self.decimal(
            self.required(rollback, "budget_ledger_total_after", "rollback"),
            "rollback.budget_ledger_total_after",
        )
        if ledger_before is not None and ledger_after is not None and ledger_before != ledger_after:
            self.fail("rollback", "rollback budget ledger totals changed")
        self.uuid(
            self.required(rollback, "smoke_run_id", "rollback"), "rollback.smoke_run_id"
        )


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Validate Radar staging SLO, recovery, and rollback evidence."
    )
    parser.add_argument("evidence", type=Path, help="path to the acceptance evidence JSON")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv or sys.argv[1:])
    try:
        with args.evidence.open("r", encoding="utf-8") as stream:
            document = json.load(stream)
    except OSError as exc:
        print(f"FAIL evidence: cannot read {args.evidence}: {exc}", file=sys.stderr)
        return 2
    except json.JSONDecodeError as exc:
        print(
            f"FAIL evidence: invalid JSON at line {exc.lineno}, column {exc.colno}",
            file=sys.stderr,
        )
        return 2

    failures = EvidenceValidator(document).validate()
    if failures:
        for failure in failures:
            print(f"FAIL {failure}", file=sys.stderr)
        print(
            f"FAIL radar staging reliability acceptance ({len(failures)} checks)",
            file=sys.stderr,
        )
        return 1

    snapshot_count = len(document["reliability_snapshots"])
    print(f"PASS radar staging reliability acceptance ({snapshot_count} reliability snapshots)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
