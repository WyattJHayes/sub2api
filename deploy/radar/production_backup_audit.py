#!/usr/bin/env python3
"""Fail-closed production backup evidence audit for Radar release gates."""

from __future__ import annotations

import argparse
import json
import re
import sys
from datetime import UTC, datetime
from pathlib import PurePosixPath, Path
from typing import Any


INPUT_SCHEMA_VERSION = "radar-production-backup-evidence-v1"
OUTPUT_SCHEMA_VERSION = "radar-production-backup-audit-v1"
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")


def utc_now() -> str:
    return datetime.now(UTC).isoformat(timespec="seconds").replace("+00:00", "Z")


def audit_backup(
    document: dict[str, Any],
    *,
    max_age_seconds: int,
    min_size_bytes: int,
    expected_schema_migrations: int,
) -> dict[str, Any]:
    checks: list[dict[str, Any]] = []
    blockers: list[str] = []

    backup_path = str(document.get("path") or "")
    deployment_dir = str(document.get("deployment_dir") or "/opt/sub2api")
    sha256 = str(document.get("sha256") or "")
    created_at = str(document.get("created_at") or "")
    checked_at = str(document.get("checked_at") or utc_now())
    size_bytes = _int_value(document.get("size_bytes"))
    restore_migrations = _int_value(document.get("restore_schema_migrations"))

    _add_check(
        checks,
        blockers,
        "production_backup_path",
        bool(backup_path.startswith("/")),
        "production backup path must be absolute",
        value=backup_path or None,
    )

    _add_check(
        checks,
        blockers,
        "production_backup_outside_deployment_dir",
        _outside_deployment_dir(backup_path, deployment_dir),
        "production backup must be outside deployment directory",
        backup_path=backup_path or None,
        deployment_dir=deployment_dir,
    )

    _add_check(
        checks,
        blockers,
        "production_backup_sha256",
        bool(SHA256_RE.fullmatch(sha256)),
        "production backup SHA256 must be 64 lowercase hex",
        value=sha256 or None,
    )

    age_seconds = _age_seconds(created_at, checked_at)
    _add_check(
        checks,
        blockers,
        "production_backup_fresh",
        age_seconds is not None and age_seconds <= max_age_seconds,
        "production backup is too old or has invalid timestamps",
        age_seconds=age_seconds,
        max_age_seconds=max_age_seconds,
        created_at=created_at or None,
        checked_at=checked_at or None,
    )

    _add_check(
        checks,
        blockers,
        "production_backup_size_bytes",
        size_bytes >= min_size_bytes,
        "production backup size is below minimum",
        size_bytes=size_bytes,
        min_size_bytes=min_size_bytes,
    )

    _add_check(
        checks,
        blockers,
        "production_backup_restore_verified",
        document.get("restore_verified") is True,
        "restore_verified must be true",
        value=document.get("restore_verified"),
    )

    _add_check(
        checks,
        blockers,
        "production_backup_schema_migrations",
        restore_migrations == expected_schema_migrations,
        "restore schema migration count does not match expected value",
        restore_schema_migrations=restore_migrations,
        expected_schema_migrations=expected_schema_migrations,
    )

    return {
        "schema_version": OUTPUT_SCHEMA_VERSION,
        "checked_at": utc_now(),
        "ok": not blockers,
        "checks": checks,
        "blockers": blockers,
        "summary": {
            "path": backup_path,
            "sha256": sha256,
            "created_at": created_at,
            "restore_schema_migrations": restore_migrations,
            "restore_verified": document.get("restore_verified") is True,
        },
    }


def _outside_deployment_dir(backup_path: str, deployment_dir: str) -> bool:
    if not backup_path or not deployment_dir:
        return False
    backup = PurePosixPath(backup_path)
    deployment = PurePosixPath(deployment_dir)
    if not backup.is_absolute() or not deployment.is_absolute():
        return False
    return backup != deployment and deployment not in backup.parents


def _age_seconds(created_at: str, checked_at: str) -> int | None:
    try:
        created = _parse_utc(created_at)
        checked = _parse_utc(checked_at)
    except ValueError:
        return None
    delta = checked - created
    seconds = int(delta.total_seconds())
    return seconds if seconds >= 0 else None


def _parse_utc(value: str) -> datetime:
    if not value:
        raise ValueError("empty timestamp")
    if value.endswith("Z"):
        value = value[:-1] + "+00:00"
    parsed = datetime.fromisoformat(value)
    if parsed.tzinfo is None:
        raise ValueError("timestamp must include timezone")
    return parsed.astimezone(UTC)


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


def _add_check(
    checks: list[dict[str, Any]],
    blockers: list[str],
    name: str,
    ok: bool,
    message: str,
    **fields: Any,
) -> None:
    check = {"name": name, "ok": ok}
    check.update(fields)
    if not ok:
        check["message"] = message
        blockers.append(name)
    checks.append(check)


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
        description="Audit Radar production backup evidence before promotion."
    )
    parser.add_argument("--backup-evidence", type=Path, required=True)
    parser.add_argument("--max-age-seconds", type=int, default=3600)
    parser.add_argument("--min-size-bytes", type=int, default=1024)
    parser.add_argument("--expected-schema-migrations", type=int, default=255)
    parser.add_argument("--output", type=Path, help="write JSON audit result to this path")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv or sys.argv[1:])
    try:
        result = audit_backup(
            read_json(args.backup_evidence),
            max_age_seconds=args.max_age_seconds,
            min_size_bytes=args.min_size_bytes,
            expected_schema_migrations=args.expected_schema_migrations,
        )
        emit_json(result, args.output)
        return 0 if result["ok"] else 1
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        print(f"FAIL production backup audit: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
