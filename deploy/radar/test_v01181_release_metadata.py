from __future__ import annotations

import importlib.util
import sys
import unittest
from pathlib import Path


RADAR_DIR = Path(__file__).resolve().parent
REPO_ROOT = RADAR_DIR.parents[1]


def load_builder(filename: str = "build_v01181_ghcr.py"):
    path = RADAR_DIR / filename
    spec = importlib.util.spec_from_file_location(f"radar_{path.stem}", path)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load {filename}")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


class V01181ReleaseMetadataTests(unittest.TestCase):
    def test_v01181_builder_remains_separate_from_current_runtime_metadata(self) -> None:
        builder = (RADAR_DIR / "build_v01181_ghcr.py").read_text(encoding="utf-8")
        self.assertIn('APP_VERSION = "0.1.181"', builder)
        self.assertIn('SCHEMA_VERSION = "radar-v01181-image-record-v1"', builder)
        self.assertIn("0.1.181-radar-v17-", builder)
        self.assertNotIn('APP_VERSION = "0.1.183"', builder)
        self.assertIn(
            "PIP_NO_INDEX=1 PIP_FIND_LINKS=/opt/radar-wheelhouse",
            (REPO_ROOT / "radar-worker" / "Dockerfile").read_text(encoding="utf-8"),
        )

    def test_current_runtime_metadata_is_v020(self) -> None:
        self.assertEqual(
            "0.2.0",
            (REPO_ROOT / "backend" / "cmd" / "server" / "VERSION").read_text(
                encoding="utf-8"
            ).strip(),
        )
        self.assertIn(
            'version = "0.2.0"',
            (REPO_ROOT / "radar-worker" / "pyproject.toml").read_text(encoding="utf-8"),
        )
        self.assertIn(
            '__version__ = "0.2.0"',
            (REPO_ROOT / "radar-worker" / "src" / "sub2api_radar" / "__init__.py").read_text(
                encoding="utf-8"
            ),
        )

    def test_v020_builder_accepts_current_input_contract(self) -> None:
        builder = load_builder("build_v020_ghcr.py")
        builder.validate_inputs(
            builder.BuildInputs(
                version="0.2.0",
                image_tag="0.2.0-radar-v20-20260902T130000Z",
                commit="a" * 40,
                source_sha256="b" * 64,
                date="2026-09-02T13:00:00Z",
                node_image="node@sha256:" + "1" * 64,
                golang_image="golang:1.27.0-alpine@sha256:" + "2" * 64,
                alpine_image="alpine@sha256:" + "3" * 64,
                worker_python_base_image="python@sha256:" + "4" * 64,
                push=True,
            )
        )

    def test_manifest_adds_only_the_official_v01181_migrations(self) -> None:
        manifest_dir = RADAR_DIR / "manifests" / "v0.1.181"
        expected = [
            line.strip()
            for line in (manifest_dir / "expected-new.txt").read_text(encoding="utf-8").splitlines()
            if line.strip()
        ]
        self.assertEqual(298, len(expected) + 285)
        for name in (
            "226_add_usage_log_effective_model_indexes_notx.sql",
            "227_composite_routes_add_cn_providers.sql",
            "228_channel_pricing_multipliers.sql",
            "229_plugins.sql",
            "230_plugin_artifacts.sql",
        ):
            self.assertIn(name, expected)

    def test_builder_accepts_v01181_input_contract(self) -> None:
        builder = load_builder()
        builder.validate_inputs(
            builder.BuildInputs(
                version="0.1.181",
                image_tag="0.1.181-radar-v17-20260825T000000Z",
                commit="a" * 40,
                source_sha256="b" * 64,
                date="2026-08-25T00:00:00Z",
                node_image="node@sha256:" + "1" * 64,
                golang_image="golang:1.27.0-alpine@sha256:" + "2" * 64,
                alpine_image="alpine@sha256:" + "3" * 64,
                worker_python_base_image="python@sha256:" + "4" * 64,
                push=True,
            )
        )


if __name__ == "__main__":
    unittest.main()
