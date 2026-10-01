from __future__ import annotations

import importlib.util
import json
import sys
import unittest
from pathlib import Path
from unittest.mock import patch

from deploy.radar.migration_ledger import (
    audit_candidate,
    audit_runtime,
    candidate_manifest,
    expected_schema_migrations,
    read_manifest,
    read_name_list,
)


RADAR_DIR = Path(__file__).resolve().parent
REPO_ROOT = RADAR_DIR.parents[1]
MANIFEST_DIR = RADAR_DIR / "manifests" / "v0.2.11"
V0211_MIGRATIONS: list[str] = []


def load_builder():
    path = RADAR_DIR / "build_v0211_ghcr.py"
    spec = importlib.util.spec_from_file_location("radar_build_v0211", path)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load {path.name}")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def build_inputs(builder, *, version: str = "0.2.11", image_tag: str = "0.2.11-radar-v211-20261001T120000Z"):
    return builder.BuildInputs(
        version=version,
        image_tag=image_tag,
        commit="a" * 40,
        source_sha256="b" * 64,
        date="2026-10-01T12:00:00Z",
        node_image="node@sha256:" + "1" * 64,
        golang_image="golang:1.27.0-alpine@sha256:" + "2" * 64,
        alpine_image="alpine@sha256:" + "3" * 64,
        worker_python_base_image="python@sha256:" + "4" * 64,
        push=True,
    )


