#!/usr/bin/env python3
"""Fail-closed production promotion input audit for Radar release gates."""

from __future__ import annotations

import argparse
import json
import re
import sys
from datetime import UTC, datetime
from pathlib import Path
from typing import Any


INPUT_SCHEMA_VERSION = "radar-production-promotion-audit-input-v1"
OUTPUT_SCHEMA_VERSION = "radar-production-promotion-audit-v1"

IMAGE_DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")


def utc_now() -> str:
    return datetime.now(UTC).isoformat(timespec="seconds").replace("+00:00", "Z")


def audit_manifest(manifest: dict[str, Any]) -> dict[str, Any]:
    checks: list[dict[str, Any]] = []
    blockers: list[str] = []

    candidate = _mapping(manifest.get("candidate"))
    production_preflight = _mapping(manifest.get("production_preflight"))
    production_backup = _mapping(manifest.get("production_backup"))
    production_active = _mapping(manifest.get("production_active"))
    rollback = _mapping(manifest.get("rollback"))
    post_rollback = _mapping(manifest.get("post_rollback"))

    accepted_digest = str(candidate.get("accepted_staging_image_digest") or "")
    _add_check(
        checks,
        blockers,
        "accepted_staging_image_digest",
        _valid_image_digest(accepted_digest),
        "accepted staging image digest must be sha256:<64 lowercase hex>",
        value=accepted_digest or None,
    )

    _add_bool_check(checks, blockers, candidate, "staging_gate_ok")
    _add_bool_check(checks, blockers, candidate, "migration_rehearsal_ok")

    preflight_ok = production_preflight.get("ok") is True and production_preflight.get(
        "promotion_ready"
    ) is True
    _add_check(
        checks,
        blockers,
        "production_preflight_ok",
        preflight_ok,
        "production target preflight must be ok and promotion_ready",
        preflight_blockers=_list_of_strings(production_preflight.get("blockers")),
    )

    exposure_event = production_preflight.get("production_exposure_event") is True
    _add_check(
        checks,
        blockers,
        "production_requires_operator_authorization",
        not exposure_event,
        "inactive production target requires explicit operator authorization",
    )

    backup_sha = str(production_backup.get("sha256") or "")
    _add_check(
        checks,
        blockers,
        "production_backup_sha256",
        _valid_sha256(backup_sha),
        "production backup SHA256 must be 64 lowercase hex",
        value=backup_sha or None,
    )
    _add_bool_check(
        checks,
        blockers,
        production_backup,
        "production_backup_restore_verified",
        source_key="restore_verified",
    )

    active_digest = str(production_active.get("image_digest") or "")
    _add_check(
        checks,
        blockers,
        "production_active_image_digest",
        _valid_image_digest(active_digest),
        "active production image digest must be sha256:<64 lowercase hex>",
        value=active_digest or None,
    )

    config_hashes = _mapping(production_active.get("config_hashes"))
    required_config_hashes = [
        "docker-compose.yml",
        "docker-compose.override.yml",
        ".env",
        "data/config.yaml",
    ]
    has_required_config_hashes = all(
        _valid_sha256(str(config_hashes.get(name) or "")) for name in required_config_hashes
    )
    _add_check(
        checks,
        blockers,
        "production_config_hashes",
        has_required_config_hashes,
        "required production config hashes are missing or malformed",
        required=required_config_hashes,
    )
    for name in sorted(config_hashes):
        value = str(config_hashes.get(name) or "")
        if not _valid_sha256(value):
            _add_check(
                checks,
                blockers,
                f"production_config_hash_{name}",
                False,
                f"config hash for {name} must be 64 lowercase hex",
                value=value or None,
            )

    previous_digest = str(rollback.get("previous_image_digest") or "")
    _add_check(
        checks,
        blockers,
        "rollback_previous_image_digest",
        _valid_image_digest(previous_digest),
        "previous image digest must be sha256:<64 lowercase hex>",
        value=previous_digest or None,
    )
    _add_bool_check(checks, blockers, rollback, "rollback_image_available")
    _add_check(
        checks,
        blockers,
        "rollback_digest_distinct_from_candidate",
        bool(previous_digest and accepted_digest and previous_digest != accepted_digest),
        "rollback digest must be distinct from accepted candidate digest",
    )

    _add_bool_check(
        checks,
        blockers,
        post_rollback,
        "accepted_candidate_restoration_planned",
    )

    return {
        "schema_version": OUTPUT_SCHEMA_VERSION,
        "checked_at": utc_now(),
        "ok": not blockers,
        "promotion_ready": not blockers,
        "checks": checks,
        "blockers": blockers,
        "summary": {
            "accepted_staging_image_digest": accepted_digest,
            "previous_image_digest": previous_digest,
            "production_active_image_digest": active_digest,
            "production_backup_sha256": backup_sha,
            "production_preflight_ok": preflight_ok,
        },
    }


def _add_bool_check(
    checks: list[dict[str, Any]],
    blockers: list[str],
    mapping: dict[str, Any],
    name: str,
    *,
    source_key: str | None = None,
) -> None:
    key = source_key or name
    _add_check(
        checks,
        blockers,
        name,
        mapping.get(key) is True,
        f"{key} must be true",
        value=mapping.get(key),
    )


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


def _mapping(value: object) -> dict[str, Any]:
    return value if isinstance(value, dict) else {}


def _list_of_strings(value: object) -> list[str]:
    if not isinstance(value, list):
        return []
    return [str(item) for item in value]


def _valid_image_digest(value: str) -> bool:
    return bool(IMAGE_DIGEST_RE.fullmatch(value))


def _valid_sha256(value: str) -> bool:
    return bool(SHA256_RE.fullmatch(value))


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
        description="Audit Radar production promotion inputs before any production mutation."
    )
    parser.add_argument("--manifest", type=Path, required=True, help="promotion input manifest JSON")
    parser.add_argument("--output", type=Path, help="write JSON result to this path")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv or sys.argv[1:])
    try:
        manifest = read_json(args.manifest)
        result = audit_manifest(manifest)
        emit_json(result, args.output)
        return 0 if result["ok"] else 1
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        print(f"FAIL production promotion audit: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
