#!/usr/bin/env python3
"""Fail-closed production rollback evidence audit for Radar release gates."""

from __future__ import annotations

import argparse
import json
import re
import sys
from datetime import UTC, datetime
from decimal import Decimal, InvalidOperation
from pathlib import Path
from typing import Any


INPUT_SCHEMA_VERSION = "radar-production-rollback-evidence-v2"
OUTPUT_SCHEMA_VERSION = "radar-production-rollback-audit-v2"

IMAGE_DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")


def utc_now() -> str:
    return datetime.now(UTC).isoformat(timespec="seconds").replace("+00:00", "Z")


def audit_rollback(
    document: dict[str, Any],
    *,
    expected_schema_migrations: int,
) -> dict[str, Any]:
    checks: list[dict[str, Any]] = []
    blockers: list[str] = []

    accepted_candidate = _mapping(document.get("accepted_candidate"))
    previous = _mapping(document.get("previous"))
    final_active = _mapping(document.get("final_active"))
    accepted_control_plane_digest = str(accepted_candidate.get("control_plane_digest") or "")
    accepted_worker_digest = str(accepted_candidate.get("worker_digest") or "")
    previous_control_plane_digest = str(previous.get("control_plane_digest") or "")
    previous_worker_digest = str(previous.get("worker_digest") or "")
    final_control_plane_digest = str(final_active.get("control_plane_digest") or "")
    final_worker_digest = str(final_active.get("worker_digest") or "")
    rollback_schema_migrations = _int_value(document.get("rollback_schema_migrations"))

    _add_check(
        checks,
        blockers,
        "accepted_candidate_control_plane_digest",
        _valid_image_digest(accepted_control_plane_digest),
        "accepted candidate control-plane digest must be sha256:<64 lowercase hex>",
        value=accepted_control_plane_digest or None,
    )
    _add_check(
        checks,
        blockers,
        "accepted_candidate_worker_digest",
        _valid_image_digest(accepted_worker_digest),
        "accepted candidate Worker digest must be sha256:<64 lowercase hex>",
        value=accepted_worker_digest or None,
    )
    _add_check(
        checks,
        blockers,
        "previous_control_plane_digest",
        _valid_image_digest(previous_control_plane_digest),
        "previous control-plane digest must be sha256:<64 lowercase hex>",
        value=previous_control_plane_digest or None,
    )
    _add_check(
        checks,
        blockers,
        "previous_worker_digest",
        _valid_image_digest(previous_worker_digest),
        "previous Worker digest must be sha256:<64 lowercase hex>",
        value=previous_worker_digest or None,
    )
    _add_check(
        checks,
        blockers,
        "previous_control_plane_digest_distinct_from_candidate",
        bool(
            previous_control_plane_digest
            and accepted_control_plane_digest
            and previous_control_plane_digest != accepted_control_plane_digest
        ),
        "previous control-plane digest must differ from the accepted candidate",
    )
    _add_check(
        checks,
        blockers,
        "previous_worker_digest_distinct_from_candidate",
        bool(
            previous_worker_digest
            and accepted_worker_digest
            and previous_worker_digest != accepted_worker_digest
        ),
        "previous Worker digest must differ from the accepted candidate",
    )
    _add_bool_check(
        checks,
        blockers,
        previous,
        "previous_control_plane_available",
        source_key="control_plane_available",
    )
    _add_bool_check(
        checks,
        blockers,
        previous,
        "previous_worker_available",
        source_key="worker_available",
    )
    _add_bool_check(checks, blockers, document, "rollback_executed")
    _add_bool_check(checks, blockers, document, "rollback_smoke_ok")

    _add_check(
        checks,
        blockers,
        "rollback_schema_migrations",
        rollback_schema_migrations == expected_schema_migrations,
        "rollback schema migration count does not match expected value",
        rollback_schema_migrations=rollback_schema_migrations,
        expected_schema_migrations=expected_schema_migrations,
    )

    before_total = document.get("budget_ledger_total_before")
    after_total = document.get("budget_ledger_total_after")
    ledger_check = _ledger_totals_unchanged(before_total, after_total)
    _add_check(
        checks,
        blockers,
        "budget_ledger_total_unchanged",
        ledger_check["ok"],
        "budget ledger totals changed or are malformed",
        before=before_total,
        after=after_total,
        checked=ledger_check["checked"],
    )

    _add_bool_check(checks, blockers, document, "accepted_candidate_restored")
    _add_bool_check(checks, blockers, document, "post_restore_smoke_ok")

    _add_check(
        checks,
        blockers,
        "final_active_control_plane_digest",
        _valid_image_digest(final_control_plane_digest),
        "final active control-plane digest must be sha256:<64 lowercase hex>",
        value=final_control_plane_digest or None,
    )
    _add_check(
        checks,
        blockers,
        "final_active_worker_digest",
        _valid_image_digest(final_worker_digest),
        "final active Worker digest must be sha256:<64 lowercase hex>",
        value=final_worker_digest or None,
    )
    _add_check(
        checks,
        blockers,
        "final_active_control_plane_digest_matches_candidate",
        bool(
            final_control_plane_digest
            and accepted_control_plane_digest
            and final_control_plane_digest == accepted_control_plane_digest
        ),
        "final active control-plane digest must match the accepted candidate",
    )
    _add_check(
        checks,
        blockers,
        "final_active_worker_digest_matches_candidate",
        bool(
            final_worker_digest
            and accepted_worker_digest
            and final_worker_digest == accepted_worker_digest
        ),
        "final active Worker digest must match the accepted candidate",
    )

    return {
        "schema_version": OUTPUT_SCHEMA_VERSION,
        "checked_at": utc_now(),
        "ok": not blockers,
        "checks": checks,
        "blockers": blockers,
        "summary": {
            "accepted_candidate": {
                "control_plane_digest": accepted_control_plane_digest,
                "worker_digest": accepted_worker_digest,
            },
            "previous": {
                "control_plane_digest": previous_control_plane_digest,
                "worker_digest": previous_worker_digest,
            },
            "rollback_schema_migrations": rollback_schema_migrations,
            "rollback_smoke_ok": document.get("rollback_smoke_ok") is True,
            "accepted_candidate_restored": document.get("accepted_candidate_restored") is True,
            "post_restore_smoke_ok": document.get("post_restore_smoke_ok") is True,
            "final_active": {
                "control_plane_digest": final_control_plane_digest,
                "worker_digest": final_worker_digest,
            },
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


def _mapping(value: object) -> dict[str, Any]:
    return value if isinstance(value, dict) else {}


def _ledger_totals_unchanged(before: object, after: object) -> dict[str, Any]:
    if before is None and after is None:
        return {"ok": True, "checked": False}
    try:
        before_decimal = _decimal_value(before)
        after_decimal = _decimal_value(after)
    except InvalidOperation:
        return {"ok": False, "checked": True}
    return {"ok": before_decimal == after_decimal, "checked": True}


def _decimal_value(value: object) -> Decimal:
    if isinstance(value, bool) or value is None:
        raise InvalidOperation
    try:
        return Decimal(str(value))
    except (InvalidOperation, ValueError):
        raise InvalidOperation from None


def _valid_image_digest(value: str) -> bool:
    return bool(IMAGE_DIGEST_RE.fullmatch(value))


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
        description="Audit Radar production rollback evidence after a rollback drill."
    )
    parser.add_argument("--rollback-evidence", type=Path, required=True)
    parser.add_argument("--expected-schema-migrations", type=int, default=255)
    parser.add_argument("--output", type=Path, help="write JSON audit result to this path")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv or sys.argv[1:])
    try:
        result = audit_rollback(
            read_json(args.rollback_evidence),
            expected_schema_migrations=args.expected_schema_migrations,
        )
        emit_json(result, args.output)
        return 0 if result["ok"] else 1
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        print(f"FAIL production rollback audit: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
