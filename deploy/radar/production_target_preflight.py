#!/usr/bin/env python3
"""Fail-closed production target preflight for Sub2API Radar promotion."""

from __future__ import annotations

import argparse
import grp
import hashlib
import json
import pwd
import re
import stat
import subprocess
import sys
from datetime import UTC, datetime
from pathlib import Path
from typing import Any


SCHEMA_VERSION = "radar-production-target-preflight-v1"
DEFAULT_TARGET_DIR = "/opt/sub2api"
DEFAULT_PROJECT = "sub2api"
DEFAULT_APP_SERVICE = "sub2api"
DEFAULT_APP_PORT = 8080
DEFAULT_DGC_NETWORK = "dramagenai-cloud_dgc-net"

INACTIVE_TARGET_AUTHORIZATIONS = [
    "confirm_target_dir",
    "authorize_inactive_stack_start",
    "authorize_env_chmod_0600",
    "authorize_fresh_backup",
    "authorize_digest_promotion",
    "authorize_rollback_drill",
]


class PreflightCaptureError(RuntimeError):
    pass


def utc_now() -> str:
    return datetime.now(UTC).isoformat(timespec="seconds").replace("+00:00", "Z")


def evaluate_snapshot(
    snapshot: dict[str, Any],
    *,
    project_name: str = DEFAULT_PROJECT,
    app_service: str = DEFAULT_APP_SERVICE,
    app_port: int = DEFAULT_APP_PORT,
) -> dict[str, Any]:
    checks: list[dict[str, Any]] = []
    blockers: list[str] = []
    required_authorizations: list[str] = []

    compose_running = _compose_project_running(snapshot, project_name)
    _add_check(
        checks,
        blockers,
        "production_compose_project_running",
        project_name,
        compose_running,
        f"Compose project {project_name!r} is not running",
    )

    app_container = _target_app_container(snapshot, app_service)
    app_present = app_container is not None
    _add_check(
        checks,
        blockers,
        "production_target_container_present",
        app_service,
        app_present,
        f"Compose service {app_service!r} has no target container",
    )

    app_running = app_present and app_container.get("running") is True
    _add_check(
        checks,
        blockers,
        "production_target_container_running",
        app_service,
        app_running,
        f"Compose service {app_service!r} is not running",
        current_running=app_container.get("running") if app_container else None,
    )

    app_health = str(app_container.get("health") or "") if app_container else ""
    app_healthy = app_present and app_health == "healthy"
    _add_check(
        checks,
        blockers,
        "production_target_container_healthy",
        app_service,
        app_healthy,
        f"Compose service {app_service!r} health is {app_health or 'missing'}",
        current_health=app_health or None,
    )

    env_mode = _normalized_mode(snapshot.get("env"))
    env_safe = env_mode == "600"
    _add_check(
        checks,
        blockers,
        "production_env_mode_0600",
        str(snapshot.get("target_dir") or DEFAULT_TARGET_DIR),
        env_safe,
        f".env mode is {env_mode or 'missing'}, expected 600",
        current_mode=env_mode or None,
    )

    active_target = compose_running or app_present
    production_exposure_event = not active_target

    if active_target:
        port_listening = _port_listening(snapshot, app_port)
        _add_check(
            checks,
            blockers,
            "production_port_8080_listening",
            str(app_port),
            port_listening,
            f"port {app_port} is not listening",
        )

        dgc_alias_present = _dgc_alias_present(snapshot, app_service)
        _add_check(
            checks,
            blockers,
            "production_dgc_alias_present",
            DEFAULT_DGC_NETWORK,
            dgc_alias_present,
            f"{app_service!r} alias is absent on {DEFAULT_DGC_NETWORK}",
        )

    if production_exposure_event:
        required_authorizations = list(INACTIVE_TARGET_AUTHORIZATIONS)
    elif not env_safe:
        required_authorizations.append("authorize_env_chmod_0600")

    return {
        "schema_version": SCHEMA_VERSION,
        "checked_at": utc_now(),
        "target_dir": str(snapshot.get("target_dir") or DEFAULT_TARGET_DIR),
        "ok": not blockers,
        "promotion_ready": not blockers,
        "production_exposure_event": production_exposure_event,
        "checks": checks,
        "blockers": blockers,
        "required_authorizations": required_authorizations,
    }


