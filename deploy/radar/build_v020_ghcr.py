#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import tomllib
from contextlib import contextmanager
from dataclasses import dataclass
from datetime import datetime
from pathlib import Path
from typing import Any, Iterator


CONTROL_REPOSITORY = "ghcr.io/wyattjhayes/sub2api-radar-control-plane"
WORKER_REPOSITORY = "ghcr.io/wyattjhayes/sub2api-radar-worker"
PLATFORM = "linux/amd64"
APP_VERSION = "0.2.0"
SCHEMA_VERSION = "radar-v020-image-record-v1"
WORKER_DISTRIBUTION = "sub2api-radar-worker"
WORKER_TEST_FILES = (
    "test_control_plane.py",
    "test_quality.py",
    "test_state.py",
    "test_synthetic_upstream.py",
    "test_worker_loops.py",
)
REPO_ROOT = Path(__file__).resolve().parents[2]
try:
    from deploy.radar.source_tree_identity import (
        create_readonly_source_snapshot,
        source_tree_sha256,
    )
except ModuleNotFoundError:
    from source_tree_identity import create_readonly_source_snapshot, source_tree_sha256

IMAGE_REF_RE = re.compile(r"^.+@sha256:[0-9a-f]{64}$")
DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")
SOURCE_SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
GO_DIRECTIVE_RE = re.compile(r"^go ([0-9]+\.[0-9]+\.[0-9]+)$", re.MULTILINE)
DATE_RE = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$")
IMAGE_TAG_RE = re.compile(r"^0\.2\.0-radar-v20-(\d{8}T\d{6}Z)$")
OCI_REVISION_LABEL = "org.opencontainers.image.revision"
SOURCE_SHA256_LABEL = "io.sub2api.radar.source-sha256"


@dataclass(frozen=True)
class BuildInputs:
    version: str
    image_tag: str
    commit: str
    source_sha256: str
    date: str
    node_image: str
    golang_image: str
    alpine_image: str
    worker_python_base_image: str
    push: bool


def validate_inputs(inputs: BuildInputs) -> None:
    if not COMMIT_RE.fullmatch(inputs.commit):
        raise ValueError("commit must be 40 lowercase hexadecimal characters")
    if not SOURCE_SHA256_RE.fullmatch(inputs.source_sha256):
        raise ValueError("source_sha256 must be 64 lowercase hexadecimal characters")
    # The Git revision identifies the upstream commit while source_sha256
    # identifies the sealed, post-patch source tree. They are independent
    # identities and must both be recorded rather than forced to share a prefix.
    if not DATE_RE.fullmatch(inputs.date):
        raise ValueError("date must be UTC in YYYY-MM-DDTHH:MM:SSZ format")
    try:
        parsed_date = datetime.strptime(inputs.date, "%Y-%m-%dT%H:%M:%SZ")
    except ValueError as exc:
        raise ValueError("date must be UTC in YYYY-MM-DDTHH:MM:SSZ format") from exc

    if inputs.version != APP_VERSION:
        raise ValueError(f"version must equal {APP_VERSION}")
    image_tag_match = IMAGE_TAG_RE.fullmatch(inputs.image_tag)
    if image_tag_match is None:
        raise ValueError("image_tag must use 0.2.0-radar-v20-YYYYMMDDTHHMMSSZ format")
    if image_tag_match.group(1) != parsed_date.strftime("%Y%m%dT%H%M%SZ"):
        raise ValueError("image_tag timestamp must match the UTC build date")

    for field_name in (
        "node_image",
        "golang_image",
        "alpine_image",
        "worker_python_base_image",
    ):
        if not IMAGE_REF_RE.fullmatch(getattr(inputs, field_name)):
            raise ValueError(f"{field_name} must be digest-qualified with lowercase sha256")
    if not inputs.push:
        raise ValueError("--push is required for private GHCR candidate builds")


