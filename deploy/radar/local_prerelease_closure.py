#!/usr/bin/env python3
"""Fail-closed evidence closure and local Worker registration helpers."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import socket
import tempfile
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any, Iterable, Mapping, Sequence

try:
    from .migration_ledger import expected_schema_migrations as manifest_expected_schema_migrations
except ImportError:  # pragma: no cover - direct script execution
    from migration_ledger import expected_schema_migrations as manifest_expected_schema_migrations


REQUIRED_GATES = (
    "immutable-inputs-and-code",
    "migration-225",
    "radar-browser-workflows",
    "restart-and-rollback",
    "evidence-closure-input",
)
GATE_FILES = (
    "gate-1-code.json",
    "gate-2-migration.json",
    "gate-3-radar.json",
    "gate-4-restart-rollback.json",
    "gate-5-closure-input.json",
)
ALLOWED_TOP_LEVEL = {
    "schema_version", "gate", "status", "run_id", "started_at", "finished_at",
    "bindings", "checks", "summary", "artifacts",
}
ALLOWED_BINDINGS = {
    "run_id", "source_sha256", "backup_sha256", "control_plane_digest", "worker_digest",
    "rollback_control_plane_digest", "rollback_worker_digest", "policy_version",
    "fixture_version", "dependency_digests", "environment_fingerprint",
}
FORBIDDEN_TEXT = {
    "prompt", "completion", "credential", "api_key", "password", "token",
    "route_trace", "account", "channel", "artifact_body", "probe_spec_hash",
    "observation_hash",
}
RUN_ID_PATTERN = re.compile(r"^[a-z0-9][a-z0-9_-]{2,62}-rehearsal$")
GATE_SCHEMA_VERSION = "radar-local-prerelease-gate-v1"
CLOSURE_SCHEMA_VERSION = "radar-local-prerelease-closure-v1"
MIGRATION_MANIFEST_DIR = Path(
    os.environ.get(
        "RADAR_MIGRATION_MANIFEST_DIR",
        str(Path(__file__).resolve().parent / "manifests" / "v0.2.0"),
    )
).resolve()
MANIFEST_EXPECTED_SCHEMA_MIGRATIONS = manifest_expected_schema_migrations(MIGRATION_MANIFEST_DIR)
MAX_RESPONSE_BYTES = 1024 * 1024
MAX_FUTURE_SKEW_SECONDS = 300
RFC3339_UTC = re.compile(
    r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z$"
)
SENSITIVE_LOG_LINE = re.compile(
    r"(?i)(prompt|completion|credential|password|api[_ -]?key|route[_ -]?trace|"
    r"account|channel|artifact[_ -]?body|probe[_ -]?spec[_ -]?hash|observation[_ -]?hash)"
    r"|raw[_ -]?observation"
)
BEARER_VALUE = re.compile(r"(?i)(authorization\s*:\s*bearer\s+)\S+")
TOKEN_VALUE = re.compile(r"(?i)(\btoken\b\s*[:=]\s*)\S+")
REGISTRATION_SPECS = (
    ("reasoning-runner", "runner", "reasoning", "RADAR_RUNNER_WORKER_TOKEN"),
    ("exact-grader", "grader", "exact", "RADAR_GRADER_WORKER_TOKEN"),
    ("reasoning-statistics", "statistics", "reasoning", "RADAR_STATISTICS_WORKER_TOKEN"),
)
GATE_CHECK_FIELDS = {
    REQUIRED_GATES[0]: (
        "preflight_passed", "backend_tests_passed", "worker_tests_passed",
        "frontend_checks_passed", "frontend_build_passed",
    ),
    REQUIRED_GATES[1]: ("migration_validator_passed",),
    REQUIRED_GATES[2]: (
        "worker_registration_passed", "fixture_provisioned", "api_verifier_passed",
        "playwright_passed",
    ),
    REQUIRED_GATES[3]: (
        "candidate_restart_passed", "browser_smoke_passed", "api_smoke_passed",
        "rollback_health_passed", "candidate_primary_restored",
        "repeated_quality_smoke_passed", "migration_replay_passed",
    ),
    REQUIRED_GATES[4]: ("prior_gate_count",),
}
GATE_ARTIFACTS = {
    REQUIRED_GATES[0]: [],
    REQUIRED_GATES[1]: ["private/gate-2-migration/summary.json"],
    REQUIRED_GATES[2]: ["private/worker-registration.json"],
    REQUIRED_GATES[3]: [
        "private/gate-4-primary-rollback/summary.json",
        "private/gate-4-migration/summary.json",
    ],
    REQUIRED_GATES[4]: [],
}


class ClosureError(ValueError):
    """An evidence or local registration contract failed closed."""


class RejectRedirects(urllib.request.HTTPRedirectHandler):
    def redirect_request(
        self,
        req: urllib.request.Request,
        fp: Any,
        code: int,
        msg: str,
        headers: Any,
        newurl: str,
    ) -> None:
        return None


def _utc_now() -> str:
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def _parse_rfc3339_utc(value: object) -> datetime:
    if not isinstance(value, str) or RFC3339_UTC.fullmatch(value) is None:
        raise ClosureError("timestamp must be RFC3339 UTC with a Z suffix")
    try:
        parsed = datetime.fromisoformat(value[:-1] + "+00:00")
    except ValueError as error:
        raise ClosureError("timestamp must be valid RFC3339 UTC") from error
    if parsed.utcoffset() != timedelta(0):
        raise ClosureError("timestamp must use UTC")
    return parsed


def _canonical(value: object) -> str:
    return json.dumps(value, ensure_ascii=True, sort_keys=True, separators=(",", ":"))


def _binding_projection(document: object, *, allow_source_exclusions: bool) -> dict[str, object]:
    if not isinstance(document, dict):
        raise ClosureError("bindings must be an object")
    keys = set(document)
    allowed = ALLOWED_BINDINGS | ({"source_exclusions"} if allow_source_exclusions else set())
    if keys != ALLOWED_BINDINGS and not (
        allow_source_exclusions and keys == allowed
    ):
        raise ClosureError("bindings must contain the exact allowed fields")
    projected = {key: document[key] for key in sorted(ALLOWED_BINDINGS)}
    run_id = projected.get("run_id")
    if not isinstance(run_id, str) or RUN_ID_PATTERN.fullmatch(run_id) is None:
        raise ClosureError("bindings contain an invalid run_id")
    return projected


def _input_binding_projection(document: object) -> dict[str, object]:
    return _binding_projection(document, allow_source_exclusions=True)


def _gate_binding_projection(document: object) -> dict[str, object]:
    return _binding_projection(document, allow_source_exclusions=False)


def _registration_evidence() -> list[dict[str, str]]:
    return [
        {"identity": identity, "worker_kind": kind, "capability": capability}
        for identity, kind, capability, _ in REGISTRATION_SPECS
    ]


def _forbidden_keys(value: object) -> bool:
    if isinstance(value, dict):
        for key, nested in value.items():
            if not isinstance(key, str) or key.lower() in FORBIDDEN_TEXT:
                return True
            if _forbidden_keys(nested):
                return True
    elif isinstance(value, list):
        return any(_forbidden_keys(item) for item in value)
    return False


def _documents(evidence: object) -> list[object]:
    if isinstance(evidence, Mapping):
        documents: list[object] = []
        for expected in REQUIRED_GATES:
            if expected in evidence:
                documents.append(evidence[expected])
        for key, value in evidence.items():
            if key not in REQUIRED_GATES:
                documents.append(value)
        return documents
    if isinstance(evidence, Sequence) and not isinstance(evidence, (str, bytes, bytearray)):
        return list(evidence)
    return []


def _migration_summary_error(summary: dict[str, object]) -> str | None:
    radar_checksum = summary.get("migration_224_checksum")
    pricing_checksum = summary.get("migration_225_checksum")
    count = summary.get("actual_migration_count")
    if not isinstance(radar_checksum, str) or re.fullmatch(r"[0-9a-f]{64}", radar_checksum) is None:
        return "migration 224 checksum is invalid"
    if not isinstance(pricing_checksum, str) or re.fullmatch(r"[0-9a-f]{64}", pricing_checksum) is None:
        return "migration 225 checksum is invalid"
    declared_count = summary.get("expected_schema_migrations")
    if not isinstance(declared_count, int) or isinstance(declared_count, bool):
        return "expected migration count is invalid"
    if declared_count != MANIFEST_EXPECTED_SCHEMA_MIGRATIONS:
        return "expected migration count does not match the configured migration manifest"
    baseline_count = summary.get("baseline_schema_migrations")
    if not isinstance(baseline_count, int) or isinstance(baseline_count, bool) or baseline_count <= 0:
        return "baseline migration count is invalid"
    if summary.get("migration_ledger_ok") is not True:
        return "migration ledger did not pass"
    if summary.get("candidate_pending_migrations") != []:
        return "candidate migration ledger is not closed"
    legacy_entries = summary.get("legacy_entries")
    if not isinstance(legacy_entries, list) or legacy_entries != sorted(legacy_entries):
        return "legacy migration ledger entries are invalid"
    if not all(isinstance(entry, str) and entry.endswith(".sql") for entry in legacy_entries):
        return "legacy migration ledger entries are invalid"
    if summary.get("checksum_mismatches") != []:
        return "migration checksum mismatches are present"
    fingerprints = (
        "baseline_ledger_sha256",
        "candidate_ledger_sha256",
        "expected_candidate_ledger_sha256",
        "expected_runtime_ledger_sha256",
        "runtime_ledger_sha256",
    )
    for field in fingerprints:
        value = summary.get(field)
        if not isinstance(value, str) or re.fullmatch(r"[0-9a-f]{64}", value) is None:
            return f"migration ledger fingerprint is invalid: {field}"
    if summary["candidate_ledger_sha256"] != summary["expected_candidate_ledger_sha256"]:
        return "candidate migration ledger fingerprint does not match expected ledger"
    if summary["runtime_ledger_sha256"] != summary["expected_runtime_ledger_sha256"]:
        return "runtime migration ledger fingerprint does not match expected ledger"
    if not isinstance(count, int) or isinstance(count, bool) or count != declared_count:
        return f"actual migration count must be {declared_count}"
    if summary.get("migration_224_semantics_ok") is not True:
        return "migration 224 semantics did not pass"
    if summary.get("migration_225_semantics_ok") is not True:
        return "migration 225 semantics did not pass"
    if summary.get("rollback_database_clone_used") is not True:
        return "rollback database clone was not used"
    return None


def _validate_gate_payload(gate: str, document: dict[str, object], errors: list[str]) -> None:
    checks = document.get("checks")
    summary = document.get("summary")
    artifacts = document.get("artifacts")
    expected_check_keys = {"exit_code", *GATE_CHECK_FIELDS[gate]}
    if not isinstance(checks, dict) or set(checks) != expected_check_keys:
        errors.append(f"gate {gate} checks violate the exact schema")
    else:
        exit_code = checks.get("exit_code")
        if not isinstance(exit_code, int) or isinstance(exit_code, bool) or exit_code != 0:
            errors.append(f"gate {gate} passed with a nonzero exit code")
        for field in GATE_CHECK_FIELDS[gate]:
            value = checks.get(field)
            if field == "prior_gate_count":
                passed = isinstance(value, int) and not isinstance(value, bool) and value == 4
            else:
                passed = value is True
            if not passed:
                errors.append(f"gate {gate} check {field} did not pass")
    if artifacts != GATE_ARTIFACTS[gate]:
        errors.append(f"gate {gate} artifacts violate the exact schema")
    if not isinstance(summary, dict):
        errors.append(f"gate {gate} summary must be an object")
        return

    expected_summary_keys: set[str]
    if gate == REQUIRED_GATES[0]:
        expected_summary_keys = {"result", "immutable_inputs_bound", "code_checks_passed"}
        if summary.get("immutable_inputs_bound") is not True or summary.get("code_checks_passed") is not True:
            errors.append(f"gate {gate} summary did not pass")
    elif gate == REQUIRED_GATES[1]:
        expected_summary_keys = {
            "result", "migration_224_checksum", "migration_225_checksum",
            "actual_migration_count", "migration_224_semantics_ok",
            "migration_225_semantics_ok", "rollback_database_clone_used",
        }
        expected_summary_keys |= {
            "expected_schema_migrations", "baseline_schema_migrations", "migration_ledger_ok",
            "candidate_pending_migrations", "legacy_entries", "checksum_mismatches",
            "baseline_ledger_sha256", "candidate_ledger_sha256",
            "expected_candidate_ledger_sha256", "expected_runtime_ledger_sha256",
            "runtime_ledger_sha256",
        }
        migration_error = _migration_summary_error(summary)
        if migration_error:
            errors.append(f"gate {gate}: {migration_error}")
    elif gate == REQUIRED_GATES[2]:
        expected_summary_keys = {"result", "registrations", "browser_workflows_passed", "api_verifier_passed"}
        if summary.get("registrations") != _registration_evidence():
            errors.append(f"gate {gate} registration evidence is invalid")
        if summary.get("browser_workflows_passed") is not True or summary.get("api_verifier_passed") is not True:
            errors.append(f"gate {gate} workflow summary did not pass")
    elif gate == REQUIRED_GATES[3]:
        expected_summary_keys = {
            "result", "migration_224_checksum", "migration_225_checksum",
            "actual_migration_count", "migration_224_semantics_ok",
            "migration_225_semantics_ok", "rollback_database_clone_used",
            "migration_replay_matched", "primary_database_clone_used",
            "rollback_control_plane_clone_only",
        }
        expected_summary_keys |= {
            "expected_schema_migrations", "baseline_schema_migrations", "migration_ledger_ok",
            "candidate_pending_migrations", "legacy_entries", "checksum_mismatches",
            "baseline_ledger_sha256", "candidate_ledger_sha256",
            "expected_candidate_ledger_sha256", "expected_runtime_ledger_sha256",
            "runtime_ledger_sha256",
        }
        migration_error = _migration_summary_error(summary)
        if migration_error:
            errors.append(f"gate {gate}: {migration_error}")
        if summary.get("migration_replay_matched") is not True:
            errors.append(f"gate {gate} migration replay did not match")
        if summary.get("primary_database_clone_used") is not True:
            errors.append(f"gate {gate} did not clone the primary Compose database")
        if summary.get("rollback_control_plane_clone_only") is not True:
            errors.append(f"gate {gate} rollback control plane was not clone-only")
    else:
        expected_summary_keys = {"result", "closure_input_validated"}
        if summary.get("closure_input_validated") is not True:
            errors.append(f"gate {gate} closure input was not validated")
    if set(summary) != expected_summary_keys:
        errors.append(f"gate {gate} summary violates the exact schema")
    if summary.get("result") != "passed":
        errors.append(f"gate {gate} summary result did not pass")


def audit_closure(
    evidence: object,
    expected_bindings: object | None = None,
    *,
    now: datetime | None = None,
) -> dict[str, object]:
    """Audit exactly five ordered gate documents and return a safe result."""
    errors: list[str] = []
    try:
        expected = _input_binding_projection(expected_bindings) if expected_bindings is not None else None
    except ClosureError as error:
        expected = None
        errors.append(str(error))
    documents = _documents(evidence)
    if not documents:
        errors.append("evidence must contain gate documents")
    seen: list[str] = []
    run_id: str | None = expected.get("run_id") if expected else None  # type: ignore[assignment]
    canonical_expected = _canonical(expected) if expected is not None else None
    valid_documents: list[dict[str, object]] = []
    observed_now = now or datetime.now(timezone.utc)
    if observed_now.tzinfo is None or observed_now.utcoffset() != timedelta(0):
        raise ClosureError("closure audit now must be timezone-aware UTC")
    future_limit = observed_now + timedelta(seconds=MAX_FUTURE_SKEW_SECONDS)
    previous_finished: datetime | None = None
    for index, raw_document in enumerate(documents):
        if not isinstance(raw_document, dict):
            errors.append(f"gate document {index + 1} is malformed")
            continue
        document = raw_document
        keys = set(document)
        if keys != ALLOWED_TOP_LEVEL:
            errors.append(f"gate document {index + 1} violates the top-level allowlist")
        gate = document.get("gate")
        if not isinstance(gate, str) or gate not in REQUIRED_GATES:
            errors.append(f"gate document {index + 1} has an unknown gate")
            continue
        if gate in seen:
            errors.append(f"duplicate gate: {gate}")
        seen.append(gate)
        if document.get("schema_version") != GATE_SCHEMA_VERSION:
            errors.append(f"gate {gate} has an invalid schema")
        if document.get("status") != "passed":
            errors.append(f"gate {gate} did not pass")
        document_run_id = document.get("run_id")
        if not isinstance(document_run_id, str) or RUN_ID_PATTERN.fullmatch(document_run_id) is None:
            errors.append(f"gate {gate} has an invalid run_id")
        elif run_id is None:
            run_id = document_run_id
        elif document_run_id != run_id:
            errors.append(f"gate {gate} has an inconsistent run_id")
        try:
            binding = _gate_binding_projection(document.get("bindings"))
            canonical_binding = _canonical(binding)
            if canonical_expected is None:
                canonical_expected = canonical_binding
                expected = binding
            elif canonical_binding != canonical_expected:
                errors.append(f"gate {gate} has inconsistent bindings")
        except ClosureError as error:
            errors.append(f"gate {gate}: {error}")
        try:
            started_at = _parse_rfc3339_utc(document.get("started_at"))
            finished_at = _parse_rfc3339_utc(document.get("finished_at"))
            if finished_at < started_at:
                errors.append(f"gate {gate} finished before it started")
            if started_at > future_limit or finished_at > future_limit:
                errors.append(f"gate {gate} timestamp exceeds the allowed future skew")
            if previous_finished is not None and started_at < previous_finished:
                errors.append(f"gate {gate} overlaps or precedes the prior gate")
            previous_finished = finished_at
        except ClosureError:
            errors.append(f"gate {gate} has an invalid timestamp")
        _validate_gate_payload(gate, document, errors)
        if _forbidden_keys(document):
            errors.append(f"gate {gate} contains a forbidden evidence key")
        valid_documents.append(document)
    missing = [gate for gate in REQUIRED_GATES if gate not in seen]
    if missing:
        errors.append("required gates are missing")
    if len(seen) != len(REQUIRED_GATES):
        errors.append("gate count must be exactly five")
    if seen != list(REQUIRED_GATES):
        errors.append("gates are not in required order")
    by_gate = {
        document["gate"]: document
        for document in valid_documents
        if isinstance(document.get("gate"), str)
    }
    gate_2_summary = by_gate.get(REQUIRED_GATES[1], {}).get("summary")
    gate_4_summary = by_gate.get(REQUIRED_GATES[3], {}).get("summary")
    if isinstance(gate_2_summary, dict) and isinstance(gate_4_summary, dict):
        for field in (
            "migration_224_checksum",
            "migration_225_checksum",
            "actual_migration_count",
            "expected_schema_migrations",
            "baseline_schema_migrations",
            "migration_ledger_ok",
            "candidate_pending_migrations",
            "legacy_entries",
            "checksum_mismatches",
            "baseline_ledger_sha256",
            "candidate_ledger_sha256",
            "expected_candidate_ledger_sha256",
            "expected_runtime_ledger_sha256",
            "runtime_ledger_sha256",
        ):
            if field in gate_2_summary or field in gate_4_summary:
                if gate_2_summary.get(field) != gate_4_summary.get(field):
                    errors.append(f"Gate 2 and Gate 4 {field} values differ")
    status = "local-isolated-prerelease-failed" if errors else "local-isolated-prerelease-passed"
    return {
        "ok": not errors,
        "schema_version": CLOSURE_SCHEMA_VERSION,
        "status": status,
        "run_id": run_id,
        "gate_count": len(valid_documents),
        "bindings": expected or {},
        "errors": errors,
    }


def _atomic_json(path: Path, document: object, mode: int = 0o600) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    temporary = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            json.dump(document, stream, ensure_ascii=True, sort_keys=True, indent=2)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.chmod(temporary, mode)
        os.replace(temporary, path)
        os.chmod(path, mode)
    finally:
        temporary.unlink(missing_ok=True)


def verify_clock_sanity(
    trusted_utc: str,
    output: Path,
    *,
    max_skew_seconds: int,
    observed_now: datetime | None = None,
) -> dict[str, object]:
    """Compare the host clock with an independently supplied UTC timestamp."""
    trusted = _parse_rfc3339_utc(trusted_utc)
    if type(max_skew_seconds) is not int or max_skew_seconds <= 0:
        raise ClosureError("clock max skew must be a positive integer")
    observed = observed_now or datetime.now(timezone.utc)
    if observed.tzinfo is None or observed.utcoffset() != timedelta(0):
        raise ClosureError("observed clock must be timezone-aware UTC")
    absolute_skew = abs((observed - trusted).total_seconds())
    if absolute_skew > max_skew_seconds:
        raise ClosureError("clock skew exceeds the allowed maximum")
    result: dict[str, object] = {
        "schema_version": "radar-local-clock-sanity-v1",
        "trusted_utc": trusted.isoformat().replace("+00:00", "Z"),
        "observed_utc": observed.isoformat().replace("+00:00", "Z"),
        "absolute_skew_seconds": int(absolute_skew),
        "max_skew_seconds": max_skew_seconds,
        "passed": True,
    }
    _atomic_json(output, result)
    return result


def _public_closure(result: Mapping[str, object]) -> dict[str, object]:
    return {
        "schema_version": CLOSURE_SCHEMA_VERSION,
        "status": result["status"],
        "run_id": result.get("run_id"),
        "finished_at": _utc_now(),
        "bindings": result.get("bindings", {}),
        "checks": {"exact_gate_count": result.get("gate_count") == 5},
        "summary": {"gate_count": result.get("gate_count", 0)},
        "artifacts": list(GATE_FILES) if result.get("ok") else [],
    }


def _failed_result(result: Mapping[str, object], error: str) -> dict[str, object]:
    failed = dict(result)
    failed["ok"] = False
    failed["status"] = "local-isolated-prerelease-failed"
    failed["errors"] = [*list(result.get("errors", [])), error]
    return failed


def _publish_public(evidence_dir: Path, documents: Mapping[str, object]) -> None:
    private = evidence_dir / "private"
    public = evidence_dir / "public"
    private.mkdir(parents=True, exist_ok=True)
    os.chmod(private, 0o700)
    staging = Path(tempfile.mkdtemp(prefix=".public-stage-", dir=private))
    backup = Path(tempfile.mkdtemp(prefix=".public-previous-", dir=private))
    backup.rmdir()
    os.chmod(staging, 0o700)
    moved_previous = False
    try:
        for name, document in documents.items():
            _atomic_json(staging / name, document)
        if public.exists():
            os.replace(public, backup)
            moved_previous = True
        os.replace(staging, public)
        os.chmod(public, 0o700)
    except Exception:
        if moved_previous and not public.exists():
            os.replace(backup, public)
            moved_previous = False
        raise
    finally:
        if staging.exists():
            shutil.rmtree(staging)
        if moved_previous and backup.exists():
            if backup.is_dir():
                shutil.rmtree(backup)
            else:
                backup.unlink()


def close_evidence(evidence_dir: Path, bindings_path: Path) -> dict[str, object]:
    try:
        expected = json.loads(bindings_path.read_text(encoding="utf-8"))
        binding_error: str | None = None
    except (OSError, json.JSONDecodeError) as error:
        expected = None
        binding_error = f"bindings could not be read: {error}"
    documents: list[object] = []
    for name in GATE_FILES:
        path = evidence_dir / name
        if path.is_file():
            try:
                documents.append(json.loads(path.read_text(encoding="utf-8")))
            except (OSError, json.JSONDecodeError):
                documents.append(None)
    result = audit_closure(documents, expected)
    if binding_error:
        result = _failed_result(result, binding_error)
    actual_gate_files = {
        path.name
        for path in evidence_dir.iterdir()
        if path.name.startswith("gate-") and path.name.endswith(".json")
    }
    if actual_gate_files != set(GATE_FILES):
        result = _failed_result(result, "evidence directory must contain exactly the five gate files")
    public_documents: dict[str, object] = {"closure.json": _public_closure(result)}
    if result["ok"]:
        for name in GATE_FILES:
            public_documents[name] = json.loads((evidence_dir / name).read_text(encoding="utf-8"))
    _publish_public(evidence_dir, public_documents)
    return result


def write_gate(
    evidence_dir: Path,
    bindings_path: Path,
    gate: str,
    status: str,
    exit_code: int,
    started_at: str,
    finished_at: str,
) -> Path:
    if gate not in REQUIRED_GATES or status not in {"passed", "failed"}:
        raise ClosureError("invalid gate result")
    if (status == "passed") != (exit_code == 0):
        raise ClosureError("gate status and exit code are inconsistent")
    parsed_started_at = _parse_rfc3339_utc(started_at)
    parsed_finished_at = _parse_rfc3339_utc(finished_at)
    if parsed_finished_at < parsed_started_at:
        raise ClosureError("gate finished before it started")
    if parsed_finished_at > datetime.now(timezone.utc) + timedelta(seconds=MAX_FUTURE_SKEW_SECONDS):
        raise ClosureError("gate timestamp exceeds the allowed future skew")
    bindings = _input_binding_projection(json.loads(bindings_path.read_text(encoding="utf-8")))
    index = REQUIRED_GATES.index(gate)
    passed = status == "passed"
    checks: dict[str, object] = {"exit_code": exit_code}
    for field in GATE_CHECK_FIELDS[gate]:
        checks[field] = 4 if field == "prior_gate_count" and passed else passed
    summary: dict[str, object] = {"result": status}
    artifacts: list[str] = []
    if gate == REQUIRED_GATES[0] and passed:
        summary.update({"immutable_inputs_bound": True, "code_checks_passed": True})
    if gate in {REQUIRED_GATES[1], REQUIRED_GATES[3]} and passed:
        migration_directory = "gate-2-migration" if gate == REQUIRED_GATES[1] else "gate-4-migration"
        migration_summary = evidence_dir / "private" / migration_directory / "summary.json"
        if not migration_summary.is_file():
            raise ClosureError("migration summary is unavailable")
        source = json.loads(migration_summary.read_text(encoding="utf-8"))
        if not isinstance(source, dict):
            raise ClosureError("migration summary is malformed")
        for key, expected_type in (
            ("migration_224_checksum", str),
            ("migration_225_checksum", str),
            ("migration_count", int),
            ("migration_224_semantics_ok", bool),
            ("migration_225_semantics_ok", bool),
            ("rollback_database_clone_used", bool),
        ):
            value = source.get(key)
            if not isinstance(value, expected_type) or expected_type is int and isinstance(value, bool):
                raise ClosureError("migration summary is incomplete")
            output_key = "actual_migration_count" if key == "migration_count" else key
            summary[output_key] = value
        for key, expected_type in (
            ("expected_schema_migrations", int),
            ("baseline_schema_migrations", int),
            ("migration_ledger_ok", bool),
            ("candidate_pending_migrations", list),
            ("legacy_entries", list),
            ("checksum_mismatches", list),
            ("baseline_ledger_sha256", str),
            ("candidate_ledger_sha256", str),
            ("expected_candidate_ledger_sha256", str),
            ("expected_runtime_ledger_sha256", str),
            ("runtime_ledger_sha256", str),
        ):
            value = source.get(key)
            if not isinstance(value, expected_type) or expected_type is int and isinstance(value, bool):
                raise ClosureError("migration ledger summary is malformed")
            summary[key] = value
        migration_error = _migration_summary_error(summary)
        if migration_error:
            raise ClosureError(migration_error)
        if gate == REQUIRED_GATES[3]:
            primary_summary_path = evidence_dir / "private" / "gate-4-primary-rollback" / "summary.json"
            if not primary_summary_path.is_file():
                raise ClosureError("primary Compose rollback summary is unavailable")
            primary = json.loads(primary_summary_path.read_text(encoding="utf-8"))
            if (
                not isinstance(primary, dict)
                or set(primary) != {
                    "schema_version",
                    "primary_database_clone_used",
                    "rollback_control_plane_clone_only",
                    "rollback_health_passed",
                }
                or primary.get("schema_version") != "radar-local-compose-rollback-v1"
                or primary.get("primary_database_clone_used") is not True
                or primary.get("rollback_control_plane_clone_only") is not True
                or primary.get("rollback_health_passed") is not True
            ):
                raise ClosureError("primary Compose rollback summary is invalid")
            summary["primary_database_clone_used"] = True
            summary["rollback_control_plane_clone_only"] = True
            summary["migration_replay_matched"] = True
            artifacts.append("private/gate-4-primary-rollback/summary.json")
        artifacts.append(f"private/{migration_directory}/summary.json")
    registration_path = evidence_dir / "private" / "worker-registration.json"
    if gate == REQUIRED_GATES[2] and passed:
        if not registration_path.is_file():
            raise ClosureError("Worker registration evidence is unavailable")
        registration = json.loads(registration_path.read_text(encoding="utf-8"))
        required_order = ["reasoning-runner", "exact-grader", "reasoning-statistics"]
        if (
            not isinstance(registration, dict)
            or registration.get("registration_order") != required_order
            or registration.get("registrations") != _registration_evidence()
            or registration.get("status") != "passed"
        ):
            raise ClosureError("Worker registration evidence is invalid")
        summary.update({
            "registrations": _registration_evidence(),
            "browser_workflows_passed": True,
            "api_verifier_passed": True,
        })
        artifacts.append("private/worker-registration.json")
    if gate == REQUIRED_GATES[4] and passed:
        summary["closure_input_validated"] = True
    document = {
        "schema_version": GATE_SCHEMA_VERSION,
        "gate": gate,
        "status": status,
        "run_id": bindings["run_id"],
        "started_at": started_at,
        "finished_at": finished_at,
        "bindings": bindings,
        "checks": checks,
        "summary": summary,
        "artifacts": artifacts,
    }
    path = evidence_dir / GATE_FILES[index]
    _atomic_json(path, document)
    return path


def write_failed_closure(
    evidence_dir: Path,
    run_id: str,
    failed_gate: str,
    exit_code: int,
    bindings_path: Path | None = None,
) -> None:
    bindings: dict[str, object] = {}
    if bindings_path is not None and bindings_path.is_file():
        bindings = _input_binding_projection(json.loads(bindings_path.read_text(encoding="utf-8")))
    document = {
        "schema_version": CLOSURE_SCHEMA_VERSION,
        "status": "local-isolated-prerelease-failed",
        "run_id": run_id,
        "finished_at": _utc_now(),
        "bindings": bindings,
        "checks": {"failed_gate": failed_gate},
        "summary": {"exit_code": exit_code},
        "artifacts": [],
    }
    _publish_public(evidence_dir, {"closure.json": document})


def _redact_log_line(line: str) -> str:
    newline = "\n" if line.endswith("\n") else ""
    content = line[:-1] if newline else line
    if SENSITIVE_LOG_LINE.search(content):
        return "[REDACTED]" + newline
    content = BEARER_VALUE.sub(r"\1[REDACTED]", content)
    content = TOKEN_VALUE.sub(r"\1[REDACTED]", content)
    return content + newline


def redact_stream(input_stream: Any, output_stream: Any) -> None:
    for line in input_stream:
        output_stream.write(_redact_log_line(line))
        output_stream.flush()


def sanitize_log(path: Path) -> None:
    """Remove credential-bearing and forbidden evidence lines from a private log."""
    sanitized = "".join(
        _redact_log_line(line)
        for line in path.read_text(encoding="utf-8", errors="replace").splitlines(keepends=True)
    )
    descriptor, temporary_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    temporary = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            stream.write(sanitized)
            stream.flush()
            os.fsync(stream.fileno())
        os.chmod(temporary, 0o600)
        os.replace(temporary, path)
        os.chmod(path, 0o600)
    finally:
        temporary.unlink(missing_ok=True)


def _loopback_origin(origin: str) -> str:
    parsed = urllib.parse.urlparse(origin)
    if (
        parsed.scheme != "http"
        or parsed.hostname not in {"127.0.0.1", "localhost", "::1"}
        or parsed.port is None
        or parsed.username
        or parsed.password
        or parsed.path not in {"", "/"}
        or parsed.params
        or parsed.query
        or parsed.fragment
    ):
        raise ClosureError("origin must be a plain HTTP loopback origin")
    return origin.rstrip("/")


def _request_json(
    origin: str,
    path: str,
    headers: Mapping[str, str],
    *,
    method: str,
    payload: object | None = None,
) -> dict[str, object]:
    request_headers = {"Accept": "application/json", **headers}
    encoded_payload: bytes | None = None
    if payload is not None:
        request_headers["Content-Type"] = "application/json"
        encoded_payload = _canonical(payload).encode("utf-8")
    request = urllib.request.Request(
        origin + path,
        data=encoded_payload,
        headers=request_headers,
        method=method,
    )
    opener = urllib.request.build_opener(RejectRedirects())
    try:
        with opener.open(request, timeout=15) as response:
            if response.status < 200 or response.status >= 300:
                raise ClosureError("local Worker registration request failed")
            response_url = urllib.parse.urlparse(response.geturl())
            expected_url = urllib.parse.urlparse(origin)
            if (
                response_url.scheme,
                response_url.hostname,
                response_url.port,
            ) != (
                expected_url.scheme,
                expected_url.hostname,
                expected_url.port,
            ):
                raise ClosureError("local Worker registration response changed origin")
            content_length = response.headers.get("Content-Length")
            if content_length is not None:
                try:
                    response_size = int(content_length)
                except ValueError as error:
                    raise ClosureError("local Worker registration response has invalid Content-Length") from error
                if response_size > MAX_RESPONSE_BYTES:
                    raise ClosureError("local Worker registration response exceeds 1 MiB")
            encoded = response.read(MAX_RESPONSE_BYTES + 1)
    except urllib.error.HTTPError as error:
        error.close()
        raise ClosureError("local Worker registration request failed") from error
    except (urllib.error.URLError, OSError, TimeoutError, socket.timeout) as error:
        raise ClosureError("local Worker registration request failed") from error
    if len(encoded) > MAX_RESPONSE_BYTES:
        raise ClosureError("local Worker registration response exceeds 1 MiB")
    try:
        document = json.loads(encoded)
    except (json.JSONDecodeError, UnicodeDecodeError) as error:
        raise ClosureError("local Worker registration request failed") from error
    if not isinstance(document, dict):
        raise ClosureError("local Worker registration returned malformed JSON")
    data = document.get("data", document)
    if not isinstance(data, dict):
        raise ClosureError("local Worker registration returned malformed data")
    return data


def _post_json(origin: str, path: str, payload: object, headers: Mapping[str, str]) -> dict[str, object]:
    return _request_json(origin, path, headers, method="POST", payload=payload)


def _get_json(origin: str, path: str, headers: Mapping[str, str]) -> dict[str, object]:
    return _request_json(origin, path, headers, method="GET")


def _ensure_admin_compliance(origin: str, access_token: str) -> None:
    headers = {"Authorization": f"Bearer {access_token}"}
    status = _get_json(origin, "/api/v1/admin/compliance", headers)
    required = status.get("required")
    if required is False:
        return
    if required is not True:
        raise ClosureError("local administrator compliance status is malformed")
    phrase = status.get("ack_phrase_en")
    if not isinstance(phrase, str) or not phrase.strip():
        raise ClosureError("local administrator compliance phrase is unavailable")
    accepted = _post_json(
        origin,
        "/api/v1/admin/compliance/accept",
        {"language": "en", "phrase": phrase},
        headers,
    )
    if accepted.get("required") is not False:
        raise ClosureError("local administrator compliance acknowledgement did not persist")


def _ensure_worker_management_roles(origin: str, access_token: str) -> None:
    headers = {"Authorization": f"Bearer {access_token}"}
    for role in ("platform_admin", "test_operator"):
        binding = _post_json(
            origin,
            "/api/v1/admin/radar/rbac/role-bindings",
            {"role": role, "scope": {}},
            headers,
        )
        if (
            binding.get("Role") != role
            or binding.get("Scope") != {}
            or binding.get("Enabled") is not True
        ):
            raise ClosureError("local Radar role binding did not persist")


def register_workers(
    origin: str,
    run_id: str,
    worker_digest: str,
    administrator_email: str,
    administrator_password: str,
    worker_tokens: Mapping[str, str],
) -> dict[str, object]:
    """Register and verify the three required Workers in their fixed order."""
    origin = _loopback_origin(origin)
    if RUN_ID_PATTERN.fullmatch(run_id) is None:
        raise ClosureError("invalid run_id")
    login = _post_json(
        origin,
        "/api/v1/auth/login",
        {"email": administrator_email, "password": administrator_password},
        {},
    )
    access_token = login.get("access_token")
    if not isinstance(access_token, str) or not access_token:
        raise ClosureError("local administrator login did not return an access token")
    _ensure_admin_compliance(origin, access_token)
    _ensure_worker_management_roles(origin, access_token)
    return register_workers_with_token(
        origin,
        run_id,
        worker_digest,
        access_token,
        worker_tokens,
    )


def register_workers_with_token(
    origin: str,
    run_id: str,
    worker_digest: str,
    access_token: str,
    worker_tokens: Mapping[str, str],
) -> dict[str, object]:
    """Register the required Workers in the tenant bound to an existing token."""
    origin = _loopback_origin(origin)
    if RUN_ID_PATTERN.fullmatch(run_id) is None:
        raise ClosureError("invalid run_id")
    if not isinstance(access_token, str) or not access_token:
        raise ClosureError("local administrator access token is unavailable")
    order: list[str] = []
    for identity, kind, capability, token_key in REGISTRATION_SPECS:
        token = worker_tokens.get(token_key)
        if not isinstance(token, str) or len(token) < 32:
            raise ClosureError("required local Worker token is unavailable")
        name = f"{run_id}-{identity}"
        payload = {
            "name": name,
            "worker_kind": kind,
            "region": "local-rehearsal",
            "image_digest": worker_digest,
            "capabilities": [capability],
            "max_concurrency": 1,
            "token": token,
        }
        key = hashlib.sha256(f"{run_id}:register:{identity}".encode()).hexdigest()
        record = _post_json(
            origin,
            "/api/v1/admin/radar/workers",
            payload,
            {"Authorization": f"Bearer {access_token}", "Idempotency-Key": key},
        )
        if (
            record.get("name") != name
            or record.get("worker_kind") != kind
            or record.get("status") != "active"
            or record.get("capabilities") != [capability]
            or record.get("image_digest") != worker_digest
            or record.get("region") != "local-rehearsal"
            or type(record.get("max_concurrency")) is not int
            or record.get("max_concurrency") != 1
        ):
            raise ClosureError("registered Worker identity or execution contract mismatch")
        order.append(identity)
    return {
        "registrations": _registration_evidence(),
        "registration_order": order,
        "status": "passed",
    }


def write_worker_registration_evidence(output: Path, result: Mapping[str, object]) -> None:
    """Write redacted Worker registration evidence using the closure's atomic contract."""
    _atomic_json(output, dict(result))


