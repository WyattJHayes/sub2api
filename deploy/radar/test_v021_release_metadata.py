from __future__ import annotations

import importlib.util
import sys
import unittest
from pathlib import Path

from deploy.radar.migration_ledger import read_manifest, read_name_list


RADAR_DIR = Path(__file__).resolve().parent
def load_builder():
    path = RADAR_DIR / "build_v021_ghcr.py"
    spec = importlib.util.spec_from_file_location("radar_build_v021", path)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load {path.name}")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


class V021ReleaseMetadataTests(unittest.TestCase):
    def test_v021_builder_has_an_immutable_release_contract(self) -> None:
        builder = load_builder()
        self.assertEqual("0.2.1", builder.APP_VERSION)
        self.assertEqual("radar-v021-image-record-v1", builder.SCHEMA_VERSION)
        builder.validate_inputs(
            builder.BuildInputs(
                version="0.2.1",
                image_tag="0.2.1-radar-v21-20260906T010203Z",
                commit="a" * 40,
                source_sha256="b" * 64,
                date="2026-09-06T01:02:03Z",
                node_image="node@sha256:" + "1" * 64,
                golang_image="golang@sha256:" + "2" * 64,
                alpine_image="alpine@sha256:" + "3" * 64,
                worker_python_base_image="python@sha256:" + "4" * 64,
                push=True,
            )
        )

    def test_v021_manifest_remains_an_immutable_historical_contract(self) -> None:
        manifest_dir = RADAR_DIR / "manifests" / "v0.2.1"
        baseline = read_manifest(manifest_dir / "migration-baseline.tsv")
        expected_new = read_name_list(manifest_dir / "expected-new.txt")
        legacy_entries = read_name_list(manifest_dir / "legacy-entries.txt")
        self.assertEqual(285, len(baseline))
        self.assertEqual(26, len(expected_new))
        self.assertEqual(2, len(legacy_entries))
        self.assertTrue(set(expected_new).isdisjoint(legacy_entries))


if __name__ == "__main__":
    unittest.main()