def validate_current_source(inputs: BuildInputs, source_root: Path = REPO_ROOT) -> None:
    go_version = go_module_version(source_root)
    if not inputs.golang_image.startswith(f"golang:{go_version}-alpine@sha256:"):
        raise ValueError("golang image must match backend go.mod version")
    worker_tests = source_root / "radar-worker" / "tests"
    if any(not (worker_tests / name).is_file() for name in WORKER_TEST_FILES):
        raise ValueError("worker test suite is missing required files")
    expected = source_tree_sha256(source_root)
    if inputs.source_sha256 != expected:
        raise ValueError("source_sha256 must match the current source tree")
    if worker_package_version(source_root) != inputs.version:
        raise ValueError("worker package version must match requested release")


def target_tag(repository: str, version: str) -> str:
    if repository not in {CONTROL_REPOSITORY, WORKER_REPOSITORY}:
        raise ValueError("repository must be an approved private GHCR target")
    return f"{repository}:{version}"


def digest_reference(repository: str, digest: str) -> str:
    if not repository.startswith("ghcr.io/"):
        raise ValueError("repository must be under ghcr.io")
    if repository not in {CONTROL_REPOSITORY, WORKER_REPOSITORY}:
        raise ValueError("repository must be an approved private GHCR target")
    validate_digest(digest, "manifest digest")
    return f"{repository}@{digest}"


def control_plane_command(inputs: BuildInputs, metadata_path: Path) -> list[str]:
    validate_inputs(inputs)
    return [
        "docker",
        "buildx",
        "build",
        "--platform",
        PLATFORM,
        "-f",
        "deploy/Dockerfile.radar-control-staging",
        "--metadata-file",
        str(metadata_path),
        "--tag",
        target_tag(CONTROL_REPOSITORY, inputs.image_tag),
        "--label",
        f"{OCI_REVISION_LABEL}={inputs.commit}",
        "--label",
        f"{SOURCE_SHA256_LABEL}={inputs.source_sha256}",
        "--build-arg",
        f"VERSION={inputs.version}",
        "--build-arg",
        f"COMMIT={inputs.commit}",
        "--build-arg",
        f"DATE={inputs.date}",
        "--build-arg",
        f"NODE_IMAGE={inputs.node_image}",
        "--build-arg",
        f"GOLANG_IMAGE={inputs.golang_image}",
        "--build-arg",
        f"ALPINE_IMAGE={inputs.alpine_image}",
        "--push",
        ".",
    ]


def worker_command(inputs: BuildInputs, metadata_path: Path) -> list[str]:
    validate_inputs(inputs)
    return [
        "docker",
        "buildx",
        "build",
        "--platform",
        PLATFORM,
        "-f",
        "radar-worker/Dockerfile",
        "--metadata-file",
        str(metadata_path),
        "--tag",
        target_tag(WORKER_REPOSITORY, inputs.image_tag),
        "--label",
        f"{OCI_REVISION_LABEL}={inputs.commit}",
        "--label",
        f"{SOURCE_SHA256_LABEL}={inputs.source_sha256}",
        "--build-arg",
        f"RADAR_WORKER_PYTHON_BASE_IMAGE={inputs.worker_python_base_image}",
        "--push",
        "radar-worker",
    ]


def prepare_worker_build_context(
    source_snapshot: Path,
    worker_context: Path,
    python_base_image: str,
) -> Path:
    """Create a derived context with hash-verified wheels for the target runtime."""
    worker_source = source_snapshot / "radar-worker"
    if not worker_source.is_dir():
        raise ValueError("sealed source snapshot is missing radar-worker")
    if worker_context.exists():
        raise ValueError("worker build context path must not already exist")

    shutil.copytree(worker_source, worker_context)
    worker_context.chmod(0o700)
    requirements_lock = worker_context / "requirements.lock"
    if not requirements_lock.is_file():
        raise ValueError("worker build context is missing requirements.lock")
    wheelhouse = worker_context / "wheelhouse"
    wheelhouse.mkdir(mode=0o700)

    run_checked(
        [
            "docker",
            "run",
            "--rm",
            "--platform",
            PLATFORM,
            "--user",
            f"{os.getuid()}:{os.getgid()}",
            "--volume",
            f"{requirements_lock.resolve()}:/requirements.lock:ro",
            "--volume",
            f"{wheelhouse.resolve()}:/wheelhouse",
            python_base_image,
            "python",
            "-m",
            "pip",
            "download",
            "--disable-pip-version-check",
            "--no-cache-dir",
            "--require-hashes",
            "--only-binary=:all:",
            "--dest",
            "/wheelhouse",
            "--requirement",
            "/requirements.lock",
        ]
    )
    wheels = list(wheelhouse.iterdir())
    if not wheels:
        raise ValueError("worker wheelhouse is empty")
    if any(path.is_symlink() or not path.is_file() or path.suffix != ".whl" for path in wheels):
        raise ValueError("worker wheelhouse must contain only regular .whl files")
    return worker_context