def capture_snapshot(
    target_dir: str | Path = DEFAULT_TARGET_DIR,
    *,
    project_name: str = DEFAULT_PROJECT,
    docker_bin: str = "docker",
    network: str = DEFAULT_DGC_NETWORK,
) -> dict[str, Any]:
    root = Path(target_dir)
    compose_projects = _capture_compose_projects(docker_bin)
    production_containers = _capture_production_containers(root, docker_bin)
    return {
        "schema_version": SCHEMA_VERSION,
        "captured_at": utc_now(),
        "target_dir": str(root),
        "project_name": project_name,
        "compose_projects": compose_projects,
        "production_containers": production_containers,
        "env": _capture_file_stat(root / ".env"),
        "config": _capture_file_stat(root / "data" / "config.yaml"),
        "hashes": _capture_hashes(
            [
                root / "docker-compose.yml",
                root / "docker-compose.override.yml",
                root / ".env",
                root / "data" / "config.yaml",
            ]
        ),
        "images": _capture_compose_images(root, docker_bin),
        "listeners": _capture_listeners(),
        "dgc_aliases": _capture_network_aliases(network, docker_bin),
    }


def _compose_project_running(snapshot: dict[str, Any], project_name: str) -> bool:
    projects = snapshot.get("compose_projects")
    if not isinstance(projects, list):
        return False
    for item in projects:
        if not isinstance(item, dict):
            continue
        name = str(item.get("name") or item.get("Name") or "").strip()
        status_value = str(item.get("status") or item.get("Status") or "").lower()
        if name == project_name and "running" in status_value:
            return True
    return False


def _target_app_container(snapshot: dict[str, Any], app_service: str) -> dict[str, Any] | None:
    containers = snapshot.get("production_containers")
    if not isinstance(containers, list):
        return None
    for item in containers:
        if not isinstance(item, dict):
            continue
        service = str(item.get("service") or item.get("Service") or "").strip()
        name = str(item.get("name") or item.get("Name") or "").strip()
        if service == app_service or name.endswith(f"-{app_service}-1"):
            return item
    return None


def _normalized_mode(env: object) -> str:
    mode_value: object = None
    if isinstance(env, dict):
        mode_value = env.get("mode")
    else:
        mode_value = env
    if isinstance(mode_value, int):
        if 0 <= mode_value <= 0o777:
            return format(mode_value, "03o")
        return str(mode_value).lstrip("0") or "0"
    if isinstance(mode_value, str):
        stripped = mode_value.strip()
        if not stripped:
            return ""
        return stripped.lstrip("0") or "0"
    return ""


def _port_listening(snapshot: dict[str, Any], app_port: int) -> bool:
    listeners = snapshot.get("listeners")
    if not isinstance(listeners, list):
        return False
    for item in listeners:
        if not isinstance(item, dict):
            continue
        if _int_value(item.get("port")) == app_port:
            return True
    return False


def _dgc_alias_present(snapshot: dict[str, Any], app_service: str) -> bool:
    aliases = snapshot.get("dgc_aliases")
    if not isinstance(aliases, list):
        return False
    for item in aliases:
        text = " ".join(str(value) for value in item.values()) if isinstance(item, dict) else str(item)
        if app_service in text:
            return True
    return False


def _add_check(
    checks: list[dict[str, Any]],
    blockers: list[str],
    name: str,
    target: str,
    ok: bool,
    message: str,
    **fields: Any,
) -> None:
    check = {"name": name, "target": target, "ok": ok}
    check.update(fields)
    if not ok:
        check["message"] = message
        blockers.append(name)
    checks.append(check)


def _capture_compose_projects(docker_bin: str) -> list[dict[str, Any]]:
    completed = _run([docker_bin, "compose", "ls", "--format", "json"])
    projects: list[dict[str, Any]] = []
    for item in _parse_json_objects(completed.stdout):
        projects.append(
            {
                "name": str(item.get("Name") or item.get("name") or ""),
                "status": str(item.get("Status") or item.get("status") or ""),
                "config_files": str(item.get("ConfigFiles") or item.get("configFiles") or ""),
            }
        )
    return projects