class V0211ReleaseMetadataTests(unittest.TestCase):
    def test_v0211_builder_accepts_only_its_release_identity(self) -> None:
        builder = load_builder()
        self.assertEqual("0.2.11", builder._BASE.APP_VERSION)
        self.assertEqual("radar-v0211-image-record-v1", builder._BASE.SCHEMA_VERSION)
        builder.validate_inputs(build_inputs(builder))

        with self.assertRaisesRegex(ValueError, "version must equal 0.2.11"):
            builder.validate_inputs(build_inputs(builder, version="0.2.8"))
        with self.assertRaisesRegex(ValueError, "image_tag"):
            builder.validate_inputs(
                build_inputs(builder, image_tag="0.2.8-radar-v28-20261001T120000Z")
            )

    def test_v0211_builder_binds_source_hash_and_worker_package_version(self) -> None:
        builder = load_builder()
        inputs = build_inputs(builder)
        with (
            patch.object(builder._BASE, "source_tree_sha256", return_value=inputs.source_sha256),
            patch.object(builder._BASE, "worker_package_version", return_value="0.2.11"),
            patch.object(builder._BASE, "go_module_version", return_value="1.27.0"),
        ):
            builder.validate_current_source(inputs, source_root=REPO_ROOT)

        with (
            patch.object(builder._BASE, "source_tree_sha256", return_value="c" * 64),
            patch.object(builder._BASE, "worker_package_version", return_value="0.2.11"),
            patch.object(builder._BASE, "go_module_version", return_value="1.27.0"),
        ):
            with self.assertRaisesRegex(ValueError, "source_sha256"):
                builder.validate_current_source(inputs, source_root=REPO_ROOT)

        with (
            patch.object(builder._BASE, "source_tree_sha256", return_value=inputs.source_sha256),
            patch.object(builder._BASE, "worker_package_version", return_value="0.2.8"),
            patch.object(builder._BASE, "go_module_version", return_value="1.27.0"),
        ):
            with self.assertRaisesRegex(ValueError, "worker package version"):
                builder.validate_current_source(inputs, source_root=REPO_ROOT)

    def test_v0211_active_metadata_defaults_to_v0211(self) -> None:
        self.assertEqual("0.2.11\n", (REPO_ROOT / "backend/cmd/server/VERSION").read_text(encoding="utf-8"))
        worker = (REPO_ROOT / "radar-worker/pyproject.toml").read_text(encoding="utf-8")
        self.assertIn('version = "0.2.11"', worker)
        lock = (REPO_ROOT / "radar-worker/uv.lock").read_text(encoding="utf-8")
        self.assertIn('name = "sub2api-radar-worker"\nversion = "0.2.11"', lock)

        for name in (
            "production_promotion_audit.py",
            "production_backup_audit.py",
            "production_rollback_audit.py",
            "local_prerelease_closure.py",
        ):
            content = (RADAR_DIR / name).read_text(encoding="utf-8")
            self.assertIn("v0.2.11", content, name)

        evidence = (RADAR_DIR / "production_evidence_envelope.py").read_text(encoding="utf-8")
        self.assertIn('DEFAULT_RELEASE_VERSION = "0.2.11"', evidence)
        preflight = (RADAR_DIR / "local_prerelease_preflight.py").read_text(encoding="utf-8")
        self.assertIn('os.environ.get("RADAR_RELEASE_VERSION", "0.2.11")', preflight)
        local_runner = (RADAR_DIR / "run-local-prerelease.sh").read_text(encoding="utf-8")
        self.assertIn("v0.2.11 ledger", local_runner)
        self.assertIn("RADAR_RELEASE_VERSION=${RADAR_RELEASE_VERSION:-0.2.11}", local_runner)
        self.assertIn(
            "RADAR_MIGRATION_MANIFEST_DIR=${RADAR_MIGRATION_MANIFEST_DIR:-$ROOT_DIR/deploy/radar/manifests/v0.2.11}",
            local_runner,
        )

    def test_v0211_manifest_covers_the_merged_migration_inventory(self) -> None:
        baseline = read_manifest(MANIFEST_DIR / "migration-baseline.tsv")
        expected_new = read_name_list(MANIFEST_DIR / "expected-new.txt")
        legacy_entries = read_name_list(MANIFEST_DIR / "legacy-entries.txt")
        duplicate_aliases = json.loads(
            (MANIFEST_DIR / "duplicate-aliases.json").read_text(encoding="utf-8")
        )
        candidate = candidate_manifest(REPO_ROOT / "backend" / "migrations")
        result = audit_candidate(
            baseline,
            candidate,
            expected_new=expected_new,
            legacy_entries=legacy_entries,
        )

        self.assertTrue(result["ok"], result)
        self.assertEqual(322, len(baseline))
        self.assertEqual(V0211_MIGRATIONS, expected_new)
        self.assertEqual(3, len(legacy_entries))
        self.assertEqual({"225_group_model_pricing.sql": "221_group_model_pricing.sql"}, duplicate_aliases)
        self.assertEqual(322, expected_schema_migrations(MANIFEST_DIR))
        self.assertEqual(319, result["candidate_file_count"])
        self.assertEqual([], result["checksum_mismatches"])

    def test_v0211_runtime_audit_accepts_the_complete_manifest_state(self) -> None:
        baseline = read_manifest(MANIFEST_DIR / "migration-baseline.tsv")
        expected_new = read_name_list(MANIFEST_DIR / "expected-new.txt")
        legacy_entries = read_name_list(MANIFEST_DIR / "legacy-entries.txt")
        duplicate_aliases = json.loads(
            (MANIFEST_DIR / "duplicate-aliases.json").read_text(encoding="utf-8")
        )
        candidate = candidate_manifest(REPO_ROOT / "backend" / "migrations")
        actual = dict(baseline)
        actual.update({name: candidate[name] for name in expected_new})

        result = audit_runtime(
            baseline,
            candidate,
            actual,
            expected_new=expected_new,
            legacy_entries=legacy_entries,
            duplicate_aliases=duplicate_aliases,
        )

        self.assertTrue(result["ok"], result)
        self.assertEqual(322, result["actual_schema_migrations"])
        self.assertEqual(322, result["expected_schema_migrations"])
        self.assertEqual([], result["runtime_missing_files"])
        self.assertEqual([], result["runtime_unknown_files"])
        self.assertEqual(
            result["expected_runtime_ledger_sha256"],
            result["runtime_ledger_sha256"],
        )

    def test_v0211_worker_runtime_reports_release_version(self) -> None:
        init_path = REPO_ROOT / "radar-worker" / "src" / "sub2api_radar" / "__init__.py"
        spec = importlib.util.spec_from_file_location("_v0211_worker_runtime", init_path)
        self.assertIsNotNone(spec)
        self.assertIsNotNone(spec.loader)
        runtime = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(runtime)

        self.assertEqual("0.2.11", runtime.__version__)


if __name__ == "__main__":
    unittest.main()
