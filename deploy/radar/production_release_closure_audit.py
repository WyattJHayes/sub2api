#!/usr/bin/env python3
"""Fail-closed final production release closure audit for Radar."""

from __future__ import annotations

import argparse
import json
import re
import sys
from datetime import UTC, datetime
from pathlib import Path
from typing import Any


INPUT_SCHEMA_VERSION = "radar-production-release-closure-input-v1"
OUTPUT_SCHEMA_VERSION = "radar-production-release-closure-audit-v1"

IMAGE_DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")


def utc_now() -> str:
    return datetime.now(UTC).isoformat(timespec="seconds").replace("+00:00", "Z")


def audit_closure(document: dict[str, Any]) -> dict[str, Any]:
    checks: list[dict[str, Any]] = []
    blockers: list[str] = []

    accepted_digest = str(document.get("accepted_candidate_digest") or "")
    preflight = _mapping(document.get("production_target_preflight"))
    backup = _mapping(document.get("production_backup_audit"))
    promotion = _mapping(document.get("production_promotion_audit"))
    smoke = _mapping(document.get("production_smoke_audit"))
    rollback = _mapping(document.get("production_rollback_audit"))

    promotion_summary = _mapping(promotion.get("summary"))
    smoke_summary = _mapping(smoke.get("summary"))
    rollback_summary = _mapping(rollback.get("summary"))
    backup_summary = _mapping(backup.get("summary"))

    promotion_candidate_digest = str(
        promotion_summary.get("accepted_staging_image_digest") or ""
    )
    smoke_candidate_digest = str(smoke_summary.get("accepted_candidate_digest") or "")
    smoke_active_digest = str(smoke_summary.get("active_image_digest") or "")
    rollback_candidate_digest = str(rollback_summary.get("accepted_candidate_digest") or "")
    rollback_previous_digest = str(rollback_summary.get("previous_image_digest") or "")
    rollback_final_digest = str(rollback_summary.get("final_active_digest") or "")
    backup_sha256 = str(backup_summary.get("sha256") or "")

    _add_check(
        checks,
        blockers,
        "accepted_candidate_digest",
        _valid_image_digest(accepted_digest),
        "accepted candidate digest must be sha256:<64 lowercase hex>",
        value=accepted_digest or None,
    )

    _add_check(
        checks,
        blockers,
        "production_target_preflight_ok",
        preflight.get("ok") is True and preflight.get("promotion_ready") is True,
        "production target preflight must be ok and promotion_ready",
        evidence_blockers=_list_of_strings(preflight.get("blockers")),
    )

    _add_check(
        checks,
        blockers,
        "production_exposure_event_cleared",
        preflight.get("production_exposure_event") is not True,
        "production exposure event must be cleared by a later active-target preflight",
        value=preflight.get("production_exposure_event"),
    )

    _add_check(
        checks,
        blockers,
        "production_backup_audit_ok",
        backup.get("ok") is True,
        "production backup audit must be ok",
        evidence_blockers=_list_of_strings(backup.get("blockers")),
    )

    _add_check(
        checks,
        blockers,
        "production_backup_sha256",
        _valid_sha256(backup_sha256),
        "production backup SHA256 must be 64 lowercase hex",
        value=backup_sha256 or None,
    )

    _add_check(
        checks,
        blockers,
        "production_promotion_audit_ok",
        promotion.get("ok") is True and promotion.get("promotion_ready") is True,
        "production promotion audit must be ok and promotion_ready",
        evidence_blockers=_list_of_strings(promotion.get("blockers")),
    )

    _add_bool_check(checks, blockers, document, "production_promotion_executed")

    _add_check(
        checks,
        blockers,
        "promotion_candidate_digest_matches_candidate",
        bool(
            accepted_digest
            and promotion_candidate_digest
            and promotion_candidate_digest == accepted_digest
        ),
        "promotion candidate digest must match accepted candidate digest",
        promotion_candidate_digest=promotion_candidate_digest or None,
        accepted_candidate_digest=accepted_digest or None,
    )

    _add_check(
        checks,
        blockers,
        "production_smoke_audit_ok",
        smoke.get("ok") is True,
        "production smoke audit must be ok",
        evidence_blockers=_list_of_strings(smoke.get("blockers")),
    )

    _add_check(
        checks,
        blockers,
        "smoke_candidate_digest_matches_candidate",
        bool(accepted_digest and smoke_candidate_digest and smoke_candidate_digest == accepted_digest),
        "smoke candidate digest must match accepted candidate digest",
        smoke_candidate_digest=smoke_candidate_digest or None,
        accepted_candidate_digest=accepted_digest or None,
    )

    _add_check(
        checks,
        blockers,
        "smoke_active_digest_matches_candidate",
        bool(accepted_digest and smoke_active_digest and smoke_active_digest == accepted_digest),
        "smoke active digest must match accepted candidate digest",
        smoke_active_digest=smoke_active_digest or None,
        accepted_candidate_digest=accepted_digest or None,
    )

    _add_bool_check(checks, blockers, document, "rollback_drill_executed")

    _add_check(
        checks,
        blockers,
        "production_rollback_audit_ok",
        rollback.get("ok") is True,
        "production rollback audit must be ok",
        evidence_blockers=_list_of_strings(rollback.get("blockers")),
    )

    _add_check(
        checks,
        blockers,
        "rollback_candidate_digest_matches_candidate",
        bool(
            accepted_digest
            and rollback_candidate_digest
            and rollback_candidate_digest == accepted_digest
        ),
        "rollback candidate digest must match accepted candidate digest",
        rollback_candidate_digest=rollback_candidate_digest or None,
        accepted_candidate_digest=accepted_digest or None,
    )

    _add_check(
        checks,
        blockers,
        "rollback_final_digest_matches_candidate",
        bool(accepted_digest and rollback_final_digest and rollback_final_digest == accepted_digest),
        "rollback final digest must match accepted candidate digest",
        rollback_final_digest=rollback_final_digest or None,
        accepted_candidate_digest=accepted_digest or None,
    )

    _add_check(
        checks,
        blockers,
        "rollback_previous_digest_distinct_from_candidate",
        bool(
            accepted_digest
            and rollback_previous_digest
            and rollback_previous_digest != accepted_digest
        ),
        "rollback previous digest must be distinct from accepted candidate digest",
        rollback_previous_digest=rollback_previous_digest or None,
        accepted_candidate_digest=accepted_digest or None,
    )

    return {
        "schema_version": OUTPUT_SCHEMA_VERSION,
        "checked_at": utc_now(),
        "ok": not blockers,
        "checks": checks,
        "blockers": blockers,
        "summary": {
            "accepted_candidate_digest": accepted_digest,
            "promotion_candidate_digest": promotion_candidate_digest,
            "production_backup_sha256": backup_sha256,
            "smoke_active_digest": smoke_active_digest,
            "rollback_previous_digest": rollback_previous_digest,
            "final_active_digest": rollback_final_digest,
        },
    }


def _add_bool_check(
    checks: list[dict[str, Any]],
    blockers: list[str],
    mapping: dict[str, Any],
    name: str,
) -> None:
    _add_check(
        checks,
        blockers,
        name,
        mapping.get(name) is True,
        f"{name} must be true",
        value=mapping.get(name),
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
        description="Audit Radar final production release closure evidence."
    )
    parser.add_argument("--closure-evidence", type=Path, required=True)
    parser.add_argument("--output", type=Path, help="write JSON audit result to this path")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv or sys.argv[1:])
    try:
        result = audit_closure(read_json(args.closure_evidence))
        emit_json(result, args.output)
        return 0 if result["ok"] else 1
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        print(f"FAIL production release closure audit: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
