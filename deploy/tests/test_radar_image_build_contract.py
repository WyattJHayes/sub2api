from __future__ import annotations

import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]


class RadarImageBuildContractTests(unittest.TestCase):
    def test_build_context_excludes_private_frontend_test_results(self) -> None:
        dockerignore = (REPO_ROOT / ".dockerignore").read_text()
        self.assertIn("frontend/test-results/", dockerignore)

    def test_control_plane_requires_external_base_images(self) -> None:
        body = (REPO_ROOT / "deploy/Dockerfile.radar-control-staging").read_text()
        required_images = (
            ("NODE_IMAGE", "FROM ${NODE_IMAGE} AS frontend-builder"),
            ("GOLANG_IMAGE", "FROM ${GOLANG_IMAGE} AS backend-builder"),
            ("ALPINE_IMAGE", "FROM ${ALPINE_IMAGE}"),
        )

        for argument, from_instruction in required_images:
            self.assertIn(f"ARG {argument}\n", body)
            self.assertNotIn(f"ARG {argument}=", body)
            self.assertIn(from_instruction, body)

    def test_worker_has_no_staging_parent_and_uses_hash_lock(self) -> None:
        body = (REPO_ROOT / "radar-worker/Dockerfile").read_text()
        self.assertNotIn("sub2api/radar-worker:staging", body)
        self.assertIn("ARG RADAR_WORKER_PYTHON_BASE_IMAGE", body)
        self.assertIn("requirements.lock", body)
        self.assertIn("--require-hashes", body)

    def test_rfc8785_is_exactly_locked(self) -> None:
        lock = (REPO_ROOT / "radar-worker/requirements.lock").read_text()
        self.assertRegex(lock, r"(?m)^rfc8785==0\.1\.4 \\")
        self.assertIn("--hash=sha256:", lock)


if __name__ == "__main__":
    unittest.main()
