#!/usr/bin/env python3
"""Canonical, content-addressed evidence primitives for Radar releases."""

from __future__ import annotations

import hashlib
import json
import os
import re
import stat
import tempfile
import uuid
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any


ENVELOPE_SCHEMA_VERSION = "radar-production-evidence-envelope-v1"
DEFAULT_RELEASE_VERSION = "0.1.178"
APP_VERSION = os.environ.get("RADAR_RELEASE_VERSION", DEFAULT_RELEASE_VERSION)
IMAGE_RECORD_SCHEMA_VERSION = f"radar-v{APP_VERSION.replace('.', '')}-image-record-v1"
PLATFORM = "linux/amd64"
CONTROL_REPOSITORY = "ghcr.io/wyattjhayes/sub2api-radar-control-plane"
WORKER_REPOSITORY = "ghcr.io/wyattjhayes/sub2api-radar-worker"
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
IMAGE_DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
IMAGE_TAG_RE = re.compile(
    rf"^{re.escape(APP_VERSION)}-radar-v\d+-\d{{8}}T\d{{6}}Z$"
)
REVISION_RE = re.compile(r"^[0-9a-f]{40}$")
UTC_TIMESTAMP_RE = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$")
REQUIRED_BINDING_KEYS = frozenset(
    {"candidate_image_record_sha256", "target_id", "host_fingerprint"}
)
ENVELOPE_KEYS = frozenset(
    {
        "schema_version",
        "evidence_type",
        "release_id",
        "started_at",
        "finished_at",
        "binding",
        "input_evidence_sha256",
        "payload",
        "payload_sha256",
        "evidence_sha256",
    }
)
EVIDENCE_TYPES = frozenset(
    {
        "candidate-image-record",
        "target-snapshot",
        "preflight",
        "isolation",
        "rehearsal",
        "migration-rehearsal",
        "browser-closure",
        "authorization",
        "backup",
        "promotion",
        "post-promotion-snapshot",
        "smoke",
        "rollback",
        "restoration",
        "closure",
    }
)


def canonical_json_bytes(value: object) -> bytes:
    """Return the sole JSON representation used for evidence hashes."""
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")


def canonical_sha256(value: object) -> str:
    return hashlib.sha256(canonical_json_bytes(value)).hexdigest()


def file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        while block := stream.read(1024 * 1024):
            digest.update(block)
    return digest.hexdigest()


def build_envelope(
    *,
    evidence_type: str,
    release_id: str,
    started_at: str,
    finished_at: str,
    binding: dict[str, str],
    input_evidence_sha256: dict[str, str],
    payload: dict[str, Any],
) -> dict[str, Any]:
    document: dict[str, Any] = {
        "schema_version": ENVELOPE_SCHEMA_VERSION,
        "evidence_type": evidence_type,
        "release_id": release_id,
        "started_at": started_at,
        "finished_at": finished_at,
        "binding": binding,
        "input_evidence_sha256": input_evidence_sha256,
        "payload": payload,
        "payload_sha256": canonical_sha256(payload),
    }
    document["evidence_sha256"] = canonical_sha256(document)
    return document


def validate_envelope(
    document: dict[str, Any],
    *,
    expected_type: str,
    now: datetime | None = None,
    max_future_skew_seconds: int = 60,
) -> dict[str, Any]:
    if not isinstance(document, dict):
        raise ValueError("evidence document must be a JSON object")
    if set(document) != ENVELOPE_KEYS:
        raise ValueError("evidence envelope keys are invalid")
    if document.get("schema_version") != ENVELOPE_SCHEMA_VERSION:
        raise ValueError("schema_version is not supported")

    evidence_type = document.get("evidence_type")
    if evidence_type != expected_type or evidence_type not in EVIDENCE_TYPES:
        raise ValueError("evidence_type is not allowed")

    release_id = document.get("release_id")
    if not isinstance(release_id, str):
        raise ValueError("release_id must be a UUID")
    try:
        uuid.UUID(release_id)
    except ValueError as exc:
        raise ValueError("release_id must be a UUID") from exc

    started_at = _parse_utc_timestamp(document.get("started_at"), "started_at")
    finished_at = _parse_utc_timestamp(document.get("finished_at"), "finished_at")
    if started_at > finished_at:
        raise ValueError("started_at must not be after finished_at")
    effective_now = now or datetime.now(UTC)
    if effective_now.tzinfo is None:
        raise ValueError("now must be timezone-aware")
    if finished_at > effective_now.astimezone(UTC) + timedelta(seconds=max_future_skew_seconds):
        raise ValueError("finished_at exceeds permitted future clock skew")

    _validate_binding(document.get("binding"))
    _validate_hash_mapping(document.get("input_evidence_sha256"), "input_evidence_sha256")

    payload = document.get("payload")
    if not isinstance(payload, dict):
        raise ValueError("payload must be a JSON object")
    payload_sha256 = document.get("payload_sha256")
    if not _is_sha256(payload_sha256):
        raise ValueError("payload_sha256 must be a lowercase SHA256")
    if payload_sha256 != canonical_sha256(payload):
        raise ValueError("payload_sha256 does not match payload")

    evidence_sha256 = document.get("evidence_sha256")
    if not _is_sha256(evidence_sha256):
        raise ValueError("evidence_sha256 must be a lowercase SHA256")
    without_evidence_hash = dict(document)
    del without_evidence_hash["evidence_sha256"]
    if evidence_sha256 != canonical_sha256(without_evidence_hash):
        raise ValueError("evidence_sha256 does not match envelope")
    return document


