"""Release contract: reject wrong identities and preserve historical migrations."""
import importlib.util
import json
import sys
import unittest
from pathlib import Path

from deploy.radar.migration_ledger import (
    audit_candidate, audit_runtime, candidate_manifest, checksum_file,
    expected_schema_migrations, read_manifest, read_name_list,
)

RADAR = Path(__file__).resolve().parent
ROOT = RADAR.parents[1]
MANIFEST = RADAR / "manifests" / "v0.2.20"


class V0220ReleaseMetadataTests(unittest.TestCase):
    def test_candidate_workflow_is_manual_main_only_and_never_deploys(self):
        workflow = ROOT / ".github/workflows/radar-candidate-v0220.yml"
        self.assertTrue(workflow.is_file(), "v0.2.20 candidate workflow is required")
        content = workflow.read_text(encoding="utf-8")
        self.assertIn("workflow_dispatch:", content)
        self.assertIn("github.ref == 'refs/heads/main'", content)
        self.assertIn("packages: write", content)
        self.assertIn("build_v0220_ghcr.py", content)
        self.assertIn("upload-artifact@v7", content)
        self.assertNotIn("production", content.lower())

    def test_candidate_identity_rejects_prior_version_and_mutable_base_images(self):
        path = RADAR / "build_v0220_ghcr.py"
        self.assertTrue(path.is_file(), "v0.2.20 immutable candidate builder is required")
        spec = importlib.util.spec_from_file_location("radar_build_v0220", path)
        builder = importlib.util.module_from_spec(spec)
        sys.modules[spec.name] = builder
        spec.loader.exec_module(builder)
        inputs = builder.BuildInputs(
            version="0.2.20", image_tag="0.2.20-radar-v220-20261107T120000Z",
            commit="a" * 40, source_sha256="b" * 64, date="2026-11-07T12:00:00Z",
            node_image="node@sha256:" + "1" * 64,
            golang_image="golang:1.27.3-alpine@sha256:" + "2" * 64,
            alpine_image="alpine@sha256:" + "3" * 64,
            worker_python_base_image="python@sha256:" + "4" * 64, push=True,
        )
        builder.validate_inputs(inputs)
        from dataclasses import replace
        with self.assertRaisesRegex(ValueError, "version must equal"):
            builder.validate_inputs(replace(inputs, version="0.2.19"))
        with self.assertRaisesRegex(ValueError, "image_tag"):
            builder.validate_inputs(replace(inputs, image_tag="0.2.19-radar-v219-20261107T120000Z"))
        with self.assertRaisesRegex(ValueError, "digest"):
            builder.validate_inputs(replace(inputs, node_image="node:24-alpine"))

    def test_249_is_the_only_pending_migration_and_runtime_requires_333(self):
        self.assertTrue(MANIFEST.is_dir(), "v0.2.20 migration manifest is required")
        baseline = read_manifest(MANIFEST / "migration-baseline.tsv")
        pending = read_name_list(MANIFEST / "expected-new.txt")
        legacy = read_name_list(MANIFEST / "legacy-entries.txt")
        aliases = json.loads((MANIFEST / "duplicate-aliases.json").read_text(encoding="utf-8"))
        historical_names = set(baseline) | set(pending)
        candidate = {
            name: checksum
            for name, checksum in candidate_manifest(ROOT / "backend/migrations").items()
            if name in historical_names
        }
        self.assertEqual(332, len(baseline))
        self.assertEqual(["249_mcp_oauth.sql"], pending)
        audit = audit_candidate(baseline, candidate, expected_new=pending, legacy_entries=legacy)
        self.assertTrue(audit["ok"], audit)
        self.assertEqual(330, audit["candidate_file_count"])
        self.assertEqual(333, expected_schema_migrations(MANIFEST))
        actual = {**baseline, "249_mcp_oauth.sql": candidate["249_mcp_oauth.sql"]}
        result = audit_runtime(baseline, candidate, actual, expected_new=pending,
                               legacy_entries=legacy, duplicate_aliases=aliases)
        self.assertTrue(result["ok"], result)
        self.assertEqual(333, result["actual_schema_migrations"])
        prior = audit_runtime(baseline, candidate, baseline, expected_new=pending,
                              legacy_entries=legacy, duplicate_aliases=aliases)
        self.assertFalse(prior["ok"], "A pre-migration runtime must not pass v0.2.20 acceptance")

    def test_prior_migration_checksums_are_unchanged(self):
        self.assertTrue(MANIFEST.is_dir())
        previous = RADAR / "manifests/v0.2.19"
        expected = read_manifest(previous / "migration-baseline.tsv")
        for name in read_name_list(previous / "expected-new.txt"):
            expected[name] = checksum_file(ROOT / "backend/migrations" / name)
        self.assertEqual(expected, read_manifest(MANIFEST / "migration-baseline.tsv"))

    def test_historical_builder_preserves_v0220_identity(self):
        spec = importlib.util.spec_from_file_location("_v0220_historical_builder", RADAR / "build_v0220_ghcr.py")
        builder = importlib.util.module_from_spec(spec)
        sys.modules[spec.name] = builder
        spec.loader.exec_module(builder)
        self.assertEqual("0.2.20", builder._BASE.APP_VERSION)
        self.assertEqual("radar-v0220-image-record-v1", builder._BASE.SCHEMA_VERSION)


if __name__ == "__main__":
    unittest.main()
