#!/usr/bin/env python3
"""Verify immutable Radar facts fetched from the control-plane API."""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path
from typing import Any
from urllib.parse import parse_qsl, urlencode, urlparse, urlunparse
from urllib.request import Request, urlopen
from uuid import UUID

from reliability_hash import canonical_metrics as canonical_metrics_json
from reliability_hash import rfc3339_nano as normalize_rfc3339_nano
from reliability_hash import snapshot_hash as compute_snapshot_hash

SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
FACTS_SCHEMA_VERSION = "radar-reliability-facts-v1"

class FactVerificationError(Exception):
    pass


def parse_uuid(value: Any, path: str) -> UUID:
    if not isinstance(value, str):
        raise FactVerificationError(f"{path} must be a UUID")
    try:
        return UUID(value)
    except ValueError as exc:
        raise FactVerificationError(f"{path} must be a UUID") from exc


def sha256(value: Any, path: str) -> str:
    if not isinstance(value, str) or SHA256_RE.fullmatch(value) is None:
        raise FactVerificationError(f"{path} must be a lowercase SHA256")
    return value


def rfc3339_nano(value: Any, path: str) -> str:
    try:
        return normalize_rfc3339_nano(value, path)
    except ValueError as exc:
        raise FactVerificationError(str(exc)) from exc


def canonical_metrics(value: Any, path: str) -> bytes:
    try:
        return json.dumps(
            canonical_metrics_json(value, path), ensure_ascii=False, separators=(",", ":")
        ).encode("utf-8")
    except ValueError as exc:
        raise FactVerificationError(str(exc)) from exc


def snapshot_hash(snapshot: dict[str, Any]) -> str:
    try:
        return compute_snapshot_hash(snapshot)
    except (KeyError, TypeError, ValueError) as exc:
        raise FactVerificationError(str(exc)) from exc


def fetch_facts(url: str, headers: dict[str, str], timeout: float) -> dict[str, Any]:
    request = Request(url, headers=headers, method="GET")
    try:
        with urlopen(request, timeout=timeout) as response:
            if response.status < 200 or response.status >= 300:
                raise FactVerificationError(f"facts endpoint returned HTTP {response.status}")
            payload = json.load(response)
    except FactVerificationError:
        raise
    except Exception as exc:  # noqa: BLE001
        raise FactVerificationError(f"facts endpoint request failed: {exc}") from exc
    if isinstance(payload, dict) and isinstance(payload.get("data"), dict):
        payload = payload["data"]
    if not isinstance(payload, dict):
        raise FactVerificationError("facts endpoint response must be an object")
    return payload


