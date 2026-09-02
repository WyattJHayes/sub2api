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


manifest_tool = load_script("radar_production_promotion_manifest", "production_promotion_manifest.py")
audit = load_script("radar_production_promotion_audit", "production_promotion_audit.py")


SHA_A = "sha256:" + "a" * 64
SHA_B = "sha256:" + "b" * 64
SHA_C = "sha256:" + "c" * 64
SHA_D = "sha256:" + "d" * 64
HEX_C = "c" * 64
HEX_D = "d" * 64
HEX_E = "e" * 64
HEX_F = "f" * 64


def preflight_result(*, ok: bool = True) -> dict[str, Any]:
    return {
        "schema_version": "radar-production-target-preflight-v1",
        "ok": ok,
        "promotion_ready": ok,
        "production_exposure_event": not ok,
        "blockers": [] if ok else ["production_compose_project_running"],
    }


def target_snapshot() -> dict[str, Any]:
    return {
        "schema_version": "radar-production-target-preflight-v1",
        "target_dir": "/opt/sub2api",
        "hashes": {
            "/opt/sub2api/docker-compose.yml": HEX_C,
            "/opt/sub2api/docker-compose.override.yml": HEX_D,
            "/opt/sub2api/.env": HEX_E,
            "/opt/sub2api/data/config.yaml": HEX_F,
            "/opt/sub2api/data/model_pricing.json": "1" * 64,
        },
    }


class ProductionPromotionManifestTests(unittest.TestCase):
    def test_runtime_digest_falls_back_to_digest_pinned_configured_image(self) -> None:
        payload = {
            "production_containers": [
                {
                    "service": "sub2api",
                    "configured_image": "ghcr.io/a895411690/sub2api-radar-control-plane@" + SHA_C,
                    "repo_digests": [],
                },
                {
                    "service": "radar-runner",
                    "configured_image": "ghcr.io/a895411690/sub2api-radar-worker@" + SHA_D,
                    "repo_digests": [],
                },
            ]
        }

        self.assertEqual(
            {
                "control_plane": SHA_C,
                "worker": SHA_D,
            },
            manifest_tool._runtime_digests(payload),
        )

    def test_builds_audit_ready_manifest_from_preflight_and_snapshot(self) -> None:
        manifest = manifest_tool.build_manifest(
            candidate_control_plane_digest=SHA_A,
            candidate_worker_digest=SHA_B,
            staging_gate_ok=True,
            migration_rehearsal_ok=True,
            production_preflight=preflight_result(),
            target_snapshot=target_snapshot(),
            production_backup_path="/opt/sub2api-backups/prod.dump",
            production_backup_sha256=HEX_C,
            production_backup_restore_verified=True,
            active_control_plane_digest=SHA_C,
            active_worker_digest=SHA_D,
            rollback_control_plane_digest=SHA_C,
            rollback_worker_digest=SHA_D,
            rollback_control_plane_available=True,
            rollback_worker_available=True,
            accepted_candidate_restoration_planned=True,
        )

        self.assertEqual("radar-production-promotion-audit-input-v2", audit.INPUT_SCHEMA_VERSION)
        self.assertEqual(audit.INPUT_SCHEMA_VERSION, manifest["schema_version"])
        self.assertEqual(SHA_A, manifest["candidate"]["control_plane_digest"])
        self.assertEqual(SHA_B, manifest["candidate"]["worker_digest"])
        self.assertTrue(manifest["candidate"]["staging_gate_ok"])
        self.assertTrue(manifest["candidate"]["migration_rehearsal_ok"])
        self.assertEqual(
            {
                "docker-compose.yml": HEX_C,
                "docker-compose.override.yml": HEX_D,
                ".env": HEX_E,
                "data/config.yaml": HEX_F,
            },
            manifest["production_active"]["config_hashes"],
        )
        self.assertEqual(SHA_C, manifest["production_active"]["control_plane_digest"])
        self.assertEqual(SHA_D, manifest["production_active"]["worker_digest"])
        self.assertEqual(
            {
                "control_plane_digest": SHA_C,
                "worker_digest": SHA_D,
                "control_plane_available": True,
                "worker_available": True,
            },
            manifest["rollback"],
        )
        self.assertTrue(audit.audit_manifest(manifest)["promotion_ready"])

    def test_missing_production_runtime_inputs_remain_empty_for_fail_closed_audit(self) -> None:
        manifest = manifest_tool.build_manifest(
            candidate_control_plane_digest=SHA_A,
            candidate_worker_digest=SHA_B,
            staging_gate_ok=True,
            migration_rehearsal_ok=True,
            production_preflight=preflight_result(ok=False),
            target_snapshot=target_snapshot(),
        )

        result = audit.audit_manifest(manifest)

        self.assertFalse(result["promotion_ready"])
        self.assertEqual("", manifest["production_backup"]["sha256"])
        self.assertEqual("", manifest["production_active"]["control_plane_digest"])
        self.assertEqual("", manifest["production_active"]["worker_digest"])
        self.assertEqual("", manifest["rollback"]["control_plane_digest"])
        self.assertEqual("", manifest["rollback"]["worker_digest"])
        self.assertFalse(manifest["rollback"]["control_plane_available"])
        self.assertFalse(manifest["rollback"]["worker_available"])
        self.assertIn("production_preflight_ok", result["blockers"])
        self.assertIn("production_backup_sha256", result["blockers"])
        self.assertIn("production_active_control_plane_digest", result["blockers"])
        self.assertIn("production_active_worker_digest", result["blockers"])

    def test_missing_required_config_hashes_are_reported_by_audit(self) -> None:
        snapshot = target_snapshot()
        del snapshot["hashes"]["/opt/sub2api/.env"]

        manifest = manifest_tool.build_manifest(
            candidate_control_plane_digest=SHA_A,
            candidate_worker_digest=SHA_B,
            staging_gate_ok=True,
            migration_rehearsal_ok=True,
            production_preflight=preflight_result(),
            target_snapshot=snapshot,
            production_backup_path="/opt/sub2api-backups/prod.dump",
            production_backup_sha256=HEX_C,
            production_backup_restore_verified=True,
            active_control_plane_digest=SHA_C,
            active_worker_digest=SHA_D,
            rollback_control_plane_digest=SHA_C,
            rollback_worker_digest=SHA_D,
            rollback_control_plane_available=True,
            rollback_worker_available=True,
            accepted_candidate_restoration_planned=True,
        )

        result = audit.audit_manifest(manifest)

        self.assertFalse(result["promotion_ready"])
        self.assertIn("production_config_hashes", result["blockers"])


if __name__ == "__main__":
    unittest.main()
