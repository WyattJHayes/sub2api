#!/usr/bin/env python3
"""Run the clone-only rollback portion of the local Compose rehearsal."""

from __future__ import annotations

import argparse
import json
import os
import re
import stat
import subprocess
import tempfile
import time
from dataclasses import dataclass
from pathlib import Path


RUN_ID_PATTERN = re.compile(r"^[a-z0-9][a-z0-9_-]{2,62}-rehearsal$")
IMAGE_PATTERN = re.compile(r"^.+@sha256:[0-9a-f]{64}$")
SCHEMA_VERSION = "radar-local-compose-rollback-v1"
ROLLBACK_DATABASE_ALIAS = "radar-rollback-db"
ROLLBACK_REDIS_ALIAS = "redis-staging"


@dataclass(frozen=True)
class ComposeRollbackInputs:
    run_id: str
    docker_bin: str
    environment_file: Path
    postgres_image: str
    rollback_control_plane_image: str
    database_password_file: Path
    pgpass_file: Path
    database_user: str
    database_name: str
    output_path: Path
    timeout_seconds: int
    retain_volume: bool


def _resource_names(run_id: str) -> dict[str, str]:
    return {
        "source_database": f"{run_id}-postgres-rehearsal",
        "network": f"{run_id}-control-rehearsal",
        "redis": f"{run_id}-redis-rehearsal",
        "clone_database": f"{run_id}-primary-clone-postgres-rehearsal",
        "clone_volume": f"{run_id}-primary-clone-postgres-volume-rehearsal",
        "rollback_control": f"{run_id}-primary-clone-control-rehearsal",
    }


def _validate(inputs: ComposeRollbackInputs) -> None:
    if RUN_ID_PATTERN.fullmatch(inputs.run_id) is None:
        raise ValueError("run_id must be a bounded rehearsal identifier")
    if not inputs.docker_bin:
        raise ValueError("docker_bin is required")
    if inputs.environment_file.is_symlink() or not inputs.environment_file.is_file():
        raise ValueError("environment_file must be a regular file")
    if stat.S_IMODE(inputs.environment_file.stat().st_mode) != 0o600:
        raise ValueError("environment_file must have mode 0600")
    for line in inputs.environment_file.read_text(encoding="utf-8").splitlines():
        key, _, _ = line.partition("=")
        if key in {"RADAR_POSTGRES_PASSWORD", "DATABASE_PASSWORD", "POSTGRES_PASSWORD", "PGPASSWORD"}:
            raise ValueError("environment_file must not contain a database password value")
    for label, path in (
        ("database_password_file", inputs.database_password_file),
        ("pgpass_file", inputs.pgpass_file),
    ):
        if path.is_symlink() or not path.is_file():
            raise ValueError(f"{label} must be a regular file")
        if stat.S_IMODE(path.stat().st_mode) != 0o600:
            raise ValueError(f"{label} must have mode 0600")
        if stat.S_IMODE(path.parent.stat().st_mode) != 0o700:
            raise ValueError(f"{label} parent directory must have mode 0700")
        with path.open("rb") as stream:
            if not stream.read(1):
                raise ValueError(f"{label} must not be empty")
    for image in (inputs.postgres_image, inputs.rollback_control_plane_image):
        if IMAGE_PATTERN.fullmatch(image) is None:
            raise ValueError("rollback drill images must use an immutable digest")
    if not inputs.database_user or not inputs.database_name:
        raise ValueError("database user and name are required")
    if inputs.timeout_seconds <= 0:
        raise ValueError("timeout_seconds must be positive")


def _run(
    inputs: ComposeRollbackInputs,
    arguments: list[str],
    *,
    environment: dict[str, str] | None = None,
    check: bool = True,
) -> subprocess.CompletedProcess[str]:
    completed = subprocess.run(
        [inputs.docker_bin, *arguments],
        env=_sanitized_environment() if environment is None else environment,
        text=True,
        capture_output=True,
        timeout=inputs.timeout_seconds,
        check=False,
    )
    if check and completed.returncode != 0:
        raise RuntimeError(
            f"docker command failed with exit code {completed.returncode}: {' '.join(arguments[:3])}"
        )
    return completed


def _sanitized_environment() -> dict[str, str]:
    environment = os.environ.copy()
    for key in ("PGPASSWORD", "POSTGRES_PASSWORD", "DATABASE_PASSWORD", "RADAR_POSTGRES_PASSWORD"):
        environment.pop(key, None)
    return environment


def _require_absent(inputs: ComposeRollbackInputs, category: str, name: str) -> None:
    completed = _run(inputs, [category, "inspect", name], check=False)
    if completed.returncode == 0:
        raise RuntimeError(f"rollback drill resource already exists: {name}")
    if completed.returncode != 1:
        raise RuntimeError(
            f"rollback drill {category} inspect failed for {name} with exit code {completed.returncode}"
        )


def _wait_for_database(inputs: ComposeRollbackInputs, container: str) -> None:
    deadline = time.monotonic() + inputs.timeout_seconds
    while time.monotonic() < deadline:
        completed = _run(
            inputs,
            ["exec", container, "pg_isready", "-U", inputs.database_user, "-d", inputs.database_name],
            check=False,
        )
        if completed.returncode == 0:
            return
        time.sleep(1)
    raise RuntimeError("rollback PostgreSQL clone did not become ready")


