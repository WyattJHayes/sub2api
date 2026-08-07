#!/usr/bin/env python3
"""Build a Radar production promotion audit manifest from recorded gate outputs."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from production_promotion_audit import INPUT_SCHEMA_VERSION


REQUIRED_CONFIG_HASH_KEYS = {
    "docker-compose.yml": ("docker-compose.yml",),
    "docker-compose.override.yml": ("docker-compose.override.yml",),
    ".env": (".env",),
    "data/config.yaml": ("data/config.yaml", "config.yaml"),
}


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
    parser.add_argument("--preflight-result", type=Path, required=True)
    parser.add_argument("--target-snapshot", type=Path, required=True)
    parser.add_argument("--candidate-control-plane-digest", required=True)
    parser.add_argument("--candidate-worker-digest", required=True)
    parser.add_argument("--staging-gate-ok", action="store_true")
    parser.add_argument("--migration-rehearsal-ok", action="store_true")
    parser.add_argument("--production-backup-path", default="")
    parser.add_argument("--production-backup-sha256", default="")
    parser.add_argument("--production-backup-restore-verified", action="store_true")
    parser.add_argument("--active-control-plane-digest", default="")
    parser.add_argument("--active-worker-digest", default="")
    parser.add_argument("--rollback-control-plane-digest", default="")
    parser.add_argument("--rollback-worker-digest", default="")
    parser.add_argument("--rollback-control-plane-available", action="store_true")
    parser.add_argument("--rollback-worker-available", action="store_true")
    parser.add_argument("--accepted-candidate-restoration-planned", action="store_true")
    parser.add_argument("--output", type=Path, help="write JSON manifest to this path")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv or sys.argv[1:])
    try:
        manifest = build_manifest(
            candidate_control_plane_digest=args.candidate_control_plane_digest,
            candidate_worker_digest=args.candidate_worker_digest,
            staging_gate_ok=args.staging_gate_ok,
            migration_rehearsal_ok=args.migration_rehearsal_ok,
            production_preflight=read_json(args.preflight_result),
            target_snapshot=read_json(args.target_snapshot),
            production_backup_path=args.production_backup_path,
            production_backup_sha256=args.production_backup_sha256,
            production_backup_restore_verified=args.production_backup_restore_verified,
            active_control_plane_digest=args.active_control_plane_digest,
            active_worker_digest=args.active_worker_digest,
            rollback_control_plane_digest=args.rollback_control_plane_digest,
            rollback_worker_digest=args.rollback_worker_digest,
            rollback_control_plane_available=args.rollback_control_plane_available,
            rollback_worker_available=args.rollback_worker_available,
            accepted_candidate_restoration_planned=args.accepted_candidate_restoration_planned,
        )
        emit_json(manifest, args.output)
        return 0
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        print(f"FAIL production promotion manifest: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