def write_runtime_environment(
    origin: str,
    run_id: str,
    administrator_email: str,
    administrator_password: str,
    fixture_password: str,
    output: Path,
) -> None:
    """Create the short-lived private environment used by API and browser checks."""
    origin = _loopback_origin(origin)
    if RUN_ID_PATTERN.fullmatch(run_id) is None:
        raise ClosureError("invalid run_id")
    fixture_email = f"radar-quality-{run_id}@example.invalid"
    administrator = _post_json(
        origin,
        "/api/v1/auth/login",
        {"email": administrator_email, "password": administrator_password},
        {},
    )
    user = _post_json(
        origin,
        "/api/v1/auth/login",
        {"email": fixture_email, "password": fixture_password},
        {},
    )
    administrator_token = administrator.get("access_token")
    user_token = user.get("access_token")
    if not all(isinstance(value, str) and value for value in (administrator_token, user_token)):
        raise ClosureError("runtime login did not return both access tokens")
    values = {
        "RADAR_QUALITY_STAGING_ADMIN_TOKEN": administrator_token,
        "RADAR_QUALITY_STAGING_USER_TOKEN": user_token,
        "RADAR_E2E_ADMIN_EMAIL": administrator_email,
        "RADAR_E2E_ADMIN_PASSWORD": administrator_password,
        "RADAR_E2E_USER_EMAIL": fixture_email,
        "RADAR_E2E_USER_PASSWORD": fixture_password,
    }
    if any("\n" in str(value) or "\r" in str(value) for value in values.values()):
        raise ClosureError("runtime environment value contains a line break")
    descriptor, temporary_name = tempfile.mkstemp(prefix=f".{output.name}.", dir=output.parent)
    temporary = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            for key, value in sorted(values.items()):
                stream.write(f"{key}={value}\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.chmod(temporary, 0o600)
        os.replace(temporary, output)
        os.chmod(output, 0o600)
    finally:
        temporary.unlink(missing_ok=True)


def read_environment(path: Path) -> dict[str, str]:
    if not path.is_file() or (path.stat().st_mode & 0o777) != 0o600:
        raise ClosureError("environment file must use mode 0600")
    values: dict[str, str] = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        if not line or "=" not in line:
            raise ClosureError("environment file is malformed")
        key, value = line.split("=", 1)
        values[key] = value
    return values


def _main() -> int:
    parser = argparse.ArgumentParser(description="Close local isolated Radar prerelease evidence")
    subparsers = parser.add_subparsers(dest="command", required=True)
    audit_parser = subparsers.add_parser("audit")
    audit_parser.add_argument("--evidence-dir", required=True, type=Path)
    audit_parser.add_argument("--bindings", required=True, type=Path)
    gate_parser = subparsers.add_parser("write-gate")
    gate_parser.add_argument("--evidence-dir", required=True, type=Path)
    gate_parser.add_argument("--bindings", required=True, type=Path)
    gate_parser.add_argument("--gate", required=True)
    gate_parser.add_argument("--status", required=True)
    gate_parser.add_argument("--exit-code", required=True, type=int)
    gate_parser.add_argument("--started-at", required=True)
    gate_parser.add_argument("--finished-at", required=True)
    failure_parser = subparsers.add_parser("write-failure")
    failure_parser.add_argument("--evidence-dir", required=True, type=Path)
    failure_parser.add_argument("--bindings", type=Path)
    failure_parser.add_argument("--run-id", required=True)
    failure_parser.add_argument("--failed-gate", required=True)
    failure_parser.add_argument("--exit-code", required=True, type=int)
    register_parser = subparsers.add_parser("register-workers")
    register_parser.add_argument("--origin", required=True)
    register_parser.add_argument("--run-id", required=True)
    register_parser.add_argument("--bindings", required=True, type=Path)
    register_parser.add_argument("--env-file", required=True, type=Path)
    register_parser.add_argument("--administrator-email", default="radar-admin@staging.local")
    register_parser.add_argument("--output", required=True, type=Path)
    runtime_parser = subparsers.add_parser("prepare-runtime")
    runtime_parser.add_argument("--origin", required=True)
    runtime_parser.add_argument("--run-id", required=True)
    runtime_parser.add_argument("--env-file", required=True, type=Path)
    runtime_parser.add_argument("--administrator-email", default="radar-admin@staging.local")
    runtime_parser.add_argument("--output", required=True, type=Path)
    sanitize_parser = subparsers.add_parser("sanitize-log")
    sanitize_parser.add_argument("--path", required=True, type=Path)
    clock_parser = subparsers.add_parser("clock-sanity")
    clock_parser.add_argument("--trusted-utc", required=True)
    clock_parser.add_argument("--max-skew-seconds", required=True, type=int)
    clock_parser.add_argument("--output", required=True, type=Path)
    subparsers.add_parser("redact-stream")
    arguments = parser.parse_args()
    if arguments.command == "audit":
        result = close_evidence(arguments.evidence_dir, arguments.bindings)
        return 0 if result["ok"] else 1
    if arguments.command == "write-gate":
        write_gate(arguments.evidence_dir, arguments.bindings, arguments.gate, arguments.status, arguments.exit_code, arguments.started_at, arguments.finished_at)
        return 0
    if arguments.command == "write-failure":
        write_failed_closure(arguments.evidence_dir, arguments.run_id, arguments.failed_gate, arguments.exit_code, arguments.bindings)
        return 0
    if arguments.command == "register-workers":
        binding = _input_binding_projection(json.loads(arguments.bindings.read_text(encoding="utf-8")))
        environment = read_environment(arguments.env_file)
        result = register_workers(
            arguments.origin,
            arguments.run_id,
            str(binding["worker_digest"]),
            arguments.administrator_email,
            environment.get("RADAR_ADMIN_PASSWORD", ""),
            environment,
        )
        _atomic_json(arguments.output, result)
        return 0
    if arguments.command == "prepare-runtime":
        environment = read_environment(arguments.env_file)
        write_runtime_environment(
            arguments.origin,
            arguments.run_id,
            arguments.administrator_email,
            environment.get("RADAR_ADMIN_PASSWORD", ""),
            os.environ.get("RADAR_FIXTURE_PASSWORD", ""),
            arguments.output,
        )
        return 0
    if arguments.command == "sanitize-log":
        sanitize_log(arguments.path)
        return 0
    if arguments.command == "clock-sanity":
        verify_clock_sanity(
            arguments.trusted_utc,
            arguments.output,
            max_skew_seconds=arguments.max_skew_seconds,
        )
        return 0
    if arguments.command == "redact-stream":
        redact_stream(os.sys.stdin, os.sys.stdout)
        return 0
    raise ClosureError("unknown command")


if __name__ == "__main__":
    try:
        raise SystemExit(_main())
    except (ClosureError, OSError, json.JSONDecodeError) as error:
        print(f"FAIL: {error}", file=os.sys.stderr)
        raise SystemExit(1)
