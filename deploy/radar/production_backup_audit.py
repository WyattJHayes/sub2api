#!/usr/bin/env python3
"""Fail-closed production backup evidence audit for Radar release gates."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import stat
import sys
from datetime import UTC, datetime
from pathlib import PurePosixPath, Path
from typing import Any

from production_evidence_envelope import (
    build_envelope,
    file_sha256,
    load_candidate_image_record,
    load_private_envelope,
    write_private_json,
)
from migration_ledger import expected_schema_migrations as manifest_expected_schema_migrations


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
    backup_path: Path | None = None,
) -> dict[str, Any]:
    checks: list[dict[str, Any]] = []
    blockers: list[str] = []

    claimed_backup_path = str(document.get("path") or "")
    deployment_dir = str(document.get("deployment_dir") or "/opt/sub2api")
    sha256 = str(document.get("sha256") or "")
    created_at = str(document.get("created_at") or "")
    checked_at = str(document.get("checked_at") or utc_now())
    size_bytes = _int_value(document.get("size_bytes"))
    restore_migrations = _int_value(document.get("restore_schema_migrations"))

    if backup_path is not None:
        facts, error = _reopen_backup(Path(backup_path), deployment_dir)
        actual_sha = str(facts.get("sha256") or "") if facts else ""
        _add_check(
            checks,
            blockers,
            "backup_file_sha256",
            error is None and actual_sha == sha256,
            error or "reopened backup SHA256 does not match the claim",
            value=actual_sha or None,
        )
        for field in ("canonical_path", "device", "inode", "size", "mtime_ns", "mode"):
            claimed = document.get(field)
            if claimed is None and isinstance(document.get("backup_file"), dict):
                claimed = document["backup_file"].get(field)
            if claimed is not None:
                _add_check(
                    checks,
                    blockers,
                    f"backup_file_{field}",
                    error is None and _same_stat_claim(field, claimed, facts.get(field)),
                    f"reopened backup {field} does not match the claim",
                )
        _add_check(
            checks,
            blockers,
            "backup_file_mode_0600",
            error is None and facts.get("mode") == "600",
            "backup file must be a regular 0600 file",
        )

    _add_check(
        checks,
        blockers,
        "production_backup_path",
        bool(claimed_backup_path.startswith("/")),
        "production backup path must be absolute",
        value=claimed_backup_path or None,
    )

    _add_check(
        checks,
        blockers,
        "production_backup_outside_deployment_dir",
        _outside_deployment_dir(claimed_backup_path, deployment_dir),
        "production backup must be outside deployment directory",
        backup_path=claimed_backup_path or None,
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
            "path": claimed_backup_path,
            "sha256": sha256,
            "created_at": created_at,
            "restore_schema_migrations": restore_migrations,
            "restore_verified": document.get("restore_verified") is True,
        },
    }


def _same_stat_claim(field: str, claimed: object, actual: object) -> bool:
    if field == "canonical_path":
        return str(claimed) == str(actual)
    if field == "mode":
        return str(claimed).lstrip("0") == str(actual).lstrip("0")
    try:
        return int(claimed) == int(actual)
    except (TypeError, ValueError):
        return False


def _reopen_backup(path: Path, deployment_dir: str) -> tuple[dict[str, Any], str | None]:
    try:
        canonical = path.resolve(strict=True)
        deployment = Path(deployment_dir).resolve()
        if canonical == deployment or deployment in canonical.parents:
            return {}, "production backup must be outside deployment directory"
        flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
        descriptor = os.open(path, flags)
    except OSError as exc:
        return {}, f"backup file cannot be opened safely: {exc}"
    try:
        info = os.fstat(descriptor)
        if not stat.S_ISREG(info.st_mode):
            return {}, "backup file must be a regular file"
        digest = hashlib.sha256()
        while block := os.read(descriptor, 1024 * 1024):
            digest.update(block)
        facts = {
            "canonical_path": str(canonical),
            "device": info.st_dev,
            "inode": info.st_ino,
            "size": info.st_size,
            "mtime_ns": info.st_mtime_ns,
            "mode": f"{stat.S_IMODE(info.st_mode):03o}",
            "sha256": digest.hexdigest(),
        }
        if facts["mode"] != "600":
            return facts, "backup file mode must be 0600"
        return facts, None
    finally:
        os.close(descriptor)


def build_bound_backup(
    *,
    release_id: str,
    candidate_image_record_path: Path,
    authorization_path: Path,
    backup_path: Path,
    restore_path: Path,
    deployment_dir: str = "/opt/sub2api",
    expected_schema_migrations: int | None = None,
    output_path: Path | None = None,
) -> dict[str, Any]:
    if expected_schema_migrations is None:
        expected_schema_migrations = manifest_expected_schema_migrations(
            Path(__file__).resolve().parent / "manifests" / "v0.2.0"
        )
    candidate = load_candidate_image_record(candidate_image_record_path)
    authorization = load_private_envelope(authorization_path, expected_type="authorization")
    restore = load_private_envelope(restore_path, expected_type="migration-rehearsal")
    if authorization["release_id"] != release_id or restore["release_id"] != release_id:
        raise ValueError("release_id does not match backup predecessors")
    if restore["binding"] != authorization["binding"]:
        raise ValueError("restore binding does not match authorization")
    facts, error = _reopen_backup(backup_path, deployment_dir)
    if error:
        raise ValueError(error)
    migration_count = _int_value(restore["payload"].get("migration_count"))
    if migration_count != expected_schema_migrations:
        raise ValueError(
            f"isolated restore migration count must be {expected_schema_migrations}"
        )
    payload = {
        "backup_file": facts,
        "restore_migration_count": migration_count,
        "candidate_source_sha256": candidate["source_sha256"],
    }
    evidence = build_envelope(
        evidence_type="backup",
        release_id=release_id,
        started_at=authorization["finished_at"],
        finished_at=utc_now(),
        binding=dict(authorization["binding"]),
        input_evidence_sha256={
            "authorization": authorization["evidence_sha256"],
            "restore": restore["evidence_sha256"],
            "candidate-image-record": file_sha256(candidate_image_record_path),
        },
        payload=payload,
    )
    if output_path is not None:
        write_private_json(output_path, evidence)
    return evidence


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
    parser.add_argument("--release-id", required=True)
    parser.add_argument("--candidate-image-record", type=Path, required=True)
    parser.add_argument("--authorization", type=Path, required=True)
    parser.add_argument("--backup-path", type=Path, required=True)
    parser.add_argument("--restore-envelope", type=Path, required=True)
    parser.add_argument("--deployment-dir", default="/opt/sub2api")
    parser.add_argument("--max-age-seconds", type=int, default=3600)
    parser.add_argument("--min-size-bytes", type=int, default=1024)
    parser.add_argument("--expected-schema-migrations", type=int, default=None)
    parser.add_argument("--output", type=Path, help="write JSON audit result to this path")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv or sys.argv[1:])
    try:
        result = build_bound_backup(
            release_id=args.release_id,
            candidate_image_record_path=args.candidate_image_record,
            authorization_path=args.authorization,
            backup_path=args.backup_path,
            restore_path=args.restore_envelope,
            deployment_dir=args.deployment_dir,
            expected_schema_migrations=args.expected_schema_migrations,
            output_path=args.output,
        )
        if args.output is None:
            raise ValueError("--output is required for private backup evidence")
        return 0
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        print(f"FAIL production backup audit: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
