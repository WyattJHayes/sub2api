#!/usr/bin/env python3
"""Fail-closed host release gate for Radar container restarts and disk capacity."""

from __future__ import annotations

import argparse
import json
import shutil
import subprocess
import sys
from datetime import UTC, datetime
from pathlib import Path
from typing import Any


SCHEMA_VERSION = "radar-release-host-gate-v1"
DEFAULT_MAX_USED_PERCENT = 85.0
DEFAULT_MIN_FREE_BYTES = 10 * 1024 * 1024 * 1024


class DockerInspectError(RuntimeError):
    pass


def utc_now() -> str:
    return datetime.now(UTC).isoformat(timespec="seconds").replace("+00:00", "Z")


def capture_disk(path: str | Path) -> dict[str, Any]:
    disk_path = Path(path)
    usage = shutil.disk_usage(disk_path)
    used_bytes = usage.total - usage.free
    used_percent = 0.0
    if usage.total > 0:
        used_percent = round((used_bytes / usage.total) * 100, 2)
    return {
        "path": str(disk_path),
        "total_bytes": usage.total,
        "free_bytes": usage.free,
        "used_percent": used_percent,
    }


def inspect_container(container: str, docker_bin: str = "docker") -> dict[str, Any]:
    completed = subprocess.run(
        [docker_bin, "inspect", container],
        check=False,
        capture_output=True,
        text=True,
    )
    if completed.returncode != 0:
        message = completed.stderr.strip() or completed.stdout.strip() or "docker inspect failed"
        raise DockerInspectError(f"{container}: {message}")
    try:
        payload = json.loads(completed.stdout)
    except json.JSONDecodeError as exc:
        raise DockerInspectError(f"{container}: invalid docker inspect JSON: {exc}") from exc
    if not isinstance(payload, list) or not payload:
        raise DockerInspectError(f"{container}: docker inspect returned no container")
    item = payload[0]
    if not isinstance(item, dict):
        raise DockerInspectError(f"{container}: docker inspect returned a malformed object")

    state = item.get("State")
    if not isinstance(state, dict):
        state = {}
    health_obj = state.get("Health")
    health = "none"
    if isinstance(health_obj, dict):
        health = str(health_obj.get("Status") or "none")

    name = str(item.get("Name") or container).lstrip("/") or container
    return {
        "name": name,
        "container_id": str(item.get("Id") or ""),
        "started_at": str(state.get("StartedAt") or ""),
        "restart_count": int(item.get("RestartCount") or 0),
        "running": bool(state.get("Running")),
        "health": health,
    }


def capture_state(
    containers: list[str],
    *,
    docker_bin: str = "docker",
    allow_missing: bool = False,
) -> dict[str, Any]:
    captured: list[dict[str, Any]] = []
    for name in containers:
        try:
            captured.append(inspect_container(name, docker_bin=docker_bin))
        except DockerInspectError as exc:
            if not allow_missing:
                raise
            captured.append(
                {
                    "name": name,
                    "container_id": "",
                    "started_at": "",
                    "restart_count": None,
                    "running": False,
                    "health": "absent",
                    "absent": True,
                    "error": str(exc),
                }
            )
    return {
        "schema_version": SCHEMA_VERSION,
        "captured_at": utc_now(),
        "containers": captured,
    }


def verify_release_state(
    baseline: dict[str, Any],
    current: dict[str, Any],
    disk: dict[str, Any],
    *,
    max_used_percent: float = DEFAULT_MAX_USED_PERCENT,
    min_free_bytes: int = DEFAULT_MIN_FREE_BYTES,
) -> dict[str, Any]:
    checks: list[dict[str, Any]] = []
    failures: list[str] = []

    baseline_containers = _containers_by_name(baseline)
    current_containers = _containers_by_name(current)

    for name, before in baseline_containers.items():
        after = current_containers.get(name)
        if after is None or after.get("absent") is True:
            _add_check(checks, failures, "container_present", name, False, "container is absent")
            continue

        _check_equal(
            checks,
            failures,
            "container_id",
            name,
            before.get("container_id"),
            after.get("container_id"),
        )
        _check_equal(
            checks,
            failures,
            "started_at",
            name,
            before.get("started_at"),
            after.get("started_at"),
        )
        _check_restart_count(checks, failures, name, before, after)

        running = after.get("running") is True
        _add_check(
            checks,
            failures,
            "running",
            name,
            running,
            "container is not running",
            current_running=after.get("running"),
        )

        health = str(after.get("health") or "")
        _add_check(
            checks,
            failures,
            "health",
            name,
            health == "healthy",
            f"health is {health or 'missing'}",
            current_health=health,
        )

    used_percent = _float_value(disk.get("used_percent"))
    used_ok = used_percent <= max_used_percent
    _add_check(
        checks,
        failures,
        "disk_used_percent",
        str(disk.get("path") or "/"),
        used_ok,
        f"used_percent {used_percent} exceeds {max_used_percent}",
        used_percent=used_percent,
        max_used_percent=max_used_percent,
    )

    free_bytes = _int_value(disk.get("free_bytes"))
    free_ok = free_bytes >= min_free_bytes
    _add_check(
        checks,
        failures,
        "disk_free_bytes",
        str(disk.get("path") or "/"),
        free_ok,
        f"free_bytes {free_bytes} is below {min_free_bytes}",
        free_bytes=free_bytes,
        min_free_bytes=min_free_bytes,
    )

    return {
        "schema_version": SCHEMA_VERSION,
        "checked_at": utc_now(),
        "ok": not failures,
        "baseline_captured_at": baseline.get("captured_at"),
        "current_captured_at": current.get("captured_at"),
        "disk": disk,
        "checks": checks,
        "failures": failures,
    }