def _capture_production_containers(target_dir: Path, docker_bin: str) -> list[dict[str, Any]]:
    completed = _run(
        [docker_bin, "compose", "ps", "--all", "--format", "json"],
        cwd=target_dir,
        allow_failure=True,
    )
    if completed.returncode != 0:
        return []
    containers: list[dict[str, Any]] = []
    for item in _parse_json_objects(completed.stdout):
        state = str(item.get("State") or item.get("state") or "")
        health = str(item.get("Health") or item.get("health") or "")
        containers.append(
            {
                "name": str(item.get("Name") or item.get("name") or ""),
                "service": str(item.get("Service") or item.get("service") or ""),
                "running": state.lower() == "running",
                "health": health or ("healthy" if state.lower() == "running" else state.lower()),
                "image": str(item.get("Image") or item.get("image") or ""),
                "image_id": str(
                    item.get("ImageID")
                    or item.get("ImageId")
                    or item.get("image_id")
                    or item.get("ID")
                    or ""
                ),
                "state": state,
            }
        )
    return containers


def _capture_compose_images(target_dir: Path, docker_bin: str) -> list[str]:
    completed = _run(
        [docker_bin, "compose", "config", "--images"],
        cwd=target_dir,
        allow_failure=True,
    )
    if completed.returncode != 0:
        return []
    return [line.strip() for line in completed.stdout.splitlines() if line.strip()]


def _capture_file_stat(path: Path) -> dict[str, Any]:
    try:
        info = path.stat()
    except OSError as exc:
        return {"path": str(path), "present": False, "error": str(exc)}
    return {
        "path": str(path),
        "present": True,
        "mode": format(stat.S_IMODE(info.st_mode), "03o"),
        "owner": f"{pwd.getpwuid(info.st_uid).pw_name}:{grp.getgrgid(info.st_gid).gr_name}",
        "size": info.st_size,
    }


def _capture_hashes(paths: list[Path]) -> dict[str, str]:
    hashes: dict[str, str] = {}
    for path in paths:
        if not path.is_file():
            continue
        digest = hashlib.sha256()
        with path.open("rb") as stream:
            for chunk in iter(lambda: stream.read(1024 * 1024), b""):
                digest.update(chunk)
        hashes[str(path)] = digest.hexdigest()
    return hashes


def _capture_listeners() -> list[dict[str, Any]]:
    completed = _run(["ss", "-ltnp"], allow_failure=True)
    if completed.returncode != 0:
        return []
    listeners: list[dict[str, Any]] = []
    for line in completed.stdout.splitlines()[1:]:
        columns = line.split()
        if len(columns) < 4:
            continue
        local = columns[3]
        match = re.search(r"(?P<address>.+):(?P<port>\d+)$", local)
        if not match:
            continue
        listeners.append(
            {
                "local_address": match.group("address"),
                "port": int(match.group("port")),
                "process": " ".join(columns[6:]) if len(columns) > 6 else "",
            }
        )
    return listeners


def _capture_network_aliases(network: str, docker_bin: str) -> list[str]:
    completed = _run(
        [
            docker_bin,
            "network",
            "inspect",
            network,
            "--format",
            "{{range .Containers}}{{.Name}} {{.IPv4Address}} {{println}}{{end}}",
        ],
        allow_failure=True,
    )
    if completed.returncode != 0:
        return []
    return [line.strip() for line in completed.stdout.splitlines() if line.strip()]


def _parse_json_objects(text: str) -> list[dict[str, Any]]:
    body = text.strip()
    if not body:
        return []
    try:
        loaded = json.loads(body)
    except json.JSONDecodeError:
        loaded = None
    if isinstance(loaded, list):
        return [item for item in loaded if isinstance(item, dict)]
    if isinstance(loaded, dict):
        return [loaded]

    objects: list[dict[str, Any]] = []
    for line in body.splitlines():
        line = line.strip()
        if not line:
            continue
        item = json.loads(line)
        if isinstance(item, dict):
            objects.append(item)
    return objects


