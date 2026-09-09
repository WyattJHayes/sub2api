from __future__ import annotations

import json
import os
import stat
import subprocess
import tempfile
import textwrap
import unittest
from unittest.mock import patch
from pathlib import Path

from deploy.radar.local_compose_rollback import ComposeRollbackInputs, _resource_names, run_drill


DIGEST = "sha256:" + "a" * 64


class LocalComposeRollbackTests(unittest.TestCase):
    def _executable(self, path: Path, body: str) -> None:
        path.write_text("#!/usr/bin/env bash\nset -eu\n" + textwrap.dedent(body), encoding="utf-8")
        path.chmod(0o700)

    def _secret_files(self, root: Path) -> tuple[Path, Path]:
        private = root / "private"
        private.mkdir(mode=0o700)
        password_file = private / "database-password"
        pgpass_file = private / "database.pgpass"
        password_file.write_text("rollback-password-sentinel\n", encoding="utf-8")
        pgpass_file.write_text("*:*:*:*:rollback-password-sentinel\n", encoding="utf-8")
        password_file.chmod(0o600)
        pgpass_file.chmod(0o600)
        return password_file, pgpass_file

    def test_clone_only_rollback_binds_previous_control_plane_to_primary_clone(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            log = root / "docker.log"
            docker = root / "docker"
            self._executable(
                docker,
                f"""
                printf '%s\n' "$*" >> {log!s}
                case "$*" in
                  "container inspect "*) exit 1 ;;
                  "volume inspect "*) exit 1 ;;
                  *" exec "*" pg_dump "*) printf 'synthetic-dump' ;;
                  *" exec -i "*" pg_restore "*) cat >/dev/null ;;
                  *" exec "*" pg_isready "*) exit 0 ;;
                  "inspect --format "*"primary-clone-control-rehearsal")
                    printf 'DATABASE_HOST=radar-rollback-db\n'
                    printf 'DATABASE_PORT=5432\n'
                    printf 'REDIS_HOST=redis-staging\n'
                    ;;
                  *" exec "*"primary-clone-control-rehearsal wget "*) exit 0 ;;
                esac
                exit 0
                """,
            )
            environment_file = root / "rehearsal.env"
            environment_file.write_text("RADAR_ADMIN_PASSWORD=local-only\n", encoding="utf-8")
            environment_file.chmod(0o600)
            password_file, pgpass_file = self._secret_files(root)
            output = root / "summary.json"

            result = run_drill(
                ComposeRollbackInputs(
                    run_id="task7-script-rehearsal",
                    docker_bin=str(docker),
                    environment_file=environment_file,
                    postgres_image=f"registry.invalid/postgres@{DIGEST}",
                    rollback_control_plane_image=f"registry.invalid/control-previous@{DIGEST}",
                    database_password_file=password_file,
                    pgpass_file=pgpass_file,
                    database_user="radar",
                    database_name="radar",
                    output_path=output,
                    timeout_seconds=10,
                    retain_volume=False,
                )
            )

            self.assertEqual(
                result,
                {
                    "schema_version": "radar-local-compose-rollback-v1",
                    "primary_database_clone_used": True,
                    "rollback_control_plane_clone_only": True,
                    "rollback_health_passed": True,
                },
            )
            self.assertEqual(json.loads(output.read_text(encoding="utf-8")), result)
            self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)
            calls = log.read_text(encoding="utf-8").splitlines()
            dump_index = next(index for index, call in enumerate(calls) if " pg_dump " in call)
            rollback_index = next(
                index
                for index, call in enumerate(calls)
                if "primary-clone-control-rehearsal" in call and call.startswith("run -d")
            )
            self.assertLess(dump_index, rollback_index)
            rollback_call = calls[rollback_index]
            clone_call = next(
                call
                for call in calls
                if call.startswith("run -d")
                and "task7-script-rehearsal-primary-clone-postgres-rehearsal" in call
            )
            self.assertIn("--network-alias radar-rollback-db", clone_call)
            self.assertIn("DATABASE_HOST=radar-rollback-db", rollback_call)
            self.assertIn("REDIS_HOST=redis-staging", rollback_call)
            self.assertIn("POSTGRES_PASSWORD_FILE=/run/secrets/radar-database-password", clone_call)
            self.assertIn("DATABASE_PASSWORD_FILE=/run/secrets/radar-database-password", rollback_call)
            self.assertIn("/run/secrets/radar-database-password", clone_call)
            self.assertIn("/run/secrets/radar-database-password", rollback_call)
            self.assertIn("--entrypoint /bin/sh", rollback_call)
            self.assertIn("-ec", rollback_call)
            self.assertIn("export DATABASE_PASSWORD=", rollback_call)
            self.assertIn("cat \"$DATABASE_PASSWORD_FILE\"", rollback_call)
            self.assertIn("/app/docker-entrypoint.sh /app/sub2api", rollback_call)
            self.assertIn("/run/secrets/radar-database.pgpass", clone_call)
            self.assertNotIn("rollback-password-sentinel", "\n".join(calls))
            self.assertNotIn(" -e POSTGRES_PASSWORD ", f" {clone_call} ")
            self.assertNotIn(" -e DATABASE_PASSWORD ", f" {rollback_call} ")
            self.assertNotIn("DATABASE_PASSWORD=rollback-password-sentinel", rollback_call)
            self.assertTrue(any("PGPASSFILE=/run/secrets/radar-database.pgpass" in call for call in calls))
            self.assertNotIn("DATABASE_HOST=task7-script-rehearsal-postgres-rehearsal", rollback_call)
            self.assertNotIn("REDIS_HOST=task7-script-rehearsal-redis-rehearsal", rollback_call)
            self.assertNotIn(
                "DATABASE_HOST=task7-script-rehearsal-primary-clone-postgres-rehearsal",
                rollback_call,
            )
            self.assertTrue(any(call.startswith("volume rm ") for call in calls))

    def test_resource_names_are_run_bound_bounded_and_rehearsal_scoped(self) -> None:
        run_id = "a" * 63 + "-rehearsal"
        names = _resource_names(run_id)
        self.assertTrue(names)
        for name in names.values():
            self.assertTrue(name.startswith(f"{run_id}-"))
            self.assertTrue(name.endswith("-rehearsal"))
            self.assertLessEqual(len(name), 128)

    def test_rejects_mutable_images_before_docker(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            marker = root / "docker-called"
            docker = root / "docker"
            self._executable(docker, f"touch {marker!s}\nexit 0\n")
            environment_file = root / "rehearsal.env"
            environment_file.write_text("RADAR_ADMIN_PASSWORD=local-only\n", encoding="utf-8")
            environment_file.chmod(0o600)
            password_file, pgpass_file = self._secret_files(root)
            with self.assertRaisesRegex(ValueError, "immutable digest"):
                run_drill(
                    ComposeRollbackInputs(
                        run_id="task7-script-rehearsal",
                        docker_bin=str(docker),
                        environment_file=environment_file,
                        postgres_image="postgres:latest",
                        rollback_control_plane_image=f"registry.invalid/control-previous@{DIGEST}",
                        database_password_file=password_file,
                        pgpass_file=pgpass_file,
                        database_user="radar",
                        database_name="radar",
                        output_path=root / "summary.json",
                        timeout_seconds=10,
                        retain_volume=False,
                    )
                )
            self.assertFalse(marker.exists())

    def test_resource_inspect_errors_fail_closed_before_creating_clone(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            marker = root / "docker-called"
            docker = root / "docker"
            self._executable(
                docker,
                f"touch {marker!s}\n"
                "if [[ \"$1 $2\" == \"container inspect\" ]]; then exit 2; fi\n"
                "exit 0\n",
            )
            environment_file = root / "rehearsal.env"
            environment_file.write_text("RADAR_ADMIN_PASSWORD=local-only\n", encoding="utf-8")
            environment_file.chmod(0o600)
            password_file, pgpass_file = self._secret_files(root)

            with self.assertRaisesRegex(RuntimeError, "inspect failed"):
                run_drill(
                    ComposeRollbackInputs(
                        run_id="task7-script-rehearsal",
                        docker_bin=str(docker),
                        environment_file=environment_file,
                        postgres_image=f"registry.invalid/postgres@{DIGEST}",
                        rollback_control_plane_image=f"registry.invalid/control-previous@{DIGEST}",
                        database_password_file=password_file,
                        pgpass_file=pgpass_file,
                        database_user="radar",
                        database_name="radar",
                        output_path=root / "summary.json",
                        timeout_seconds=10,
                        retain_volume=False,
                    )
                )
            self.assertTrue(marker.exists())

    def test_cleanup_exceptions_preserve_business_error_and_are_aggregated(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            environment_file = root / "rehearsal.env"
            environment_file.write_text("RADAR_ADMIN_PASSWORD=local-only\n", encoding="utf-8")
            environment_file.chmod(0o600)
            password_file, pgpass_file = self._secret_files(root)
            inputs = ComposeRollbackInputs(
                run_id="task7-script-rehearsal",
                docker_bin="docker",
                environment_file=environment_file,
                postgres_image=f"registry.invalid/postgres@{DIGEST}",
                rollback_control_plane_image=f"registry.invalid/control-previous@{DIGEST}",
                database_password_file=password_file,
                pgpass_file=pgpass_file,
                database_user="radar",
                database_name="radar",
                output_path=root / "summary.json",
                timeout_seconds=10,
                retain_volume=False,
            )

            def fake_run(_inputs: ComposeRollbackInputs, arguments: list[str], **_: object) -> subprocess.CompletedProcess[str]:
                if arguments[:2] in (["container", "inspect"], ["volume", "inspect"]):
                    return subprocess.CompletedProcess(arguments, 1, "", "not found")
                if arguments[:2] == ["inspect", "--format"]:
                    return subprocess.CompletedProcess(
                        arguments,
                        0,
                        "DATABASE_HOST=unexpected-primary\n",
                        "",
                    )
                if arguments[:2] == ["rm", "-f"]:
                    if arguments[-1].endswith("primary-clone-control-rehearsal"):
                        raise subprocess.TimeoutExpired(arguments, timeout=1)
                    return subprocess.CompletedProcess(arguments, 0, "", "")
                return subprocess.CompletedProcess(arguments, 0, "", "")

            with (
                patch("deploy.radar.local_compose_rollback._run", side_effect=fake_run),
                patch("deploy.radar.local_compose_rollback._clone_database"),
            ):
                with self.assertRaisesRegex(
                    RuntimeError,
                    "rollback control plane is not bound exclusively to the primary clone",
                ) as raised:
                    run_drill(inputs)
            self.assertTrue(any("cleanup failed for" in str(note) for note in raised.exception.__notes__))


if __name__ == "__main__":
    unittest.main()
