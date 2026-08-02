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


audit = load_script("radar_production_promotion_audit", "production_promotion_audit.py")


SHA_A = "sha256:" + "a" * 64
SHA_B = "sha256:" + "b" * 64
SHA_C = "sha256:" + "c" * 64
HEX_D = "d" * 64
HEX_E = "e" * 64


def complete_manifest() -> dict[str, Any]:
    return {
        "schema_version": audit.INPUT_SCHEMA_VERSION,
        "candidate": {
            "accepted_staging_image_digest": SHA_A,
            "staging_gate_ok": True,
            "migration_rehearsal_ok": True,
        },
        "production_preflight": {
            "ok": True,
            "promotion_ready": True,
            "production_exposure_event": False,
            "blockers": [],
        },
        "production_backup": {
            "path": "/opt/sub2api-backups/prod-20260802.dump",
            "sha256": HEX_D,
            "restore_verified": True,
        },
        "production_active": {
            "image_digest": SHA_B,
            "config_hashes": {
                "docker-compose.yml": HEX_D,
                "docker-compose.override.yml": HEX_E,
                ".env": HEX_D,
                "data/config.yaml": HEX_E,
            },
        },
        "rollback": {
            "previous_image_digest": SHA_B,
            "rollback_image_available": True,
        },
        "post_rollback": {
            "accepted_candidate_restoration_planned": True,
        },
    }


class ProductionPromotionAuditTests(unittest.TestCase):
    def test_complete_manifest_is_ready_for_promotion(self) -> None:
        result = audit.audit_manifest(complete_manifest())

        self.assertTrue(result["ok"], result)
        self.assertTrue(result["promotion_ready"], result)
        self.assertEqual([], result["blockers"])
        self.assertEqual(SHA_A, result["summary"]["accepted_staging_image_digest"])
        self.assertEqual(SHA_B, result["summary"]["previous_image_digest"])

    def test_current_remote_state_fails_until_authorized_production_inputs_exist(self) -> None:
        manifest = complete_manifest()
        manifest["production_preflight"] = {
            "ok": False,
            "promotion_ready": False,
            "production_exposure_event": True,
            "blockers": [
                "production_compose_project_running",
                "production_target_container_present",
            ],
        }
        manifest["production_backup"] = {}
        manifest["production_active"] = {"config_hashes": {}}

        result = audit.audit_manifest(manifest)

        self.assertFalse(result["ok"], result)
        self.assertFalse(result["promotion_ready"], result)
        self.assertIn("production_preflight_ok", result["blockers"])
        self.assertIn("production_backup_sha256", result["blockers"])
        self.assertIn("production_backup_restore_verified", result["blockers"])
        self.assertIn("production_active_image_digest", result["blockers"])
        self.assertIn("production_config_hashes", result["blockers"])
        self.assertIn("production_requires_operator_authorization", result["blockers"])

    def test_malformed_digests_and_hashes_fail_closed(self) -> None:
        manifest = complete_manifest()
        manifest["candidate"]["accepted_staging_image_digest"] = "sha256:bad"
        manifest["production_backup"]["sha256"] = "not-a-hash"
        manifest["production_active"]["config_hashes"][".env"] = "short"

        result = audit.audit_manifest(manifest)

        self.assertFalse(result["ok"], result)
        self.assertIn("accepted_staging_image_digest", result["blockers"])
        self.assertIn("production_backup_sha256", result["blockers"])
        self.assertIn("production_config_hash_.env", result["blockers"])

    def test_rollback_digest_must_be_available_and_distinct_from_candidate(self) -> None:
        manifest = complete_manifest()
        manifest["rollback"]["previous_image_digest"] = SHA_A
        manifest["rollback"]["rollback_image_available"] = False

        result = audit.audit_manifest(manifest)

        self.assertFalse(result["ok"], result)
        self.assertIn("rollback_image_available", result["blockers"])
        self.assertIn("rollback_digest_distinct_from_candidate", result["blockers"])

    def test_restore_candidate_after_rollback_must_be_planned(self) -> None:
        manifest = complete_manifest()
        manifest["post_rollback"] = {}

        result = audit.audit_manifest(manifest)

        self.assertFalse(result["ok"], result)
        self.assertIn("accepted_candidate_restoration_planned", result["blockers"])


if __name__ == "__main__":
    unittest.main()
