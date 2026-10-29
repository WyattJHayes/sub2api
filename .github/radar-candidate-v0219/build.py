"""Build the reviewed application commit without mixing in automation changes."""
from __future__ import annotations

import hashlib
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

COMMIT = "59afe2824c5ca16935f53881c0bb4a7e9e6f44da"
TREE = "f74da6e5ad93dca40dabc9956d90c22b54e6775a"
SOURCE_HASH = "2bd6ad3569ff6808141a33872aedd160ef5d38ba95d5d5226abe1830eb107968"
LOCK_HASH = "94649c07b3e6054ca2a1e684fb0a32313351ebb3cdf458b0947290228277fcc8"
SOURCE_LABEL = "io.sub2api.radar.source-sha256"
GIB = 1024**3
REPOSITORIES = {
    "control_plane": "ghcr.io/wyattjhayes/sub2api-radar-control-plane",
    "worker": "ghcr.io/wyattjhayes/sub2api-radar-worker",
}
BASE_IMAGES = {
    "node-image": "node:24-alpine@sha256:ebfe2f90462722a7a4de65e91990e97fe0d401c70e0e762c5b53302f905ec1c1",
    "golang-image": "golang:1.27.3-alpine@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c",
    "alpine-image": "alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6",
    "worker-python-base-image": "python:3.14-slim@sha256:a2b82f3c48559aa0a8446d9af49826b6e2b2016f4cd2afabfe6013ec53729170",
}


def command(args: list[str], cwd: Path | None = None, *, combined: bool = False) -> str:
    return subprocess.run(args, cwd=cwd, check=True, text=True, stdout=subprocess.PIPE,
                          stderr=subprocess.STDOUT if combined else subprocess.PIPE,
                          timeout=600).stdout.strip()


def validate_resources(disk: int, memory: int, *, initial: bool) -> None:
    if disk < (12 if initial else 2) * GIB:
        raise ValueError("available disk is below candidate build reserve")
    if memory < (4 * GIB if initial else 256 * 1024**2):
        raise ValueError("available memory is below candidate build reserve")


def resources(app: Path, *, initial: bool) -> dict[str, int]:
    docker_root = command(["docker", "info", "--format", "{{.DockerRootDir}}"])
    disk = min(shutil.disk_usage(app).free, shutil.disk_usage(docker_root).free)
    memory = next(int(line.split()[1]) * 1024 for line in
                  Path("/proc/meminfo").read_text(encoding="utf-8").splitlines()
                  if line.startswith("MemAvailable:"))
    validate_resources(disk, memory, initial=initial)
    return {"disk_available_bytes": disk, "memory_available_bytes": memory}


def preflight(app: Path) -> dict[str, int]:
    if os.environ.get("GITHUB_REPOSITORY") != "WyattJHayes/sub2api":
        raise ValueError("candidate build is restricted to the authorized repository")
    if os.environ.get("GITHUB_REF") != "refs/heads/main":
        raise ValueError("candidate workflow must run from main")
    if command(["git", "rev-parse", "HEAD"], app) != COMMIT:
        raise ValueError("application commit changed")
    if command(["git", "rev-parse", "HEAD^{tree}"], app) != TREE:
        raise ValueError("application tree changed")
    if command(["git", "status", "--porcelain", "--untracked-files=all"], app):
        raise ValueError("application checkout is not clean")
    sys.path.insert(0, str(app))
    from deploy.radar.source_tree_identity import source_tree_sha256
    if source_tree_sha256(app) != SOURCE_HASH:
        raise ValueError("canonical application source hash changed")
    lock = (app / "radar-worker/requirements.lock").read_bytes()
    if hashlib.sha256(lock).hexdigest() != LOCK_HASH:
        raise ValueError("worker dependency lock changed")
    return resources(app, initial=True)


def validate_record(record: dict) -> None:
    expected = {"schema_version": "radar-v0219-image-record-v1", "version": "0.2.19",
                "revision": COMMIT, "source_commit": COMMIT,
                "source_sha256": SOURCE_HASH, "platform": "linux/amd64"}
    for field, value in expected.items():
        if record.get(field) != value:
            raise ValueError(f"image record {field} does not match reviewed source")
    for role, repository in REPOSITORIES.items():
        item = record[role]
        if item.get("repository") != repository:
            raise ValueError("image record repository changed")
        for field in ("manifest_digest", "config_digest"):
            if not re.fullmatch(r"sha256:[0-9a-f]{64}", item.get(field, "")):
                raise ValueError(f"invalid image {field}")


