from __future__ import annotations

import importlib.util
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path
from typing import Any
from unittest.mock import patch


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
evidence = load_script("radar_production_evidence_envelope", "production_evidence_envelope.py")


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
    def test_v01178_derives_manifest_migration_count_for_cli_default(self) -> None:
        args = backup_audit.parse_args([
            "--release-id", "release-v01178",
            "--candidate-image-record", "candidate.json",
            "--authorization", "authorization.json",
            "--backup-path", "backup.dump",
            "--restore-envelope", "restore.json",
        ])
        self.assertIsNone(args.expected_schema_migrations)

    def test_bound_backup_uses_predecessor_evidence_hash_for_authorization(self) -> None:
        release_id = "170d4be0-da57-447e-a30a-acdb5dd551c3"
        binding = {
            "candidate_image_record_sha256": "a" * 64,
            "target_id": "b" * 64,
            "host_fingerprint": "c" * 64,
        }
        authorization = evidence.build_envelope(
            evidence_type="authorization",
            release_id=release_id,
            started_at="2026-08-14T12:00:00Z",
            finished_at="2026-08-14T12:01:00Z",
            binding=binding,
            input_evidence_sha256={},
            payload={"operator": "release-owner"},
        )
        restore = evidence.build_envelope(
            evidence_type="migration-rehearsal",
            release_id=release_id,
            started_at="2026-08-14T12:01:00Z",
            finished_at="2026-08-14T12:02:00Z",
            binding=binding,
            input_evidence_sha256={"isolation": "d" * 64},
            payload={"migration_count": 286},
        )
        candidate = {"source_sha256": "e" * 64}

        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            os.chmod(root, 0o700)
            backup_path = root / "backup.dump"
            backup_path.write_bytes(b"x" * 2048)
            os.chmod(backup_path, 0o600)
            candidate_path = root / "candidate.json"
            candidate_path.write_text("{}", encoding="utf-8")
            os.chmod(candidate_path, 0o600)
            authorization_path = root / "authorization.json"
            restore_path = root / "restore.json"
            evidence.write_private_json(authorization_path, authorization)
            evidence.write_private_json(restore_path, restore)
            output_path = root / "backup-envelope.json"

            with patch.object(
                backup_audit,
                "load_candidate_image_record",
                return_value=candidate,
            ), patch.object(
                backup_audit,
                "load_private_envelope",
                side_effect=[authorization, restore],
            ):
                result = backup_audit.build_bound_backup(
                    release_id=release_id,
                    candidate_image_record_path=candidate_path,
                    authorization_path=authorization_path,
                    backup_path=backup_path,
                    restore_path=restore_path,
                    expected_schema_migrations=286,
                    output_path=output_path,
                )

        self.assertEqual(authorization["evidence_sha256"], result["input_evidence_sha256"]["authorization"])

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