def _run(
    argv: list[str],
    *,
    cwd: Path | None = None,
    allow_failure: bool = False,
) -> subprocess.CompletedProcess[str]:
    completed = subprocess.run(argv, cwd=cwd, check=False, capture_output=True, text=True)
    if completed.returncode != 0 and not allow_failure:
        message = completed.stderr.strip() or completed.stdout.strip() or "command failed"
        raise PreflightCaptureError(f"{' '.join(argv)}: {message}")
    return completed


def _int_value(value: object) -> int:
    if isinstance(value, bool):
        return 0
    if isinstance(value, int):
        return value
    if isinstance(value, float):
        return int(value)
    if isinstance(value, str):
        try:
            return int(value)
        except ValueError:
            return 0
    return 0


def read_json(path: Path) -> dict[str, Any]:
    with path.open("r", encoding="utf-8") as stream:
        document = json.load(stream)
    if not isinstance(document, dict):
        raise ValueError(f"{path} must contain a JSON object")
    return document


def emit_json(document: dict[str, Any], output: Path | None) -> None:
    body = json.dumps(document, ensure_ascii=False, indent=2, sort_keys=True) + "\n"
    if output is None:
        sys.stdout.write(body)
        return
    output.write_text(body, encoding="utf-8")


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Gate Sub2API Radar production promotion on target identity and exposure state."
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    capture = subparsers.add_parser("capture", help="capture a read-only production target snapshot")
    capture.add_argument("--target-dir", default=DEFAULT_TARGET_DIR)
    capture.add_argument("--project", default=DEFAULT_PROJECT)
    capture.add_argument("--docker-bin", default="docker")
    capture.add_argument("--network", default=DEFAULT_DGC_NETWORK)
    capture.add_argument("--output", type=Path, help="write JSON snapshot to this path")

    evaluate = subparsers.add_parser("evaluate", help="evaluate a captured target snapshot")
    evaluate.add_argument("--snapshot", type=Path, required=True)
    evaluate.add_argument("--project", default=DEFAULT_PROJECT)
    evaluate.add_argument("--app-service", default=DEFAULT_APP_SERVICE)
    evaluate.add_argument("--app-port", type=int, default=DEFAULT_APP_PORT)
    evaluate.add_argument("--output", type=Path, help="write JSON result to this path")

    check = subparsers.add_parser("check", help="capture and evaluate in one read-only command")
    check.add_argument("--target-dir", default=DEFAULT_TARGET_DIR)
    check.add_argument("--project", default=DEFAULT_PROJECT)
    check.add_argument("--app-service", default=DEFAULT_APP_SERVICE)
    check.add_argument("--app-port", type=int, default=DEFAULT_APP_PORT)
    check.add_argument("--docker-bin", default="docker")
    check.add_argument("--network", default=DEFAULT_DGC_NETWORK)
    check.add_argument("--output", type=Path, help="write JSON result to this path")

    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv or sys.argv[1:])
    try:
        if args.command == "capture":
            document = capture_snapshot(
                args.target_dir,
                project_name=args.project,
                docker_bin=args.docker_bin,
                network=args.network,
            )
            emit_json(document, args.output)
            return 0

        if args.command == "evaluate":
            snapshot = read_json(args.snapshot)
            result = evaluate_snapshot(
                snapshot,
                project_name=args.project,
                app_service=args.app_service,
                app_port=args.app_port,
            )
            emit_json(result, args.output)
            return 0 if result["ok"] else 1

        snapshot = capture_snapshot(
            args.target_dir,
            project_name=args.project,
            docker_bin=args.docker_bin,
            network=args.network,
        )
        result = evaluate_snapshot(
            snapshot,
            project_name=args.project,
            app_service=args.app_service,
            app_port=args.app_port,
        )
        emit_json(result, args.output)
        return 0 if result["ok"] else 1
    except (PreflightCaptureError, OSError, ValueError, json.JSONDecodeError) as exc:
        print(f"FAIL production target preflight: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
