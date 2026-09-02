#!/usr/bin/env python3
"""Build a Radar production promotion audit manifest from recorded gate outputs."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from production_promotion_audit import INPUT_SCHEMA_VERSION
from production_evidence_envelope import (
    ENVELOPE_SCHEMA_VERSION,
    file_sha256,
    load_candidate_image_record,
    load_private_envelope,
    write_private_json,
)


REQUIRED_CONFIG_HASH_KEYS = {
    "docker-compose.yml": ("docker-compose.yml",),
    "docker-compose.override.yml": ("docker-compose.override.yml",),
    ".env": (".env",),
    "data/config.yaml": ("data/config.yaml", "config.yaml"),
}

BOUND_MANIFEST_SCHEMA_VERSION = "radar-production-promotion-manifest-v3"


def build_manifest(
    *,
    candidate_control_plane_digest: str,
    candidate_worker_digest: str,
    staging_gate_ok: bool,
    migration_rehearsal_ok: bool,
    production_preflight: dict[str, Any],
    target_snapshot: dict[str, Any],
    production_backup_path: str = "",
    production_backup_sha256: str = "",
    production_backup_restore_verified: bool = False,
    active_control_plane_digest: str = "",
    active_worker_digest: str = "",
    rollback_control_plane_digest: str = "",
    rollback_worker_digest: str = "",
    rollback_control_plane_available: bool = False,
    rollback_worker_available: bool = False,
    accepted_candidate_restoration_planned: bool = False,
) -> dict[str, Any]:
    return {
        "schema_version": INPUT_SCHEMA_VERSION,
        "candidate": {
            "control_plane_digest": candidate_control_plane_digest,
            "worker_digest": candidate_worker_digest,
            "staging_gate_ok": staging_gate_ok,
            "migration_rehearsal_ok": migration_rehearsal_ok,
        },
        "production_preflight": {
            "ok": production_preflight.get("ok") is True,
            "promotion_ready": production_preflight.get("promotion_ready") is True,
            "production_exposure_event": production_preflight.get("production_exposure_event")
            is True,
            "blockers": _list_of_strings(production_preflight.get("blockers")),
        },
        "production_backup": {
            "path": production_backup_path,
            "sha256": production_backup_sha256,
            "restore_verified": production_backup_restore_verified,
        },
        "production_active": {
            "control_plane_digest": active_control_plane_digest,
            "worker_digest": active_worker_digest,
            "config_hashes": extract_config_hashes(target_snapshot),
        },
        "rollback": {
            "control_plane_digest": rollback_control_plane_digest,
            "worker_digest": rollback_worker_digest,
            "control_plane_available": rollback_control_plane_available,
            "worker_available": rollback_worker_available,
        },
        "post_rollback": {
            "accepted_candidate_restoration_planned": accepted_candidate_restoration_planned,
        },
    }


def extract_config_hashes(target_snapshot: dict[str, Any]) -> dict[str, str]:
    hashes = target_snapshot.get("hashes")
    if not isinstance(hashes, dict):
        return {}
    result: dict[str, str] = {}
    for output_key, suffixes in REQUIRED_CONFIG_HASH_KEYS.items():
        value = _find_hash_by_suffix(hashes, suffixes)
        if value:
            result[output_key] = value
    return result


def _find_hash_by_suffix(hashes: dict[object, object], suffixes: tuple[str, ...]) -> str:
    for raw_path, raw_value in hashes.items():
        path = str(raw_path)
        for suffix in suffixes:
            if path == suffix or path.endswith(f"/{suffix}"):
                return str(raw_value)
    return ""


def _list_of_strings(value: object) -> list[str]:
    if not isinstance(value, list):
        return []
    return [str(item) for item in value]


def build_manifest_from_paths(
    *,
    release_id: str,
    candidate_image_record: Path,
    isolation: Path,
    migration_closure: Path,
    browser_closure: Path,
    preflight: Path,
    target_snapshot: Path,
    authorization: Path,
    backup: Path,
    output: Path | None = None,
) -> dict[str, Any]:
    """Build a promotion manifest from private evidence paths only."""
    candidate = load_candidate_image_record(candidate_image_record)
    envelopes = {
        "isolation": load_private_envelope(isolation, expected_type="isolation"),
        "migration_closure": load_private_envelope(migration_closure, expected_type="migration-rehearsal"),
        "browser_closure": load_private_envelope(browser_closure, expected_type="browser-closure"),
        "preflight": load_private_envelope(preflight, expected_type="preflight"),
        "target_snapshot": load_private_envelope(target_snapshot, expected_type="target-snapshot"),
        "authorization": load_private_envelope(authorization, expected_type="authorization"),
        "backup": load_private_envelope(backup, expected_type="backup"),
    }
    for name, envelope in envelopes.items():
        if envelope["release_id"] != release_id:
            raise ValueError(f"{name} release_id does not match manifest")
    candidate_hash = file_sha256(candidate_image_record)
    target_binding = envelopes["target_snapshot"]["binding"]
    if target_binding["candidate_image_record_sha256"] != candidate_hash:
        raise ValueError("candidate image record hash does not match target snapshot")
    for name in ("isolation", "migration_closure", "browser_closure", "preflight", "authorization", "backup"):
        if envelopes[name]["binding"] != target_binding:
            raise ValueError(f"{name} binding does not match target snapshot")
    refs: dict[str, dict[str, str]] = {
        "candidate_image_record": {
            "path": str(candidate_image_record),
            "file_sha256": candidate_hash,
        }
    }
    for name, path in (
        ("isolation", isolation),
        ("migration_closure", migration_closure),
        ("browser_closure", browser_closure),
        ("preflight", preflight),
        ("target_snapshot", target_snapshot),
        ("authorization", authorization),
        ("backup", backup),
    ):
        refs[name] = {
            "path": str(path),
            "file_sha256": file_sha256(path),
            "evidence_sha256": envelopes[name]["evidence_sha256"],
        }
    rollback = _runtime_digests(envelopes["target_snapshot"]["payload"])
    manifest = {
        "schema_version": BOUND_MANIFEST_SCHEMA_VERSION,
        "release_id": release_id,
        "binding": target_binding,
        "candidate": {
            "version": candidate["version"],
            "platform": candidate["platform"],
            "source_sha256": candidate["source_sha256"],
            "control_plane": candidate["control_plane"],
            "worker": candidate["worker"],
        },
        "evidence": refs,
        "derived": {
            "rollback_control_plane_repo_digest": rollback.get("control_plane", ""),
            "rollback_worker_repo_digest": rollback.get("worker", ""),
        },
    }
    if output is not None:
        write_private_json(output, manifest)
    return manifest


def _runtime_digests(payload: dict[str, Any]) -> dict[str, str]:
    result: dict[str, str] = {}
    containers = payload.get("production_containers") or payload.get("containers")
    if not isinstance(containers, list):
        direct = payload.get("repo_digests")
        return {str(k): str(v) for k, v in direct.items()} if isinstance(direct, dict) else result
    for item in containers:
        if not isinstance(item, dict):
            continue
        service = str(item.get("service") or item.get("name") or "").lower()
        values = item.get("repo_digests") or item.get("RepoDigests")
        if isinstance(values, str):
            values = [values]
        digest = ""
        if isinstance(values, list):
            digest = next((str(v).split("@", 1)[-1] for v in values if "@sha256:" in str(v)), "")
        if not digest:
            configured_image = str(item.get("configured_image") or item.get("ConfigImage") or "")
            if "@sha256:" in configured_image:
                digest = configured_image.split("@", 1)[-1]
        if digest and any(token in service for token in ("control", "sub2api", "app")):
            result["control_plane"] = digest
        if digest and ("worker" in service or any(token in service for token in ("runner", "grader", "statistics"))):
            result["worker"] = digest
    return result


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
        description="Build a Radar production promotion audit manifest."
    )
    parser.add_argument("--release-id", required=True)
    parser.add_argument("--candidate-image-record", type=Path, required=True)
    parser.add_argument("--isolation", type=Path, required=True)
    parser.add_argument("--migration-closure", type=Path, required=True)
    parser.add_argument("--browser-closure", type=Path, required=True)
    parser.add_argument("--preflight", type=Path, required=True)
    parser.add_argument("--target-snapshot", type=Path, required=True)
    parser.add_argument("--authorization", type=Path, required=True)
    parser.add_argument("--backup", type=Path, required=True)
    parser.add_argument("--output", type=Path, help="write JSON manifest to this path")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv or sys.argv[1:])
    try:
        manifest = build_manifest_from_paths(
            release_id=args.release_id,
            candidate_image_record=args.candidate_image_record,
            isolation=args.isolation,
            migration_closure=args.migration_closure,
            browser_closure=args.browser_closure,
            preflight=args.preflight,
            target_snapshot=args.target_snapshot,
            authorization=args.authorization,
            backup=args.backup,
            output=args.output,
        )
        if args.output is None:
            raise ValueError("--output is required for private promotion manifest")
        return 0
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        print(f"FAIL production promotion manifest: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