def verify_facts(facts: dict[str, Any], run_id: UUID, policy_id: UUID, profile_id: str) -> None:
    if facts.get("schema_version") != FACTS_SCHEMA_VERSION:
        raise FactVerificationError("facts.schema_version is unsupported")
    if parse_uuid(facts.get("run_id"), "facts.run_id") != run_id:
        raise FactVerificationError("facts.run_id does not match the requested run")
    if parse_uuid(facts.get("policy_id"), "facts.policy_id") != policy_id:
        raise FactVerificationError("facts.policy_id does not match the requested policy")
    if facts.get("profile_id") != profile_id:
        raise FactVerificationError("facts.profile_id does not match the requested profile")
    load_plan_id = parse_uuid(facts.get("load_plan_id"), "facts.load_plan_id")
    sha256(facts.get("load_plan_sha256"), "facts.load_plan_sha256")
    sha256(facts.get("policy_hash"), "facts.policy_hash")
    parse_uuid(facts.get("release_subject_id"), "facts.release_subject_id")
    sha256(facts.get("release_subject_hash"), "facts.release_subject_hash")

    snapshots = facts.get("snapshots")
    if not isinstance(snapshots, list) or not snapshots:
        raise FactVerificationError("facts.snapshots must be a non-empty list")
    seen: set[UUID] = set()
    for index, snapshot in enumerate(snapshots):
        path = f"facts.snapshots[{index}]"
        if not isinstance(snapshot, dict):
            raise FactVerificationError(f"{path} must be an object")
        snapshot_id = parse_uuid(snapshot.get("snapshot_id"), f"{path}.snapshot_id")
        if snapshot_id in seen:
            raise FactVerificationError(f"{path}.snapshot_id is duplicated")
        seen.add(snapshot_id)
        if parse_uuid(snapshot.get("run_id"), f"{path}.run_id") != run_id:
            raise FactVerificationError(f"{path}.run_id does not match the requested run")
        if parse_uuid(snapshot.get("load_plan_id"), f"{path}.load_plan_id") != load_plan_id:
            raise FactVerificationError(f"{path}.load_plan_id does not match facts.load_plan_id")
        if snapshot.get("profile_id") != profile_id:
            raise FactVerificationError(f"{path}.profile_id does not match the requested profile")
        if not isinstance(snapshot.get("slice_key"), str) or not snapshot["slice_key"].strip():
            raise FactVerificationError(f"{path}.slice_key must be a non-empty string")
        if not isinstance(snapshot.get("query_version"), str) or not snapshot["query_version"].strip():
            raise FactVerificationError(f"{path}.query_version must be a non-empty string")
        expected_hash = snapshot_hash(snapshot)
        if sha256(snapshot.get("snapshot_hash"), f"{path}.snapshot_hash") != expected_hash:
            raise FactVerificationError(f"{path}.snapshot_hash does not recompute from immutable fields")
        sha256(snapshot.get("source_watermark"), f"{path}.source_watermark")

    recovery = facts.get("recovery")
    if not isinstance(recovery, dict):
        raise FactVerificationError("facts.recovery is required")
    parse_uuid(recovery.get("evidence_id"), "facts.recovery.evidence_id")
    parse_uuid(recovery.get("run_id"), "facts.recovery.run_id")
    parse_uuid(recovery.get("experiment_id"), "facts.recovery.experiment_id")
    if parse_uuid(recovery.get("run_id"), "facts.recovery.run_id") != run_id:
        raise FactVerificationError("facts.recovery.run_id does not match the requested run")
    sha256(recovery.get("evidence_hash"), "facts.recovery.evidence_hash")
    sha256(recovery.get("source_watermark"), "facts.recovery.source_watermark")
    if not isinstance(recovery.get("recovery_generation"), int) or recovery["recovery_generation"] < 0:
        raise FactVerificationError("facts.recovery.recovery_generation is invalid")

    artifacts = facts.get("artifact_manifest_hashes")
    if not isinstance(artifacts, list) or not artifacts:
        raise FactVerificationError("facts.artifact_manifest_hashes must be a non-empty list")
    for index, artifact in enumerate(artifacts):
        sha256(artifact, f"facts.artifact_manifest_hashes[{index}]")