def _wait_for_health(inputs: ComposeRollbackInputs, container: str) -> None:
    deadline = time.monotonic() + inputs.timeout_seconds
    while time.monotonic() < deadline:
        completed = _run(
            inputs,
            [
                "exec",
                container,
                "wget",
                "-q",
                "-T",
                "5",
                "-O",
                "/dev/null",
                "http://127.0.0.1:8080/health",
            ],
            check=False,
        )
        if completed.returncode == 0:
            return
        time.sleep(1)
    raise RuntimeError("rollback control plane did not become healthy")


def _clone_database(
    inputs: ComposeRollbackInputs,
    source_container: str,
    target_container: str,
) -> None:
    deadline = time.monotonic() + inputs.timeout_seconds
    dump_process: subprocess.Popen[bytes] | None = None
    restore_process: subprocess.Popen[bytes] | None = None
    try:
        dump_process = subprocess.Popen(
            [
                inputs.docker_bin,
                "exec",
                source_container,
                "pg_dump",
                "-Fc",
                "-U",
                inputs.database_user,
                inputs.database_name,
            ],
            stdout=subprocess.PIPE,
            env=_sanitized_environment(),
        )
        restore_process = subprocess.Popen(
            [
                inputs.docker_bin,
                "exec",
                "-i",
                "-e",
                "PGPASSFILE=/run/secrets/radar-database.pgpass",
                target_container,
                "pg_restore",
                "--clean",
                "--if-exists",
                "--no-owner",
                "-U",
                inputs.database_user,
                "-d",
                inputs.database_name,
            ],
            stdin=dump_process.stdout,
            env=_sanitized_environment(),
        )
        if dump_process.stdout is not None:
            dump_process.stdout.close()
        remaining = max(0.001, deadline - time.monotonic())
        dump_code = dump_process.wait(timeout=remaining)
        remaining = max(0.001, deadline - time.monotonic())
        restore_code = restore_process.wait(timeout=remaining)
    except subprocess.TimeoutExpired as error:
        for process in (dump_process, restore_process):
            if process is not None and process.poll() is None:
                process.kill()
        raise RuntimeError("primary database clone timed out") from error
    if dump_code != 0:
        raise RuntimeError(f"primary database dump failed with exit code {dump_code}")
    if restore_code != 0:
        raise RuntimeError(f"primary database clone restore failed with exit code {restore_code}")


