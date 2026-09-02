#!/usr/bin/env python3
"""Fail-closed production promotion input audit for Radar release gates."""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from production_evidence_envelope import (
    file_sha256,
    load_candidate_image_record,
    load_private_envelope,
    load_private_json,
)
from migration_ledger import expected_schema_migrations


INPUT_SCHEMA_VERSION = "radar-production-promotion-audit-input-v2"
OUTPUT_SCHEMA_VERSION = "radar-production-promotion-audit-v2"

IMAGE_DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
BOUND_MANIFEST_SCHEMA_VERSION = "radar-production-promotion-manifest-v3"
MIGRATION_MANIFEST_DIR = Path(
    os.environ.get(
        "RADAR_MIGRATION_MANIFEST_DIR",
        str(Path(__file__).resolve().parent / "manifests" / "v0.2.0"),
    )
).resolve()
EXPECTED_SCHEMA_MIGRATIONS = expected_schema_migrations(MIGRATION_MANIFEST_DIR)


def utc_now() -> str:
    return datetime.now(UTC).isoformat(timespec="seconds").replace("+00:00", "Z")


def audit_manifest(manifest: dict[str, Any]) -> dict[str, Any]:
    if manifest.get("schema_version") == BOUND_MANIFEST_SCHEMA_VERSION:
        return audit_bound_manifest(manifest)
    checks: list[dict[str, Any]] = []
    blockers: list[str] = []

    candidate = _mapping(manifest.get("candidate"))
    production_preflight = _mapping(manifest.get("production_preflight"))
    production_backup = _mapping(manifest.get("production_backup"))
    production_active = _mapping(manifest.get("production_active"))
    rollback = _mapping(manifest.get("rollback"))
    post_rollback = _mapping(manifest.get("post_rollback"))

    candidate_control_plane_digest = str(candidate.get("control_plane_digest") or "")
    _add_check(
        checks,
        blockers,
        "candidate_control_plane_digest",
        _valid_image_digest(candidate_control_plane_digest),
        "candidate control-plane digest must be sha256:<64 lowercase hex>",
        value=candidate_control_plane_digest or None,
    )
    candidate_worker_digest = str(candidate.get("worker_digest") or "")
    _add_check(
        checks,
        blockers,
        "candidate_worker_digest",
        _valid_image_digest(candidate_worker_digest),
        "candidate Worker digest must be sha256:<64 lowercase hex>",
        value=candidate_worker_digest or None,
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

    active_control_plane_digest = str(production_active.get("control_plane_digest") or "")
    _add_check(
        checks,
        blockers,
        "production_active_control_plane_digest",
        _valid_image_digest(active_control_plane_digest),
        "active production control-plane digest must be sha256:<64 lowercase hex>",
        value=active_control_plane_digest or None,
    )
    active_worker_digest = str(production_active.get("worker_digest") or "")
    _add_check(
        checks,
        blockers,
        "production_active_worker_digest",
        _valid_image_digest(active_worker_digest),
        "active production Worker digest must be sha256:<64 lowercase hex>",
        value=active_worker_digest or None,
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

    rollback_control_plane_digest = str(rollback.get("control_plane_digest") or "")
    _add_check(
        checks,
        blockers,
        "rollback_control_plane_digest",
        _valid_image_digest(rollback_control_plane_digest),
        "rollback control-plane digest must be sha256:<64 lowercase hex>",
        value=rollback_control_plane_digest or None,
    )
    rollback_worker_digest = str(rollback.get("worker_digest") or "")
    _add_check(
        checks,
        blockers,
        "rollback_worker_digest",
        _valid_image_digest(rollback_worker_digest),
        "rollback Worker digest must be sha256:<64 lowercase hex>",
        value=rollback_worker_digest or None,
    )
    _add_bool_check(
        checks,
        blockers,
        rollback,
        "rollback_control_plane_available",
        source_key="control_plane_available",
    )
    _add_bool_check(
        checks,
        blockers,
        rollback,
        "rollback_worker_available",
        source_key="worker_available",
    )
    _add_check(
        checks,
        blockers,
        "rollback_control_plane_digest_distinct_from_candidate",
        bool(
            rollback_control_plane_digest
            and candidate_control_plane_digest
            and rollback_control_plane_digest != candidate_control_plane_digest
        ),
        "rollback control-plane digest must be distinct from the candidate",
    )
    _add_check(
        checks,
        blockers,
        "rollback_worker_digest_distinct_from_candidate",
        bool(
            rollback_worker_digest
            and candidate_worker_digest
            and rollback_worker_digest != candidate_worker_digest
        ),
        "rollback Worker digest must be distinct from the candidate",
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
            "candidate_control_plane_digest": candidate_control_plane_digest,
            "candidate_worker_digest": candidate_worker_digest,
            "rollback_control_plane_digest": rollback_control_plane_digest,
            "rollback_worker_digest": rollback_worker_digest,
            "production_active_control_plane_digest": active_control_plane_digest,
            "production_active_worker_digest": active_worker_digest,
            "production_backup_sha256": backup_sha,
            "production_preflight_ok": preflight_ok,
        },
    }


def audit_bound_manifest(manifest: dict[str, Any]) -> dict[str, Any]:
    """Validate a path-only promotion manifest and derive all digests from it."""
    blockers: list[str] = []
    checks: list[dict[str, Any]] = []
    refs = manifest.get("evidence")
    binding = manifest.get("binding")
    release_id = str(manifest.get("release_id") or "")
    if not isinstance(refs, dict) or not release_id or not isinstance(binding, dict):
        return _bound_result(False, ["manifest_schema"], checks)
    expected = {
        "isolation": "isolation",
        "migration_closure": "migration-rehearsal",
        "browser_closure": "browser-closure",
        "preflight": "preflight",
        "target_snapshot": "target-snapshot",
        "authorization": "authorization",
        "backup": "backup",
    }
    envelopes: dict[str, dict[str, Any]] = {}
    candidate_path = _reference_path(refs, "candidate_image_record", blockers)
    candidate = None
    if candidate_path is not None:
        try:
            candidate = load_candidate_image_record(candidate_path)
            _verify_reference(refs["candidate_image_record"], candidate_path, None, blockers)
        except (OSError, ValueError, json.JSONDecodeError) as exc:
            blockers.append(f"candidate_image_record:{type(exc).__name__}")
    for name, evidence_type in expected.items():
        path = _reference_path(refs, name, blockers)
        if path is None:
            continue
        try:
            envelope = load_private_envelope(path, expected_type=evidence_type)
            _verify_reference(refs[name], path, envelope, blockers)
            envelopes[name] = envelope
        except (OSError, ValueError, json.JSONDecodeError) as exc:
            blockers.append(f"{name}:{type(exc).__name__}")
    if candidate is None:
        blockers.append("candidate_image_record")
    for name, envelope in envelopes.items():
        if envelope.get("release_id") != release_id:
            blockers.append(f"{name}_release_id")
        if envelope.get("binding") != binding:
            blockers.append(f"{name}_binding")
    if candidate is not None and envelopes.get("target_snapshot"):
        candidate_hash = str(refs["candidate_image_record"].get("file_sha256") or "")
        if binding.get("candidate_image_record_sha256") != candidate_hash:
            blockers.append("candidate_image_record_binding")
    preflight = envelopes.get("preflight")
    isolation = envelopes.get("isolation")
    migration = envelopes.get("migration_closure")
    browser = envelopes.get("browser_closure")
    authorization = envelopes.get("authorization")
    backup = envelopes.get("backup")
    if not preflight or not preflight.get("payload", {}).get("promotion_ready", True):
        blockers.append("preflight_not_ready")
    if (
        not isolation
        or not migration
        or _int_value((migration or {}).get("payload", {}).get("migration_count"))
        != EXPECTED_SCHEMA_MIGRATIONS
    ):
        blockers.append("isolation_or_migration")
    if not browser or not browser.get("payload", {}).get("passed", True):
        blockers.append("browser_closure")
    if not authorization or not backup:
        blockers.append("authorization_or_backup")
    if authorization and backup:
        if backup.get("input_evidence_sha256", {}).get("authorization") != authorization.get("evidence_sha256"):
            blockers.append("backup_authorization_predecessor")
    candidate_cp = str((candidate or {}).get("control_plane", {}).get("manifest_digest") or "")
    candidate_worker = str((candidate or {}).get("worker", {}).get("manifest_digest") or "")
    derived = manifest.get("derived") if isinstance(manifest.get("derived"), dict) else {}
    rollback_cp = str(derived.get("rollback_control_plane_repo_digest") or "")
    rollback_worker = str(derived.get("rollback_worker_repo_digest") or "")
    if not _valid_image_digest(candidate_cp):
        blockers.append("candidate_control_plane_digest")
    if not _valid_image_digest(candidate_worker):
        blockers.append("candidate_worker_digest")
    if not _valid_image_digest(rollback_cp) or rollback_cp == candidate_cp:
        blockers.append("rollback_control_plane_repo_digest")
    if not _valid_image_digest(rollback_worker) or rollback_worker == candidate_worker:
        blockers.append("rollback_worker_repo_digest")
    return _bound_result(not blockers, blockers, checks, candidate_cp, candidate_worker, rollback_cp, rollback_worker, release_id)


def _reference_path(refs: dict[str, Any], name: str, blockers: list[str]) -> Path | None:
    value = refs.get(name)
    if not isinstance(value, dict) or not isinstance(value.get("path"), str):
        blockers.append(f"{name}_reference")
        return None
    return Path(value["path"])


def _verify_reference(reference: dict[str, Any], path: Path, envelope: dict[str, Any] | None, blockers: list[str]) -> None:
    if file_sha256(path) != reference.get("file_sha256"):
        blockers.append(f"{path.name}_file_sha256")
    if envelope is not None and envelope.get("evidence_sha256") != reference.get("evidence_sha256"):
        blockers.append(f"{path.name}_evidence_sha256")


def _bound_result(
    ok: bool,
    blockers: list[str],
    checks: list[dict[str, Any]],
    candidate_cp: str = "",
    candidate_worker: str = "",
    rollback_cp: str = "",
    rollback_worker: str = "",
    release_id: str = "",
) -> dict[str, Any]:
    return {
        "schema_version": "radar-production-promotion-audit-v3",
        "checked_at": utc_now(),
        "ok": ok,
        "promotion_ready": ok,
        "checks": checks,
        "blockers": list(dict.fromkeys(blockers)),
        "summary": {
            "release_id": release_id,
            "candidate_control_plane_digest": candidate_cp,
            "candidate_worker_digest": candidate_worker,
            "rollback_control_plane_digest": rollback_cp,
            "rollback_worker_digest": rollback_worker,
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


def _int_value(value: object) -> int:
    if isinstance(value, bool):
        return 0
    if isinstance(value, int):
        return value
    try:
        return int(str(value))
    except (TypeError, ValueError):
        return 0


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