def _containers_by_name(document: dict[str, Any]) -> dict[str, dict[str, Any]]:
    containers = document.get("containers")
    if not isinstance(containers, list):
        return {}
    out: dict[str, dict[str, Any]] = {}
    for item in containers:
        if not isinstance(item, dict):
            continue
        name = str(item.get("name") or "").strip()
        if name:
            out[name] = item
    return out


def _add_check(
    checks: list[dict[str, Any]],
    failures: list[str],
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
        failures.append(f"{target} {name}: {message}")
    checks.append(check)


def _check_equal(
    checks: list[dict[str, Any]],
    failures: list[str],
    name: str,
    target: str,
    baseline_value: object,
    current_value: object,
) -> None:
    _add_check(
        checks,
        failures,
        name,
        target,
        baseline_value == current_value,
        f"{name} changed from {baseline_value!r} to {current_value!r}",
        **{f"baseline_{name}": baseline_value, f"current_{name}": current_value},
    )


def _check_restart_count(
    checks: list[dict[str, Any]],
    failures: list[str],
    target: str,
    before: dict[str, Any],
    after: dict[str, Any],
) -> None:
    baseline_restart_count = _int_value(before.get("restart_count"))
    current_restart_count = _int_value(after.get("restart_count"))
    _add_check(
        checks,
        failures,
        "restart_count",
        target,
        current_restart_count <= baseline_restart_count,
        f"restart_count increased from {baseline_restart_count} to {current_restart_count}",
        baseline_restart_count=baseline_restart_count,
        current_restart_count=current_restart_count,
    )


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


def _float_value(value: object) -> float:
    if isinstance(value, bool):
        return 0.0
    if isinstance(value, int | float):
        return float(value)
    if isinstance(value, str):
        try:
            return float(value)
        except ValueError:
            return 0.0
    return 0.0


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
        description="Gate Radar releases on container restart deltas and host capacity."
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    capture = subparsers.add_parser("capture", help="capture the current container state")
    capture.add_argument("--container", action="append", required=True, help="container name or ID")
    capture.add_argument("--docker-bin", default="docker", help="Docker CLI path")
    capture.add_argument("--output", type=Path, help="write JSON state to this path")

    verify = subparsers.add_parser("verify", help="verify current state against a baseline")
    verify.add_argument("--baseline", type=Path, required=True, help="baseline JSON from capture")
    verify.add_argument("--container", action="append", required=True, help="container name or ID")
    verify.add_argument("--disk-path", default="/", help="filesystem path to check")
    verify.add_argument("--docker-bin", default="docker", help="Docker CLI path")
    verify.add_argument("--max-used-percent", type=float, default=DEFAULT_MAX_USED_PERCENT)
    verify.add_argument("--min-free-gib", type=float, default=10.0)
    verify.add_argument("--output", type=Path, help="write JSON result to this path")

    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv or sys.argv[1:])
    try:
        if args.command == "capture":
            state = capture_state(args.container, docker_bin=args.docker_bin)
            emit_json(state, args.output)
            return 0

        baseline = read_json(args.baseline)
        current = capture_state(args.container, docker_bin=args.docker_bin, allow_missing=True)
        disk = capture_disk(args.disk_path)
        result = verify_release_state(
            baseline,
            current,
            disk,
            max_used_percent=args.max_used_percent,
            min_free_bytes=int(args.min_free_gib * 1024 * 1024 * 1024),
        )
        emit_json(result, args.output)
        return 0 if result["ok"] else 1
    except (DockerInspectError, OSError, ValueError, json.JSONDecodeError) as exc:
        print(f"FAIL release host gate: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
