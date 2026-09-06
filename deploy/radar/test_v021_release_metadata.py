from __future__ import annotations

import importlib.util
import sys
import unittest
from pathlib import Path

from deploy.radar.migration_ledger import audit_candidate, candidate_manifest, read_manifest, read_name_list


RADAR_DIR = Path(__file__).resolve().parent
REPO_ROOT = RADAR_DIR.parents[1]


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
    def test_current_runtime_metadata_is_v021(self) -> None:
        self.assertEqual(
            "0.2.1",
            (REPO_ROOT / "backend" / "cmd" / "server" / "VERSION").read_text(
                encoding="utf-8"
            ).strip(),
        )
        self.assertIn(
            'version = "0.2.1"',
            (REPO_ROOT / "radar-worker" / "pyproject.toml").read_text(encoding="utf-8"),
        )
        self.assertIn(
            '__version__ = "0.2.1"',
            (REPO_ROOT / "radar-worker" / "src" / "sub2api_radar" / "__init__.py").read_text(
                encoding="utf-8"
            ),
        )

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

    def test_v021_manifest_covers_all_current_schema_migrations(self) -> None:
        manifest_dir = RADAR_DIR / "manifests" / "v0.2.1"
        baseline = read_manifest(manifest_dir / "migration-baseline.tsv")
        expected_new = read_name_list(manifest_dir / "expected-new.txt")
        legacy_entries = read_name_list(manifest_dir / "legacy-entries.txt")
        result = audit_candidate(
            baseline,
            candidate_manifest(REPO_ROOT / "backend" / "migrations"),
            expected_new=expected_new,
            legacy_entries=legacy_entries,
        )
        self.assertTrue(result["ok"], result)
        self.assertEqual(285, len(baseline))
        self.assertEqual(26, len(expected_new))
        self.assertEqual(2, len(legacy_entries))
        self.assertEqual(311, result["expected_schema_migrations"])
        self.assertEqual(309, result["candidate_file_count"])

    def test_current_release_tools_default_to_v021(self) -> None:
        for name in (
            "rehearse-v01171-migrations.sh",
            "production_promotion_audit.py",
            "production_backup_audit.py",
            "production_rollback_audit.py",
            "local_prerelease_closure.py",
        ):
            content = (RADAR_DIR / name).read_text(encoding="utf-8")
            self.assertIn("v0.2.1", content, name)

        evidence = (RADAR_DIR / "production_evidence_envelope.py").read_text(encoding="utf-8")
        self.assertIn('DEFAULT_RELEASE_VERSION = "0.2.1"', evidence)
        preflight = (RADAR_DIR / "local_prerelease_preflight.py").read_text(encoding="utf-8")
        self.assertIn('os.environ.get("RADAR_RELEASE_VERSION", "0.2.1")', preflight)


if __name__ == "__main__":
    unittest.main()
