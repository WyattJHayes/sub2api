from __future__ import annotations

import importlib.util
import sys
import unittest
from pathlib import Path
from unittest.mock import patch

from deploy.radar.migration_ledger import (
    audit_candidate,
    candidate_manifest,
    expected_schema_migrations,
    read_manifest,
    read_name_list,
)


RADAR_DIR = Path(__file__).resolve().parent
REPO_ROOT = RADAR_DIR.parents[1]
MANIFEST_DIR = RADAR_DIR / "manifests" / "v0.2.8"


def load_builder():
    path = RADAR_DIR / "build_v028_ghcr.py"
    spec = importlib.util.spec_from_file_location("radar_build_v028", path)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load {path.name}")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


class V028ReleaseMetadataTests(unittest.TestCase):
    def test_v028_builder_stays_pinned_to_its_own_release(self) -> None:
        builder = load_builder()
        self.assertEqual("0.2.8", builder._BASE.APP_VERSION)
        self.assertEqual("radar-v028-image-record-v1", builder._BASE.SCHEMA_VERSION)
        builder.validate_inputs(
            builder.BuildInputs(
                version="0.2.8",
                image_tag="0.2.8-radar-v28-20260922T010203Z",
                commit="a" * 40,
                source_sha256="b" * 64,
                date="2026-09-22T01:02:03Z",
                node_image="node@sha256:" + "1" * 64,
                golang_image="golang@sha256:" + "2" * 64,
                alpine_image="alpine@sha256:" + "3" * 64,
                worker_python_base_image="python@sha256:" + "4" * 64,
                push=True,
            )
        )

    def test_v028_builder_rejects_a_mismatched_worker_package(self) -> None:
        builder = load_builder()
        inputs = builder.BuildInputs(
            version="0.2.8",
            image_tag="0.2.8-radar-v28-20260922T010203Z",
            commit="a" * 40,
            source_sha256="b" * 64,
            date="2026-09-22T01:02:03Z",
            node_image="node@sha256:" + "1" * 64,
            golang_image="golang:1.27.0-alpine@sha256:" + "2" * 64,
            alpine_image="alpine@sha256:" + "3" * 64,
            worker_python_base_image="python@sha256:" + "4" * 64,
            push=True,
        )
        with (
            patch.object(builder._BASE, "source_tree_sha256", return_value=inputs.source_sha256),
            patch.object(builder._BASE, "worker_package_version", return_value="0.2.7"),
            patch.object(builder._BASE, "go_module_version", return_value="1.27.0"),
        ):
            with self.assertRaisesRegex(ValueError, "worker package version"):
                builder.validate_current_source(inputs)

    def test_v028_manifest_covers_all_current_schema_migrations(self) -> None:
        baseline = read_manifest(MANIFEST_DIR / "migration-baseline.tsv")
        expected_new = read_name_list(MANIFEST_DIR / "expected-new.txt")
        legacy_entries = read_name_list(MANIFEST_DIR / "legacy-entries.txt")
        result = audit_candidate(
            baseline,
            candidate_manifest(REPO_ROOT / "backend" / "migrations"),
            expected_new=expected_new,
            legacy_entries=legacy_entries,
        )
        self.assertTrue(result["ok"], result)
        # Production had applied 318 migrations when this release was cut; the
        # new schedule migration brings the runtime ledger to 319.
        self.assertEqual(318, len(baseline))
        self.assertEqual(["240_add_radar_plan_schedule.sql"], expected_new)
        self.assertEqual(3, len(legacy_entries))
        self.assertEqual(319, expected_schema_migrations(MANIFEST_DIR))
        self.assertEqual(316, result["candidate_file_count"])
        self.assertEqual([], result["checksum_mismatches"])

    def test_current_release_tools_default_to_v028(self) -> None:
        for name in (
            "rehearse-v01171-migrations.sh",
            "production_promotion_audit.py",
            "production_backup_audit.py",
            "production_rollback_audit.py",
            "local_prerelease_closure.py",
        ):
            content = (RADAR_DIR / name).read_text(encoding="utf-8")
            self.assertIn("v0.2.8", content, name)

        evidence = (RADAR_DIR / "production_evidence_envelope.py").read_text(encoding="utf-8")
        self.assertIn('DEFAULT_RELEASE_VERSION = "0.2.8"', evidence)
        preflight = (RADAR_DIR / "local_prerelease_preflight.py").read_text(encoding="utf-8")
        self.assertIn('os.environ.get("RADAR_RELEASE_VERSION", "0.2.8")', preflight)


if __name__ == "__main__":
    unittest.main()
