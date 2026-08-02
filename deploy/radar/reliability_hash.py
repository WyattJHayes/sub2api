"""Hash the immutable reliability snapshot identity exactly like the Go writer."""

from __future__ import annotations

import hashlib
import json
import re
from datetime import UTC, datetime
from typing import Any
from uuid import UUID


METRIC_FIELDS = (
    ("request_count", False),
    ("success_count", True),
    ("error_count", True),
    ("timeout_count", True),
    ("retry_count", True),
    ("protocol_error_count", True),
    ("billing_idempotency_failures", True),
    ("successful_latency_count", False),
    ("valid_pair_count", False),
    ("upstream_failure_count", False),
    ("gateway_failure_count", False),
    ("client_cancellation_count", False),
    ("error_numerator", False),
    ("error_denominator", False),
    ("p99_latency_ms", False),
    ("histogram_or_sketch_hash", False),
    ("ttft_histogram_hash", True),
    ("latency_histogram_hash", True),
    ("ttft_histogram", True),
    ("latency_histogram", True),
    ("source_manifest", True),
    ("error_rate", True),
    ("cost_amount", True),
    ("ongoing_confirmed_p0_incident", False),
)

_RFC3339_NANO_RE = re.compile(
    r"^(?P<date>\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})"
    r"(?:\.(?P<fraction>\d{1,9}))?(?P<timezone>Z|[+-]\d{2}:\d{2})$"
)


def go_json(value: object) -> bytes:
    """Encode JSON with the compact UTF-8 and HTML escaping used by encoding/json."""

    encoded = json.dumps(value, ensure_ascii=False, separators=(",", ":"), allow_nan=False)
    return (
        encoded.replace("<", "\\u003c")
        .replace(">", "\\u003e")
        .replace("&", "\\u0026")
        .replace("\u2028", "\\u2028")
        .replace("\u2029", "\\u2029")
        .encode("utf-8")
    )


def rfc3339_nano(value: Any, path: str) -> str:
    """Normalize RFC3339 to Go's UTC RFC3339Nano representation.

    Python's datetime parser truncates fractions beyond microseconds. Parsing the
    date, timezone, and nine-digit fraction separately preserves the timestamp
    precision that participates in the Go snapshot hash.
    """

    if not isinstance(value, str):
        raise ValueError(f"{path} must be an RFC3339 timestamp")
    match = _RFC3339_NANO_RE.fullmatch(value)
    if match is None:
        raise ValueError(f"{path} must be an RFC3339 timestamp")
    timezone = match.group("timezone")
    normalized_timezone = "+00:00" if timezone == "Z" else timezone
    try:
        parsed = datetime.fromisoformat(match.group("date") + normalized_timezone)
    except ValueError as exc:
        raise ValueError(f"{path} must be an RFC3339 timestamp") from exc
    if parsed.tzinfo is None:
        raise ValueError(f"{path} must include a timezone")
    normalized = parsed.astimezone(UTC)
    fraction = (match.group("fraction") or "").ljust(9, "0").rstrip("0")
    suffix = f".{fraction}" if fraction else ""
    return normalized.strftime("%Y-%m-%dT%H:%M:%S") + suffix + "Z"


def canonical_metrics(value: Any, path: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise ValueError(f"{path} must be an object")
    encoded: dict[str, Any] = {}
    for key, omitempty in METRIC_FIELDS:
        if key not in value:
            continue
        item = value[key]
        if omitempty and (item is None or item == 0 or item == "" or item is False):
            continue
        encoded[key] = item
    return encoded


def snapshot_hash(snapshot: dict[str, Any]) -> str:
    try:
        run_id = str(UUID(str(snapshot["run_id"])))
        profile_id = snapshot["profile_id"]
        slice_key = snapshot["slice_key"]
        query_version = snapshot["query_version"]
        source_hash = snapshot["source_hash"]
        if not isinstance(profile_id, str) or not profile_id:
            raise ValueError("snapshot.profile_id must be a non-empty string")
        if not isinstance(slice_key, str) or not slice_key:
            raise ValueError("snapshot.slice_key must be a non-empty string")
        if not isinstance(query_version, str) or not query_version:
            raise ValueError("snapshot.query_version must be a non-empty string")
        if not isinstance(source_hash, str) or re.fullmatch(r"[0-9a-f]{64}", source_hash) is None:
            raise ValueError("snapshot.source_hash must be a lowercase SHA256")
        outer = {
            "run_id": run_id,
            "reliability_profile_id": profile_id,
            "slice_key": slice_key,
            "window_start": rfc3339_nano(snapshot["window_start"], "snapshot.window_start"),
            "window_end": rfc3339_nano(snapshot["window_end"], "snapshot.window_end"),
            "query_version": query_version,
            "source_hash": source_hash,
            "metrics": canonical_metrics(snapshot["metrics"], "snapshot.metrics"),
            "fresh_until": rfc3339_nano(snapshot["fresh_until"], "snapshot.fresh_until"),
        }
        return hashlib.sha256(go_json(outer)).hexdigest()
    except KeyError as exc:
        raise ValueError(f"snapshot.{exc.args[0]} is required") from exc