@contextmanager
def temporary_build_workspace(source_root: Path = REPO_ROOT) -> Iterator[Path]:
    with tempfile.TemporaryDirectory(
        prefix="radar-v020-build-",
        dir=source_root.resolve().parent,
    ) as directory:
        yield Path(directory)


def validate_digest(value: object, label: str) -> str:
    if not isinstance(value, str) or not DIGEST_RE.fullmatch(value):
        raise ValueError(f"{label} must be a lowercase sha256 digest")
    return value


def resolve_config_digest(image_name: str, manifest_digest: str) -> str:
    """Resolve the amd64 config digest when Buildx omits it from metadata."""
    raw = run_checked(
        [
            "docker",
            "buildx",
            "imagetools",
            "inspect",
            f"{image_name}@{manifest_digest}",
            "--raw",
        ]
    )
    try:
        document = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise ValueError("OCI manifest response must be valid JSON") from exc
    if not isinstance(document, dict):
        raise ValueError("OCI manifest response must be an object")
    if document.get("mediaType") == "application/vnd.oci.image.index.v1+json":
        manifests = document.get("manifests")
        if not isinstance(manifests, list):
            raise ValueError("OCI image index is missing manifests")
        amd64 = next(
            (
                item
                for item in manifests
                if isinstance(item, dict)
                and item.get("platform", {}).get("os") == "linux"
                and item.get("platform", {}).get("architecture") == "amd64"
            ),
            None,
        )
        if not isinstance(amd64, dict) or not isinstance(amd64.get("digest"), str):
            raise ValueError("OCI image index has no linux/amd64 manifest")
        return resolve_config_digest(image_name, validate_digest(amd64["digest"], "platform manifest digest"))
    config = document.get("config")
    if not isinstance(config, dict):
        raise ValueError("OCI manifest is missing config")
    return validate_digest(config.get("digest"), "containerimage.config.digest")


def parse_metadata(path: Path) -> tuple[str, str]:
    try:
        document = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise ValueError(f"cannot read Buildx metadata from {path}") from exc
    if not isinstance(document, dict):
        raise ValueError("Buildx metadata must be a JSON object")
    manifest = validate_digest(document.get("containerimage.digest"), "containerimage.digest")
    config_value = document.get("containerimage.config.digest")
    if config_value is None:
        image_name = document.get("image.name")
        if not isinstance(image_name, str) or not image_name:
            raise ValueError("containerimage.config.digest")
        config = resolve_config_digest(image_name, manifest)
    else:
        config = validate_digest(config_value, "containerimage.config.digest")
    return manifest, config


def validate_version_output(output: str, expected: str, label: str) -> str:
    normalized = output.strip()
    pattern = re.compile(rf"(?<![0-9]){re.escape(expected)}(?![0-9])")
    if not normalized or pattern.search(normalized) is None:
        raise ValueError(f"{label} version output does not contain {expected}")
    return expected


def worker_package_version(source_root: Path = REPO_ROOT) -> str:
    with (source_root / "radar-worker" / "pyproject.toml").open("rb") as stream:
        document = tomllib.load(stream)
    version = document.get("project", {}).get("version")
    if not isinstance(version, str) or not version:
        raise ValueError("radar-worker project version is missing")
    return version


