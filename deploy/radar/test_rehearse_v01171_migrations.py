from __future__ import annotations

import hashlib
import json
import os
import shlex
import subprocess
import tempfile
import unittest
from datetime import datetime, timedelta, timezone
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
SCRIPT = REPO_ROOT / "deploy" / "radar" / "rehearse-v01176-migrations.sh"
LEGACY_SCRIPT = REPO_ROOT / "deploy" / "radar" / "rehearse-v01171-migrations.sh"
IMPLEMENTATION_SCRIPT = LEGACY_SCRIPT


class MigrationRehearsalContractTests(unittest.TestCase):
    def test_v01176_entrypoint_is_present_and_legacy_implementation_is_retained(self) -> None:
        self.assertTrue(SCRIPT.is_file())
        self.assertTrue(LEGACY_SCRIPT.is_file())

    def setUp(self) -> None:
        self.temp_dir = tempfile.TemporaryDirectory(prefix="radar-migration-rehearsal-")
        root = Path(self.temp_dir.name)
        self.backup = root / "production.dump"
        self.backup.write_bytes(b"disposable pg dump fixture\n")
        self.backup.chmod(0o600)
        self.env_file = root / "rehearsal.env"
        self.env_file.write_text("DATABASE_USER=radar\n", encoding="utf-8")
        self.env_file.chmod(0o600)
        self.private = root / "private"
        self.private.mkdir(mode=0o700)
        self.postgres_password_file = self.private / "postgres-password"
        self.database_password_file = self.private / "database-password"
        self.pgpass_file = self.private / "database.pgpass"
        self.postgres_password_file.write_text("redacted-fixture\n", encoding="utf-8")
        self.database_password_file.write_text("redacted-fixture\n", encoding="utf-8")
        self.pgpass_file.write_text("*:*:*:*:redacted-fixture\n", encoding="utf-8")
        for path in (self.postgres_password_file, self.database_password_file, self.pgpass_file):
            path.chmod(0o600)
        self.log = root / "docker.log"
        self.retention_run_id = "task7-helper-rehearsal"
        self.retention_evidence_dir = root / "top-evidence"
        self.retention_script = REPO_ROOT / "deploy" / "radar" / "run-local-prerelease.sh"
        self.retention_gate2_project = "sub2api-radar-v11-rehearsal"
        self.retention_gate4_project = "sub2api-radar-v11-gate4-rehearsal"
        self.retention_mutator = root / "retention-mutator"
        self.retention_mutator.write_text(
            "#!/usr/bin/env python3\n"
            "import json, os, shlex, time\n"
            "from pathlib import Path\n"
            "mutation = os.environ.get('RADAR_FAKE_RETENTION_MUTATION', '')\n"
            "record = Path(os.environ['RADAR_MIGRATION_REHEARSAL_RETENTION_RECORD'])\n"
            "if mutation == 'expire':\n"
            "    time.sleep(float(os.environ['RADAR_FAKE_RETENTION_DELAY_SECONDS']))\n"
            "elif mutation == 'mode':\n"
            "    record.chmod(0o640)\n"
            "else:\n"
            "    document = json.loads(record.read_text(encoding='utf-8'))\n"
            "    run_id = os.environ['RADAR_MIGRATION_REHEARSAL_RETENTION_RUN_ID']\n"
            "    evidence_dir = os.environ['RADAR_MIGRATION_REHEARSAL_RETENTION_EVIDENCE_DIR']\n"
            "    script = os.environ['RADAR_MIGRATION_REHEARSAL_RETENTION_SCRIPT']\n"
            "    if mutation == 'project':\n"
            "        document['migration_projects'] = ['other-phase-rehearsal']\n"
            "    elif mutation == 'cleanup-command':\n"
            "        document['cleanup_command'] = 'printf --cleanup-retained'\n"
            "    elif mutation == 'deadline-window':\n"
            "        document['deadline_seconds'] = 1\n"
            "    elif mutation == 'run-id':\n"
            "        run_id = 'other-rehearsal'\n"
            "    elif mutation == 'evidence-dir':\n"
            "        evidence_dir = '/tmp/other-evidence'\n"
            "    elif mutation == 'script':\n"
            "        script = '/tmp/other-run-local-prerelease.sh'\n"
            "    elif mutation == 'schema-missing':\n"
            "        document.pop('schema_version')\n"
            "    elif mutation == 'schema-extra':\n"
            "        document['extra'] = True\n"
            "    elif mutation == 'seconds-bool':\n"
            "        document['deadline_seconds'] = True\n"
            "    elif mutation == 'seconds-float':\n"
            "        document['deadline_seconds'] = 600.0\n"
            "    elif mutation == 'seconds-mismatch':\n"
            "        document['deadline_seconds'] = 601\n"
            "    elif mutation == 'project-order':\n"
            "        document['migration_projects'].reverse()\n"
            "    if mutation in {'run-id', 'evidence-dir', 'script'}:\n"
            "        document['cleanup_command'] = shlex.join([\n"
            "            'env', f'RADAR_LOCAL_RUN_ID={run_id}',\n"
            "            f'RADAR_LOCAL_EVIDENCE_DIR={evidence_dir}', script, '--cleanup-retained',\n"
            "        ])\n"
            "    record.write_text(json.dumps(document), encoding='utf-8')\n"
            "    record.chmod(0o600)\n",
            encoding="utf-8",
        )
        self.retention_mutator.chmod(0o700)
        fake_bin = root / "bin"
        fake_bin.mkdir()
        fake_docker = fake_bin / "docker"
        fake_docker.write_text(
            "#!/bin/sh\n"
            "if env | /usr/bin/grep -Fqx 'RADAR_MIGRATION_REHEARSAL_DATABASE_PASSWORD=raw-process-sentinel'; then\n"
            "  printf '%s\\n' 'raw-process-sentinel' >> \"$RADAR_FAKE_DOCKER_LOG\"\n"
            "  exit 97\n"
            "fi\n"
            "case \"$*\" in\n"
            "  exec\\ *\\ pg_dump\\ -Fc*)\n"
            "    printf '%s\\n' \"$*\" >> \"$RADAR_FAKE_DOCKER_LOG\"\n"
            "    : > \"${RADAR_FAKE_DOCKER_LOG}.clone-dump-started\"\n"
            "    ;;\n"
            "  exec\\ -i\\ -e\\ *rollback-postgres\\ pg_restore*)\n"
            "    while [ ! -f \"${RADAR_FAKE_DOCKER_LOG}.clone-dump-started\" ]; do /bin/sleep 0.01; done\n"
            "    /bin/cat >/dev/null\n"
            "    printf '%s\\n' \"$*\" >> \"$RADAR_FAKE_DOCKER_LOG\"\n"
            "    ;;\n"
            "  *) printf '%s\\n' \"$*\" >> \"$RADAR_FAKE_DOCKER_LOG\" ;;\n"
            "esac\n"
            "case \"$*\" in\n"
            "  run\\ --rm\\ --name\\ *-rollback-worker*)\n"
            "    if [ -n \"${RADAR_FAKE_RETENTION_MUTATION:-}\" ]; then \"$RADAR_FAKE_RETENTION_MUTATOR\"; fi\n"
            "    ;;\n"
            "esac\n"
            "if [ \"$1 $2 $3\" = \"container inspect ${RADAR_FAKE_EXISTING_CONTAINER:-}\" ]; "
            "then exit 0; fi\n"
            "if [ \"$1 $2\" = 'container inspect' ] || [ \"$1 $2\" = 'volume inspect' ]; then exit 1; fi\n"
            "if [ \"$1 $2\" = 'network inspect' ] && [ \"${RADAR_FAKE_EXISTING_NETWORK:-0}\" = 1 ]; then exit 0; fi\n"
            "if [ \"$1 $2\" = 'network inspect' ]; then exit 1; fi\n"
            "if [ \"$1\" = inspect ]; then\n"
            "  case \"$4\" in\n"
            "    *rollback-postgres) printf '%s\\n' '10.77.0.12' ;;\n"
            "    *) printf '%s\\n' '10.77.0.11' ;;\n"
            "  esac\n"
            "  exit 0\n"
            "fi\n"
            "case \"$*\" in\n"
            "  *\"SELECT filename || '|' || checksum FROM schema_migrations\"*)\n"
            "    printf '%s\\n' \"$RADAR_FAKE_SCHEMA_MIGRATIONS\"\n"
            "    exit 0\n"
            "    ;;\n"
            "  *\"SELECT COUNT(*) FROM schema_migrations\"*)\n"
            "    printf '%s\\n' \"$RADAR_FAKE_MIGRATION_COUNT\"\n"
            "    exit 0\n"
            "    ;;\n"
            "  *\"information_schema.columns\"*) printf '%s\\n' 2; exit 0 ;;\n"
            "  *\"FROM groups WHERE long_context_pricing_enabled\"*) printf '%s\\n' 0; exit 0 ;;\n"
            "  *\"uq_quality_reports_tenant_run_model_revision\"*) printf '%s\\n' 1; exit 0 ;;\n"
            "  *\"ARRAY['id','tenant_id']::name[]\"*) printf '%s\\n' 1; exit 0 ;;\n"
            "  *\"FROM pg_constraint c\"*) printf '%s\\n' 0; exit 0 ;;\n"
            "  *\" pg_dump \"*)\n"
            "    if [ -n \"${RADAR_FAKE_CLONE_DUMP_DELAY_SECONDS:-}\" ]; then\n"
            "      /bin/sleep \"$RADAR_FAKE_CLONE_DUMP_DELAY_SECONDS\"\n"
            "    fi\n"
            "    if [ \"${RADAR_FAKE_CLONE_MODE:-success}\" = sleep ]; then exec /bin/sleep 5; fi\n"
            "    printf '%s\\n' 'fake-clone-archive'\n"
            "    exit 0\n"
            "    ;;\n"
            "esac\n"
            "exit 0\n",
            encoding="utf-8",
        )
        fake_docker.chmod(0o700)
        fake_sleep = fake_bin / "sleep"
        fake_sleep.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        fake_sleep.chmod(0o700)
        self.fake_bin = fake_bin
        self.ledger_stub = root / "migration-ledger-stub.py"
        self.ledger_stub.write_text(
            "#!/usr/bin/env python3\n"
            "import json, sys\n"
            "args = sys.argv[1:]\n"
            "output = args[args.index('--output') + 1]\n"
            "with open(output, 'w', encoding='utf-8') as stream:\n"
            "    json.dump({\n"
            "        'ok': True, 'baseline_schema_migrations': 285,\n"
            "        'actual_schema_migrations': 293, 'expected_schema_migrations': 293,\n"
            "        'candidate_pending_migrations': [],\n"
            "        'legacy_entries': ['202_add_radar_tracked_models.sql', '207_scope_radar_tracked_models_by_tenant.sql'],\n"
            "        'checksum_mismatches': [],\n"
            "        'baseline_ledger_sha256': '1' * 64,\n"
            "        'candidate_ledger_sha256': '2' * 64,\n"
            "        'expected_candidate_ledger_sha256': '2' * 64,\n"
            "        'expected_runtime_ledger_sha256': '3' * 64,\n"
            "        'runtime_ledger_sha256': '3' * 64,\n"
            "    }, stream)\n",
            encoding="utf-8",
        )
        self.ledger_stub.chmod(0o700)
        migration_names = (
            "191_add_radar_control_plane.sql",
            "191_passkey_credentials.sql",
            "192_add_evaluation_sample_execution_identity.sql",
            "192_group_profit_control.sql",
            "193_add_radar_grading_statistics.sql",
            "193_group_profit_control_auth_cache_invalidation.sql",
            "221_add_radar_tracked_models.sql",
            "222_add_radar_quality_reports.sql",
            "223_add_quality_observation_context.sql",
            "224_add_quality_report_aggregate_revision.sql",
            "221_group_model_pricing.sql",
            "222_group_usage_daily_rollups.sql",
            "223_group_usage_rollup_timezone.sql",
            "224_user_platform_quotas_add_cn_providers.sql",
            "225_backfill_codex_fingerprint_seed.sql",
            "225_channel_model_time_pricing.sql",
            "226_channel_monitor_quota_mode.sql",
            "227_ops_model_not_found_sla_classification.sql",
        )
        self.schema_migrations = "\n".join(
            f"{name}|{self._normalized_migration_checksum(name)}" for name in migration_names
        )

    @staticmethod
    def _normalized_migration_checksum(name: str) -> str:
        content = (REPO_ROOT / "backend" / "migrations" / name).read_text(encoding="utf-8").strip()
        return hashlib.sha256(content.encode("utf-8")).hexdigest()

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
            "RADAR_MIGRATION_REHEARSAL_POSTGRES_PASSWORD_FILE": str(self.postgres_password_file),
            "RADAR_MIGRATION_REHEARSAL_DATABASE_PASSWORD_FILE": str(self.database_password_file),
            "RADAR_MIGRATION_REHEARSAL_PGPASS_FILE": str(self.pgpass_file),
            "RADAR_FAKE_SCHEMA_MIGRATIONS": self.schema_migrations,
            "RADAR_FAKE_MIGRATION_COUNT": "293",
            "RADAR_MIGRATION_LEDGER_TOOL": str(self.ledger_stub),
            "RADAR_FAKE_RETENTION_MUTATOR": str(self.retention_mutator),
            "RADAR_MIGRATION_REHEARSAL_RETENTION_SECONDS": "600",
            "RADAR_MIGRATION_REHEARSAL_RETENTION_RUN_ID": self.retention_run_id,
            "RADAR_MIGRATION_REHEARSAL_RETENTION_EVIDENCE_DIR": str(self.retention_evidence_dir),
            "RADAR_MIGRATION_REHEARSAL_RETENTION_SCRIPT": str(self.retention_script),
            "RADAR_MIGRATION_REHEARSAL_RETENTION_GATE2_PROJECT": self.retention_gate2_project,
            "RADAR_MIGRATION_REHEARSAL_RETENTION_GATE4_PROJECT": self.retention_gate4_project,
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

    def _retention_record(
        self,
        project: str = "sub2api-radar-v11-rehearsal",
        deadline_seconds: int = 600,
    ) -> Path:
        path = self.backup.parent / "retention.json"
        path.write_text(json.dumps({
            "schema_version": "radar-local-retention-v1",
            "deadline": (datetime.now(timezone.utc) + timedelta(seconds=deadline_seconds)).isoformat().replace("+00:00", "Z"),
            "deadline_seconds": deadline_seconds,
            "cleanup_command": shlex.join([
                "env",
                f"RADAR_LOCAL_RUN_ID={self.retention_run_id}",
                f"RADAR_LOCAL_EVIDENCE_DIR={self.retention_evidence_dir}",
                str(self.retention_script),
                "--cleanup-retained",
            ]),
            "migration_projects": [project, self.retention_gate4_project],
        }))
        path.chmod(0o600)
        return path

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

    def test_legacy_raw_password_environment_is_removed_before_docker(self) -> None:
        result = self._run(
            RADAR_MIGRATION_REHEARSAL_DRY_RUN="0",
            RADAR_MIGRATION_REHEARSAL_DATABASE_PASSWORD="raw-process-sentinel",
        )
        self.assertEqual(0, result.returncode, result.stderr)
        commands = self.log.read_text(encoding="utf-8")
        self.assertNotIn("raw-process-sentinel", commands)

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

    def test_v01178_requires_official_migrations_and_rollback_clone(self) -> None:
        body = IMPLEMENTATION_SCRIPT.read_text(encoding="utf-8")
        self.assertIn('"221_add_radar_tracked_models.sql"', body)
        self.assertIn('"222_add_radar_quality_reports.sql"', body)
        self.assertIn('"223_add_quality_observation_context.sql"', body)
        self.assertIn('"224_add_quality_report_aggregate_revision.sql"', body)
        self.assertIn('"221_group_model_pricing.sql"', body)
        self.assertIn('"226_channel_monitor_quota_mode.sql"', body)
        self.assertIn('"227_ops_model_not_found_sla_classification.sql"', body)
        self.assertIn('ROLLBACK_DB_CONTAINER="${RESOURCE_PREFIX}-rollback-postgres"', body)
        self.assertIn('ROLLBACK_VOLUME="${RESOURCE_PREFIX}-rollback-postgres"', body)
        self.assertIn("migration_224_semantics_ok", body)
        self.assertIn("migration_225_semantics_ok", body)
        self.assertIn('[[ "$MIGRATION_COUNT" == "$EXPECTED_SCHEMA_MIGRATIONS" ]]', body)
        self.assertIn("migration ledger validation failed", body)
        self.assertIn("rollback_database_clone_used", body)
        self.assertIn("192.255.134.229", body)

    def test_migration_224_preservation_check_uses_constraint_columns(self) -> None:
        body = IMPLEMENTATION_SCRIPT.read_text(encoding="utf-8")
        self.assertNotIn("quality_reports_id_tenant_key", body)
        self.assertRegex(
            body,
            r"array_agg\(a\.attname ORDER BY a\.attname\).*ARRAY\['id',\s*'tenant_id'\]::name\[\]",
        )

    def test_overseas_production_host_is_rejected_before_docker(self) -> None:
        result = self._run(
            RADAR_MIGRATION_REHEARSAL_DATABASE_HOST="192.255.134.229",
        )
        self.assertNotEqual(0, result.returncode)
        self.assertIn("production database host", result.stderr)
        self.assertFalse(self.log.exists())

    def test_invalid_clone_timeout_is_rejected_before_docker(self) -> None:
        result = self._run(
            RADAR_MIGRATION_REHEARSAL_CLONE_TIMEOUT_SECONDS="0",
        )
        self.assertNotEqual(0, result.returncode)
        self.assertIn("CLONE_TIMEOUT_SECONDS", result.stderr)
        self.assertFalse(self.log.exists())

    def test_invalid_retain_value_is_rejected_before_docker(self) -> None:
        result = self._run(RADAR_MIGRATION_REHEARSAL_RETAIN_VOLUMES="yes")
        self.assertNotEqual(0, result.returncode)
        self.assertIn("RETAIN_VOLUMES", result.stderr)
        self.assertFalse(self.log.exists())

    def test_retain_requires_matching_bounded_authorization_record(self) -> None:
        missing = self._run(RADAR_MIGRATION_REHEARSAL_RETAIN_VOLUMES="1")
        self.assertNotEqual(0, missing.returncode)
        self.assertFalse(self.log.exists())
        wrong_record = self._retention_record("other-phase-rehearsal")
        wrong = self._run(
            RADAR_MIGRATION_REHEARSAL_RETAIN_VOLUMES="1",
            RADAR_MIGRATION_REHEARSAL_RETENTION_RECORD=str(wrong_record),
        )
        self.assertNotEqual(0, wrong.returncode)
        self.assertIn("retention", wrong.stderr)
        self.assertFalse(self.log.exists())

    def test_default_cleanup_removes_both_migration_volumes(self) -> None:
        result = self._run(RADAR_MIGRATION_REHEARSAL_DRY_RUN="0")
        self.assertEqual(0, result.returncode, result.stderr)
        commands = self.log.read_text().splitlines()
        self.assertIn("volume rm sub2api-radar-v11-rehearsal-postgres", commands)
        self.assertIn("volume rm sub2api-radar-v11-rehearsal-rollback-postgres", commands)

    def test_authorized_retain_keeps_only_exact_labeled_volumes(self) -> None:
        for project in (self.retention_gate2_project, self.retention_gate4_project):
            with self.subTest(project=project):
                self.log.unlink(missing_ok=True)
                record = self._retention_record()
                result = self._run(
                    RADAR_MIGRATION_REHEARSAL_DRY_RUN="0",
                    RADAR_MIGRATION_REHEARSAL_PROJECT_NAME=project,
                    RADAR_MIGRATION_REHEARSAL_RETAIN_VOLUMES="1",
                    RADAR_MIGRATION_REHEARSAL_RETENTION_RECORD=str(record),
                )
                self.assertEqual(0, result.returncode, result.stderr)
                commands = self.log.read_text().splitlines()
                self.assertNotIn(f"volume rm {project}-postgres", commands)
                self.assertNotIn(f"volume rm {project}-rollback-postgres", commands)
                self.assertTrue(any(command.startswith("rm -f ") for command in commands))
                self.assertIn(f"network rm {project}-network", commands)
                self.assertIn(
                    f"volume create --label radar.rehearsal.project={project} {project}-postgres",
                    commands,
                )
                self.assertIn(
                    f"volume create --label radar.rehearsal.project={project} {project}-rollback-postgres",
                    commands,
                )

    def test_cleanup_revalidates_expiry_mode_and_project_before_retaining_volumes(self) -> None:
        for mutation, deadline_seconds, delay_seconds in (
            ("expire", 4, "5"),
            ("mode", 600, "0"),
            ("project", 600, "0"),
        ):
            with self.subTest(mutation=mutation):
                self.log.unlink(missing_ok=True)
                record = self._retention_record(deadline_seconds=deadline_seconds)
                result = self._run(
                    RADAR_MIGRATION_REHEARSAL_DRY_RUN="0",
                    RADAR_MIGRATION_REHEARSAL_RETAIN_VOLUMES="1",
                    RADAR_MIGRATION_REHEARSAL_RETENTION_RECORD=str(record),
                    RADAR_MIGRATION_REHEARSAL_RETENTION_SECONDS=str(deadline_seconds),
                    RADAR_FAKE_RETENTION_MUTATION=mutation,
                    RADAR_FAKE_RETENTION_DELAY_SECONDS=delay_seconds,
                )
                self.assertEqual(0, result.returncode, result.stderr)
                commands = self.log.read_text().splitlines()
                self.assertIn("volume rm sub2api-radar-v11-rehearsal-postgres", commands)
                self.assertIn("volume rm sub2api-radar-v11-rehearsal-rollback-postgres", commands)

    def test_cleanup_revalidates_exact_retention_contract_before_retaining_volumes(self) -> None:
        for mutation in (
            "cleanup-command",
            "deadline-window",
            "run-id",
            "evidence-dir",
            "script",
            "schema-missing",
            "schema-extra",
            "seconds-bool",
            "seconds-float",
            "seconds-mismatch",
            "project-order",
        ):
            with self.subTest(mutation=mutation):
                self.log.unlink(missing_ok=True)
                record = self._retention_record()
                result = self._run(
                    RADAR_MIGRATION_REHEARSAL_DRY_RUN="0",
                    RADAR_MIGRATION_REHEARSAL_RETAIN_VOLUMES="1",
                    RADAR_MIGRATION_REHEARSAL_RETENTION_RECORD=str(record),
                    RADAR_FAKE_RETENTION_MUTATION=mutation,
                    RADAR_FAKE_RETENTION_DELAY_SECONDS="0",
                )
                self.assertEqual(0, result.returncode, result.stderr)
                commands = self.log.read_text().splitlines()
                self.assertIn("volume rm sub2api-radar-v11-rehearsal-postgres", commands)
                self.assertIn("volume rm sub2api-radar-v11-rehearsal-rollback-postgres", commands)

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
        self.assertEqual("radar-v01178-migration-rehearsal-v4", summary["schema_version"])
        self.assertEqual("sha256:" + "a" * 64, summary["candidate_control_plane_digest"])
        self.assertEqual("sha256:" + "c" * 64, summary["candidate_worker_digest"])
        self.assertEqual("sha256:" + "b" * 64, summary["rollback_control_plane_digest"])
        self.assertEqual("sha256:" + "d" * 64, summary["rollback_worker_digest"])
        self.assertFalse(summary["rollback_worker_probe_ok"])

    def test_clone_precedes_rollback_container_and_uses_clone_address(self) -> None:
        result = self._run(RADAR_MIGRATION_REHEARSAL_DRY_RUN="0")
        self.assertEqual(0, result.returncode, result.stderr)

        commands = self.log.read_text(encoding="utf-8").splitlines()
        clone_dump_index = next(
            index
            for index, command in enumerate(commands)
            if command.startswith("exec sub2api-radar-v11-rehearsal-postgres pg_dump -Fc")
        )
        clone_restore_index = next(
            index
            for index, command in enumerate(commands)
            if command.startswith("exec -i -e PGPASSFILE=/run/secrets/radar-database.pgpass ")
            and "sub2api-radar-v11-rehearsal-rollback-postgres pg_restore" in command
        )
        rollback_start_index = next(
            index
            for index, command in enumerate(commands)
            if command.startswith("run -d --name sub2api-radar-v11-rehearsal-rollback ")
        )

        self.assertLess(clone_dump_index, clone_restore_index)
        self.assertLess(clone_restore_index, rollback_start_index)
        self.assertIn("DATABASE_HOST=10.77.0.12", commands[rollback_start_index])
        recorded = "\n".join(commands)
        self.assertNotIn("redacted-fixture", recorded)
        self.assertNotIn("POSTGRES_PASSWORD=", recorded)
        self.assertNotIn("-e DATABASE_PASSWORD=", recorded)
        summary = json.loads(
            (self.backup.parent / "evidence" / "summary.json").read_text(encoding="utf-8")
        )
        self.assertTrue(summary["rollback_database_clone_used"])
        self.assertEqual(285, summary["baseline_schema_migrations"])
        self.assertEqual(293, summary["actual_schema_migrations"])
        self.assertEqual([], summary["candidate_pending_migrations"])
        self.assertEqual(
            ["202_add_radar_tracked_models.sql", "207_scope_radar_tracked_models_by_tenant.sql"],
            summary["legacy_entries"],
        )
        self.assertEqual([], summary["checksum_mismatches"])
        self.assertEqual("1" * 64, summary["baseline_ledger_sha256"])
        self.assertEqual("2" * 64, summary["candidate_ledger_sha256"])
        self.assertEqual("2" * 64, summary["expected_candidate_ledger_sha256"])
        self.assertEqual("3" * 64, summary["expected_runtime_ledger_sha256"])
        self.assertEqual("3" * 64, summary["runtime_ledger_sha256"])

    def test_clone_stream_handles_a_delayed_dump_producer(self) -> None:
        result = self._run(
            RADAR_MIGRATION_REHEARSAL_DRY_RUN="0",
            RADAR_FAKE_CLONE_DUMP_DELAY_SECONDS="0.2",
        )
        self.assertEqual(0, result.returncode, result.stderr)

    def test_clone_timeout_returns_nonzero_and_triggers_cleanup(self) -> None:
        result = self._run(
            RADAR_MIGRATION_REHEARSAL_DRY_RUN="0",
            RADAR_MIGRATION_REHEARSAL_CLONE_TIMEOUT_SECONDS="1",
            RADAR_FAKE_CLONE_MODE="sleep",
        )
        self.assertNotEqual(0, result.returncode)
        self.assertIn("rollback PostgreSQL clone timed out after 1 seconds", result.stderr)
        commands = self.log.read_text(encoding="utf-8")
        self.assertIn("exec sub2api-radar-v11-rehearsal-postgres pg_dump -Fc", commands)
        self.assertIn("rm -f", commands)

    def test_postgres_18_and_runtime_checksum_contracts_are_preserved(self) -> None:
        body = IMPLEMENTATION_SCRIPT.read_text(encoding="utf-8")
        self.assertIn('-v "$VOLUME:/var/lib/postgresql"', body)
        self.assertNotIn('-v "$VOLUME:/var/lib/postgresql/data"', body)
        self.assertIn('content = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8").strip()', body)
        self.assertEqual(2, body.count('checksum=$(migration_checksum "$file")'))
        self.assertIn(
            "DATABASE_HOST=$(docker inspect -f "
            "'{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' \"$DB_CONTAINER\")",
            body,
        )

    def test_rollback_control_plane_bootstraps_disposable_configuration(self) -> None:
        body = IMPLEMENTATION_SCRIPT.read_text(encoding="utf-8")
        rollback_start = body.index('docker run -d --name "$ROLLBACK_CONTAINER"')
        rollback_worker_start = body.index('docker run --rm --name "$ROLLBACK_WORKER_CONTAINER"')
        rollback_block = body[rollback_start:rollback_worker_start]
        self.assertIn('-e AUTO_SETUP=true', rollback_block)
        self.assertNotIn('-e AUTO_SETUP=false', rollback_block)

    def test_candidate_control_plane_reads_password_file_inside_container(self) -> None:
        result = self._run(RADAR_MIGRATION_REHEARSAL_DRY_RUN="0")
        self.assertEqual(0, result.returncode, result.stderr)

        candidate_command = next(
            command
            for command in self.log.read_text(encoding="utf-8").splitlines()
            if command.startswith("run -d --name sub2api-radar-v11-rehearsal-candidate ")
        )
        self.assertIn("--entrypoint /bin/sh", candidate_command)
        self.assertIn(
            'export DATABASE_PASSWORD="$(cat "$DATABASE_PASSWORD_FILE")"',
            candidate_command,
        )
        self.assertIn("exec /app/docker-entrypoint.sh /app/sub2api", candidate_command)
        self.assertNotIn("-e DATABASE_PASSWORD=", candidate_command)
        self.assertNotIn("redacted-fixture", candidate_command)

    def test_rollback_control_plane_reads_password_file_inside_container(self) -> None:
        result = self._run(RADAR_MIGRATION_REHEARSAL_DRY_RUN="0")
        self.assertEqual(0, result.returncode, result.stderr)

        rollback_command = next(
            command
            for command in self.log.read_text(encoding="utf-8").splitlines()
            if command.startswith("run -d --name sub2api-radar-v11-rehearsal-rollback ")
        )
        self.assertIn("--entrypoint /bin/sh", rollback_command)
        self.assertIn(
            'export DATABASE_PASSWORD="$(cat "$DATABASE_PASSWORD_FILE")"',
            rollback_command,
        )
        self.assertIn("exec /app/docker-entrypoint.sh /app/sub2api", rollback_command)
        self.assertNotIn("-e DATABASE_PASSWORD=", rollback_command)
        self.assertNotIn("redacted-fixture", rollback_command)


class MigrationManifestSelectionTests(unittest.TestCase):
    def test_current_rehearsal_defaults_to_the_v020_migration_manifest(self) -> None:
        body = IMPLEMENTATION_SCRIPT.read_text(encoding="utf-8")
        self.assertIn("/deploy/radar/manifests/v0.2.0", body)
        self.assertNotIn("/deploy/radar/manifests/v0.1.178", body)
        self.assertNotIn("/deploy/radar/manifests/v0.1.181", body)


if __name__ == "__main__":
    unittest.main()
