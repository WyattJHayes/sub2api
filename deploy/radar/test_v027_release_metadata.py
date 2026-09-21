from __future__ import annotations

import importlib.util
import sys
import unittest
from pathlib import Path
from unittest.mock import patch

from deploy.radar.migration_ledger import read_manifest, read_name_list


RADAR_DIR = Path(__file__).resolve().parent
REPO_ROOT = RADAR_DIR.parents[1]


def load_builder():
    path = RADAR_DIR / "build_v027_ghcr.py"
    spec = importlib.util.spec_from_file_location("radar_build_v027", path)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load {path.name}")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


class V027ReleaseMetadataTests(unittest.TestCase):
    def test_v027_builder_stays_pinned_to_its_own_release(self) -> None:
        builder = load_builder()
        self.assertEqual("0.2.7", builder._BASE.APP_VERSION)
        self.assertEqual("radar-v027-image-record-v1", builder._BASE.SCHEMA_VERSION)
        builder.validate_inputs(
            builder.BuildInputs(
                version="0.2.7",
                image_tag="0.2.7-radar-v27-20260919T010203Z",
                commit="a" * 40,
                source_sha256="b" * 64,
                date="2026-09-19T01:02:03Z",
                node_image="node@sha256:" + "1" * 64,
                golang_image="golang@sha256:" + "2" * 64,
                alpine_image="alpine@sha256:" + "3" * 64,
                worker_python_base_image="python@sha256:" + "4" * 64,
                push=True,
            )
        )

    def test_v027_builder_rejects_a_mismatched_worker_package(self) -> None:
        builder = load_builder()
        inputs = builder.BuildInputs(
            version="0.2.7",
            image_tag="0.2.7-radar-v27-20260919T010203Z",
            commit="a" * 40,
            source_sha256="b" * 64,
            date="2026-09-19T01:02:03Z",
            node_image="node@sha256:" + "1" * 64,
            golang_image="golang:1.27.0-alpine@sha256:" + "2" * 64,
            alpine_image="alpine@sha256:" + "3" * 64,
            worker_python_base_image="python@sha256:" + "4" * 64,
            push=True,
        )
        with (
            patch.object(builder._BASE, "source_tree_sha256", return_value=inputs.source_sha256),
            patch.object(builder._BASE, "worker_package_version", return_value="0.2.8"),
            patch.object(builder._BASE, "go_module_version", return_value="1.27.0"),
        ):
            with self.assertRaisesRegex(ValueError, "worker package version"):
                builder.validate_current_source(inputs)

    def test_v027_manifest_remains_an_immutable_historical_contract(self) -> None:
        # This manifest records what the v0.2.7 release shipped. Later releases
        # add migrations, so it must not be re-checked against the live tree.
        manifest_dir = RADAR_DIR / "manifests" / "v0.2.7"
        baseline = read_manifest(manifest_dir / "migration-baseline.tsv")
        expected_new = read_name_list(manifest_dir / "expected-new.txt")
        legacy_entries = read_name_list(manifest_dir / "legacy-entries.txt")
        self.assertEqual(285, len(baseline))
        self.assertEqual(31, len(expected_new))
        self.assertEqual(2, len(legacy_entries))
        self.assertTrue(set(expected_new).isdisjoint(legacy_entries))

    def test_v027_release_tools_stay_pinned_to_their_own_release(self) -> None:
        # v0.2.7 is a historical release. Its builder must keep targeting the
        # v0.2.7 image tag/SHA contract even after newer releases exist.
        builder = load_builder()
        self.assertEqual("0.2.7", builder._BASE.APP_VERSION)
        self.assertEqual("radar-v027-image-record-v1", builder._BASE.SCHEMA_VERSION)


if __name__ == "__main__":
    unittest.main()