def go_module_version(source_root: Path = REPO_ROOT) -> str:
    content = (source_root / "backend" / "go.mod").read_text(encoding="utf-8")
    match = GO_DIRECTIVE_RE.search(content)
    if match is None:
        raise ValueError("backend go.mod go directive is missing")
    return match.group(1)


def image_record(
    repository: str,
    tag: str,
    manifest_digest: str,
    config_digest: str,
    version_output: str,
) -> dict[str, str]:
    return {
        "repository": repository,
        "tag": tag,
        "manifest_digest": validate_digest(manifest_digest, "manifest digest"),
        "config_digest": validate_digest(config_digest, "config digest"),
        "version_output": version_output,
    }


def build_record(
    inputs: BuildInputs,
    *,
    control_manifest: str,
    control_config: str,
    control_version: str,
    worker_manifest: str,
    worker_config: str,
    worker_version: str,
) -> dict[str, Any]:
    validate_inputs(inputs)
    return {
        "schema_version": SCHEMA_VERSION,
        "source_sha256": inputs.source_sha256,
        "revision": inputs.commit,
        "source_commit": inputs.commit,
        "version": inputs.version,
        "image_tag": inputs.image_tag,
        "build_date": inputs.date,
        "platform": PLATFORM,
        "control_plane": image_record(
            CONTROL_REPOSITORY,
            target_tag(CONTROL_REPOSITORY, inputs.image_tag),
            control_manifest,
            control_config,
            validate_version_output(control_version, inputs.version, "control-plane"),
        ),
        "worker": image_record(
            WORKER_REPOSITORY,
            target_tag(WORKER_REPOSITORY, inputs.image_tag),
            worker_manifest,
            worker_config,
            validate_version_output(worker_version, inputs.version, "Worker"),
        ),
    }


def write_record(path: Path, record: dict[str, Any]) -> None:
    body = json.dumps(record, ensure_ascii=False, indent=2, sort_keys=True) + "\n"
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            descriptor = -1
            stream.write(body)
            stream.flush()
            os.fsync(stream.fileno())
    finally:
        if descriptor >= 0:
            os.close(descriptor)
    path.chmod(0o600)


def run_checked(command: list[str], *, cwd: Path = REPO_ROOT) -> str:
    completed = subprocess.run(
        command,
        cwd=cwd,
        check=False,
        capture_output=True,
        text=True,
    )
    if completed.returncode != 0:
        detail = completed.stderr.strip() or completed.stdout.strip() or "command failed"
        raise RuntimeError(f"{' '.join(command)}: {detail}")
    return completed.stdout


def verify_control_plane(
    repository: str,
    digest: str,
    expected_config_digest: str,
    expected_version: str,
) -> str:
    reference = digest_reference(repository, digest)
    expected_config_digest = validate_digest(expected_config_digest, "expected config digest")
    run_checked(["docker", "pull", reference])
    actual_config_digest = resolve_config_digest(repository, digest)
    if actual_config_digest != expected_config_digest:
        raise ValueError("image config digest does not match build metadata")
    output = run_checked(
        [
            "docker",
            "run",
            "--rm",
            "--entrypoint",
            "/bin/sh",
            reference,
            "-c",
            "/app/sub2api -version 2>&1",
        ]
    )
    return validate_version_output(output, expected_version, "control-plane")


def verify_worker(repository: str, digest: str, expected_version: str) -> str:
    reference = digest_reference(repository, digest)
    run_checked(["docker", "pull", reference])
    output = run_checked(
        [
            "docker",
            "run",
            "--rm",
            "--entrypoint",
            "python",
            reference,
            "-c",
            (
                "import importlib.metadata as m; "
                f"print(m.version({WORKER_DISTRIBUTION!r}))"
            ),
        ]
    )
    return validate_version_output(output, expected_version, "Worker")


