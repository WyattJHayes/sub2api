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
MANIFEST = RADAR / "manifests" / "v0.2.21"


class V0221ReleaseMetadataTests(unittest.TestCase):
    def test_current_build_paths_use_patched_go_toolchain(self):
        self.assertIn("go 1.27.4\n", (ROOT / "backend/go.mod").read_text(encoding="utf-8"))
        self.assertIn("ARG GOLANG_IMAGE=golang:1.27.4-alpine", (ROOT / "Dockerfile").read_text(encoding="utf-8"))
        self.assertIn("golang:1.27.4-alpine", (ROOT / "backend/Dockerfile").read_text(encoding="utf-8"))
        self.assertIn("ARG GOLANG_IMAGE=golang:1.27.4-alpine", (ROOT / "deploy/Dockerfile").read_text(encoding="utf-8"))
        for name in ("backend-ci.yml", "security-scan.yml", "release.yml"):
            content = (ROOT / ".github/workflows" / name).read_text(encoding="utf-8")
            self.assertIn("go1.27.4", content, name)
            self.assertNotIn("go1.27.3", content, name)
        content = (ROOT / ".github/workflows/radar-candidate-v0221.yml").read_text(encoding="utf-8")
        self.assertRegex(content, r"golang:1\.27\.4-alpine@sha256:[0-9a-f]{64}")

    def test_current_release_gates_use_v0221_identity_and_full_ledger(self):
        from unittest.mock import patch
        with patch.object(sys, "path", [str(RADAR), *sys.path]):
            from deploy.radar import production_evidence_envelope as evidence
            from deploy.radar import production_promotion_audit as promotion
            from deploy.radar import local_prerelease_closure as closure
        self.assertEqual("0.2.21", evidence.DEFAULT_RELEASE_VERSION)
        self.assertEqual(335, promotion.EXPECTED_SCHEMA_MIGRATIONS)
        self.assertEqual(335, closure.MANIFEST_EXPECTED_SCHEMA_MIGRATIONS)

    def test_candidate_workflow_is_manual_main_only_and_never_deploys(self):
        workflow = ROOT / ".github/workflows/radar-candidate-v0221.yml"
        self.assertTrue(workflow.is_file(), "v0.2.21 candidate workflow is required")
        content = workflow.read_text(encoding="utf-8")
        self.assertIn("workflow_dispatch:", content)
        self.assertIn("github.ref == 'refs/heads/main'", content)
        self.assertIn("packages: write", content)
        self.assertIn("build_v0221_ghcr.py", content)
        self.assertIn("upload-artifact@v7", content)
        self.assertNotIn("production", content.lower())

    def test_candidate_identity_rejects_prior_version_and_mutable_base_images(self):
        path = RADAR / "build_v0221_ghcr.py"
        self.assertTrue(path.is_file(), "v0.2.21 immutable candidate builder is required")
        spec = importlib.util.spec_from_file_location("radar_build_v0221", path)
        builder = importlib.util.module_from_spec(spec)
        sys.modules[spec.name] = builder
        spec.loader.exec_module(builder)
        inputs = builder.BuildInputs(
            version="0.2.21", image_tag="0.2.21-radar-v221-20261111T120000Z",
            commit="a" * 40, source_sha256="b" * 64, date="2026-11-11T12:00:00Z",
            node_image="node@sha256:" + "1" * 64,
            golang_image="golang:1.27.4-alpine@sha256:" + "2" * 64,
            alpine_image="alpine@sha256:" + "3" * 64,
            worker_python_base_image="python@sha256:" + "4" * 64, push=True,
        )
        builder.validate_inputs(inputs)
        from dataclasses import replace
        from unittest.mock import patch
        with (
            patch.object(builder._BASE, "source_tree_sha256", return_value=inputs.source_sha256),
            patch.object(builder._BASE, "worker_package_version", return_value="0.2.21"),
        ):
            builder.validate_current_source(inputs, source_root=ROOT)
            with self.assertRaisesRegex(ValueError, "golang image must match backend go.mod version"):
                builder.validate_current_source(
                    replace(inputs, golang_image="golang:1.27.3-alpine@sha256:" + "2" * 64),
                    source_root=ROOT,
                )
        with self.assertRaisesRegex(ValueError, "version must equal"):
            builder.validate_inputs(replace(inputs, version="0.2.19"))
        with self.assertRaisesRegex(ValueError, "image_tag"):
            builder.validate_inputs(replace(inputs, image_tag="0.2.19-radar-v219-20261111T120000Z"))
        with self.assertRaisesRegex(ValueError, "digest"):
            builder.validate_inputs(replace(inputs, node_image="node:24-alpine"))

    def test_official_249_and_250_are_pending_and_runtime_requires_335(self):
        self.assertTrue(MANIFEST.is_dir(), "v0.2.21 migration manifest is required")
        baseline = read_manifest(MANIFEST / "migration-baseline.tsv")
        pending = read_name_list(MANIFEST / "expected-new.txt")
        legacy = read_name_list(MANIFEST / "legacy-entries.txt")
        aliases = json.loads((MANIFEST / "duplicate-aliases.json").read_text(encoding="utf-8"))
        candidate = candidate_manifest(ROOT / "backend/migrations")
        self.assertEqual(333, len(baseline))
        self.assertEqual(["249_add_usage_logs_api_key_cost_index_notx.sql", "250_openai_cache_write_ttl.sql"], pending)
        self.assertIn("249_mcp_oauth.sql", baseline)
        audit = audit_candidate(baseline, candidate, expected_new=pending, legacy_entries=legacy)
        self.assertTrue(audit["ok"], audit)
        self.assertEqual(332, audit["candidate_file_count"])
        self.assertEqual(335, expected_schema_migrations(MANIFEST))
        actual = {**baseline, **{name: candidate[name] for name in pending}}
        result = audit_runtime(baseline, candidate, actual, expected_new=pending,
                               legacy_entries=legacy, duplicate_aliases=aliases)
        self.assertTrue(result["ok"], result)
        self.assertEqual(335, result["actual_schema_migrations"])
        prior = audit_runtime(baseline, candidate, baseline, expected_new=pending,
                              legacy_entries=legacy, duplicate_aliases=aliases)
        self.assertFalse(prior["ok"], "A pre-migration runtime must not pass v0.2.21 acceptance")

    def test_prior_migration_checksums_are_unchanged(self):
        self.assertTrue(MANIFEST.is_dir())
        previous = RADAR / "manifests/v0.2.20"
        expected = read_manifest(previous / "migration-baseline.tsv")
        for name in read_name_list(previous / "expected-new.txt"):
            expected[name] = checksum_file(ROOT / "backend/migrations" / name)
        self.assertEqual(expected, read_manifest(MANIFEST / "migration-baseline.tsv"))

    def test_control_worker_and_local_release_version_agree(self):
        self.assertEqual("0.2.21", (ROOT / "backend/cmd/server/VERSION").read_text(encoding="utf-8").strip())
        import tomllib
        worker = tomllib.loads((ROOT / "radar-worker/pyproject.toml").read_text(encoding="utf-8"))
        self.assertEqual("0.2.21", worker["project"]["version"])
        lock = tomllib.loads((ROOT / "radar-worker/uv.lock").read_text(encoding="utf-8"))
        self.assertEqual(["0.2.21"], [p["version"] for p in lock["package"] if p["name"] == "sub2api-radar-worker"])
        spec = importlib.util.spec_from_file_location("_v0221_worker_runtime", ROOT / "radar-worker/src/sub2api_radar/__init__.py")
        runtime = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(runtime)
        self.assertEqual("0.2.21", runtime.__version__)


if __name__ == "__main__":
    unittest.main()
