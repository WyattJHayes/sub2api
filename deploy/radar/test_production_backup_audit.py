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


backup_audit = load_script("radar_production_backup_audit", "production_backup_audit.py")


HEX_A = "a" * 64


def backup_document(**overrides: Any) -> dict[str, Any]:
    document: dict[str, Any] = {
        "schema_version": backup_audit.INPUT_SCHEMA_VERSION,
        "path": "/opt/sub2api-backups/radar-prod-20260802/sub2api.dump",
        "sha256": HEX_A,
        "created_at": "2026-08-02T17:20:00Z",
        "checked_at": "2026-08-02T17:30:00Z",
        "size_bytes": 25_000_000,
        "restore_verified": True,
        "restore_schema_migrations": 255,
        "deployment_dir": "/opt/sub2api",
    }
    document.update(overrides)
    return document


class ProductionBackupAuditTests(unittest.TestCase):
    def test_fresh_backup_outside_deployment_with_restore_verification_passes(self) -> None:
        result = backup_audit.audit_backup(
            backup_document(),
            max_age_seconds=3600,
            min_size_bytes=1024,
            expected_schema_migrations=255,
        )

        self.assertTrue(result["ok"], result)
        self.assertEqual([], result["blockers"])
        self.assertEqual(HEX_A, result["summary"]["sha256"])
        self.assertEqual("/opt/sub2api-backups/radar-prod-20260802/sub2api.dump", result["summary"]["path"])

    def test_old_backup_fails_freshness_gate(self) -> None:
        result = backup_audit.audit_backup(
            backup_document(created_at="2026-08-02T15:00:00Z"),
            max_age_seconds=3600,
            min_size_bytes=1024,
            expected_schema_migrations=255,
        )

        self.assertFalse(result["ok"], result)
        self.assertIn("production_backup_fresh", result["blockers"])

    def test_backup_inside_deployment_dir_fails(self) -> None:
        result = backup_audit.audit_backup(
            backup_document(path="/opt/sub2api/backups/sub2api.dump"),
            max_age_seconds=3600,
            min_size_bytes=1024,
            expected_schema_migrations=255,
        )

        self.assertFalse(result["ok"], result)
        self.assertIn("production_backup_outside_deployment_dir", result["blockers"])

    def test_bad_hash_and_missing_restore_verification_fail(self) -> None:
        result = backup_audit.audit_backup(
            backup_document(sha256="not-a-sha", restore_verified=False),
            max_age_seconds=3600,
            min_size_bytes=1024,
            expected_schema_migrations=255,
        )

        self.assertFalse(result["ok"], result)
        self.assertIn("production_backup_sha256", result["blockers"])
        self.assertIn("production_backup_restore_verified", result["blockers"])

    def test_schema_migration_count_must_match_expected(self) -> None:
        result = backup_audit.audit_backup(
            backup_document(restore_schema_migrations=254),
            max_age_seconds=3600,
            min_size_bytes=1024,
            expected_schema_migrations=255,
        )

        self.assertFalse(result["ok"], result)
        self.assertIn("production_backup_schema_migrations", result["blockers"])


if __name__ == "__main__":
    unittest.main()
