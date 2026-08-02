#!/usr/bin/env python3
"""Build final Radar production release closure evidence from gate outputs."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from production_release_closure_audit import INPUT_SCHEMA_VERSION


def build_closure_evidence(
    *,
    accepted_candidate_digest: str,
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
        "accepted_candidate_digest": accepted_candidate_digest,
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
    parser.add_argument("--accepted-candidate-digest", required=True)
    parser.add_argument("--production-target-preflight", type=Path)
    parser.add_argument("--production-backup-audit", type=Path)
    parser.add_argument("--production-promotion-audit", type=Path)
    parser.add_argument("--production-smoke-audit", type=Path)
    parser.add_argument("--production-rollback-audit", type=Path)
    parser.add_argument("--production-promotion-executed", action="store_true")
    parser.add_argument("--rollback-drill-executed", action="store_true")
    parser.add_argument("--output", type=Path, help="write JSON closure evidence to this path")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv or sys.argv[1:])
    try:
        document = build_closure_evidence(
            accepted_candidate_digest=args.accepted_candidate_digest,
            production_target_preflight=read_json(args.production_target_preflight),
            production_backup_audit=read_json(args.production_backup_audit),
            production_promotion_audit=read_json(args.production_promotion_audit),
            production_smoke_audit=read_json(args.production_smoke_audit),
            production_rollback_audit=read_json(args.production_rollback_audit),
            production_promotion_executed=args.production_promotion_executed,
            rollback_drill_executed=args.rollback_drill_executed,
        )
        emit_json(document, args.output)
        return 0
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        print(f"FAIL production release closure evidence: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
