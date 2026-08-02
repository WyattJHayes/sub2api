#!/usr/bin/env python3
"""Fail-closed production smoke evidence audit for Radar release gates."""

from __future__ import annotations

import argparse
import json
import re
import sys
from datetime import UTC, datetime
from pathlib import Path
from typing import Any


INPUT_SCHEMA_VERSION = "radar-production-smoke-evidence-v1"
OUTPUT_SCHEMA_VERSION = "radar-production-smoke-audit-v1"

IMAGE_DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
VALID_PRICING_SOURCES = {"local", "remote", "embedded"}


def utc_now() -> str:
    return datetime.now(UTC).isoformat(timespec="seconds").replace("+00:00", "Z")


def audit_smoke(
    document: dict[str, Any],
    *,
    min_api_success_count: int,
) -> dict[str, Any]:
    checks: list[dict[str, Any]] = []
    blockers: list[str] = []

    accepted_digest = str(document.get("accepted_candidate_digest") or "")
    active_digest = str(document.get("active_image_digest") or "")
    pricing_source = str(document.get("pricing_source") or "")
    pricing_resource_sha256 = str(document.get("pricing_resource_sha256") or "")
    p99_latency_ms = _int_value(document.get("p99_latency_ms"))
    p99_slo_ms = _int_value(document.get("p99_slo_ms"))

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
        "active_image_digest",
        _valid_image_digest(active_digest),
        "active image digest must be sha256:<64 lowercase hex>",
        value=active_digest or None,
    )

    _add_check(
        checks,
        blockers,
        "active_image_digest_matches_candidate",
        bool(active_digest and accepted_digest and active_digest == accepted_digest),
        "active image digest must match accepted candidate digest",
        active_image_digest=active_digest or None,
        accepted_candidate_digest=accepted_digest or None,
    )

    _add_check(
        checks,
        blockers,
        "app_health",
        str(document.get("app_health") or "") == "healthy",
        "application health must be healthy",
        value=document.get("app_health"),
    )

    _add_check(
        checks,
        blockers,
        "health_http_status",
        _int_value(document.get("health_http_status")) == 200,
        "health endpoint must return 200",
        value=document.get("health_http_status"),
    )

    _add_bool_check(checks, blockers, document, "api_smoke_ok")

    api_success_count = _int_value(document.get("api_success_count"))
    _add_check(
        checks,
        blockers,
        "api_success_count",
        api_success_count >= min_api_success_count,
        "API smoke success count is below minimum",
        value=api_success_count,
        min_api_success_count=min_api_success_count,
    )

    _add_zero_check(checks, blockers, document, "api_error_count")

    _add_check(
        checks,
        blockers,
        "p99_latency_slo",
        p99_slo_ms > 0 and p99_latency_ms <= p99_slo_ms,
        "P99 latency exceeds SLO or SLO is missing",
        p99_latency_ms=p99_latency_ms,
        p99_slo_ms=p99_slo_ms,
    )

    _add_zero_check(checks, blockers, document, "terminalization_outbox_pending")
    _add_zero_check(checks, blockers, document, "evaluation_outbox_pending")

    _add_check(
        checks,
        blockers,
        "pricing_source",
        pricing_source in VALID_PRICING_SOURCES,
        "pricing source must be local, remote, or embedded",
        value=pricing_source or None,
    )

    _add_check(
        checks,
        blockers,
        "pricing_resource_sha256",
        _valid_sha256(pricing_resource_sha256),
        "pricing resource SHA256 must be 64 lowercase hex",
        value=pricing_resource_sha256 or None,
    )

    _add_zero_check(checks, blockers, document, "pricing_fallback_failure_count")
    _add_zero_check(checks, blockers, document, "artifact_cleanup_error_count")
    _add_zero_check(checks, blockers, document, "billing_idempotency_failures")
    _add_zero_check(checks, blockers, document, "http_5xx_count")
    _add_zero_check(checks, blockers, document, "panic_count")
    _add_zero_check(checks, blockers, document, "control_plane_error_count")

    return {
        "schema_version": OUTPUT_SCHEMA_VERSION,
        "checked_at": utc_now(),
        "ok": not blockers,
        "checks": checks,
        "blockers": blockers,
        "summary": {
            "accepted_candidate_digest": accepted_digest,
            "active_image_digest": active_digest,
            "app_health": str(document.get("app_health") or ""),
            "health_http_status": _int_value(document.get("health_http_status")),
            "api_success_count": api_success_count,
            "p99_latency_ms": p99_latency_ms,
            "p99_slo_ms": p99_slo_ms,
            "terminalization_outbox_pending": _int_value(
                document.get("terminalization_outbox_pending")
            ),
            "evaluation_outbox_pending": _int_value(document.get("evaluation_outbox_pending")),
            "pricing_source": pricing_source,
            "billing_idempotency_failures": _int_value(
                document.get("billing_idempotency_failures")
            ),
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


def _add_zero_check(
    checks: list[dict[str, Any]],
    blockers: list[str],
    mapping: dict[str, Any],
    name: str,
) -> None:
    value = _int_value(mapping.get(name))
    _add_check(
        checks,
        blockers,
        name,
        value == 0,
        f"{name} must be zero",
        value=value,
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
        description="Audit Radar production smoke evidence after digest promotion."
    )
    parser.add_argument("--smoke-evidence", type=Path, required=True)
    parser.add_argument("--min-api-success-count", type=int, default=1)
    parser.add_argument("--output", type=Path, help="write JSON audit result to this path")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv or sys.argv[1:])
    try:
        result = audit_smoke(
            read_json(args.smoke_evidence),
            min_api_success_count=args.min_api_success_count,
        )
        emit_json(result, args.output)
        return 0 if result["ok"] else 1
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        print(f"FAIL production smoke audit: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