def validate_inspection(inspection: dict, item: dict) -> None:
    labels = inspection.get("Config", {}).get("Labels", {})
    if (inspection.get("Id") != item["config_digest"]
            or inspection.get("Os") != "linux" or inspection.get("Architecture") != "amd64"
            or labels.get("org.opencontainers.image.revision") != COMMIT
            or labels.get(SOURCE_LABEL) != SOURCE_HASH):
        raise ValueError("image platform, config digest or source labels changed")


def guarded_build(args: list[str], app: Path) -> None:
    started = time.monotonic()
    process = subprocess.Popen(args, cwd=app, start_new_session=True)
    try:
        while process.poll() is None:
            if time.monotonic() - started > 80 * 60:
                raise ValueError("candidate build exceeded 80 minute guard")
            measured = resources(app, initial=False)
            print(json.dumps({"stage": "build", **measured}), flush=True)
            time.sleep(30)
        if process.returncode != 0:
            raise RuntimeError(f"candidate helper failed with exit {process.returncode}")
    finally:
        if process.poll() is None:
            os.killpg(process.pid, signal.SIGTERM)
            try:
                process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait()


def offline_checks(record: dict) -> dict:
    verified = {}
    for role in REPOSITORIES:
        item = record[role]
        reference = item["repository"] + "@" + item["manifest_digest"]
        inspection = json.loads(command(["docker", "image", "inspect", reference]))[0]
        validate_inspection(inspection, item)
        run = ["docker", "run", "--rm", "--network", "none", "--read-only",
               "--memory", "256m", "--pids-limit", "64", "--cap-drop", "ALL",
               "--security-opt", "no-new-privileges", "--tmpfs", "/tmp"]
        if role == "control_plane":
            output = command(run + ["--entrypoint", "/app/sub2api", reference, "-version"],
                             combined=True)
            if "0.2.19" not in output or COMMIT not in output:
                raise ValueError("offline control version or revision mismatch")
        else:
            output = command(run + ["--entrypoint", "python", reference, "-c",
                "import importlib.metadata as m; import sub2api_radar.runner; "
                "import sub2api_radar.grader; import sub2api_radar.statistics.service; "
                "assert m.version('sub2api-radar-worker') == '0.2.19'; print('0.2.19')"])
        verified[role] = {"reference": reference, "offline_runtime_passed": True,
                          "version_output": output, "source_labels_verified": True}
    return verified


def main() -> None:
    app = Path(os.environ["GITHUB_WORKSPACE"]) / "application"
    measured = preflight(app)
    print(json.dumps({"stage": "preflight", **measured}), flush=True)
    if sys.argv[1:] == ["preflight"]:
        return
    if sys.argv[1:] != ["build"]:
        raise ValueError("expected preflight or build")
    output = Path(os.environ["RUNNER_TEMP"]) / "radar-v0219-result"
    output.mkdir(mode=0o700)
    now = datetime.now(timezone.utc)
    tag = "0.2.19-radar-v219-" + now.strftime("%Y%m%dT%H%M%SZ")
    args = [sys.executable, "deploy/radar/build_v0219_ghcr.py", "--version", "0.2.19",
            "--image-tag", tag, "--commit", COMMIT, "--source-sha256", SOURCE_HASH,
            "--date", now.strftime("%Y-%m-%dT%H:%M:%SZ"), "--push",
            "--output", str(output / "image-record.json")]
    for flag, image in BASE_IMAGES.items():
        args.extend(["--" + flag, image])
    guarded_build(args, app)
    record = json.loads((output / "image-record.json").read_text(encoding="utf-8"))
    validate_record(record)
    verification = {"application_commit": COMMIT, "application_tree": TREE,
                    "source_sha256": SOURCE_HASH, "workflow_commit": os.environ["GITHUB_SHA"],
                    "run_id": os.environ["GITHUB_RUN_ID"], "initial_resources": measured,
                    "offline_checks": offline_checks(record)}
    with (output / "verification.json").open("x", encoding="utf-8") as stream:
        json.dump(verification, stream, indent=2)
        stream.write("\n")
    print(json.dumps({"stage": "verified", "image_tag": tag}), flush=True)


if __name__ == "__main__":
    main()