def _atomic_json(path: Path, document: dict[str, object]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    temporary = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            json.dump(document, stream, ensure_ascii=True, sort_keys=True, indent=2)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.chmod(temporary, 0o600)
        os.replace(temporary, path)
        os.chmod(path, 0o600)
    finally:
        temporary.unlink(missing_ok=True)


def run_drill(inputs: ComposeRollbackInputs) -> dict[str, object]:
    """Clone the active Compose database and run the previous control plane only on the clone."""
    _validate(inputs)
    names = _resource_names(inputs.run_id)
    clone_database = names["clone_database"]
    clone_volume = names["clone_volume"]
    rollback_control = names["rollback_control"]
    _require_absent(inputs, "container", clone_database)
    _require_absent(inputs, "container", rollback_control)
    _require_absent(inputs, "volume", clone_volume)

    clone_started = False
    rollback_started = False
    volume_created = False
    operation_error: BaseException | None = None
    result: dict[str, object] | None = None
    try:
        _run(
            inputs,
            ["volume", "create", "--label", f"com.docker.compose.project={inputs.run_id}", clone_volume],
        )
        volume_created = True
        _run(
            inputs,
            [
                "run",
                "-d",
                "--name",
                clone_database,
                "--network",
                names["network"],
                "--network-alias",
                ROLLBACK_DATABASE_ALIAS,
                "--label",
                f"com.docker.compose.project={inputs.run_id}",
                "--mount",
                (
                    "type=bind,src="
                    f"{inputs.database_password_file.resolve()},"
                    "dst=/run/secrets/radar-database-password,readonly"
                ),
                "--mount",
                (
                    "type=bind,src="
                    f"{inputs.pgpass_file.resolve()},"
                    "dst=/run/secrets/radar-database.pgpass,readonly"
                ),
                "-e",
                f"POSTGRES_USER={inputs.database_user}",
                "-e",
                "POSTGRES_PASSWORD_FILE=/run/secrets/radar-database-password",
                "-e",
                f"POSTGRES_DB={inputs.database_name}",
                "-e",
                "PGDATA=/var/lib/postgresql/data",
                "-v",
                f"{clone_volume}:/var/lib/postgresql/data",
                inputs.postgres_image,
            ],
        )
        clone_started = True
        _wait_for_database(inputs, clone_database)
        _clone_database(inputs, names["source_database"], clone_database)

        _run(
            inputs,
            [
                "run",
                "-d",
                "--name",
                rollback_control,
                "--network",
                names["network"],
                "--label",
                f"com.docker.compose.project={inputs.run_id}",
                "--mount",
                (
                    "type=bind,src="
                    f"{inputs.database_password_file.resolve()},"
                    "dst=/run/secrets/radar-database-password,readonly"
                ),
                "--env-file",
                str(inputs.environment_file),
                "-e",
                "AUTO_SETUP=true",
                "-e",
                f"DATABASE_HOST={ROLLBACK_DATABASE_ALIAS}",
                "-e",
                "DATABASE_PORT=5432",
                "-e",
                f"DATABASE_USER={inputs.database_user}",
                "-e",
                "DATABASE_PASSWORD_FILE=/run/secrets/radar-database-password",
                "-e",
                f"DATABASE_DBNAME={inputs.database_name}",
                "-e",
                f"REDIS_HOST={ROLLBACK_REDIS_ALIAS}",
                "--entrypoint",
                "/bin/sh",
                inputs.rollback_control_plane_image,
                "-ec",
                'export DATABASE_PASSWORD="$(cat "$DATABASE_PASSWORD_FILE")"; exec /app/docker-entrypoint.sh /app/sub2api',
            ],
        )
        rollback_started = True
        inspected = _run(
            inputs,
            ["inspect", "--format", "{{range .Config.Env}}{{println .}}{{end}}", rollback_control],
        ).stdout.splitlines()
        database_hosts = [line for line in inspected if line.startswith("DATABASE_HOST=")]
        if database_hosts != [f"DATABASE_HOST={ROLLBACK_DATABASE_ALIAS}"]:
            raise RuntimeError("rollback control plane is not bound exclusively to the primary clone")
        redis_hosts = [line for line in inspected if line.startswith("REDIS_HOST=")]
        if redis_hosts != [f"REDIS_HOST={ROLLBACK_REDIS_ALIAS}"]:
            raise RuntimeError("rollback control plane is not bound to the isolated Redis service")
        if any(
            line.startswith(("PGPASSWORD=", "POSTGRES_PASSWORD=", "DATABASE_PASSWORD=", "RADAR_POSTGRES_PASSWORD="))
            for line in inspected
        ):
            raise RuntimeError("rollback control plane inspect exposes a database password")
        _wait_for_health(inputs, rollback_control)
        result = {
            "schema_version": SCHEMA_VERSION,
            "primary_database_clone_used": True,
            "rollback_control_plane_clone_only": True,
            "rollback_health_passed": True,
        }
    except BaseException as error:
        operation_error = error

    cleanup_errors: list[str] = []

    def cleanup_resource(label: str, arguments: list[str]) -> None:
        try:
            completed = _run(inputs, arguments, check=False)
        except (OSError, subprocess.SubprocessError) as error:
            cleanup_errors.append(f"{label} ({type(error).__name__})")
            return
        if completed.returncode != 0:
            cleanup_errors.append(f"{label} (exit code {completed.returncode})")

    if rollback_started:
        cleanup_resource("rollback control container", ["rm", "-f", rollback_control])
    if clone_started:
        cleanup_resource("rollback database clone container", ["rm", "-f", clone_database])
    if volume_created and not inputs.retain_volume:
        cleanup_resource("rollback database clone volume", ["volume", "rm", clone_volume])

    if operation_error is not None:
        if cleanup_errors:
            operation_error.add_note("cleanup failed for: " + ", ".join(cleanup_errors))
        raise operation_error
    if cleanup_errors:
        raise RuntimeError("rollback drill cleanup failed for: " + ", ".join(cleanup_errors))
    if result is None:
        raise RuntimeError("rollback drill did not produce a result")
    _atomic_json(inputs.output_path, result)
    return result


def main() -> int:
    parser = argparse.ArgumentParser(description="Run the local Compose primary clone rollback drill")
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--docker-bin", default="docker")
    parser.add_argument("--environment-file", required=True, type=Path)
    parser.add_argument("--postgres-image", required=True)
    parser.add_argument("--rollback-control-plane-image", required=True)
    parser.add_argument("--database-password-file", required=True, type=Path)
    parser.add_argument("--pgpass-file", required=True, type=Path)
    parser.add_argument("--database-user", default="radar")
    parser.add_argument("--database-name", default="radar")
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--timeout-seconds", type=int, default=120)
    parser.add_argument("--retain-volume", action="store_true")
    arguments = parser.parse_args()
    try:
        run_drill(
            ComposeRollbackInputs(
                run_id=arguments.run_id,
                docker_bin=arguments.docker_bin,
                environment_file=arguments.environment_file,
                postgres_image=arguments.postgres_image,
                rollback_control_plane_image=arguments.rollback_control_plane_image,
                database_password_file=arguments.database_password_file,
                pgpass_file=arguments.pgpass_file,
                database_user=arguments.database_user,
                database_name=arguments.database_name,
                output_path=arguments.output,
                timeout_seconds=arguments.timeout_seconds,
                retain_volume=arguments.retain_volume,
            )
        )
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
        print(f"ERROR: {error}", file=os.sys.stderr)
        for note in getattr(error, "__notes__", ()):
            print(f"ERROR: {note}", file=os.sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
