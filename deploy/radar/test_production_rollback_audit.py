from __future__ import annotations

import copy
import importlib.util
import sys
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


rollback_audit = load_script("radar_production_rollback_audit", "production_rollback_audit.py")


SHA_A = "sha256:" + "a" * 64
SHA_B = "sha256:" + "b" * 64
SHA_C = "sha256:" + "c" * 64
SHA_D = "sha256:" + "d" * 64


def rollback_document(**overrides: Any) -> dict[str, Any]:
    document: dict[str, Any] = {
        "schema_version": rollback_audit.INPUT_SCHEMA_VERSION,
        "accepted_candidate": {
            "control_plane_digest": SHA_A,
            "worker_digest": SHA_B,
        },
        "previous": {
            "control_plane_digest": SHA_C,
            "worker_digest": SHA_D,
            "control_plane_available": True,
            "worker_available": True,
        },
        "rollback_executed": True,
        "rollback_smoke_ok": True,
        "rollback_schema_migrations": 255,
        "budget_ledger_total_before": "123.45",
        "budget_ledger_total_after": "123.45",
        "accepted_candidate_restored": True,
        "post_restore_smoke_ok": True,
        "final_active": {
            "control_plane_digest": SHA_A,
            "worker_digest": SHA_B,
        },
    }
    document.update(overrides)
    return document


class ProductionRollbackAuditTests(unittest.TestCase):
    def test_v01178_rollback_cli_derives_manifest_migration_count(self) -> None:
        args = rollback_audit.parse_args(["--rollback-evidence", "rollback.json"])
        self.assertIsNone(args.expected_schema_migrations)

    def test_successful_rollback_and_candidate_restore_passes(self) -> None:
        result = rollback_audit.audit_rollback(
            rollback_document(),
            expected_schema_migrations=255,
        )

        self.assertEqual("radar-production-rollback-audit-v2", result["schema_version"])
        self.assertTrue(result["ok"], result)
        self.assertEqual([], result["blockers"])
        self.assertEqual(
            {"control_plane_digest": SHA_C, "worker_digest": SHA_D},
            result["summary"]["previous"],
        )
        self.assertEqual(
            {"control_plane_digest": SHA_A, "worker_digest": SHA_B},
            result["summary"]["final_active"],
        )

    def test_previous_digests_must_be_available_and_distinct_from_candidate(self) -> None:
        result = rollback_audit.audit_rollback(
            rollback_document(
                previous={
                    "control_plane_digest": SHA_A,
                    "worker_digest": SHA_B,
                    "control_plane_available": False,
                    "worker_available": False,
                }
            ),
            expected_schema_migrations=255,
        )

        self.assertFalse(result["ok"], result)
        self.assertIn("previous_control_plane_digest_distinct_from_candidate", result["blockers"])
        self.assertIn("previous_worker_digest_distinct_from_candidate", result["blockers"])
        self.assertIn("previous_control_plane_available", result["blockers"])
        self.assertIn("previous_worker_available", result["blockers"])

    def test_rollback_smoke_and_schema_migration_count_are_required(self) -> None:
        result = rollback_audit.audit_rollback(
            rollback_document(rollback_smoke_ok=False, rollback_schema_migrations=254),
            expected_schema_migrations=255,
        )

        self.assertFalse(result["ok"], result)
        self.assertIn("rollback_smoke_ok", result["blockers"])
        self.assertIn("rollback_schema_migrations", result["blockers"])

    def test_budget_ledger_totals_must_remain_unchanged_when_reported(self) -> None:
        result = rollback_audit.audit_rollback(
            rollback_document(budget_ledger_total_after="123.46"),
            expected_schema_migrations=255,
        )

        self.assertFalse(result["ok"], result)
        self.assertIn("budget_ledger_total_unchanged", result["blockers"])

    def test_candidate_restore_must_finish_with_accepted_digest_and_smoke_success(self) -> None:
        result = rollback_audit.audit_rollback(
            rollback_document(
                accepted_candidate_restored=False,
                post_restore_smoke_ok=False,
                final_active={
                    "control_plane_digest": SHA_C,
                    "worker_digest": SHA_D,
                },
            ),
            expected_schema_migrations=255,
        )

        self.assertFalse(result["ok"], result)
        self.assertIn("accepted_candidate_restored", result["blockers"])
        self.assertIn("post_restore_smoke_ok", result["blockers"])
        self.assertIn("final_active_control_plane_digest_matches_candidate", result["blockers"])
        self.assertIn("final_active_worker_digest_matches_candidate", result["blockers"])

    def test_worker_identity_corruption_has_worker_specific_blockers(self) -> None:
        cases = (
            (
                "accepted candidate",
                ("accepted_candidate", "worker_digest", "bad"),
                "accepted_candidate_worker_digest",
            ),
            (
                "previous digest",
                ("previous", "worker_digest", "bad"),
                "previous_worker_digest",
            ),
            (
                "previous availability",
                ("previous", "worker_available", False),
                "previous_worker_available",
            ),
            (
                "final active",
                ("final_active", "worker_digest", SHA_D),
                "final_active_worker_digest_matches_candidate",
            ),
        )
        for name, (mapping_name, field, value), expected_blocker in cases:
            with self.subTest(name=name):
                document = copy.deepcopy(rollback_document())
                document[mapping_name][field] = value
                result = rollback_audit.audit_rollback(
                    document,
                    expected_schema_migrations=255,
                )

                self.assertFalse(result["ok"], result)
                self.assertIn(expected_blocker, result["blockers"])


if __name__ == "__main__":
    unittest.main()
