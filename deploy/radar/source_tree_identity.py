"""Canonical source-tree identity shared by candidate build and prerelease gates."""

from __future__ import annotations

import hashlib
import shutil
from pathlib import Path


SOURCE_EXCLUDED_DIRECTORY_COMPONENTS = (
    ".git",
    ".superpowers",
    ".pytest_cache",
    ".mypy_cache",
    ".ruff_cache",
    "node_modules",
    "work",
    ".venv",
    "__pycache__",
    "dist",
    "test-results",
    "release-evidence",
)
SOURCE_EXCLUDED_FILE_SUFFIXES = (".tsbuildinfo",)
SENSITIVE_SOURCE_FILE_NAMES = frozenset(
    {
        ".env",
        ".git-credentials",
        ".netrc",
        ".npmrc",
        ".pypirc",
        "config.yaml",
        "config.local.yaml",
    }
)
GENERATED_TASK_ARTIFACT_ROOT = Path("docs/superpowers/sdd/2026-08-11-local-isolated-radar-release-readiness")
GENERATED_TASK_ARTIFACT_PATTERNS = (
    "progress.md",
    "task-*-after.sha256",
    "task-*-before.sha256",
    "task-*-final.sha256",
    "task-*.diff",
    "task-*-report.md",
    "task-*-review.md",
    "task-*-package.md",
    "task-*-review-package.md",
    "task-*-fix-round-*-before/**",
)
HASH_CHUNK_BYTES = 1024 * 1024
SOURCE_EXCLUSIONS = {
    "directory_components": SOURCE_EXCLUDED_DIRECTORY_COMPONENTS,
    "file_suffixes": SOURCE_EXCLUDED_FILE_SUFFIXES,
    "generated_task_artifact_root": GENERATED_TASK_ARTIFACT_ROOT.as_posix(),
    "generated_task_artifact_patterns": GENERATED_TASK_ARTIFACT_PATTERNS,
    "sensitive_file_names": sorted(SENSITIVE_SOURCE_FILE_NAMES),
    "sensitive_file_patterns": [".env.* except .env.example", "*.key", "*.pem"],
}


def _is_excluded(relative: Path) -> bool:
    if any(component in SOURCE_EXCLUDED_DIRECTORY_COMPONENTS for component in relative.parts):
        return True
    if relative.name.endswith(SOURCE_EXCLUDED_FILE_SUFFIXES):
        return True
    name = relative.name
    if name in SENSITIVE_SOURCE_FILE_NAMES:
        return True
    if name.startswith(".env.") and name != ".env.example":
        return True
    if name.endswith((".key", ".pem")):
        return True
    try:
        task_relative = relative.relative_to(GENERATED_TASK_ARTIFACT_ROOT)
    except ValueError:
        return False
    if not task_relative.parts:
        return False
    name = task_relative.parts[0]
    lowered = name.lower()
    if name == "progress.md":
        return True
    if not name.startswith("task-"):
        return False
    if name.endswith((".sha256", ".diff")) or any(
        marker in lowered for marker in ("-report", "-review", "-package", "-before")
    ):
        return True
    return not any(marker in lowered for marker in ("brief", "plan", "specification", "spec"))


def _included_source_files(root: Path) -> list[Path]:
    files: list[Path] = []
    for path in sorted(root.rglob("*")):
        relative = path.relative_to(root)
        if _is_excluded(relative):
            continue
        if path.is_symlink():
            raise ValueError(f"source identity refuses symlink: {relative.as_posix()}")
        if path.is_file():
            files.append(path)
    return files


def source_tree_sha256(root: Path) -> str:
    """Hash non-generated source files in a stable path and content order."""
    digest = hashlib.sha256()
    for path in _included_source_files(root):
        relative = path.relative_to(root)
        if _is_excluded(relative):
            continue
        encoded_path = relative.as_posix().encode("utf-8")
        metadata = path.stat()
        content_size = metadata.st_size
        digest.update(len(encoded_path).to_bytes(8, "big"))
        digest.update(encoded_path)
        digest.update(bytes((1 if metadata.st_mode & 0o111 else 0,)))
        digest.update(content_size.to_bytes(8, "big"))
        bytes_read = 0
        with path.open("rb") as stream:
            while chunk := stream.read(HASH_CHUNK_BYTES):
                bytes_read += len(chunk)
                digest.update(chunk)
        if bytes_read != content_size:
            raise RuntimeError(f"source file changed while hashing: {relative.as_posix()}")
    return digest.hexdigest()


def create_readonly_source_snapshot(source_root: Path, snapshot_root: Path) -> str:
    """Copy the non-sensitive source tree once, then lock it for both image builds."""
    source_root = source_root.resolve()
    snapshot_root = snapshot_root.resolve()
    if not source_root.is_dir():
        raise ValueError("source root must be a directory")
    if snapshot_root.exists():
        raise ValueError("source snapshot path must not already exist")
    if snapshot_root.is_relative_to(source_root):
        raise ValueError("source snapshot must be outside the source root")

    snapshot_root.mkdir(mode=0o700, parents=True)
    for path in _included_source_files(source_root):
        relative = path.relative_to(source_root)
        destination = snapshot_root / relative
        destination.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        shutil.copy2(path, destination)

    snapshot_sha256 = source_tree_sha256(snapshot_root)
    for path in sorted(snapshot_root.rglob("*"), reverse=True):
        if path.is_file():
            path.chmod(0o500 if path.stat().st_mode & 0o111 else 0o400)
        else:
            path.chmod(0o500)
    snapshot_root.chmod(0o500)
    return snapshot_sha256
