from __future__ import annotations

import hashlib
import hmac
import importlib.util
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path
from typing import Any


RADAR_DIR = Path(__file__).resolve().parent
sys.path.insert(0, str(RADAR_DIR))


def load_script(name: str, filename: str) -> Any:
    spec = importlib.util.spec_from_file_location(name, RADAR_DIR / filename)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load {filename}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


acceptance = load_script("radar_reliability_acceptance", "reliability-acceptance.py")
facts_verifier = load_script("radar_reliability_facts", "verify-reliability-facts.py")


RUN_ID = "00000000-0000-4000-8000-000000000011"
LOAD_PLAN_ID = "00000000-0000-4000-8000-000000000010"
POLICY_ID = "00000000-0000-4000-8000-000000000015"
SUBJECT_ID = "00000000-0000-4000-8000-000000000016"
SNAPSHOT_ID = "00000000-0000-4000-8000-000000000022"
EVIDENCE_ID = "00000000-0000-4000-8000-000000000014"
EXPERIMENT_ID = "00000000-0000-4000-8000-000000000012"


def canonical(value: object) -> bytes:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode(
        "utf-8"
    )


def snapshot() -> dict[str, Any]:
    return {
        "snapshot_id": SNAPSHOT_ID,
        "run_id": RUN_ID,
        "load_plan_id": LOAD_PLAN_ID,
        "profile_id": "staging-v1",
        "slice_key": "model=deepseek-chat|region=staging|concurrency=100",
        "window_start": "2026-07-30T08:00:00.123456789+08:00",
        "window_end": "2026-07-30T08:10:00.987654321+08:00",
        "query_version": "reliability-query-v1",
        "source_hash": "b" * 64,
        "source_watermark": "b" * 64,
        "fresh_until": "2026-07-30T08:20:00.000000001+08:00",
        "metrics": {
            "request_count": 2,
            "success_count": 2,
            "successful_latency_count": 2,
            "valid_pair_count": 30,
            "upstream_failure_count": 0,
            "gateway_failure_count": 0,
            "client_cancellation_count": 0,
            "error_numerator": 0,
            "error_denominator": 2,
            "p99_latency_ms": 480,
            "histogram_or_sketch_hash": "c" * 64,
            "error_rate": "0",
            "cost_amount": "0",
            "ongoing_confirmed_p0_incident": False,
        },
    }


def facts_document() -> dict[str, Any]:
    item = snapshot()
    item["snapshot_hash"] = facts_verifier.snapshot_hash(item)
    return {
        "schema_version": "radar-reliability-facts-v1",
        "run_id": RUN_ID,
        "policy_id": POLICY_ID,
        "profile_id": "staging-v1",
        "load_plan_id": LOAD_PLAN_ID,
        "load_plan_sha256": "d" * 64,
        "policy_hash": "e" * 64,
        "release_subject_id": SUBJECT_ID,
        "release_subject_hash": "f" * 64,
        "snapshots": [item],
        "recovery": {
            "evidence_id": EVIDENCE_ID,
            "evidence_hash": "1" * 64,
            "run_id": RUN_ID,
            "experiment_id": EXPERIMENT_ID,
            "source_watermark": "2" * 64,
            "recovery_generation": 1,
        },
        "artifact_manifest_hashes": ["3" * 64],
    }