def verify_image_provenance(
    repository: str,
    digest: str,
    expected_config_digest: str,
    inputs: BuildInputs,
) -> None:
    reference = digest_reference(repository, digest)
    expected_config_digest = validate_digest(expected_config_digest, "expected config digest")
    run_checked(["docker", "pull", reference])
    actual_config_digest = resolve_config_digest(repository, digest)
    if actual_config_digest != expected_config_digest:
        raise ValueError("image config digest does not match build metadata")
    output = run_checked(
        [
            "docker",
            "image",
            "inspect",
            "--format",
            "{{json .}}",
            reference,
        ]
    )
    try:
        inspection = json.loads(output)
    except json.JSONDecodeError as exc:
        raise ValueError("image inspection must be valid JSON") from exc
    if not isinstance(inspection, dict):
        raise ValueError("image inspection must be an object")
    config = inspection.get("Config")
    if not isinstance(config, dict):
        raise ValueError("image config must be an object")
    labels = config.get("Labels")
    if not isinstance(labels, dict):
        raise ValueError("image labels must be an object")
    if labels.get(OCI_REVISION_LABEL) != inputs.commit:
        raise ValueError("image revision label does not match source revision")
    if labels.get(SOURCE_SHA256_LABEL) != inputs.source_sha256:
        raise ValueError("image source hash label does not match source_sha256")


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Build and record immutable Sub2API v0.2.0 Radar private GHCR candidates."
    )
    parser.add_argument("--version", required=True)
    parser.add_argument("--image-tag", required=True)
    parser.add_argument("--commit", required=True)
    parser.add_argument("--source-sha256", required=True)
    parser.add_argument("--date", required=True)
    parser.add_argument("--node-image", required=True)
    parser.add_argument("--golang-image", required=True)
    parser.add_argument("--alpine-image", required=True)
    parser.add_argument("--worker-python-base-image", required=True)
    parser.add_argument("--push", action="store_true")
    parser.add_argument("--output", type=Path, required=True)
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv or sys.argv[1:])
    inputs = BuildInputs(
        version=args.version,
        image_tag=args.image_tag,
        commit=args.commit,
        source_sha256=args.source_sha256,
        date=args.date,
        node_image=args.node_image,
        golang_image=args.golang_image,
        alpine_image=args.alpine_image,
        worker_python_base_image=args.worker_python_base_image,
        push=args.push,
    )
    try:
        validate_inputs(inputs)
        validate_current_source(inputs)
        if args.output.exists():
            raise ValueError(f"output already exists: {args.output}")
        with temporary_build_workspace(REPO_ROOT) as workspace:
            source_snapshot = workspace / "source"
            snapshot_sha256 = create_readonly_source_snapshot(REPO_ROOT, source_snapshot)
            if inputs.source_sha256 != snapshot_sha256:
                raise ValueError("source_sha256 must match the sealed source snapshot")
            control_metadata = workspace / "control.json"
            worker_metadata = workspace / "worker.json"
            run_checked(control_plane_command(inputs, control_metadata), cwd=source_snapshot)
            worker_context = prepare_worker_build_context(
                source_snapshot,
                workspace / "worker-build" / "radar-worker",
                inputs.worker_python_base_image,
            )
            run_checked(
                worker_command(inputs, worker_metadata),
                cwd=worker_context.parent,
            )
            control_manifest, control_config = parse_metadata(control_metadata)
            worker_manifest, worker_config = parse_metadata(worker_metadata)
            worker_expected = inputs.version
            control_version = verify_control_plane(
                CONTROL_REPOSITORY, control_manifest, control_config, inputs.version
            )
            worker_version = verify_worker(
                WORKER_REPOSITORY, worker_manifest, worker_expected
            )
            verify_image_provenance(
                CONTROL_REPOSITORY, control_manifest, control_config, inputs
            )
            verify_image_provenance(
                WORKER_REPOSITORY, worker_manifest, worker_config, inputs
            )
            record = build_record(
                inputs,
                control_manifest=control_manifest,
                control_config=control_config,
                control_version=control_version,
                worker_manifest=worker_manifest,
                worker_config=worker_config,
                worker_version=worker_version,
            )
            write_record(args.output, record)
    except (OSError, RuntimeError, ValueError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
