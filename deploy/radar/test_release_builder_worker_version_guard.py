from __future__ import annotations

import importlib.util
import sys
import unittest
from contextlib import nullcontext
from dataclasses import replace
from pathlib import Path
from unittest.mock import patch


RADAR_DIR = Path(__file__).resolve().parent


def load_builder(filename: str) -> object:
    path = RADAR_DIR / filename
    spec = importlib.util.spec_from_file_location(path.stem, path)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load {filename}")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


class ReleaseBuilderWorkerVersionGuardTests(unittest.TestCase):
    def test_release_builders_reject_mismatched_worker_package_versions(self) -> None:
        cases = (
            ("build_v01176_ghcr.py", "0.1.176", "0.1.176-radar-v14-20260815T154200Z", "0.1.177"),
            ("build_v01177_ghcr.py", "0.1.177", "0.1.177-radar-v15-20260815T154200Z", "0.1.178"),
            ("build_v01178_ghcr.py", "0.1.178", "0.1.178-radar-v16-20260816T021900Z", "0.1.179"),
            ("build_v01181_ghcr.py", "0.1.181", "0.1.181-radar-v17-20260815T154200Z", "0.1.182"),
            ("build_v01182_ghcr.py", "0.1.182", "0.1.182-radar-v18-20260815T154200Z", "0.1.183"),
            ("build_v01183_ghcr.py", "0.1.183", "0.1.183-radar-v19-20260815T154200Z", "0.1.184"),
            ("build_v020_ghcr.py", "0.2.0", "0.2.0-radar-v20-20260815T154200Z", "0.2.1"),
        )
        for filename, version, image_tag, worker_version in cases:
            with self.subTest(filename=filename):
                builder = load_builder(filename)
                golang_image = "golang@sha256:" + "2" * 64
                if filename == "build_v01178_ghcr.py":
                    golang_image = "golang:1.26.6-alpine@sha256:" + "2" * 64
                elif filename in {
                    "build_v01181_ghcr.py",
                    "build_v01182_ghcr.py",
                    "build_v01183_ghcr.py",
                    "build_v020_ghcr.py",
                }:
                    golang_image = "golang:1.27.0-alpine@sha256:" + "2" * 64
                inputs = builder.BuildInputs(
                    version=version,
                    image_tag=image_tag,
                    commit="a" * 40,
                    source_sha256="a" * 64,
                    date="2026-08-15T15:42:00Z",
                    node_image="node@sha256:" + "1" * 64,
                    golang_image=golang_image,
                    alpine_image="alpine@sha256:" + "3" * 64,
                    worker_python_base_image="python@sha256:" + "4" * 64,
                    push=True,
                )
                with (
                    patch.object(builder, "source_tree_sha256", return_value=inputs.source_sha256),
                    patch.object(builder, "worker_package_version", return_value=worker_version),
                ):
                    go_version_patch = (
                        patch.object(builder, "go_module_version", return_value="1.26.6")
                        if filename == "build_v01178_ghcr.py"
                        else nullcontext()
                    )
                    with go_version_patch:
                        with self.assertRaisesRegex(ValueError, "worker package version"):
                            builder.validate_current_source(inputs)


if __name__ == "__main__":
    unittest.main()
