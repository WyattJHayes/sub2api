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

from production_evidence_envelope import canonical_sha256


SCHEMA_VERSION = "radar-production-target-preflight-v1"
DEFAULT_TARGET_DIR = "/opt/sub2api"
DEFAULT_PROJECT = "sub2api"
DEFAULT_APP_SERVICE = "sub2api"
DEFAULT_APP_PORT = 8080
DEFAULT_DGC_NETWORK = "dramagenai-cloud_dgc-net"
CONTROL_REPOSITORY = "ghcr.io/wyattjhayes/sub2api-radar-control-plane"
WORKER_REPOSITORY = "ghcr.io/wyattjhayes/sub2api-radar-worker"
RADAR_WORKER_SERVICES = ("radar-runner", "radar-grader", "radar-statistics")

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
    candidate_image_record: dict[str, Any] | None = None,
) -> dict[str, Any]:
    checks: list[dict[str, Any]] = []
    blockers: list[str] = []
    required_authorizations: list[str] = []

    target_identity_ok = _snapshot_target_identity_matches(
        snapshot,
        project_name=project_name,
        app_service=app_service,
    )
    _add_check(
        checks,
        blockers,
        "production_target_identity",
        str(snapshot.get("target_dir") or DEFAULT_TARGET_DIR),
        target_identity_ok,
        "target identity is missing or does not match the captured host and directory",
    )

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

    if candidate_image_record is not None:
        _add_candidate_runtime_checks(
            checks,
            blockers,
            snapshot,
            candidate_image_record,
            app_service=app_service,
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
    root = Path(target_dir).resolve()
    root_stat = root.stat()
    machine_id_sha256 = _machine_id_sha256()
    descriptor = target_descriptor(
        machine_id_sha256=machine_id_sha256,
        target_dir=str(root),
        target_dir_device=root_stat.st_dev,
        target_dir_inode=root_stat.st_ino,
        project_name=project_name,
        app_service=DEFAULT_APP_SERVICE,
    )
    compose_projects = _capture_compose_projects(docker_bin)
    production_containers = _capture_production_containers(root, docker_bin)
    snapshot = {
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
        "host_fingerprint": descriptor["host_fingerprint"],
        "target_descriptor": descriptor,
        "target_id": target_id(descriptor),
    }
    snapshot["snapshot_sha256"] = canonical_sha256(snapshot)
    return snapshot


def target_descriptor(
    *,
    machine_id_sha256: str,
    target_dir: str,
    target_dir_device: int,
    target_dir_inode: int,
    project_name: str,
    app_service: str,
) -> dict[str, object]:
    if not re.fullmatch(r"[0-9a-f]{64}", machine_id_sha256):
        raise ValueError("machine_id_sha256 must be a lowercase SHA256")
    return {
        "host_fingerprint": canonical_sha256({"machine_id_sha256": machine_id_sha256}),
        "target_dir": str(Path(target_dir).resolve()),
        "target_dir_device": int(target_dir_device),
        "target_dir_inode": int(target_dir_inode),
        "project_name": project_name,
        "app_service": app_service,
    }


def target_id(descriptor: dict[str, object]) -> str:
    return canonical_sha256(descriptor)


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


def _target_service_container(snapshot: dict[str, Any], service: str) -> dict[str, Any] | None:
    containers = snapshot.get("production_containers")
    if not isinstance(containers, list):
        return None
    for item in containers:
        if isinstance(item, dict) and str(item.get("service") or "") == service:
            return item
    return None


def _snapshot_target_identity_matches(
    snapshot: dict[str, Any],
    *,
    project_name: str,
    app_service: str,
) -> bool:
    descriptor = snapshot.get("target_descriptor")
    target_value = snapshot.get("target_id")
    host_fingerprint = snapshot.get("host_fingerprint")
    if not isinstance(descriptor, dict) or not isinstance(target_value, str):
        return False
    if descriptor.get("host_fingerprint") != host_fingerprint:
        return False
    if descriptor.get("target_dir") != str(snapshot.get("target_dir") or ""):
        return False
    if descriptor.get("project_name") != project_name or descriptor.get("app_service") != app_service:
        return False
    return target_value == target_id(descriptor)


def _add_candidate_runtime_checks(
    checks: list[dict[str, Any]],
    blockers: list[str],
    snapshot: dict[str, Any],
    candidate_image_record: dict[str, Any],
    *,
    app_service: str,
) -> None:
    control = candidate_image_record.get("control_plane")
    worker = candidate_image_record.get("worker")
    control_reference = _candidate_reference(control, CONTROL_REPOSITORY)
    worker_reference = _candidate_reference(worker, WORKER_REPOSITORY)
    control_container = _target_service_container(snapshot, app_service)
    _add_check(
        checks,
        blockers,
        "control_plane_repo_digest",
        control_reference or CONTROL_REPOSITORY,
        _container_has_repo_digest(control_container, control_reference),
        "running control-plane container does not expose the approved repository digest",
    )
    for service in RADAR_WORKER_SERVICES:
        _add_check(
            checks,
            blockers,
            f"{service.replace('-', '_')}_repo_digest",
            worker_reference or WORKER_REPOSITORY,
            _container_has_repo_digest(_target_service_container(snapshot, service), worker_reference),
            f"running {service} container does not expose the approved repository digest",
        )


def _candidate_reference(image_record: object, repository: str) -> str:
    if not isinstance(image_record, dict):
        return ""
    digest = image_record.get("manifest_digest")
    if image_record.get("repository") != repository or not isinstance(digest, str):
        return ""
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        return ""
    return f"{repository}@{digest}"


def _container_has_repo_digest(container: dict[str, Any] | None, reference: str) -> bool:
    if container is None or not reference:
        return False
    repo_digests = container.get("repo_digests")
    return isinstance(repo_digests, list) and reference in repo_digests


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
        container_id = str(item.get("ID") or item.get("Id") or item.get("id") or "")
        runtime_identity = _capture_container_runtime_identity(container_id, docker_bin)
        containers.append(
            {
                "name": str(item.get("Name") or item.get("name") or ""),
                "service": str(item.get("Service") or item.get("service") or ""),
                "running": state.lower() == "running",
                "health": health or ("healthy" if state.lower() == "running" else state.lower()),
                "image": str(item.get("Image") or item.get("image") or ""),
                "configured_image": str(
                    runtime_identity.get("configured_image")
                    or item.get("Image")
                    or item.get("image")
                    or ""
                ),
                "image_id": str(
                    runtime_identity.get("image_config_id")
                    or item.get("ImageID")
                    or item.get("ImageId")
                    or item.get("image_id")
                    or ""
                ),
                "image_config_id": str(runtime_identity.get("image_config_id") or ""),
                "repo_digests": runtime_identity.get("repo_digests") or [],
                "state": state,
            }
        )
    return containers


def _capture_container_runtime_identity(container_id: str, docker_bin: str) -> dict[str, object]:
    if not container_id:
        return {"configured_image": "", "image_config_id": "", "repo_digests": []}
    completed = _run([docker_bin, "inspect", container_id], allow_failure=True)
    if completed.returncode != 0:
        return {"configured_image": "", "image_config_id": "", "repo_digests": []}
    try:
        document = json.loads(completed.stdout)
    except json.JSONDecodeError:
        return {"configured_image": "", "image_config_id": "", "repo_digests": []}
    if not isinstance(document, list) or len(document) != 1 or not isinstance(document[0], dict):
        return {"configured_image": "", "image_config_id": "", "repo_digests": []}
    inspected = document[0]
    config = inspected.get("Config")
    repo_digests = inspected.get("RepoDigests")
    return {
        "configured_image": str(config.get("Image") or "") if isinstance(config, dict) else "",
        "image_config_id": str(inspected.get("Image") or ""),
        "repo_digests": sorted(
            str(value) for value in repo_digests if isinstance(value, str)
        ) if isinstance(repo_digests, list) else [],
    }


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


def _machine_id_sha256() -> str:
    path = Path("/etc/machine-id")
    try:
        value = path.read_text(encoding="utf-8").strip()
    except OSError as exc:
        raise PreflightCaptureError(f"cannot read {path}: {exc}") from exc
    if not value:
        raise PreflightCaptureError(f"cannot read a non-empty machine identity from {path}")
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


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
