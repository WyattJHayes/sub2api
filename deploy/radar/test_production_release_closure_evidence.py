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


closure_builder = load_script(
    "radar_production_release_closure_evidence",
    "production_release_closure_evidence.py",
)
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


def preflight_result() -> dict[str, Any]:
    return {
        "schema_version": "radar-production-target-preflight-v1",
        "ok": True,
        "promotion_ready": True,
        "production_exposure_event": False,
        "blockers": [],
    }


def backup_audit() -> dict[str, Any]:
    return {
        "schema_version": "radar-production-backup-audit-v1",
        "ok": True,
        "summary": {
            "sha256": HEX_A,
            "restore_verified": True,
            "restore_schema_migrations": 255,
        },
        "blockers": [],
    }


def promotion_audit() -> dict[str, Any]:
    return {
        "schema_version": "radar-production-promotion-audit-v1",
        "ok": True,
        "promotion_ready": True,
        "summary": {
            "accepted_staging_image_digest": SHA_A,
            "previous_image_digest": SHA_B,
            "production_active_image_digest": SHA_B,
        },
        "blockers": [],
    }


def smoke_audit() -> dict[str, Any]:
    return {
        "schema_version": "radar-production-smoke-audit-v1",
        "ok": True,
        "summary": {
            "accepted_candidate_digest": SHA_A,
            "active_image_digest": SHA_A,
        },
        "blockers": [],
    }


def rollback_audit() -> dict[str, Any]:
    return {
        "schema_version": "radar-production-rollback-audit-v1",
        "ok": True,
        "summary": {
            "accepted_candidate_digest": SHA_A,
            "previous_image_digest": SHA_B,
            "final_active_digest": SHA_A,
            "accepted_candidate_restored": True,
        },
        "blockers": [],
    }


class ProductionReleaseClosureEvidenceTests(unittest.TestCase):
    def test_builds_closure_evidence_from_gate_outputs(self) -> None:
        document = closure_builder.build_closure_evidence(
            accepted_candidate_digest=SHA_A,
            production_authorization_audit=authorization_audit(),
            production_target_preflight=preflight_result(),
            production_backup_audit=backup_audit(),
            production_promotion_audit=promotion_audit(),
            production_smoke_audit=smoke_audit(),
            production_rollback_audit=rollback_audit(),
            production_promotion_executed=True,
            rollback_drill_executed=True,
        )

        self.assertEqual(closure_audit.INPUT_SCHEMA_VERSION, document["schema_version"])
        self.assertEqual(SHA_A, document["accepted_candidate_digest"])
        self.assertEqual(preflight_result(), document["production_target_preflight"])
        self.assertTrue(document["production_promotion_executed"])
        self.assertTrue(document["rollback_drill_executed"])
        self.assertTrue(closure_audit.audit_closure(document)["ok"])

    def test_execution_flags_default_to_false_for_fail_closed_closure(self) -> None:
        document = closure_builder.build_closure_evidence(
            accepted_candidate_digest=SHA_A,
            production_target_preflight=preflight_result(),
            production_backup_audit=backup_audit(),
            production_promotion_audit=promotion_audit(),
            production_smoke_audit=smoke_audit(),
            production_rollback_audit=rollback_audit(),
        )

        result = closure_audit.audit_closure(document)

        self.assertFalse(document["production_promotion_executed"])
        self.assertFalse(document["rollback_drill_executed"])
        self.assertFalse(result["ok"])
        self.assertIn("production_promotion_executed", result["blockers"])
        self.assertIn("rollback_drill_executed", result["blockers"])

    def test_uses_empty_objects_for_missing_gate_outputs_to_fail_closed(self) -> None:
        document = closure_builder.build_closure_evidence(
            accepted_candidate_digest=SHA_A,
        )

        result = closure_audit.audit_closure(document)

        self.assertEqual({}, document["production_target_preflight"])
        self.assertEqual({}, document["production_backup_audit"])
        self.assertFalse(result["ok"])
        self.assertIn("production_target_preflight_ok", result["blockers"])
        self.assertIn("production_backup_audit_ok", result["blockers"])


if __name__ == "__main__":
    unittest.main()