def validate_predecessor(child: dict[str, Any], name: str, parent: dict[str, Any]) -> None:
    child_type = child.get("evidence_type") if isinstance(child, dict) else ""
    parent_type = parent.get("evidence_type") if isinstance(parent, dict) else ""
    if not isinstance(child_type, str) or not isinstance(parent_type, str):
        raise ValueError("predecessor evidence type is invalid")
    validate_envelope(child, expected_type=child_type)
    validate_envelope(parent, expected_type=parent_type)
    if child["release_id"] != parent["release_id"]:
        raise ValueError("release_id predecessor binding does not match")
    input_hashes = child["input_evidence_sha256"]
    if input_hashes.get(name) != parent["evidence_sha256"]:
        raise ValueError(f"{name} predecessor evidence_sha256 does not match")
    for key in REQUIRED_BINDING_KEYS:
        if child["binding"][key] != parent["binding"][key]:
            raise ValueError(f"{key} predecessor binding does not match")
    child_started_at = _parse_utc_timestamp(child["started_at"], "started_at")
    parent_finished_at = _parse_utc_timestamp(parent["finished_at"], "finished_at")
    if child_started_at < parent_finished_at:
        raise ValueError("predecessor time ordering is invalid")


def write_private_json(path: Path, document: dict[str, Any]) -> None:
    _require_private_parent(path.parent)
    body = json.dumps(document, ensure_ascii=False, indent=2, sort_keys=True) + "\n"
    descriptor, temporary_name = tempfile.mkstemp(prefix=".radar-evidence-", dir=path.parent)
    temporary = Path(temporary_name)
    try:
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            descriptor = -1
            stream.write(body)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        os.chmod(path, 0o600)
    finally:
        if descriptor >= 0:
            os.close(descriptor)
        temporary.unlink(missing_ok=True)


def load_private_envelope(
    path: Path,
    *,
    expected_type: str,
    now: datetime | None = None,
) -> dict[str, Any]:
    return validate_envelope(_load_private_json(path), expected_type=expected_type, now=now)


def load_private_json(path: Path) -> dict[str, Any]:
    """Load a private JSON object while applying the evidence file contract."""
    return _load_private_json(path)


def load_candidate_image_record(
    path: Path,
    *,
    expected_source_sha256: str | None = None,
) -> dict[str, Any]:
    record = _load_private_json(path)
    allowed_keys = {
        "schema_version",
        "source_sha256",
        "source_commit",
        "revision",
        "version",
        "image_tag",
        "build_date",
        "platform",
        "control_plane",
        "worker",
    }
    if set(record) - allowed_keys:
        raise ValueError("candidate image record contains unsupported keys")
    if record.get("schema_version") != IMAGE_RECORD_SCHEMA_VERSION:
        raise ValueError("candidate image record schema_version is invalid")
    source_sha256 = record.get("source_sha256")
    revision = record.get("revision")
    if not _is_sha256(source_sha256):
        raise ValueError("candidate image record source_sha256 is invalid")
    if not isinstance(revision, str) or not REVISION_RE.fullmatch(revision):
        raise ValueError("candidate image record revision is invalid")
    source_commit = record.get("source_commit")
    if not isinstance(source_commit, str) or source_commit != revision:
        raise ValueError("candidate image record source_commit must match revision")
    if expected_source_sha256 is not None and source_sha256 != expected_source_sha256:
        raise ValueError("candidate image record source_sha256 does not match expected source")
    if record.get("version") != APP_VERSION:
        raise ValueError("candidate image record version is invalid")
    image_tag = record.get("image_tag")
    if not isinstance(image_tag, str) or IMAGE_TAG_RE.fullmatch(image_tag) is None:
        raise ValueError("candidate image record image_tag is invalid")
    if record.get("platform") != PLATFORM:
        raise ValueError("candidate image record platform is invalid")
    _validate_image_record_image(
        record.get("control_plane"), CONTROL_REPOSITORY, image_tag, "control-plane"
    )
    _validate_image_record_image(record.get("worker"), WORKER_REPOSITORY, image_tag, "worker")
    return record


