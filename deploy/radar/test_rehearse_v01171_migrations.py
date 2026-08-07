from __future__ import annotations

import hashlib
import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
SCRIPT = REPO_ROOT / "deploy" / "radar" / "rehearse-v01171-migrations.sh"


class MigrationRehearsalContractTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp_dir = tempfile.TemporaryDirectory(prefix="radar-migration-rehearsal-")
        root = Path(self.temp_dir.name)
        self.backup = root / "production.dump"
        self.backup.write_bytes(b"disposable pg dump fixture\n")
        self.backup.chmod(0o600)
        self.env_file = root / "rehearsal.env"
        self.env_file.write_text("DATABASE_USER=radar\nDATABASE_PASSWORD=redacted-fixture\n", encoding="utf-8")
        self.env_file.chmod(0o600)
        self.log = root / "docker.log"
        fake_bin = root / "bin"
        fake_bin.mkdir()
        fake_docker = fake_bin / "docker"
        fake_docker.write_text(
            "#!/bin/sh\n"
            "printf '%s\\n' \"$*\" >> \"$RADAR_FAKE_DOCKER_LOG\"\n"
            "if [ \"$1 $2 $3\" = \"container inspect ${RADAR_FAKE_EXISTING_CONTAINER:-}\" ]; "
            "then exit 0; fi\n"
            "if [ \"$1 $2\" = 'container inspect' ] || [ \"$1 $2\" = 'volume inspect' ]; then exit 1; fi\n"
            "if [ \"$1 $2\" = 'network inspect' ] && [ \"${RADAR_FAKE_EXISTING_NETWORK:-0}\" = 1 ]; then exit 0; fi\n"
            "if [ \"$1 $2\" = 'network inspect' ]; then exit 1; fi\n"
            "exit 0\n",
            encoding="utf-8",
        )
        fake_docker.chmod(0o700)
        self.fake_bin = fake_bin

    def tearDown(self) -> None:
        self.temp_dir.cleanup()

    def _base_env(self) -> dict[str, str]:
        digest = hashlib.sha256(self.backup.read_bytes()).hexdigest()
        return {
            "PATH": f"{self.fake_bin}:{os.environ.get('PATH', '')}",
            "RADAR_FAKE_DOCKER_LOG": str(self.log),
            "RADAR_MIGRATION_REHEARSAL_DRY_RUN": "1",
            "RADAR_MIGRATION_REHEARSAL_ENV_FILE": str(self.env_file),
            "RADAR_MIGRATION_REHEARSAL_EVIDENCE_DIR": str(self.backup.parent / "evidence"),
            "RADAR_MIGRATION_REHEARSAL_BACKUP": str(self.backup),
            "RADAR_MIGRATION_REHEARSAL_BACKUP_SHA256": digest,
            "RADAR_MIGRATION_REHEARSAL_PROJECT_NAME": "sub2api-radar-v11-rehearsal",
            "RADAR_MIGRATION_REHEARSAL_DATABASE_HOST": "radar-rehearsal-postgres",
            "RADAR_CONTROL_PLANE_IMAGE": "registry.example.invalid/sub2api/control@sha256:" + "a" * 64,
            "RADAR_CONTROL_PLANE_IMAGE_DIGEST": "sha256:" + "a" * 64,
            "RADAR_WORKER_IMAGE": "registry.example.invalid/sub2api/worker@sha256:" + "c" * 64,
            "RADAR_WORKER_IMAGE_DIGEST": "sha256:" + "c" * 64,
            "RADAR_V10_ROLLBACK_CONTROL_PLANE_IMAGE": "registry.example.invalid/sub2api/control@sha256:" + "b" * 64,
            "RADAR_V10_ROLLBACK_CONTROL_PLANE_IMAGE_DIGEST": "sha256:" + "b" * 64,
            "RADAR_V10_ROLLBACK_WORKER_IMAGE": (
                "registry.example.invalid/sub2api/worker@sha256:" + "d" * 64
            ),
            "RADAR_V10_ROLLBACK_WORKER_IMAGE_DIGEST": "sha256:" + "d" * 64,
        }

    def _run(self, **overrides: str) -> subprocess.CompletedProcess[str]:
        env = self._base_env()
        env.update(overrides)
        return subprocess.run(
            ["bash", str(SCRIPT)],
            cwd=REPO_ROOT,
            env=env,
            capture_output=True,
            text=True,
            check=False,
        )

    def test_missing_backup_is_rejected_before_docker(self) -> None:
        result = self._run(RADAR_MIGRATION_REHEARSAL_BACKUP="")
        self.assertNotEqual(0, result.returncode)
        self.assertIn("backup path", result.stderr)
        self.assertFalse(self.log.exists())

    def test_backup_checksum_mismatch_is_rejected(self) -> None:
        result = self._run(RADAR_MIGRATION_REHEARSAL_BACKUP_SHA256="0" * 64)
        self.assertNotEqual(0, result.returncode)
        self.assertIn("SHA256 mismatch", result.stderr)
        self.assertFalse(self.log.exists())

    def test_candidate_digest_is_required(self) -> None:
        result = self._run(RADAR_CONTROL_PLANE_IMAGE_DIGEST="")
        self.assertNotEqual(0, result.returncode)
        self.assertIn("candidate control-plane digest", result.stderr)
        self.assertFalse(self.log.exists())

    def test_rollback_digest_is_required(self) -> None:
        result = self._run(RADAR_V10_ROLLBACK_CONTROL_PLANE_IMAGE_DIGEST="")
        self.assertNotEqual(0, result.returncode)
        self.assertIn("rollback control-plane digest", result.stderr)
        self.assertFalse(self.log.exists())

    def test_rollback_worker_digest_is_required(self) -> None:
        result = self._run(RADAR_V10_ROLLBACK_WORKER_IMAGE_DIGEST="")
        self.assertNotEqual(0, result.returncode)
        self.assertIn("rollback Worker digest", result.stderr)
        self.assertFalse(self.log.exists())

    def test_rollback_worker_image_must_match_digest(self) -> None:
        result = self._run(
            RADAR_V10_ROLLBACK_WORKER_IMAGE=(
                "registry.example.invalid/sub2api/worker@sha256:" + "e" * 64
            )
        )
        self.assertNotEqual(0, result.returncode)
        self.assertIn("rollback Worker image must end with its digest", result.stderr)
        self.assertFalse(self.log.exists())

    def test_production_project_name_is_rejected(self) -> None:
        result = self._run(RADAR_MIGRATION_REHEARSAL_PROJECT_NAME="sub2api")
        self.assertNotEqual(0, result.returncode)
        self.assertIn("rehearsal project", result.stderr)
        self.assertFalse(self.log.exists())

    def test_production_database_host_is_rejected(self) -> None:
        result = self._run(RADAR_MIGRATION_REHEARSAL_DATABASE_HOST="sub2api-postgres")
        self.assertNotEqual(0, result.returncode)
        self.assertIn("database host", result.stderr)
        self.assertFalse(self.log.exists())

    def test_existing_rehearsal_network_is_rejected_before_create(self) -> None:
        result = self._run(
            RADAR_MIGRATION_REHEARSAL_DRY_RUN="0",
            RADAR_FAKE_EXISTING_NETWORK="1",
        )
        self.assertNotEqual(0, result.returncode)
        self.assertIn("rehearsal network already exists", result.stderr)
        commands = self.log.read_text(encoding="utf-8")
        self.assertIn("network inspect sub2api-radar-v11-rehearsal-network", commands)
        self.assertNotIn("network create", commands)

    def test_existing_rollback_worker_probe_is_rejected_before_create(self) -> None:
        result = self._run(
            RADAR_MIGRATION_REHEARSAL_DRY_RUN="0",
            RADAR_FAKE_EXISTING_CONTAINER="sub2api-radar-v11-rehearsal-rollback-worker",
        )
        self.assertNotEqual(0, result.returncode)
        self.assertIn("rehearsal container already exists", result.stderr)
        commands = self.log.read_text(encoding="utf-8")
        self.assertIn(
            "container inspect sub2api-radar-v11-rehearsal-rollback-worker",
            commands,
        )
        self.assertNotIn("network create", commands)

    def test_valid_fixture_renders_disposable_commands(self) -> None:
        result = self._run()
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("migration rehearsal dry-run passed", result.stdout)
        self.assertIn("pg_restore", result.stdout)
        self.assertIn("sub2api-radar-v11-rehearsal", result.stdout)
        self.assertIn(
            "rollback_worker_image=registry.example.invalid/sub2api/worker@sha256:" + "d" * 64,
            result.stdout,
        )
        self.assertNotIn("redacted-fixture", result.stdout)
        self.assertTrue(self.log.exists())
        summary = json.loads(
            (self.backup.parent / "evidence" / "summary.json").read_text(encoding="utf-8")
        )
        self.assertEqual("radar-v01171-migration-rehearsal-v2", summary["schema_version"])
        self.assertEqual("sha256:" + "a" * 64, summary["candidate_control_plane_digest"])
        self.assertEqual("sha256:" + "c" * 64, summary["candidate_worker_digest"])
        self.assertEqual("sha256:" + "b" * 64, summary["rollback_control_plane_digest"])
        self.assertEqual("sha256:" + "d" * 64, summary["rollback_worker_digest"])
        self.assertFalse(summary["rollback_worker_probe_ok"])


if __name__ == "__main__":
    unittest.main()
