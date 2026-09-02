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

from production_evidence_envelope import (
    ENVELOPE_SCHEMA_VERSION,
    build_envelope,
    file_sha256,
    load_candidate_image_record,
    load_private_envelope,
    load_private_json,
    validate_predecessor,
    write_private_json,
)


INPUT_SCHEMA_VERSION = "radar-production-authorization-evidence-v2"
OUTPUT_SCHEMA_VERSION = "radar-production-authorization-audit-v2"
IMAGE_DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")

REQUIRED_AUTHORIZATIONS = (
    "confirm_target_dir",
    "authorize_inactive_stack_start",
    "authorize_env_chmod_0600",
    "authorize_fresh_backup",
    "authorize_digest_promotion",
    "authorize_rollback_drill",
)

BOUND_OUTPUT_SCHEMA_VERSION = ENVELOPE_SCHEMA_VERSION


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
    accepted_candidate = _mapping(document.get("accepted_candidate"))
    candidate_control_plane_digest = str(accepted_candidate.get("control_plane_digest") or "")
    candidate_worker_digest = str(accepted_candidate.get("worker_digest") or "")
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
        "accepted_candidate_control_plane_digest",
        bool(IMAGE_DIGEST_RE.fullmatch(candidate_control_plane_digest)),
        "accepted candidate control-plane digest must be sha256:<64 lowercase hex>",
        value=candidate_control_plane_digest or None,
    )
    _add_check(
        checks,
        blockers,
        "accepted_candidate_worker_digest",
        bool(IMAGE_DIGEST_RE.fullmatch(candidate_worker_digest)),
        "accepted candidate Worker digest must be sha256:<64 lowercase hex>",
        value=candidate_worker_digest or None,
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
            "accepted_candidate": {
                "control_plane_digest": candidate_control_plane_digest,
                "worker_digest": candidate_worker_digest,
            },
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


def _mapping(value: object) -> dict[str, Any]:
    return value if isinstance(value, dict) else {}


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


def build_bound_authorization(
    *,
    release_id: str,
    candidate_image_record_path: Path,
    pre_promotion_snapshot_path: Path,
    preflight_path: Path,
    authorization_claim_path: Path,
    checked_at: str,
    max_age_seconds: int = 3600,
) -> dict[str, Any]:
    """Create an authorization envelope from immutable predecessor evidence.

    The operator claim is intentionally kept separate from the envelope.  Only
    its six scopes, operator name, target directory and timestamps are copied.
    Candidate digests and target identity always come from the predecessor
    files, never from command-line values.
    """
    candidate = load_candidate_image_record(candidate_image_record_path)
    candidate_hash = file_sha256(candidate_image_record_path)
    snapshot = load_private_envelope(pre_promotion_snapshot_path, expected_type="target-snapshot")
    preflight = load_private_envelope(preflight_path, expected_type="preflight")
    if snapshot["release_id"] != release_id or preflight["release_id"] != release_id:
        raise ValueError("release_id does not match predecessor evidence")
    validate_predecessor(preflight, "target-snapshot", snapshot)
    claim = load_private_json(authorization_claim_path)

    binding = dict(snapshot["binding"])
    if binding["candidate_image_record_sha256"] != candidate_hash:
        raise ValueError("candidate image record hash does not match target snapshot")
    if preflight["binding"] != binding:
        raise ValueError("preflight binding does not match target snapshot")
    target_dir = str(claim.get("target_dir") or "")
    expected_target_dir = str(snapshot["payload"].get("target_dir") or "")
    if not expected_target_dir or target_dir != expected_target_dir:
        raise ValueError("authorization target directory does not match target snapshot")
    checked_timestamp = _parse_timestamp(checked_at)
    authorized_at = str(claim.get("authorized_at") or "")
    authorized_timestamp = _parse_timestamp(authorized_at)
    if checked_timestamp is None or authorized_timestamp is None:
        raise ValueError("authorization timestamps must include timezone")
    age_seconds = int((checked_timestamp - authorized_timestamp).total_seconds())
    if age_seconds < 0 or age_seconds > max_age_seconds:
        raise ValueError("authorization is stale or from the future")
    if not str(claim.get("operator") or "").strip():
        raise ValueError("operator must be recorded")
    missing = [name for name in REQUIRED_AUTHORIZATIONS if claim.get(name) is not True]
    if missing:
        raise ValueError("missing authorization scopes: " + ",".join(missing))

    started_at = _parse_timestamp(str(snapshot["finished_at"])) or checked_timestamp
    finished_at = checked_timestamp
    if finished_at < started_at:
        raise ValueError("authorization must start after pre-promotion snapshot")
    payload = {
        "target_dir": target_dir,
        "operator": str(claim["operator"]).strip(),
        "authorized_at": authorized_at,
        "checked_at": checked_at,
        "age_seconds": age_seconds,
        "required_authorizations": list(REQUIRED_AUTHORIZATIONS),
        "candidate_version": candidate["version"],
        "candidate_source_sha256": candidate["source_sha256"],
        "candidate_control_plane_manifest_digest": candidate["control_plane"]["manifest_digest"],
        "candidate_worker_manifest_digest": candidate["worker"]["manifest_digest"],
    }
    return build_envelope(
        evidence_type="authorization",
        release_id=release_id,
        started_at=started_at.isoformat(timespec="seconds").replace("+00:00", "Z"),
        finished_at=finished_at.isoformat(timespec="seconds").replace("+00:00", "Z"),
        binding=binding,
        input_evidence_sha256={
            "target-snapshot": file_sha256(pre_promotion_snapshot_path),
            "preflight": file_sha256(preflight_path),
            "authorization-claim": file_sha256(authorization_claim_path),
        },
        payload=payload,
    )


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
    parser.add_argument("--release-id", required=True)
    parser.add_argument("--candidate-image-record", type=Path, required=True)
    parser.add_argument("--pre-promotion-snapshot", type=Path, required=True)
    parser.add_argument("--preflight", type=Path, required=True)
    parser.add_argument("--authorization-claim", type=Path, required=True)
    parser.add_argument("--checked-at")
    parser.add_argument("--max-age-seconds", type=int, default=3600)
    parser.add_argument("--output", type=Path, help="write JSON audit result to this path")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv or sys.argv[1:])
    try:
        checked_at = args.checked_at or utc_now()
        result = build_bound_authorization(
            release_id=args.release_id,
            candidate_image_record_path=args.candidate_image_record,
            pre_promotion_snapshot_path=args.pre_promotion_snapshot,
            preflight_path=args.preflight,
            authorization_claim_path=args.authorization_claim,
            checked_at=checked_at,
            max_age_seconds=args.max_age_seconds,
        )
        if args.output is None:
            raise ValueError("--output is required for private authorization evidence")
        write_private_json(args.output, result)
        return 0
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        print(f"FAIL production authorization audit: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