def _validate_binding(value: object) -> None:
    if not isinstance(value, dict) or set(value) != REQUIRED_BINDING_KEYS:
        raise ValueError("binding must contain the exact required keys")
    for key in REQUIRED_BINDING_KEYS:
        if not _is_sha256(value.get(key)):
            raise ValueError(f"binding {key} must be a lowercase SHA256")


def _validate_hash_mapping(value: object, label: str) -> None:
    if not isinstance(value, dict):
        raise ValueError(f"{label} must be an object")
    for key, digest in value.items():
        if not isinstance(key, str) or not key or not _is_sha256(digest):
            raise ValueError(f"{label} must contain non-empty names and lowercase SHA256 values")


def _validate_image_record_image(
    value: object, repository: str, image_tag: str, label: str
) -> None:
    if not isinstance(value, dict):
        raise ValueError(f"candidate image record {label} must be an object")
    if value.get("repository") != repository:
        raise ValueError(f"candidate image record {label} repository is invalid")
    if value.get("tag") != f"{repository}:{image_tag}":
        raise ValueError(f"candidate image record {label} tag is invalid")
    manifest = value.get("manifest_digest")
    config = value.get("config_digest")
    if not isinstance(manifest, str) or not IMAGE_DIGEST_RE.fullmatch(manifest):
        raise ValueError(f"candidate image record {label} manifest_digest is invalid")
    if not isinstance(config, str) or not IMAGE_DIGEST_RE.fullmatch(config):
        raise ValueError(f"candidate image record {label} config_digest is invalid")
    if manifest == config:
        raise ValueError(f"candidate image record {label} manifest and config digests must differ")
    version_output = value.get("version_output")
    version_pattern = re.compile(rf"(?<![0-9]){re.escape(APP_VERSION)}(?![0-9])")
    if not isinstance(version_output, str) or version_pattern.search(version_output) is None:
        raise ValueError(f"candidate image record {label} version_output is invalid")


def _load_private_json(path: Path) -> dict[str, Any]:
    _require_private_parent(path.parent)
    try:
        path_stat = path.lstat()
    except OSError as exc:
        raise ValueError(f"private evidence path is unavailable: {exc}") from exc
    if stat.S_ISLNK(path_stat.st_mode):
        raise ValueError("private evidence path must not be a symlink")
    if not stat.S_ISREG(path_stat.st_mode):
        raise ValueError("private evidence path must be a regular file")
    if stat.S_IMODE(path_stat.st_mode) != 0o600:
        raise ValueError("private evidence file mode must be 0600")
    flags = os.O_RDONLY
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(path, flags)
    except OSError as exc:
        raise ValueError(f"private evidence path cannot be opened safely: {exc}") from exc
    try:
        opened_stat = os.fstat(descriptor)
        if (opened_stat.st_dev, opened_stat.st_ino) != (path_stat.st_dev, path_stat.st_ino):
            raise ValueError("private evidence path changed while opening")
        with os.fdopen(descriptor, "r", encoding="utf-8") as stream:
            descriptor = -1
            document = json.load(stream)
    except json.JSONDecodeError as exc:
        raise ValueError("private evidence file is not valid JSON") from exc
    finally:
        if descriptor >= 0:
            os.close(descriptor)
    if not isinstance(document, dict):
        raise ValueError("private evidence file must contain a JSON object")
    return document


def _require_private_parent(path: Path) -> None:
    try:
        parent_stat = path.lstat()
    except OSError as exc:
        raise ValueError(f"private evidence directory is unavailable: {exc}") from exc
    if stat.S_ISLNK(parent_stat.st_mode):
        raise ValueError("private evidence directory must not be a symlink")
    if not stat.S_ISDIR(parent_stat.st_mode) or stat.S_IMODE(parent_stat.st_mode) != 0o700:
        raise ValueError("private evidence directory mode must be 0700")


def _parse_utc_timestamp(value: object, label: str) -> datetime:
    if not isinstance(value, str) or not UTC_TIMESTAMP_RE.fullmatch(value):
        raise ValueError(f"{label} must be an RFC3339 UTC timestamp")
    try:
        return datetime.strptime(value, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=UTC)
    except ValueError as exc:
        raise ValueError(f"{label} must be an RFC3339 UTC timestamp") from exc


def _is_sha256(value: object) -> bool:
    return isinstance(value, str) and SHA256_RE.fullmatch(value) is not None