def compare_acceptance_manifest(path: Path, facts: dict[str, Any]) -> None:
    try:
        document = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise FactVerificationError(f"cannot read acceptance evidence: {exc}") from exc
    manifest = document.get("fact_manifest") if isinstance(document, dict) else None
    if not isinstance(manifest, dict):
        raise FactVerificationError("acceptance evidence has no fact_manifest")
    for key in ("run_id", "load_plan_id", "profile_id", "policy_id", "policy_hash", "release_subject_id", "release_subject_hash"):
        if manifest.get(key) != facts.get(key):
            raise FactVerificationError(f"acceptance fact_manifest.{key} does not match backend facts")
    expected_snapshots = {item.get("snapshot_id"): item for item in facts["snapshots"]}
    refs = manifest.get("snapshot_refs")
    if not isinstance(refs, list) or len(refs) != len(expected_snapshots):
        raise FactVerificationError("acceptance fact_manifest.snapshot_refs do not match backend facts")
    seen: set[Any] = set()
    reference_fields = (
        "snapshot_id",
        "snapshot_hash",
        "run_id",
        "load_plan_id",
        "profile_id",
        "slice_key",
        "source_watermark",
    )
    for index, ref in enumerate(refs):
        if not isinstance(ref, dict):
            raise FactVerificationError(
                f"acceptance fact_manifest.snapshot_refs[{index}] must be an object"
            )
        snapshot_id = ref.get("snapshot_id")
        if snapshot_id in seen or snapshot_id not in expected_snapshots:
            raise FactVerificationError("acceptance fact_manifest.snapshot_refs do not match backend facts")
        seen.add(snapshot_id)
        expected = expected_snapshots[snapshot_id]
        for key in reference_fields:
            if ref.get(key) != expected.get(key):
                raise FactVerificationError(
                    f"acceptance fact_manifest.snapshot_refs[{index}].{key} does not match backend facts"
                )
    if manifest.get("artifact_manifest_hashes") != facts.get("artifact_manifest_hashes"):
        raise FactVerificationError("acceptance artifact manifest hashes do not match backend facts")
    recovery_ref = manifest.get("recovery_ref")
    recovery = facts.get("recovery")
    if not isinstance(recovery_ref, dict) or not isinstance(recovery, dict):
        raise FactVerificationError("acceptance recovery reference is incomplete")
    for key, fact_key in (("evidence_id", "evidence_id"), ("evidence_hash", "evidence_hash"), ("run_id", "run_id"), ("experiment_id", "experiment_id"), ("source_watermark", "source_watermark"), ("recovery_generation", "recovery_generation")):
        if recovery_ref.get(key) != recovery.get(fact_key):
            raise FactVerificationError(f"acceptance recovery_ref.{key} does not match backend facts")


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Verify immutable Radar reliability facts from the backend API")
    parser.add_argument("--url", required=True, help="GET endpoint for /admin/radar/runs/:id/reliability-facts")
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--policy-id", required=True)
    parser.add_argument("--profile-id", required=True)
    parser.add_argument("--bearer-token", default="")
    parser.add_argument("--tenant-id", default="")
    parser.add_argument("--header", action="append", default=[], metavar="NAME=VALUE")
    parser.add_argument("--acceptance", type=Path, help="optional acceptance evidence to bind to fetched facts")
    parser.add_argument("--timeout", type=float, default=15.0)
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv or sys.argv[1:])
    try:
        run_id = parse_uuid(args.run_id, "--run-id")
        policy_id = parse_uuid(args.policy_id, "--policy-id")
        profile_id = args.profile_id.strip()
        if not profile_id:
            raise FactVerificationError("--profile-id is required")
        parsed = urlparse(args.url)
        query = dict(parse_qsl(parsed.query, keep_blank_values=True))
        query.update({"policy_id": str(policy_id), "profile_id": profile_id})
        url = urlunparse(parsed._replace(query=urlencode(query)))
        headers = {"Accept": "application/json"}
        if args.bearer_token.strip():
            headers["Authorization"] = f"Bearer {args.bearer_token.strip()}"
        if args.tenant_id.strip():
            headers["X-Radar-Tenant-ID"] = args.tenant_id.strip()
        for item in args.header:
            if "=" not in item:
                raise FactVerificationError("--header must use NAME=VALUE")
            name, value = item.split("=", 1)
            if not name.strip():
                raise FactVerificationError("--header name is required")
            headers[name.strip()] = value
        facts = fetch_facts(url, headers, args.timeout)
        verify_facts(facts, run_id, policy_id, profile_id)
        if args.acceptance:
            compare_acceptance_manifest(args.acceptance, facts)
        print(f"PASS radar reliability facts verification ({len(facts['snapshots'])} snapshots)")
        return 0
    except FactVerificationError as exc:
        print(f"FAIL radar reliability facts verification: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