def acceptance_document(snapshot_hash: str | None = None) -> dict[str, Any]:
    item = snapshot()
    item["metrics"] = {
        **item["metrics"],
        "error_count": 0,
        "timeout_count": 0,
        "billing_idempotency_failures": 0,
        "p99_latency_ms": 480,
        "error_rate": "0",
        "cost_amount": "0",
    }
    item["snapshot_hash"] = snapshot_hash or facts_verifier.snapshot_hash(item)
    manifest = {
        "schema_version": "radar-fact-manifest-v1",
        "run_id": RUN_ID,
        "load_plan_id": LOAD_PLAN_ID,
        "profile_id": "staging-v1",
        "policy_id": POLICY_ID,
        "policy_hash": "e" * 64,
        "release_subject_id": SUBJECT_ID,
        "release_subject_hash": "f" * 64,
        "snapshot_refs": [
            {
                key: item[key]
                for key in (
                    "snapshot_id",
                    "snapshot_hash",
                    "run_id",
                    "load_plan_id",
                    "profile_id",
                    "slice_key",
                    "source_watermark",
                )
            }
        ],
        "recovery_ref": {
            "evidence_id": EVIDENCE_ID,
            "evidence_hash": "1" * 64,
            "run_id": RUN_ID,
            "experiment_id": EXPERIMENT_ID,
            "source_watermark": "2" * 64,
            "recovery_generation": 1,
        },
        "artifact_manifest_hashes": ["3" * 64],
    }
    manifest["manifest_sha256"] = hashlib.sha256(canonical(manifest)).hexdigest()
    manifest["signature"] = hmac.new(
        b"k" * 32,
        canonical({**manifest, "manifest_sha256": manifest["manifest_sha256"]}),
        hashlib.sha256,
    ).hexdigest()
    return {
        "schema_version": "radar-staging-reliability-acceptance-v1",
        "release": {
            "run_id": RUN_ID,
            "load_plan_id": LOAD_PLAN_ID,
            "control_plane_image_digest": "sha256:" + "a" * 64,
            "worker_image_digest": "sha256:" + "b" * 64,
        },
        "fact_manifest": manifest,
        "reliability_snapshots": [
            {
                **item,
                "request_count": 2,
                "success_count": 2,
                "error_count": 0,
                "timeout_count": 0,
                "billing_idempotency_failures": 0,
                "p99_latency_ms": 480,
                "p99_slo_ms": 500,
                "error_rate": "0",
            }
        ],
        "recovery": {
            "evidence_id": EVIDENCE_ID,
            "evidence_hash": "1" * 64,
            "recovery_generation": 1,
            "source_watermark": "2" * 64,
            "rpo_seconds": 1,
            "rpo_limit_seconds": 2,
            "rto_seconds": 1,
            "rto_limit_seconds": 2,
            "worker_reregistered": True,
            "leases_recovered": True,
            "duplicate_score_count": 0,
            "evidence_hash_consistent": True,
            "ledger_consistent": True,
            "deterministic_acceptance": {
                "valid_pairs": 30,
                "pre_recovery_hash": "4" * 64,
                "post_recovery_hash": "4" * 64,
            },
        },
        "rollback": {
            "recorded_at": "2026-07-30T00:00:00Z",
            "failed_run_ids": [RUN_ID],
            "active_lease_ids": ["00000000-0000-4000-8000-000000000020"],
            "previous_control_plane_image_digest": "sha256:" + "5" * 64,
            "previous_worker_image_digest": "sha256:" + "6" * 64,
            "control_plane_binary_sha256": "7" * 64,
            "worker_binary_sha256": "8" * 64,
            "migration_checksums": ["9" * 64],
            "score_hashes": ["a" * 64],
            "aggregate_hashes": ["b" * 64],
            "artifact_manifest_hashes": ["3" * 64],
            "budget_ledger_total_before": "1.00000000",
            "budget_ledger_total_after": "1.00000000",
            "smoke_run_id": "00000000-0000-4000-8000-000000000021",
        },
    }


class ReliabilityEvidenceTests(unittest.TestCase):
    def test_snapshot_hash_matches_go_rfc3339_nano_and_field_order(self) -> None:
        item = snapshot()
        expected_metrics = {
            "request_count": 2,
            "success_count": 2,
            "successful_latency_count": 2,
            "valid_pair_count": 30,
            "upstream_failure_count": 0,
            "gateway_failure_count": 0,
            "client_cancellation_count": 0,
            "error_numerator": 0,
            "error_denominator": 2,
            "p99_latency_ms": 480,
            "histogram_or_sketch_hash": "c" * 64,
            "error_rate": "0",
            "cost_amount": "0",
            "ongoing_confirmed_p0_incident": False,
        }
        outer = {
            "run_id": RUN_ID,
            "reliability_profile_id": "staging-v1",
            "slice_key": item["slice_key"],
            "window_start": "2026-07-30T00:00:00.123456789Z",
            "window_end": "2026-07-30T00:10:00.987654321Z",
            "query_version": "reliability-query-v1",
            "source_hash": "b" * 64,
            "metrics": expected_metrics,
            "fresh_until": "2026-07-30T00:20:00.000000001Z",
        }
        expected = hashlib.sha256(
            json.dumps(outer, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
        ).hexdigest()
        self.assertEqual(expected, facts_verifier.snapshot_hash(item))

    def test_acceptance_recomputes_snapshot_hash_even_when_manifest_is_rewritten(self) -> None:
        document = acceptance_document(snapshot_hash="d" * 64)
        os.environ["RADAR_EVIDENCE_MANIFEST_KEY"] = "k" * 32
        failures = acceptance.EvidenceValidator(document).validate()
        self.assertTrue(
            any("snapshot_hash" in failure and "recomput" in failure for failure in failures),
            failures,
        )

    def test_verifier_compares_all_snapshot_reference_fields(self) -> None:
        facts = facts_document()
        manifest = {
            "fact_manifest": {
                "run_id": facts["run_id"],
                "load_plan_id": facts["load_plan_id"],
                "profile_id": facts["profile_id"],
                "policy_id": facts["policy_id"],
                "policy_hash": facts["policy_hash"],
                "release_subject_id": facts["release_subject_id"],
                "release_subject_hash": facts["release_subject_hash"],
                "snapshot_refs": [
                    {
                        "snapshot_id": facts["snapshots"][0]["snapshot_id"],
                        "snapshot_hash": facts["snapshots"][0]["snapshot_hash"],
                        "run_id": "00000000-0000-4000-8000-000000000099",
                        "load_plan_id": facts["snapshots"][0]["load_plan_id"],
                        "profile_id": facts["snapshots"][0]["profile_id"],
                        "slice_key": facts["snapshots"][0]["slice_key"],
                        "source_watermark": facts["snapshots"][0]["source_watermark"],
                    }
                ],
                "artifact_manifest_hashes": facts["artifact_manifest_hashes"],
                "recovery_ref": facts["recovery"],
            }
        }
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "acceptance.json"
            path.write_text(json.dumps(manifest), encoding="utf-8")
            with self.assertRaisesRegex(Exception, "snapshot_refs"):
                facts_verifier.compare_acceptance_manifest(path, facts)


if __name__ == "__main__":
    unittest.main()
