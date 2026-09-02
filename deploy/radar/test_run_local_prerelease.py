from __future__ import annotations

import json
import os
import stat
import subprocess
import tempfile
import textwrap
import time
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "deploy" / "radar" / "run-local-prerelease.sh"


def bindings(run_id: str = "task7-script-rehearsal") -> dict[str, object]:
    return {
        "run_id": run_id,
        "source_sha256": "a" * 64,
        "backup_sha256": "b" * 64,
        "control_plane_digest": "sha256:" + "c" * 64,
        "worker_digest": "sha256:" + "d" * 64,
        "rollback_control_plane_digest": "sha256:" + "e" * 64,
        "rollback_worker_digest": "sha256:" + "f" * 64,
        "policy_version": "quality-v1",
        "fixture_version": "local-quality-fixture-v1",
        "dependency_digests": ["sha256:" + "1" * 64],
        "environment_fingerprint": "2" * 64,
        "source_exclusions": {"metadata": True},
    }


class RunLocalPrereleaseTests(unittest.TestCase):
    def _executable(self, path: Path, body: str) -> None:
        path.write_text("#!/usr/bin/env bash\nset -eu\n" + textwrap.dedent(body), encoding="utf-8")
        path.chmod(0o700)

    def _private_material(self, evidence: Path, frontend_results: Path) -> None:
        (evidence / "private").mkdir(parents=True, exist_ok=True, mode=0o700)
        for relative in (
            "rehearsal.env",
            "private/runtime.env",
            "private/fixture-manifest.json",
            "private/postgres-password",
            "private/database-password",
            "private/database.pgpass",
        ):
            target = evidence / relative
            target.write_text("SYNTHETIC_PRIVATE_VALUE\n")
            target.chmod(0o600)
        (frontend_results / ".auth").mkdir(parents=True)
        (frontend_results / "artifacts").mkdir()
        (frontend_results / "playwright.json").write_text("private")
        (frontend_results / "playwright.xml").write_text("private")

    def _assert_private_material_removed(self, evidence: Path, frontend_results: Path) -> None:
        self.assertFalse((evidence / "rehearsal.env").exists())
        self.assertFalse((evidence / "private" / "runtime.env").exists())
        self.assertFalse((evidence / "private" / "fixture-manifest.json").exists())
        self.assertFalse((evidence / "private" / "postgres-password").exists())
        self.assertFalse((evidence / "private" / "database-password").exists())
        self.assertFalse((evidence / "private" / "database.pgpass").exists())
        self.assertFalse((frontend_results / ".auth").exists())
        self.assertFalse((frontend_results / "artifacts").exists())
        self.assertFalse((frontend_results / "playwright.json").exists())
        self.assertFalse((frontend_results / "playwright.xml").exists())

    def test_dry_run_prints_only_ordered_phases_and_redacted_paths(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            marker = root / "external-called"
            fake_bin = root / "bin"
            fake_bin.mkdir()
            for name in ("docker", "curl", "npm", "pnpm", "go"):
                self._executable(fake_bin / name, f"touch {marker!s}\nexit 91\n")
            environment = os.environ | {
                "PATH": f"{fake_bin}:{os.environ['PATH']}",
                "RADAR_LOCAL_PRERELEASE_DRY_RUN": "1",
            }
            completed = subprocess.run([str(SCRIPT)], cwd=ROOT, env=environment, text=True, capture_output=True, check=False)
            self.assertEqual(completed.returncode, 0, completed.stderr)
            self.assertFalse(marker.exists())
            lines = completed.stdout.splitlines()
            self.assertEqual([line for line in lines if line.startswith("phase=")], [
                "phase=1 immutable-inputs-and-code",
                "phase=2 migration-225",
                "phase=3 radar-browser-workflows",
                "phase=4 restart-and-rollback",
                "phase=5 evidence-closure-input",
            ])
            self.assertTrue(any("<redacted-evidence>" in line for line in lines))
            self.assertNotIn("192.255.134.229", completed.stdout + completed.stderr)
            self.assertNotIn("sub2api.weihub.cloud", completed.stdout + completed.stderr)
            invalid = subprocess.run(
                [str(SCRIPT)],
                cwd=ROOT,
                env=environment | {"RADAR_LOCAL_BROWSER_ORIGIN": "http://127.0.0.1:99999"},
                text=True,
                capture_output=True,
                check=False,
            )
            self.assertNotEqual(invalid.returncode, 0)
            self.assertFalse(marker.exists())

    def test_gate1_declares_the_complete_required_validation_command_set(self) -> None:
        body = SCRIPT.read_text(encoding="utf-8")
        required = (
            'python3 "$PREFLIGHT_TOOL" --candidate-root "$ROOT_DIR"',
            'python3 "$CLOSURE_TOOL" clock-sanity --trusted-utc "$TRUSTED_UTC"',
            'run_in_dir "$ROOT_DIR/backend" go test ./...',
            'run_in_dir "$ROOT_DIR/backend" go test -tags unit ./internal/server/middleware',
            'run_in_dir "$ROOT_DIR/radar-worker" uv run --extra dev pytest -q',
            'run_in_dir "$ROOT_DIR/radar-worker" uv run --extra dev ruff check src tests',
            'run_in_dir "$ROOT_DIR/radar-worker" uv run --extra dev mypy src',
            'pnpm --dir "$ROOT_DIR/frontend" run lint:check',
            'pnpm --dir "$ROOT_DIR/frontend" run typecheck',
            'pnpm --dir "$ROOT_DIR/frontend" run test:run',
            'pnpm --dir "$ROOT_DIR/frontend" run build',
            'run_in_dir "$ROOT_DIR" python3 -m unittest discover -s deploy/radar -p \'test_*.py\' -v',
            'run_in_dir "$ROOT_DIR/radar-worker" uv run --extra dev pytest "$ROOT_DIR/deploy/tests/test_radar_quality_report_contract.py" -q',
        )
        for command in required:
            self.assertIn(command, body)
        positions = [body.index(command) for command in required]
        self.assertEqual(positions, sorted(positions))

    def test_gate3_registers_workers_inside_fixture_tenant_orchestration(self) -> None:
        body = SCRIPT.read_text(encoding="utf-8")
        fixture_command = body.index('python3 "$FIXTURE_TOOL"')
        self.assertNotIn('python3 "$CLOSURE_TOOL" register-workers', body)
        for argument in (
            '--worker-bindings "$EVIDENCE_DIR/bindings.json"',
            '--worker-environment "$environment_file"',
            '--worker-registration-output "$EVIDENCE_DIR/private/worker-registration.json"',
        ):
            self.assertGreater(body.index(argument), fixture_command)

    def test_gate3_starts_and_cleans_loopback_forwarder_before_fixture(self) -> None:
        body = SCRIPT.read_text(encoding="utf-8")
        compose_start = body.index('compose_command up -d --no-build --pull never --wait --wait-timeout')
        runtime_isolation = body.index("verify_runtime_isolation || return $?")
        signing_key_init = body.index("initialize_evidence_signing_key || return $?")
        forwarder_start = body.index('start_loopback_forwarder || return $?')
        fixture_command = body.index('python3 "$FIXTURE_TOOL"')
        self.assertLess(compose_start, signing_key_init)
        self.assertLess(compose_start, runtime_isolation)
        self.assertLess(runtime_isolation, signing_key_init)
        self.assertIn(
            '--output "$EVIDENCE_DIR/private/runtime-isolation.json"',
            body,
        )
        self.assertLess(signing_key_init, forwarder_start)
        self.assertLess(compose_start, forwarder_start)
        self.assertLess(forwarder_start, fixture_command)
        self.assertIn('stop_loopback_forwarder', body)
        self.assertIn('loopback-forwarder.pid', body)
        self.assertIn('chmod 600 "$forwarder_pid_file"', body)

    def test_gate3_initializes_only_the_environment_backed_active_evidence_key(self) -> None:
        body = SCRIPT.read_text(encoding="utf-8")
        start = body.index("initialize_evidence_signing_key()")
        end = body.index("\n}\n", start) + 2
        function = body[start:end]
        self.assertIn("env:RADAR_EVIDENCE_HASH_KEY", function)
        self.assertIn('exec -i -e PGPASSFILE=/run/secrets/radar-database.pgpass', function)
        self.assertIn("WHERE NOT EXISTS", function)
        self.assertIn("status = 'active'", function)
        self.assertNotIn("RADAR_EVIDENCE_HASH_KEY=", function)
        self.assertNotIn("printf", function)

    def test_migration_and_primary_clone_receive_only_private_database_file_paths(self) -> None:
        body = SCRIPT.read_text(encoding="utf-8")
        migration_start = body.index("run_migration_rehearsal()")
        migration_end = body.index("\n}\n", migration_start) + 2
        migration = body[migration_start:migration_end]
        self.assertIn("RADAR_MIGRATION_REHEARSAL_POSTGRES_PASSWORD_FILE=", migration)
        self.assertIn("RADAR_MIGRATION_REHEARSAL_DATABASE_PASSWORD_FILE=", migration)
        self.assertIn("RADAR_MIGRATION_REHEARSAL_PGPASS_FILE=", migration)
        self.assertNotIn("RADAR_MIGRATION_REHEARSAL_DATABASE_PASSWORD=", migration)
        self.assertNotIn('read_env_value RADAR_POSTGRES_PASSWORD "$environment_file"', migration)

        rollback_start = body.index("run_primary_compose_rollback()")
        rollback_end = body.index("\n}\n", rollback_start) + 2
        rollback = body[rollback_start:rollback_end]
        self.assertIn('"--database-password-file" "$database_password_file"', rollback)
        self.assertIn('"--pgpass-file" "$pgpass_file"', rollback)
        self.assertNotIn('read_env_value RADAR_POSTGRES_PASSWORD "$environment_file"', rollback)
        self.assertNotIn("env RADAR_POSTGRES_PASSWORD=", rollback)

    def test_failure_preserves_exit_code_stops_later_gates_and_writes_failed_closure(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            evidence = root / "evidence"
            evidence.mkdir(mode=0o700)
            (evidence / "bindings.json").write_text(json.dumps(bindings()))
            (evidence / "bindings.json").chmod(0o600)
            calls = root / "calls"
            driver = root / "driver"
            self._executable(driver, f"""
                printf '%s\\n' "$1" >> {calls!s}
                printf 'password=local-secret\\nAuthorization: Bearer local-token\\n'
                if [[ "$1" == migration-225 ]]; then
                    mkdir -p {evidence!s}/private/gate-2-migration
                    printf '%s\\n' '{{"migration_224_checksum":"{'9' * 64}","migration_225_checksum":"{'8' * 64}","migration_count":307,"baseline_schema_migrations":285,"actual_schema_migrations":307,"expected_schema_migrations":307,"migration_ledger_ok":true,"candidate_pending_migrations":[],"legacy_entries":["202_add_radar_tracked_models.sql","207_scope_radar_tracked_models_by_tenant.sql"],"checksum_mismatches":[],"baseline_ledger_sha256":"{'1' * 64}","candidate_ledger_sha256":"{'2' * 64}","expected_candidate_ledger_sha256":"{'2' * 64}","expected_runtime_ledger_sha256":"{'3' * 64}","runtime_ledger_sha256":"{'3' * 64}","migration_224_semantics_ok":true,"migration_225_semantics_ok":true,"rollback_database_clone_used":true}}' > {evidence!s}/private/gate-2-migration/summary.json
                fi
                [[ "$1" != radar-browser-workflows ]] || exit 23
            """)
            docker = root / "docker"
            self._executable(docker, "exit 0\n")
            environment = os.environ | {
                "RADAR_LOCAL_RUN_ID": "task7-script-rehearsal",
                "RADAR_LOCAL_EVIDENCE_DIR": str(evidence),
                "RADAR_LOCAL_BROWSER_ORIGIN": "http://127.0.0.1:18080",
                "RADAR_LOCAL_TEST_DRIVER": str(driver),
                "RADAR_LOCAL_DOCKER_BIN": str(docker),
            }
            completed = subprocess.run([str(SCRIPT)], cwd=ROOT, env=environment, text=True, capture_output=True, check=False)
            self.assertEqual(completed.returncode, 23, completed.stderr)
            self.assertEqual(calls.read_text().splitlines(), list(("immutable-inputs-and-code", "migration-225", "radar-browser-workflows")))
            closure = json.loads((evidence / "public" / "closure.json").read_text())
            self.assertEqual(closure["status"], "local-isolated-prerelease-failed")
            self.assertEqual(closure["summary"]["exit_code"], 23)
            self.assertFalse((evidence / "gate-4-restart-rollback.json").exists())
            private_log = (evidence / "private" / "radar-browser-workflows.log").read_text()
            self.assertNotIn("local-secret", private_log)
            self.assertNotIn("local-token", private_log)
            self.assertIn("[REDACTED]", private_log)

    def test_gate2_and_gate4_use_valid_bounded_rehearsal_project_names(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            evidence = root / "evidence"
            evidence.mkdir(mode=0o700)
            (evidence / "bindings.json").write_text(json.dumps(bindings()))
            (evidence / "bindings.json").chmod(0o600)
            calls = root / "calls"
            driver = root / "driver"
            self._executable(driver, f"""
                printf '%s\\n' "$*" >> {calls!s}
                case "$1" in
                  migration-225)
                    mkdir -p {evidence!s}/private/gate-2-migration
                    printf '%s\\n' '{{"migration_224_checksum":"{'9' * 64}","migration_225_checksum":"{'8' * 64}","migration_count":307,"baseline_schema_migrations":285,"actual_schema_migrations":307,"expected_schema_migrations":307,"migration_ledger_ok":true,"candidate_pending_migrations":[],"legacy_entries":["202_add_radar_tracked_models.sql","207_scope_radar_tracked_models_by_tenant.sql"],"checksum_mismatches":[],"baseline_ledger_sha256":"{'1' * 64}","candidate_ledger_sha256":"{'2' * 64}","expected_candidate_ledger_sha256":"{'2' * 64}","expected_runtime_ledger_sha256":"{'3' * 64}","runtime_ledger_sha256":"{'3' * 64}","migration_224_semantics_ok":true,"migration_225_semantics_ok":true,"rollback_database_clone_used":true}}' > {evidence!s}/private/gate-2-migration/summary.json
                    ;;
                  radar-browser-workflows)
                    mkdir -p {evidence!s}/private
                    printf '%s\\n' '{{"registrations":[{{"identity":"reasoning-runner","worker_kind":"runner","capability":"reasoning"}},{{"identity":"exact-grader","worker_kind":"grader","capability":"exact"}},{{"identity":"reasoning-statistics","worker_kind":"statistics","capability":"reasoning"}}],"registration_order":["reasoning-runner","exact-grader","reasoning-statistics"],"status":"passed"}}' > {evidence!s}/private/worker-registration.json
                    ;;
                  candidate-restart-smoke)
                    exit 31
                    ;;
                esac
            """)
            docker = root / "docker"
            self._executable(docker, "exit 0\n")
            environment = os.environ | {
                "RADAR_LOCAL_RUN_ID": "task7-script-rehearsal",
                "RADAR_LOCAL_EVIDENCE_DIR": str(evidence),
                "RADAR_LOCAL_BROWSER_ORIGIN": "http://127.0.0.1:18080",
                "RADAR_LOCAL_TEST_DRIVER": str(driver),
                "RADAR_LOCAL_DOCKER_BIN": str(docker),
            }
            completed = subprocess.run([str(SCRIPT)], cwd=ROOT, env=environment, text=True, capture_output=True, check=False)
            self.assertEqual(completed.returncode, 31)
            recorded_calls = calls.read_text().splitlines()
            self.assertEqual(recorded_calls[1], "migration-225 task7-script-gate2-rehearsal task7-script-gate2-rehearsal")
            self.assertEqual(recorded_calls[3], "candidate-restart-smoke")

            long_run_id = "a" * 54 + "-rehearsal"
            long_evidence = root / "long-evidence"
            long_evidence.mkdir(mode=0o700)
            (long_evidence / "bindings.json").write_text(json.dumps(bindings(long_run_id)))
            (long_evidence / "bindings.json").chmod(0o600)
            rejected = subprocess.run(
                [str(SCRIPT)], cwd=ROOT, env=environment | {
                    "RADAR_LOCAL_RUN_ID": long_run_id,
                    "RADAR_LOCAL_EVIDENCE_DIR": str(long_evidence),
                }, text=True, capture_output=True, check=False,
            )
            self.assertNotEqual(rejected.returncode, 0)
            self.assertIn("migration project", rejected.stderr)

    def test_gate4_orders_primary_clone_restore_before_fresh_migration_replay(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            evidence = root / "evidence"
            evidence.mkdir(mode=0o700)
            (evidence / "bindings.json").write_text(json.dumps(bindings()))
            (evidence / "bindings.json").chmod(0o600)
            calls = root / "calls"
            driver = root / "driver"
            self._executable(driver, f"""
                printf '%s\n' "$*" >> {calls!s}
                case "$1" in
                  migration-225)
                    mkdir -p {evidence!s}/private/gate-2-migration
                    printf '%s\n' '{{"migration_224_checksum":"{'9' * 64}","migration_225_checksum":"{'8' * 64}","migration_count":307,"baseline_schema_migrations":285,"actual_schema_migrations":307,"expected_schema_migrations":307,"migration_ledger_ok":true,"candidate_pending_migrations":[],"legacy_entries":["202_add_radar_tracked_models.sql","207_scope_radar_tracked_models_by_tenant.sql"],"checksum_mismatches":[],"baseline_ledger_sha256":"{'1' * 64}","candidate_ledger_sha256":"{'2' * 64}","expected_candidate_ledger_sha256":"{'2' * 64}","expected_runtime_ledger_sha256":"{'3' * 64}","runtime_ledger_sha256":"{'3' * 64}","migration_224_semantics_ok":true,"migration_225_semantics_ok":true,"rollback_database_clone_used":true}}' > {evidence!s}/private/gate-2-migration/summary.json
                    ;;
                  radar-browser-workflows)
                    mkdir -p {evidence!s}/private
                    printf '%s\n' '{{"registrations":[{{"identity":"reasoning-runner","worker_kind":"runner","capability":"reasoning"}},{{"identity":"exact-grader","worker_kind":"grader","capability":"exact"}},{{"identity":"reasoning-statistics","worker_kind":"statistics","capability":"reasoning"}}],"registration_order":["reasoning-runner","exact-grader","reasoning-statistics"],"status":"passed"}}' > {evidence!s}/private/worker-registration.json
                    ;;
                  primary-compose-clone-rollback)
                    mkdir -p {evidence!s}/private/gate-4-primary-rollback
                    printf '%s\n' '{{"schema_version":"radar-local-compose-rollback-v1","primary_database_clone_used":true,"rollback_control_plane_clone_only":true,"rollback_health_passed":true}}' > {evidence!s}/private/gate-4-primary-rollback/summary.json
                    ;;
                  fresh-migration-replay)
                    mkdir -p {evidence!s}/private/gate-4-migration
                    printf '%s\n' '{{"migration_224_checksum":"{'9' * 64}","migration_225_checksum":"{'8' * 64}","migration_count":307,"baseline_schema_migrations":285,"actual_schema_migrations":307,"expected_schema_migrations":307,"migration_ledger_ok":true,"candidate_pending_migrations":[],"legacy_entries":["202_add_radar_tracked_models.sql","207_scope_radar_tracked_models_by_tenant.sql"],"checksum_mismatches":[],"baseline_ledger_sha256":"{'1' * 64}","candidate_ledger_sha256":"{'2' * 64}","expected_candidate_ledger_sha256":"{'2' * 64}","expected_runtime_ledger_sha256":"{'3' * 64}","runtime_ledger_sha256":"{'3' * 64}","migration_224_semantics_ok":true,"migration_225_semantics_ok":true,"rollback_database_clone_used":true}}' > {evidence!s}/private/gate-4-migration/summary.json
                    ;;
                esac
            """)
            docker = root / "docker"
            self._executable(docker, "exit 0\n")
            completed = subprocess.run(
                [str(SCRIPT)],
                cwd=ROOT,
                env=os.environ | {
                    "RADAR_LOCAL_RUN_ID": "task7-script-rehearsal",
                    "RADAR_LOCAL_EVIDENCE_DIR": str(evidence),
                    "RADAR_LOCAL_BROWSER_ORIGIN": "http://127.0.0.1:18080",
                    "RADAR_LOCAL_TEST_DRIVER": str(driver),
                    "RADAR_LOCAL_DOCKER_BIN": str(docker),
                },
                text=True,
                capture_output=True,
                check=False,
            )
            self.assertEqual(completed.returncode, 97, completed.stderr)
            self.assertEqual(calls.read_text().splitlines(), [
                "immutable-inputs-and-code",
                "migration-225 task7-script-gate2-rehearsal task7-script-gate2-rehearsal",
                "radar-browser-workflows",
                "candidate-restart-smoke",
                "primary-compose-clone-rollback",
                "candidate-primary-restore",
                "fresh-migration-replay task7-script-gate4-rehearsal task7-script-gate4-rehearsal",
                "evidence-closure-input",
            ])

    def test_log_is_redacted_while_phase_is_still_running(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            evidence = root / "evidence"
            evidence.mkdir(mode=0o700)
            (evidence / "bindings.json").write_text(json.dumps(bindings()))
            (evidence / "bindings.json").chmod(0o600)
            release = root / "release"
            driver = root / "driver"
            self._executable(driver, f"printf 'password=SYNTHETIC_RAW\\nAuthorization: Bearer SYNTHETIC_BEARER\\nprompt=SYNTHETIC_PROMPT\\nraw observation=SYNTHETIC_OBSERVATION\\n'\nwhile [[ ! -f {release!s} ]]; do sleep 0.05; done\nexit 23\n")
            docker = root / "docker"
            self._executable(docker, "exit 0\n")
            process = subprocess.Popen(
                [str(SCRIPT)], cwd=ROOT, env=os.environ | {
                    "RADAR_LOCAL_RUN_ID": "task7-script-rehearsal",
                    "RADAR_LOCAL_EVIDENCE_DIR": str(evidence),
                    "RADAR_LOCAL_BROWSER_ORIGIN": "http://127.0.0.1:18080",
                    "RADAR_LOCAL_TEST_DRIVER": str(driver),
                    "RADAR_LOCAL_DOCKER_BIN": str(docker),
                }, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            )
            log = evidence / "private" / "immutable-inputs-and-code.log"
            try:
                deadline = time.monotonic() + 5
                while (not log.exists() or not log.read_text()) and time.monotonic() < deadline:
                    time.sleep(0.05)
                self.assertIsNone(process.poll())
                during = log.read_text()
                for secret in ("SYNTHETIC_RAW", "SYNTHETIC_BEARER", "SYNTHETIC_PROMPT", "SYNTHETIC_OBSERVATION"):
                    self.assertNotIn(secret, during)
                self.assertIn("[REDACTED]", during)
            finally:
                release.touch()
                process.communicate(timeout=5)
            self.assertEqual(process.returncode, 23)

    def test_test_driver_can_never_manufacture_a_passing_closure(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            evidence = root / "evidence"
            evidence.mkdir(mode=0o700)
            (evidence / "bindings.json").write_text(json.dumps(bindings()))
            (evidence / "bindings.json").chmod(0o600)
            driver = root / "driver"
            self._executable(driver, "exit 0\n")
            docker = root / "docker"
            self._executable(docker, "exit 0\n")
            environment = os.environ | {
                "RADAR_LOCAL_RUN_ID": "task7-script-rehearsal",
                "RADAR_LOCAL_EVIDENCE_DIR": str(evidence),
                "RADAR_LOCAL_BROWSER_ORIGIN": "http://127.0.0.1:18080",
                "RADAR_LOCAL_TEST_DRIVER": str(driver),
                "RADAR_LOCAL_DOCKER_BIN": str(docker),
            }
            completed = subprocess.run([str(SCRIPT)], cwd=ROOT, env=environment, text=True, capture_output=True, check=False)
            self.assertNotEqual(completed.returncode, 0)
            closure = json.loads((evidence / "public" / "closure.json").read_text())
            self.assertEqual(closure["status"], "local-isolated-prerelease-failed")

    def test_cleanup_uses_verified_compose_project_labels_and_removes_private_artifacts(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            evidence = root / "evidence"
            private = evidence / "private"
            private.mkdir(parents=True, mode=0o700)
            (evidence / "bindings.json").write_text(json.dumps(bindings()))
            (evidence / "bindings.json").chmod(0o600)
            (evidence / "rehearsal.env").write_text("RADAR_ADMIN_PASSWORD=secret\n")
            (evidence / "rehearsal.env").chmod(0o600)
            frontend_results = root / "frontend-results"
            (frontend_results / ".auth").mkdir(parents=True)
            (frontend_results / "artifacts").mkdir()
            (frontend_results / "playwright.json").write_text("private browser report")
            (frontend_results / "playwright.xml").write_text("private browser report")
            docker_log = root / "docker.log"
            docker = root / "docker"
            self._executable(
                docker,
                f"""
                printf '%s\\n' \"$*\" >> {docker_log!s}
                if [[ \"$*\" == \"ps -aq --filter label=com.docker.compose.project=task7-script-rehearsal\" ]]; then printf 'cid\\n'; fi
                if [[ \"$*\" == \"volume ls -q --filter label=com.docker.compose.project=task7-script-rehearsal\" ]]; then printf 'vid\\n'; fi
                if [[ \"$*\" == \"network ls -q --filter label=com.docker.compose.project=task7-script-rehearsal\" ]]; then printf 'nid\\n'; fi
                if [[ \"$*\" == *\"inspect --format\"* ]]; then printf 'task7-script-rehearsal\\n'; fi
                """,
            )
            environment = os.environ | {
                "RADAR_LOCAL_RUN_ID": "task7-script-rehearsal",
                "RADAR_LOCAL_EVIDENCE_DIR": str(evidence),
                "RADAR_LOCAL_DOCKER_BIN": str(docker),
                "RADAR_LOCAL_FRONTEND_RESULTS_DIR": str(frontend_results),
            }
            completed = subprocess.run([str(SCRIPT), "--cleanup-retained"], cwd=ROOT, env=environment, text=True, capture_output=True, check=False)
            self.assertEqual(completed.returncode, 0, completed.stderr)
            log = docker_log.read_text()
            self.assertIn("label=com.docker.compose.project=task7-script-rehearsal", log)
            self.assertIn("container inspect", log)
            self.assertIn("rm -f cid", log)
            self.assertIn("volume rm vid", log)
            self.assertIn("network rm nid", log)
            self.assertFalse((evidence / "rehearsal.env").exists())
            self.assertFalse((frontend_results / ".auth").exists())
            self.assertFalse((frontend_results / "artifacts").exists())
            self.assertFalse((frontend_results / "playwright.json").exists())
            self.assertFalse((frontend_results / "playwright.xml").exists())

    def test_cleanup_failures_are_aggregated_and_business_exit_code_has_priority(self) -> None:
        for failure_kind in ("container", "volume", "network"):
            with self.subTest(failure_kind=failure_kind), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                evidence = root / "evidence"
                evidence.mkdir(mode=0o700)
                (evidence / "bindings.json").write_text(json.dumps(bindings()))
                (evidence / "bindings.json").chmod(0o600)
                driver = root / "driver"
                self._executable(driver, "exit 19\n")
                docker = root / "docker"
                docker_log = root / "docker.log"
                self._executable(docker, f"""
                    printf '%s\\n' "$*" >> {docker_log!s}
                    [[ "$*" != "ps -aq --filter label=com.docker.compose.project=task7-script-rehearsal" ]] || printf 'cid\\n'
                    [[ "$*" != "volume ls -q --filter label=com.docker.compose.project=task7-script-rehearsal" ]] || printf 'vid\\n'
                    [[ "$*" != "network ls -q --filter label=com.docker.compose.project=task7-script-rehearsal" ]] || printf 'nid\\n'
                    if [[ "$1 $2" == "container inspect" || "$1 $2" == "volume inspect" || "$1 $2" == "network inspect" ]]; then printf 'task7-script-rehearsal\\n'; fi
                    if [[ "${{RADAR_FAIL_REMOVE:-}}" == container && "$1" == rm ]]; then exit 41; fi
                    if [[ "${{RADAR_FAIL_REMOVE:-}}" == volume && "$1 $2" == "volume rm" ]]; then exit 42; fi
                    if [[ "${{RADAR_FAIL_REMOVE:-}}" == network && "$1 $2" == "network rm" ]]; then exit 43; fi
                """)
                completed = subprocess.run([str(SCRIPT)], cwd=ROOT, env=os.environ | {
                    "RADAR_LOCAL_RUN_ID": "task7-script-rehearsal",
                    "RADAR_LOCAL_EVIDENCE_DIR": str(evidence),
                    "RADAR_LOCAL_BROWSER_ORIGIN": "http://127.0.0.1:18080",
                    "RADAR_LOCAL_TEST_DRIVER": str(driver),
                    "RADAR_LOCAL_DOCKER_BIN": str(docker),
                    "RADAR_FAIL_REMOVE": failure_kind,
                    "RADAR_LOCAL_RETAIN_DEBUG": "0",
                }, text=True, capture_output=True, check=False)
                self.assertEqual(completed.returncode, 19)
                failure = json.loads((evidence / "private" / "cleanup-failure.json").read_text())
                self.assertIn(failure_kind, failure["failed_categories"])

                cleanup_only = subprocess.run([str(SCRIPT), "--cleanup-retained"], cwd=ROOT, env=os.environ | {
                    "RADAR_LOCAL_RUN_ID": "task7-script-rehearsal",
                    "RADAR_LOCAL_EVIDENCE_DIR": str(evidence),
                    "RADAR_LOCAL_DOCKER_BIN": str(docker),
                    "RADAR_FAIL_REMOVE": failure_kind,
                    "RADAR_LOCAL_RETAIN_DEBUG": "0",
                }, text=True, capture_output=True, check=False)
                self.assertNotEqual(cleanup_only.returncode, 0)
                closure = json.loads((evidence / "public" / "closure.json").read_text())
                self.assertEqual(closure["status"], "local-isolated-prerelease-failed")

    def test_retention_record_failure_forces_volume_cleanup(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            evidence = root / "evidence"
            (evidence / "private" / "retention.json").mkdir(parents=True)
            (evidence / "bindings.json").write_text(json.dumps(bindings()))
            (evidence / "bindings.json").chmod(0o600)
            driver = root / "driver"
            self._executable(driver, "exit 19\n")
            docker_log = root / "docker.log"
            docker = root / "docker"
            self._executable(docker, f"printf '%s\\n' \"$*\" >> {docker_log!s}\nif [[ \"$*\" == \"volume ls -q --filter label=com.docker.compose.project=task7-script-rehearsal\" ]]; then printf 'vid\\n'; fi\nif [[ \"$1 $2\" == \"volume inspect\" ]]; then printf 'task7-script-rehearsal\\n'; fi\n")
            completed = subprocess.run([str(SCRIPT)], cwd=ROOT, env=os.environ | {
                "RADAR_LOCAL_RUN_ID": "task7-script-rehearsal",
                "RADAR_LOCAL_EVIDENCE_DIR": str(evidence),
                "RADAR_LOCAL_BROWSER_ORIGIN": "http://127.0.0.1:18080",
                "RADAR_LOCAL_TEST_DRIVER": str(driver),
                "RADAR_LOCAL_DOCKER_BIN": str(docker),
                "RADAR_LOCAL_RETAIN_DEBUG": "1",
            }, text=True, capture_output=True, check=False)
            self.assertNotEqual(completed.returncode, 0)
            self.assertIn("volume rm vid", docker_log.read_text())

    def test_cleanup_revalidates_retention_before_preserving_volumes(self) -> None:
        for mutation in ("expired", "mode", "project", "cleanup-command"):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                evidence = root / "evidence"
                evidence.mkdir(mode=0o700)
                (evidence / "bindings.json").write_text(json.dumps(bindings()))
                (evidence / "bindings.json").chmod(0o600)
                retention = evidence / "private" / "retention.json"
                driver = root / "driver"
                self._executable(driver, """
                    if [[ "$RADAR_TEST_MUTATION" == expired ]]; then
                        /bin/sleep 3
                    elif [[ "$RADAR_TEST_MUTATION" == mode ]]; then
                        chmod 0640 "$RADAR_TEST_RETENTION_RECORD"
                    else
                        python3 - "$RADAR_TEST_RETENTION_RECORD" "$RADAR_TEST_MUTATION" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
document = json.loads(path.read_text(encoding="utf-8"))
if sys.argv[2] == "project":
    document["migration_projects"][0] = "other-phase-rehearsal"
else:
    document["cleanup_command"] = "env RADAR_LOCAL_RUN_ID=other-rehearsal cleanup"
path.write_text(json.dumps(document), encoding="utf-8")
PY
                    fi
                    exit 19
                """)
                docker_log = root / "docker.log"
                docker = root / "docker"
                self._executable(docker, f"""
                    printf '%s\\n' "$*" >> {docker_log!s}
                    case "$*" in
                      "volume ls -q --filter label=com.docker.compose.project=task7-script-rehearsal") printf 'compose-volume\\n' ;;
                      "volume ls -q --filter label=radar.rehearsal.project=task7-script-gate2-rehearsal") printf 'gate2-volume\\n' ;;
                      "volume ls -q --filter label=radar.rehearsal.project=task7-script-gate4-rehearsal") printf 'gate4-volume\\n' ;;
                    esac
                    if [[ "$1 $2" == "volume inspect" ]]; then
                        case "$5" in
                          compose-volume) printf 'task7-script-rehearsal\\n' ;;
                          gate2-volume) printf 'task7-script-gate2-rehearsal\\n' ;;
                          gate4-volume) printf 'task7-script-gate4-rehearsal\\n' ;;
                        esac
                    fi
                """)
                completed = subprocess.run([str(SCRIPT)], cwd=ROOT, env=os.environ | {
                    "RADAR_LOCAL_RUN_ID": "task7-script-rehearsal",
                    "RADAR_LOCAL_EVIDENCE_DIR": str(evidence),
                    "RADAR_LOCAL_BROWSER_ORIGIN": "http://127.0.0.1:18080",
                    "RADAR_LOCAL_TEST_DRIVER": str(driver),
                    "RADAR_LOCAL_DOCKER_BIN": str(docker),
                    "RADAR_LOCAL_RETAIN_DEBUG": "1",
                    "RADAR_LOCAL_RETAIN_DEBUG_SECONDS": "2" if mutation == "expired" else "600",
                    "RADAR_TEST_MUTATION": mutation,
                    "RADAR_TEST_RETENTION_RECORD": str(retention),
                }, text=True, capture_output=True, check=False)
                self.assertEqual(completed.returncode, 19, completed.stderr)
                log = docker_log.read_text()
                self.assertIn("volume rm compose-volume", log)
                self.assertIn("volume rm gate2-volume", log)
                self.assertIn("volume rm gate4-volume", log)

    def test_cleanup_retained_covers_gate2_and_gate4_exact_project_labels(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            evidence = root / "evidence"
            evidence.mkdir(mode=0o700)
            docker_log = root / "docker.log"
            docker = root / "docker"
            self._executable(docker, f"""
                printf '%s\\n' "$*" >> {docker_log!s}
                case "$*" in
                  "volume ls -q --filter label=radar.rehearsal.project=task7-script-gate2-rehearsal") printf 'gate2-volume\\n' ;;
                  "volume ls -q --filter label=radar.rehearsal.project=task7-script-gate4-rehearsal") printf 'gate4-volume\\n' ;;
                  "volume inspect --format {{{{ index .Labels \\"radar.rehearsal.project\\" }}}} gate2-volume") printf 'task7-script-gate2-rehearsal\\n' ;;
                  "volume inspect --format {{{{ index .Labels \\"radar.rehearsal.project\\" }}}} gate4-volume") printf 'task7-script-gate4-rehearsal\\n' ;;
                esac
            """)
            completed = subprocess.run([str(SCRIPT), "--cleanup-retained"], cwd=ROOT, env=os.environ | {
                "RADAR_LOCAL_RUN_ID": "task7-script-rehearsal",
                "RADAR_LOCAL_EVIDENCE_DIR": str(evidence),
                "RADAR_LOCAL_DOCKER_BIN": str(docker),
            }, text=True, capture_output=True, check=False)
            self.assertEqual(completed.returncode, 0, completed.stderr)
            log = docker_log.read_text()
            self.assertIn("volume rm gate2-volume", log)
            self.assertIn("volume rm gate4-volume", log)

    def test_private_cleanup_trap_covers_origin_and_static_validation_failures(self) -> None:
        for static_failure in (False, True):
            with self.subTest(static_failure=static_failure), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                evidence = root / "evidence"
                frontend_results = root / "frontend-results"
                self._private_material(evidence, frontend_results)
                script = SCRIPT
                origin = "http://public.example.invalid:18080"
                if static_failure:
                    copied_root = root / "copy"
                    script = copied_root / "deploy" / "radar" / "run-local-prerelease.sh"
                    script.parent.mkdir(parents=True)
                    script.write_bytes(SCRIPT.read_bytes())
                    script.chmod(0o700)
                    origin = "http://127.0.0.1:18080"
                completed = subprocess.run([str(script)], cwd=ROOT, env=os.environ | {
                    "RADAR_LOCAL_RUN_ID": "task7-script-rehearsal",
                    "RADAR_LOCAL_EVIDENCE_DIR": str(evidence),
                    "RADAR_LOCAL_BROWSER_ORIGIN": origin,
                    "RADAR_LOCAL_FRONTEND_RESULTS_DIR": str(frontend_results),
                }, text=True, capture_output=True, check=False)
                self.assertNotEqual(completed.returncode, 0)
                self._assert_private_material_removed(evidence, frontend_results)

    def test_compose_start_and_restarts_have_bounded_health_waits(self) -> None:
        body = SCRIPT.read_text(encoding="utf-8")
        self.assertIn("up -d --no-build --pull never --wait --wait-timeout", body)
        self.assertIn("RADAR_LOCAL_HEALTH_TIMEOUT_SECONDS", body)
        first_restart = body.index("compose_command restart sub2api-staging")
        first_wait = body.index("wait_for_candidate_health", first_restart)
        first_smoke = body.index("playwright_smoke", first_restart)
        primary_stop = body.index("compose_command stop", first_restart)
        clone_drill = body.index("run_primary_compose_rollback", primary_stop)
        primary_start = body.index("compose_command start sub2api-staging", clone_drill)
        second_wait = body.index("wait_for_candidate_health", primary_start)
        second_smoke = body.index("quality_smoke", primary_start)
        self.assertLess(first_restart, first_wait, first_smoke)
        self.assertEqual(
            [primary_stop, clone_drill, primary_start, second_wait, second_smoke],
            sorted([primary_stop, clone_drill, primary_start, second_wait, second_smoke]),
        )

    def test_retain_debug_keeps_labeled_volumes_with_bounded_exact_cleanup_command(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            evidence = root / "evidence"
            evidence.mkdir(mode=0o700)
            (evidence / "bindings.json").write_text(json.dumps(bindings()))
            (evidence / "bindings.json").chmod(0o600)
            driver = root / "driver"
            self._executable(driver, "exit 19\n")
            docker_log = root / "docker.log"
            docker = root / "docker"
            self._executable(
                docker,
                f"printf '%s\\n' \"$*\" >> {docker_log!s}\nif [[ \"$*\" == \"volume ls -q --filter label=com.docker.compose.project=task7-script-rehearsal\" ]]; then printf 'vid\\n'; fi\nif [[ \"$*\" == *\"inspect --format\"* ]]; then printf 'task7-script-rehearsal\\n'; fi\n",
            )
            environment = os.environ | {
                "RADAR_LOCAL_RUN_ID": "task7-script-rehearsal",
                "RADAR_LOCAL_EVIDENCE_DIR": str(evidence),
                "RADAR_LOCAL_BROWSER_ORIGIN": "http://127.0.0.1:18080",
                "RADAR_LOCAL_TEST_DRIVER": str(driver),
                "RADAR_LOCAL_DOCKER_BIN": str(docker),
                "RADAR_LOCAL_RETAIN_DEBUG": "1",
                "RADAR_LOCAL_RETAIN_DEBUG_SECONDS": "600",
            }
            completed = subprocess.run([str(SCRIPT)], cwd=ROOT, env=environment, text=True, capture_output=True, check=False)
            self.assertEqual(completed.returncode, 19)
            log = docker_log.read_text()
            self.assertNotIn("volume rm vid", log)
            retention = json.loads((evidence / "private" / "retention.json").read_text())
            self.assertIn("--cleanup-retained", retention["cleanup_command"])
            self.assertEqual(retention["deadline_seconds"], 600)
            self.assertEqual(stat.S_IMODE((evidence / "private" / "retention.json").stat().st_mode), 0o600)


if __name__ == "__main__":
    unittest.main()
