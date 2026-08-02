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


authorization_audit = load_script(
    "radar_production_authorization_audit",
    "production_authorization_audit.py",
)


SHA_A = "sha256:" + "a" * 64


def authorization_document(**overrides: Any) -> dict[str, Any]:
    document: dict[str, Any] = {
        "schema_version": authorization_audit.INPUT_SCHEMA_VERSION,
        "target_dir": "/opt/sub2api",
        "accepted_candidate_digest": SHA_A,
        "operator": "release-owner",
        "authorized_at": "2026-08-02T21:00:00Z",
        "checked_at": "2026-08-02T21:30:00Z",
        "confirm_target_dir": True,
        "authorize_inactive_stack_start": True,
        "authorize_env_chmod_0600": True,
        "authorize_fresh_backup": True,
        "authorize_digest_promotion": True,
        "authorize_rollback_drill": True,
    }
    document.update(overrides)
    return document


class ProductionAuthorizationAuditTests(unittest.TestCase):
    def test_complete_authorization_evidence_passes(self) -> None:
        result = authorization_audit.audit_authorization(
            authorization_document(),
            expected_target_dir="/opt/sub2api",
            checked_at="2026-08-02T21:30:00Z",
            max_age_seconds=3600,
        )

        self.assertTrue(result["ok"], result)
        self.assertEqual([], result["blockers"])
        self.assertEqual("release-owner", result["summary"]["operator"])
        self.assertEqual(SHA_A, result["summary"]["accepted_candidate_digest"])

    def test_missing_authorization_flags_fail_closed(self) -> None:
        result = authorization_audit.audit_authorization(
            authorization_document(
                authorize_fresh_backup=False,
                authorize_rollback_drill=False,
            ),
            expected_target_dir="/opt/sub2api",
            checked_at="2026-08-02T21:30:00Z",
            max_age_seconds=3600,
        )

        self.assertFalse(result["ok"], result)
        self.assertIn("authorize_fresh_backup", result["blockers"])
        self.assertIn("authorize_rollback_drill", result["blockers"])

    def test_target_and_candidate_digest_must_match_scope(self) -> None:
        result = authorization_audit.audit_authorization(
            authorization_document(
                target_dir="/opt/other",
                accepted_candidate_digest="bad",
            ),
            expected_target_dir="/opt/sub2api",
            checked_at="2026-08-02T21:30:00Z",
            max_age_seconds=3600,
        )

        self.assertFalse(result["ok"], result)
        self.assertIn("target_dir", result["blockers"])
        self.assertIn("accepted_candidate_digest", result["blockers"])

    def test_operator_and_timestamp_are_required(self) -> None:
        result = authorization_audit.audit_authorization(
            authorization_document(
                operator="",
                authorized_at="2026-08-02T20:00:00",
            ),
            expected_target_dir="/opt/sub2api",
            checked_at="2026-08-02T21:30:00Z",
            max_age_seconds=3600,
        )

        self.assertFalse(result["ok"], result)
        self.assertIn("operator", result["blockers"])
        self.assertIn("authorization_timestamp", result["blockers"])

    def test_expired_authorization_fails_closed(self) -> None:
        result = authorization_audit.audit_authorization(
            authorization_document(authorized_at="2026-08-02T19:00:00Z"),
            expected_target_dir="/opt/sub2api",
            checked_at="2026-08-02T21:30:00Z",
            max_age_seconds=3600,
        )

        self.assertFalse(result["ok"], result)
        self.assertIn("authorization_freshness", result["blockers"])


if __name__ == "__main__":
    unittest.main()
