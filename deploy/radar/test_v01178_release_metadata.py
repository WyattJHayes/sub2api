from __future__ import annotations

import importlib.util
import stat
import sys
import unittest
from pathlib import Path


RADAR_DIR = Path(__file__).resolve().parent
REPO_ROOT = RADAR_DIR.parents[1]


class V01178ReleaseMetadataTests(unittest.TestCase):
    def test_cutover_scripts_are_owner_executable(self) -> None:
        for name in (
            "radar_migration_198_cutover.sh",
            "radar_migration_199_cutover.sh",
        ):
            path = REPO_ROOT / "backend" / "scripts" / name
            self.assertTrue(
                path.stat().st_mode & stat.S_IXUSR,
                f"{name} must be executable for backend integration tests",
            )

    def test_legacy_v01178_builder_contract_remains_available(self) -> None:
        builder = RADAR_DIR / "build_v01178_ghcr.py"
        self.assertTrue(builder.is_file())
        content = builder.read_text(encoding="utf-8")
        self.assertIn('APP_VERSION = "0.1.178"', content)
        self.assertIn('SCHEMA_VERSION = "radar-v01178-image-record-v1"', content)

    def test_v01178_builder_has_its_own_release_contract(self) -> None:
        builder = RADAR_DIR / "build_v01178_ghcr.py"
        self.assertTrue(builder.is_file())
        content = builder.read_text(encoding="utf-8")
        self.assertIn('APP_VERSION = "0.1.178"', content)
        self.assertIn('SCHEMA_VERSION = "radar-v01178-image-record-v1"', content)
        self.assertIn('0.1.178-radar-v16-', content)
        spec = importlib.util.spec_from_file_location("radar_build_v01178", builder)
        self.assertIsNotNone(spec)
        self.assertIsNotNone(spec.loader if spec else None)
        module = importlib.util.module_from_spec(spec)
        sys.modules[spec.name] = module
        assert spec.loader is not None
        spec.loader.exec_module(module)
        module.validate_inputs(
            module.BuildInputs(
                version="0.1.178",
                image_tag="0.1.178-radar-v16-20260816T021900Z",
                commit="a" * 40,
                source_sha256="a" * 64,
                date="2026-08-16T02:19:00Z",
                node_image="node@sha256:" + "1" * 64,
                golang_image="golang@sha256:" + "2" * 64,
                alpine_image="alpine@sha256:" + "3" * 64,
                worker_python_base_image="python@sha256:" + "4" * 64,
                push=True,
            )
        )

    def test_v01178_builder_keeps_git_revision_independent_from_source_hash(self) -> None:
        builder = RADAR_DIR / "build_v01178_ghcr.py"
        spec = importlib.util.spec_from_file_location("radar_build_v01178_independent", builder)
        self.assertIsNotNone(spec)
        self.assertIsNotNone(spec.loader if spec else None)
        module = importlib.util.module_from_spec(spec)
        sys.modules[spec.name] = module
        assert spec.loader is not None
        spec.loader.exec_module(module)

        module.validate_inputs(
            module.BuildInputs(
                version="0.1.178",
                image_tag="0.1.178-radar-v16-20260816T021900Z",
                commit="e0c48a19ed794a565e3858662520afe0a1f9f0ba",
                source_sha256="f" * 64,
                date="2026-08-16T02:19:00Z",
                node_image="node@sha256:" + "1" * 64,
                golang_image="golang@sha256:" + "2" * 64,
                alpine_image="alpine@sha256:" + "3" * 64,
                worker_python_base_image="python@sha256:" + "4" * 64,
                push=True,
            )
        )


if __name__ == "__main__":
    unittest.main()
