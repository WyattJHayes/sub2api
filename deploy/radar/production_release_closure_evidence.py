#!/usr/bin/env python3
"""Build final Radar production release closure evidence from gate outputs."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from production_release_closure_audit import INPUT_SCHEMA_VERSION
from production_evidence_envelope import (
    file_sha256,
    load_candidate_image_record,
    load_private_envelope,
    write_private_json,
)

BOUND_CLOSURE_SCHEMA_VERSION = "radar-production-release-closure-v3"


def build_closure_evidence(
    *,
    accepted_candidate_control_plane_digest: str,
    accepted_candidate_worker_digest: str,
    production_authorization_audit: dict[str, Any] | None = None,
    production_target_preflight: dict[str, Any] | None = None,
    production_backup_audit: dict[str, Any] | None = None,
    production_promotion_audit: dict[str, Any] | None = None,
    production_smoke_audit: dict[str, Any] | None = None,
    production_rollback_audit: dict[str, Any] | None = None,
    production_promotion_executed: bool = False,
    rollback_drill_executed: bool = False,
) -> dict[str, Any]:
    return {
        "schema_version": INPUT_SCHEMA_VERSION,
        "accepted_candidate": {
            "control_plane_digest": accepted_candidate_control_plane_digest,
            "worker_digest": accepted_candidate_worker_digest,
        },
        "production_authorization_audit": _mapping_or_empty(production_authorization_audit),
        "production_target_preflight": _mapping_or_empty(production_target_preflight),
        "production_backup_audit": _mapping_or_empty(production_backup_audit),
        "production_promotion_audit": _mapping_or_empty(production_promotion_audit),
        "production_promotion_executed": production_promotion_executed is True,
        "production_smoke_audit": _mapping_or_empty(production_smoke_audit),
        "rollback_drill_executed": rollback_drill_executed is True,
        "production_rollback_audit": _mapping_or_empty(production_rollback_audit),
    }


def _mapping_or_empty(value: object) -> dict[str, Any]:
    return value if isinstance(value, dict) else {}


def build_bound_closure(
    *,
    release_id: str,
    candidate_image_record: Path,
    isolation: Path,
    promotion: Path,
    smoke: Path,
    rollback: Path,
    restoration: Path,
    output: Path | None = None,
) -> dict[str, Any]:
    candidate = load_candidate_image_record(candidate_image_record)
    paths = {
        "isolation": (isolation, "isolation"),
        "promotion": (promotion, "promotion"),
        "smoke": (smoke, "smoke"),
        "rollback": (rollback, "rollback"),
        "restoration": (restoration, "restoration"),
    }
    envelopes: dict[str, dict[str, Any]] = {}
    for name, (path, kind) in paths.items():
        envelope = load_private_envelope(path, expected_type=kind)
        if envelope["release_id"] != release_id:
            raise ValueError(f"{name} release_id does not match closure")
        envelopes[name] = envelope
    binding = envelopes["promotion"]["binding"]
    for name, envelope in envelopes.items():
        if envelope["binding"] != binding:
            raise ValueError(f"{name} binding does not match closure")
    refs: dict[str, dict[str, str]] = {
        "candidate_image_record": {
            "path": str(candidate_image_record),
            "file_sha256": file_sha256(candidate_image_record),
        }
    }
    for name, (path, _) in paths.items():
        refs[name] = {
            "path": str(path),
            "file_sha256": file_sha256(path),
            "evidence_sha256": envelopes[name]["evidence_sha256"],
        }
    document = {
        "schema_version": BOUND_CLOSURE_SCHEMA_VERSION,
        "release_id": release_id,
        "binding": binding,
        "candidate": {
            "source_sha256": candidate["source_sha256"],
            "control_plane_manifest_digest": candidate["control_plane"]["manifest_digest"],
            "worker_manifest_digest": candidate["worker"]["manifest_digest"],
        },
        "evidence": refs,
    }
    if output is not None:
        write_private_json(output, document)
    return document


def read_json(path: Path | None) -> dict[str, Any]:
    if path is None:
        return {}
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
        description="Build final Radar production release closure evidence."
    )
    parser.add_argument("--release-id", required=True)
    parser.add_argument("--candidate-image-record", type=Path, required=True)
    parser.add_argument("--isolation", type=Path, required=True)
    parser.add_argument("--promotion", type=Path, required=True)
    parser.add_argument("--smoke", type=Path, required=True)
    parser.add_argument("--rollback", type=Path, required=True)
    parser.add_argument("--restoration", type=Path, required=True)
    parser.add_argument("--output", type=Path, help="write JSON closure evidence to this path")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv or sys.argv[1:])
    try:
        document = build_bound_closure(
            release_id=args.release_id,
            candidate_image_record=args.candidate_image_record,
            isolation=args.isolation,
            promotion=args.promotion,
            smoke=args.smoke,
            rollback=args.rollback,
            restoration=args.restoration,
            output=args.output,
        )
        if args.output is None:
            raise ValueError("--output is required for private closure evidence")
        return 0
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        print(f"FAIL production release closure evidence: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
