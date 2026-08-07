#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
import tempfile
import tomllib
from dataclasses import dataclass
from datetime import datetime
from pathlib import Path
from typing import Any


CONTROL_REPOSITORY = "ghcr.io/a895411690/sub2api-radar-control-plane"
WORKER_REPOSITORY = "ghcr.io/a895411690/sub2api-radar-worker"
PLATFORM = "linux/amd64"
SCHEMA_VERSION = "radar-v01171-image-record-v1"
WORKER_DISTRIBUTION = "sub2api-radar-worker"
REPO_ROOT = Path(__file__).resolve().parents[2]

IMAGE_REF_RE = re.compile(r"^.+@sha256:[0-9a-f]{64}$")
DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")
DATE_RE = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$")
VERSION_RE = re.compile(r"^0\.1\.171-radar-v11-(\d{8}T\d{6}Z)$")


@dataclass(frozen=True)
class BuildInputs:
    version: str
    commit: str
    date: str
    node_image: str
    golang_image: str
    alpine_image: str
    worker_python_base_image: str
    push: bool


def validate_inputs(inputs: BuildInputs) -> None:
    if not COMMIT_RE.fullmatch(inputs.commit):
        raise ValueError("commit must be 40 lowercase hexadecimal characters")
    if not DATE_RE.fullmatch(inputs.date):
        raise ValueError("date must be UTC in YYYY-MM-DDTHH:MM:SSZ format")
    try:
        parsed_date = datetime.strptime(inputs.date, "%Y-%m-%dT%H:%M:%SZ")
    except ValueError as exc:
        raise ValueError("date must be UTC in YYYY-MM-DDTHH:MM:SSZ format") from exc

    version_match = VERSION_RE.fullmatch(inputs.version)
    if version_match is None:
        raise ValueError("version must use 0.1.171-radar-v11-YYYYMMDDTHHMMSSZ format")
    if version_match.group(1) != parsed_date.strftime("%Y%m%dT%H%M%SZ"):
        raise ValueError("version timestamp must match the UTC build date")

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
        target_tag(CONTROL_REPOSITORY, inputs.version),
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
        target_tag(WORKER_REPOSITORY, inputs.version),
        "--build-arg",
        f"RADAR_WORKER_PYTHON_BASE_IMAGE={inputs.worker_python_base_image}",
        "--push",
        "radar-worker",
    ]


def validate_digest(value: object, label: str) -> str:
    if not isinstance(value, str) or not DIGEST_RE.fullmatch(value):
        raise ValueError(f"{label} must be a lowercase sha256 digest")
    return value


def parse_metadata(path: Path) -> tuple[str, str]:
    try:
        document = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise ValueError(f"cannot read Buildx metadata from {path}") from exc
    if not isinstance(document, dict):
        raise ValueError("Buildx metadata must be a JSON object")
    manifest = validate_digest(document.get("containerimage.digest"), "containerimage.digest")
    config = validate_digest(
        document.get("containerimage.config.digest"), "containerimage.config.digest"
    )
    return manifest, config


def validate_version_output(output: str, expected: str, label: str) -> str:
    normalized = output.strip()
    if not normalized or expected not in normalized:
        raise ValueError(f"{label} version output does not contain {expected}")
    return normalized


def worker_package_version() -> str:
    with (REPO_ROOT / "radar-worker" / "pyproject.toml").open("rb") as stream:
        document = tomllib.load(stream)
    version = document.get("project", {}).get("version")
    if not isinstance(version, str) or not version:
        raise ValueError("radar-worker project version is missing")
    return version


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
        "source_commit": inputs.commit,
        "version": inputs.version,
        "build_date": inputs.date,
        "platform": PLATFORM,
        "control_plane": image_record(
            CONTROL_REPOSITORY,
            target_tag(CONTROL_REPOSITORY, inputs.version),
            control_manifest,
            control_config,
            validate_version_output(control_version, inputs.version, "control-plane"),
        ),
        "worker": image_record(
            WORKER_REPOSITORY,
            target_tag(WORKER_REPOSITORY, inputs.version),
            worker_manifest,
            worker_config,
            validate_version_output(worker_version, worker_package_version(), "Worker"),
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


def run_checked(command: list[str]) -> str:
    completed = subprocess.run(
        command,
        cwd=REPO_ROOT,
        check=False,
        capture_output=True,
        text=True,
    )
    if completed.returncode != 0:
        detail = completed.stderr.strip() or completed.stdout.strip() or "command failed"
        raise RuntimeError(f"{' '.join(command)}: {detail}")
    return completed.stdout


def verify_control_plane(repository: str, digest: str, expected_version: str) -> str:
    reference = digest_reference(repository, digest)
    run_checked(["docker", "pull", reference])
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


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Build and record immutable Sub2API v0.1.171 private GHCR candidates."
    )
    parser.add_argument("--version", required=True)
    parser.add_argument("--commit", required=True)
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
        commit=args.commit,
        date=args.date,
        node_image=args.node_image,
        golang_image=args.golang_image,
        alpine_image=args.alpine_image,
        worker_python_base_image=args.worker_python_base_image,
        push=args.push,
    )
    try:
        validate_inputs(inputs)
        if args.output.exists():
            raise ValueError(f"output already exists: {args.output}")
        with tempfile.TemporaryDirectory(prefix="radar-v01171-build-") as directory:
            control_metadata = Path(directory) / "control.json"
            worker_metadata = Path(directory) / "worker.json"
            run_checked(control_plane_command(inputs, control_metadata))
            run_checked(worker_command(inputs, worker_metadata))
            control_manifest, control_config = parse_metadata(control_metadata)
            worker_manifest, worker_config = parse_metadata(worker_metadata)
            worker_expected = worker_package_version()
            control_version = verify_control_plane(
                CONTROL_REPOSITORY, control_manifest, inputs.version
            )
            worker_version = verify_worker(
                WORKER_REPOSITORY, worker_manifest, worker_expected
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
