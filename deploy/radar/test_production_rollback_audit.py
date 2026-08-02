from __future__ import annotations

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


def rollback_document(**overrides: Any) -> dict[str, Any]:
    document: dict[str, Any] = {
        "schema_version": rollback_audit.INPUT_SCHEMA_VERSION,
        "accepted_candidate_digest": SHA_A,
        "previous_image_digest": SHA_B,
        "rollback_image_available": True,
        "rollback_executed": True,
        "rollback_smoke_ok": True,
        "rollback_schema_migrations": 255,
        "budget_ledger_total_before": "123.45",
        "budget_ledger_total_after": "123.45",
        "accepted_candidate_restored": True,
        "post_restore_smoke_ok": True,
        "final_active_digest": SHA_A,
    }
    document.update(overrides)
    return document


class ProductionRollbackAuditTests(unittest.TestCase):
    def test_successful_rollback_and_candidate_restore_passes(self) -> None:
        result = rollback_audit.audit_rollback(
            rollback_document(),
            expected_schema_migrations=255,
        )

        self.assertTrue(result["ok"], result)
        self.assertEqual([], result["blockers"])
        self.assertEqual(SHA_B, result["summary"]["previous_image_digest"])
        self.assertEqual(SHA_A, result["summary"]["final_active_digest"])

    def test_previous_digest_must_be_available_and_distinct_from_candidate(self) -> None:
        result = rollback_audit.audit_rollback(
            rollback_document(previous_image_digest=SHA_A, rollback_image_available=False),
            expected_schema_migrations=255,
        )

        self.assertFalse(result["ok"], result)
        self.assertIn("rollback_digest_distinct_from_candidate", result["blockers"])
        self.assertIn("rollback_image_available", result["blockers"])

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
                final_active_digest=SHA_B,
            ),
            expected_schema_migrations=255,
        )

        self.assertFalse(result["ok"], result)
        self.assertIn("accepted_candidate_restored", result["blockers"])
        self.assertIn("post_restore_smoke_ok", result["blockers"])
        self.assertIn("final_active_digest_matches_candidate", result["blockers"])


if __name__ == "__main__":
    unittest.main()
