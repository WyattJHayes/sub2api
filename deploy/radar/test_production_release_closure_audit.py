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


closure_audit = load_script(
    "radar_production_release_closure_audit",
    "production_release_closure_audit.py",
)


SHA_A = "sha256:" + "a" * 64
SHA_B = "sha256:" + "b" * 64
HEX_A = "1" * 64


def authorization_audit() -> dict[str, Any]:
    return {
        "schema_version": "radar-production-authorization-audit-v1",
        "ok": True,
        "summary": {
            "target_dir": "/opt/sub2api",
            "operator": "release-owner",
            "accepted_candidate_digest": SHA_A,
        },
        "blockers": [],
    }


def closure_document(**overrides: Any) -> dict[str, Any]:
    document: dict[str, Any] = {
        "schema_version": closure_audit.INPUT_SCHEMA_VERSION,
        "accepted_candidate_digest": SHA_A,
        "production_authorization_audit": authorization_audit(),
        "production_target_preflight": {
            "schema_version": "radar-production-target-preflight-v1",
            "ok": True,
            "promotion_ready": True,
            "production_exposure_event": False,
            "blockers": [],
        },
        "production_backup_audit": {
            "schema_version": "radar-production-backup-audit-v1",
            "ok": True,
            "summary": {
                "sha256": HEX_A,
                "restore_verified": True,
                "restore_schema_migrations": 255,
            },
            "blockers": [],
        },
        "production_promotion_audit": {
            "schema_version": "radar-production-promotion-audit-v1",
            "ok": True,
            "promotion_ready": True,
            "summary": {
                "accepted_staging_image_digest": SHA_A,
                "previous_image_digest": SHA_B,
                "production_active_image_digest": SHA_B,
            },
            "blockers": [],
        },
        "production_promotion_executed": True,
        "production_smoke_audit": {
            "schema_version": "radar-production-smoke-audit-v1",
            "ok": True,
            "summary": {
                "accepted_candidate_digest": SHA_A,
                "active_image_digest": SHA_A,
            },
            "blockers": [],
        },
        "rollback_drill_executed": True,
        "production_rollback_audit": {
            "schema_version": "radar-production-rollback-audit-v1",
            "ok": True,
            "summary": {
                "accepted_candidate_digest": SHA_A,
                "previous_image_digest": SHA_B,
                "final_active_digest": SHA_A,
                "accepted_candidate_restored": True,
            },
            "blockers": [],
        },
    }
    document.update(overrides)
    return document


class ProductionReleaseClosureAuditTests(unittest.TestCase):
    def test_complete_production_closure_evidence_passes(self) -> None:
        result = closure_audit.audit_closure(closure_document())

        self.assertTrue(result["ok"], result)
        self.assertEqual([], result["blockers"])
        self.assertEqual(SHA_A, result["summary"]["accepted_candidate_digest"])
        self.assertEqual(SHA_A, result["summary"]["final_active_digest"])

    def test_target_preflight_and_backup_audit_must_pass(self) -> None:
        result = closure_audit.audit_closure(
            closure_document(
                production_target_preflight={"ok": False, "promotion_ready": False},
                production_backup_audit={"ok": False, "summary": {}, "blockers": ["x"]},
            )
        )

        self.assertFalse(result["ok"], result)
        self.assertIn("production_target_preflight_ok", result["blockers"])
        self.assertIn("production_backup_audit_ok", result["blockers"])

    def test_production_authorization_audit_must_pass(self) -> None:
        result = closure_audit.audit_closure(
            closure_document(
                production_authorization_audit={
                    "ok": False,
                    "summary": {},
                    "blockers": ["authorize_digest_promotion"],
                }
            )
        )

        self.assertFalse(result["ok"], result)
        self.assertIn("production_authorization_audit_ok", result["blockers"])

    def test_promotion_and_smoke_audit_must_pass(self) -> None:
        result = closure_audit.audit_closure(
            closure_document(
                production_promotion_audit={"ok": False, "promotion_ready": False},
                production_promotion_executed=False,
                production_smoke_audit={"ok": False, "summary": {}, "blockers": ["x"]},
            )
        )

        self.assertFalse(result["ok"], result)
        self.assertIn("production_promotion_audit_ok", result["blockers"])
        self.assertIn("production_promotion_executed", result["blockers"])
        self.assertIn("production_smoke_audit_ok", result["blockers"])

    def test_rollback_drill_and_audit_must_pass(self) -> None:
        result = closure_audit.audit_closure(
            closure_document(
                rollback_drill_executed=False,
                production_rollback_audit={"ok": False, "summary": {}, "blockers": ["x"]},
            )
        )

        self.assertFalse(result["ok"], result)
        self.assertIn("rollback_drill_executed", result["blockers"])
        self.assertIn("production_rollback_audit_ok", result["blockers"])

    def test_candidate_digest_must_be_consistent_across_evidence(self) -> None:
        result = closure_audit.audit_closure(
            closure_document(
                production_smoke_audit={
                    "ok": True,
                    "summary": {
                        "accepted_candidate_digest": SHA_A,
                        "active_image_digest": SHA_B,
                    },
                    "blockers": [],
                },
                production_rollback_audit={
                    "ok": True,
                    "summary": {
                        "accepted_candidate_digest": SHA_B,
                        "previous_image_digest": SHA_A,
                        "final_active_digest": SHA_B,
                    },
                    "blockers": [],
                },
            )
        )

        self.assertFalse(result["ok"], result)
        self.assertIn("smoke_active_digest_matches_candidate", result["blockers"])
        self.assertIn("rollback_candidate_digest_matches_candidate", result["blockers"])
        self.assertIn("rollback_final_digest_matches_candidate", result["blockers"])


if __name__ == "__main__":
    unittest.main()
