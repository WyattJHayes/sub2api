#!/usr/bin/env python3
"""Fail-closed production mutation authorization audit for Radar."""

from __future__ import annotations

import argparse
import json
import re
import sys
from datetime import UTC, datetime
from pathlib import Path
from typing import Any


INPUT_SCHEMA_VERSION = "radar-production-authorization-evidence-v1"
OUTPUT_SCHEMA_VERSION = "radar-production-authorization-audit-v1"
IMAGE_DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")

REQUIRED_AUTHORIZATIONS = (
    "confirm_target_dir",
    "authorize_inactive_stack_start",
    "authorize_env_chmod_0600",
    "authorize_fresh_backup",
    "authorize_digest_promotion",
    "authorize_rollback_drill",
)


def utc_now() -> str:
    return datetime.now(UTC).isoformat(timespec="seconds").replace("+00:00", "Z")


def audit_authorization(
    document: dict[str, Any],
    *,
    expected_target_dir: str,
    checked_at: str | None = None,
    max_age_seconds: int = 3600,
) -> dict[str, Any]:
    checks: list[dict[str, Any]] = []
    blockers: list[str] = []

    effective_checked_at = checked_at or str(document.get("checked_at") or utc_now())
    target_dir = str(document.get("target_dir") or "")
    operator = str(document.get("operator") or "").strip()
    candidate_digest = str(document.get("accepted_candidate_digest") or "")
    authorized_at = str(document.get("authorized_at") or "")

    _add_check(
        checks,
        blockers,
        "target_dir",
        target_dir == expected_target_dir,
        "authorization target directory does not match expected production target",
        value=target_dir or None,
        expected=expected_target_dir,
    )

    _add_check(
        checks,
        blockers,
        "accepted_candidate_digest",
        bool(IMAGE_DIGEST_RE.fullmatch(candidate_digest)),
        "accepted candidate digest must be sha256:<64 lowercase hex>",
        value=candidate_digest or None,
    )

    _add_check(
        checks,
        blockers,
        "operator",
        bool(operator),
        "operator must be recorded",
        value=operator or None,
    )

    authorized_timestamp = _parse_timestamp(authorized_at)
    checked_timestamp = _parse_timestamp(effective_checked_at)
    timestamps_valid = authorized_timestamp is not None and checked_timestamp is not None
    _add_check(
        checks,
        blockers,
        "authorization_timestamp",
        timestamps_valid,
        "authorized_at and checked_at must be timezone-aware ISO 8601 timestamps",
        authorized_at=authorized_at or None,
        checked_at=effective_checked_at or None,
    )

    age_seconds: int | None = None
    if timestamps_valid:
        age_seconds = int((checked_timestamp - authorized_timestamp).total_seconds())
    freshness_ok = (
        age_seconds is not None
        and age_seconds >= 0
        and age_seconds <= max_age_seconds
    )
    _add_check(
        checks,
        blockers,
        "authorization_freshness",
        freshness_ok,
        "authorization must be current and cannot be from the future",
        age_seconds=age_seconds,
        max_age_seconds=max_age_seconds,
    )

    for name in REQUIRED_AUTHORIZATIONS:
        _add_check(
            checks,
            blockers,
            name,
            document.get(name) is True,
            f"{name} must be true",
            value=document.get(name),
        )

    return {
        "schema_version": OUTPUT_SCHEMA_VERSION,
        "checked_at": utc_now(),
        "ok": not blockers,
        "checks": checks,
        "blockers": blockers,
        "summary": {
            "target_dir": target_dir,
            "operator": operator,
            "accepted_candidate_digest": candidate_digest,
            "authorized_at": authorized_at,
            "checked_at": effective_checked_at,
            "age_seconds": age_seconds,
            "required_authorizations": list(REQUIRED_AUTHORIZATIONS),
        },
    }


def _parse_timestamp(value: str) -> datetime | None:
    if not value:
        return None
    try:
        normalized = value[:-1] + "+00:00" if value.endswith("Z") else value
        parsed = datetime.fromisoformat(normalized)
    except ValueError:
        return None
    if parsed.tzinfo is None:
        return None
    return parsed.astimezone(UTC)


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
        description="Audit Radar production mutation authorization evidence."
    )
    parser.add_argument("--authorization-evidence", type=Path, required=True)
    parser.add_argument("--expected-target-dir", default="/opt/sub2api")
    parser.add_argument("--checked-at")
    parser.add_argument("--max-age-seconds", type=int, default=3600)
    parser.add_argument("--output", type=Path, help="write JSON audit result to this path")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv or sys.argv[1:])
    try:
        result = audit_authorization(
            read_json(args.authorization_evidence),
            expected_target_dir=args.expected_target_dir,
            checked_at=args.checked_at,
            max_age_seconds=args.max_age_seconds,
        )
        emit_json(result, args.output)
        return 0 if result["ok"] else 1
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        print(f"FAIL production authorization audit: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
