#!/usr/bin/env python3
"""Fail-closed, filename-aware migration ledger validation."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
from pathlib import Path
from typing import Any, Iterable


CHECKSUM_RE = re.compile(r"^[0-9a-f]{64}$")
FILENAME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]*\.sql$")


class LedgerError(ValueError):
    """Raised when a migration ledger is malformed."""


def checksum_text(content: str) -> str:
    """Match the checksum contract used by the migration runner."""
    return hashlib.sha256(content.strip().encode("utf-8")).hexdigest()


def checksum_file(path: Path) -> str:
    return checksum_text(path.read_text(encoding="utf-8"))


def _validate_entry(filename: str, checksum: str) -> None:
    if not FILENAME_RE.fullmatch(filename):
        raise LedgerError(f"invalid migration filename: {filename!r}")
    if not CHECKSUM_RE.fullmatch(checksum):
        raise LedgerError(f"invalid migration checksum for {filename!r}")


def _entry_map(entries: Iterable[tuple[str, str]]) -> dict[str, str]:
    result: dict[str, str] = {}
    for filename, checksum in entries:
        _validate_entry(filename, checksum)
        if filename in result:
            raise LedgerError(f"duplicate migration filename: {filename}")
        result[filename] = checksum
    return dict(sorted(result.items()))


def ledger_sha256(entries: dict[str, str]) -> str:
    """Hash a sorted filename/checksum ledger without exposing migration contents."""
    canonical = "".join(f"{filename}\t{checksum}\n" for filename, checksum in sorted(entries.items()))
    return hashlib.sha256(canonical.encode("ascii")).hexdigest()


def read_manifest(path: Path) -> dict[str, str]:
    """Read a filename<TAB>checksum manifest, rejecting malformed rows."""
    entries: list[tuple[str, str]] = []
    for line_number, raw_line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        if not raw_line.strip():
            continue
        fields = raw_line.split("\t")
        if len(fields) != 2:
            raise LedgerError(f"manifest row {line_number} must contain filename and checksum")
        entries.append((fields[0].strip(), fields[1].strip()))
    return _entry_map(entries)


def read_runtime_manifest(path: Path) -> dict[str, str]:
    """Read runtime output; legacy runners used a pipe separator."""
    entries: list[tuple[str, str]] = []
    for line_number, raw_line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        if not raw_line.strip():
            continue
        separator = "\t" if "\t" in raw_line else "|"
        fields = raw_line.split(separator)
        if len(fields) != 2:
            raise LedgerError(f"runtime row {line_number} must contain filename and checksum")
        entries.append((fields[0].strip(), fields[1].strip()))
    return _entry_map(entries)


def read_name_list(path: Path) -> list[str]:
    names = [line.strip() for line in path.read_text(encoding="utf-8").splitlines() if line.strip()]
    if len(names) != len(set(names)):
        raise LedgerError("migration name list contains duplicates")
    for name in names:
        if not FILENAME_RE.fullmatch(name):
            raise LedgerError(f"invalid migration filename: {name!r}")
    return sorted(names)


def expected_schema_migrations(manifest_dir: Path) -> int:
    """Derive the expected runtime count from the authoritative manifest inputs."""
    baseline = read_manifest(manifest_dir / "migration-baseline.tsv")
    expected_new = read_name_list(manifest_dir / "expected-new.txt")
    return len(baseline) + len(expected_new)


def candidate_manifest(migrations_dir: Path) -> dict[str, str]:
    files = sorted(migrations_dir.glob("*.sql"), key=lambda path: path.name)
    return _entry_map((path.name, checksum_file(path)) for path in files)


def _validated_name_list(names: Iterable[str], *, label: str) -> list[str]:
    values = list(names)
    if len(values) != len(set(values)):
        raise LedgerError(f"{label} contains duplicate migration filenames")
    for name in values:
        if not FILENAME_RE.fullmatch(name):
            raise LedgerError(f"invalid migration filename: {name!r}")
    return sorted(values)


def audit_candidate(
    baseline: dict[str, str],
    candidate: dict[str, str],
    *,
    expected_new: Iterable[str],
    legacy_entries: Iterable[str],
) -> dict[str, Any]:
    baseline = _entry_map(baseline.items())
    candidate = _entry_map(candidate.items())
    expected = _validated_name_list(expected_new, label="expected-new list")
    legacy = _validated_name_list(legacy_entries, label="legacy list")

    baseline_names = set(baseline)
    candidate_names = set(candidate)
    missing_baseline = sorted(baseline_names - candidate_names)
    pending = sorted(candidate_names - baseline_names)
    checksum_mismatches = sorted(
        name for name in baseline_names & candidate_names if baseline[name] != candidate[name]
    )
    unexpected_missing = sorted(set(missing_baseline) - set(legacy))
    legacy_not_missing = sorted(set(legacy) - set(missing_baseline))
    unknown_candidate = sorted(set(pending) - set(expected))
    missing_expected = sorted(set(expected) - candidate_names)
    blockers = {
        "unexpected_missing_baseline": unexpected_missing,
        "legacy_entries_not_missing": legacy_not_missing,
        "unknown_candidate_files": unknown_candidate,
        "missing_expected_new": missing_expected,
        "checksum_mismatches": checksum_mismatches,
    }
    expected_candidate = dict(baseline)
    for name in legacy:
        expected_candidate.pop(name, None)
    for name in expected:
        if name in candidate:
            expected_candidate[name] = candidate[name]
    return {
        "ok": not any(blockers.values()),
        "baseline_schema_migrations": len(baseline),
        "baseline_unique_filenames": len(baseline),
        "candidate_file_count": len(candidate),
        "expected_schema_migrations": len(baseline) + len(expected),
        "candidate_pending_migrations": pending,
        "expected_new_migrations": expected,
        "legacy_entries": legacy,
        "missing_baseline_entries": missing_baseline,
        "baseline_ledger_sha256": ledger_sha256(baseline),
        "candidate_ledger_sha256": ledger_sha256(candidate),
        "expected_candidate_ledger_sha256": ledger_sha256(expected_candidate),
        **blockers,
    }


def audit_runtime(
    baseline: dict[str, str],
    candidate: dict[str, str],
    actual: dict[str, str],
    *,
    expected_new: Iterable[str],
    legacy_entries: Iterable[str],
) -> dict[str, Any]:
    candidate_result = audit_candidate(
        baseline,
        candidate,
        expected_new=expected_new,
        legacy_entries=legacy_entries,
    )
    expected_runtime = dict(baseline)
    for name in candidate_result["expected_new_migrations"]:
        if name in candidate:
            expected_runtime[name] = candidate[name]
    runtime_missing = sorted(set(expected_runtime) - set(actual))
    runtime_unknown = sorted(set(actual) - set(expected_runtime))
    runtime_checksum_mismatches = sorted(
        name for name in expected_runtime.keys() & actual.keys() if expected_runtime[name] != actual[name]
    )
    result = dict(candidate_result)
    result.update(
        {
            "actual_schema_migrations": len(actual),
            "runtime_expected_schema_migrations": len(expected_runtime),
            "candidate_expected_new_migrations": candidate_result["candidate_pending_migrations"],
            "candidate_pending_migrations": runtime_missing,
            "runtime_missing_files": runtime_missing,
            "runtime_unknown_files": runtime_unknown,
            "runtime_checksum_mismatches": runtime_checksum_mismatches,
            "expected_runtime_ledger_sha256": ledger_sha256(expected_runtime),
            "runtime_ledger_sha256": ledger_sha256(actual),
        }
    )
    result["ok"] = bool(
        result["ok"]
        and not runtime_missing
        and not runtime_unknown
        and not runtime_checksum_mismatches
        and len(actual) == len(expected_runtime)
    )
    return result


def _write_json(path: Path, document: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(document, ensure_ascii=True, sort_keys=True, indent=2) + "\n", encoding="utf-8")
    path.chmod(0o600)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline", type=Path, required=True)
    parser.add_argument("--candidate-dir", type=Path, required=True)
    parser.add_argument("--actual", type=Path)
    parser.add_argument("--expected-new", type=Path, required=True)
    parser.add_argument("--legacy-entries", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args(argv)

    baseline = read_manifest(args.baseline)
    candidate = candidate_manifest(args.candidate_dir)
    expected_new = read_name_list(args.expected_new)
    legacy_entries = read_name_list(args.legacy_entries)
    if args.actual is None:
        result = audit_candidate(
            baseline,
            candidate,
            expected_new=expected_new,
            legacy_entries=legacy_entries,
        )
    else:
        result = audit_runtime(
            baseline,
            candidate,
            read_runtime_manifest(args.actual),
            expected_new=expected_new,
            legacy_entries=legacy_entries,
        )
    _write_json(args.output, result)
    print(json.dumps(result, ensure_ascii=True, sort_keys=True))
    return 0 if result["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
