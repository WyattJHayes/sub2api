from __future__ import annotations

import json
import os
import re
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


IMAGE_DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
REPO_ROOT = Path(__file__).resolve().parents[2]
COMPOSE_FILE = REPO_ROOT / "deploy" / "docker-compose.radar-staging.yml"
RELIABILITY_COMPOSE_FILE = REPO_ROOT / "deploy" / "docker-compose.radar-reliability.yml"


class RadarCandidateImageConfigTest(unittest.TestCase):
    def _render(self, *, include_reliability: bool = False) -> dict:
        if shutil.which("docker") is None:
            raise RuntimeError("Docker Compose is required for candidate image configuration checks")

        control_digest = "sha256:" + "a" * 64
        worker_digest = "sha256:" + "b" * 64
        env = {
            "RADAR_COMPOSE_PROJECT_NAME": "sub2api-radar-v11-test",
            "RADAR_COMPOSE_RESOURCE_PREFIX": "sub2api-radar-v11-test",
            "RADAR_REGISTRY": "registry.example.invalid",
            "RADAR_CONTROL_PLANE_IMAGE_DIGEST": control_digest,
            "RADAR_WORKER_IMAGE_DIGEST": worker_digest,
            "RADAR_CONTROL_PLANE_IMAGE": (
                f"registry.example.invalid/sub2api/radar-control-plane@{control_digest}"
            ),
            "RADAR_WORKER_IMAGE": f"registry.example.invalid/sub2api/radar-worker@{worker_digest}",
            "RADAR_IMAGE_PULL_POLICY": "always",
            "RADAR_RELEASE_VERSION": "0.1.171-radar-v11-test",
            "RADAR_RELEASE_COMMIT": "d" * 40,
            "RADAR_RELEASE_DATE": "2026-08-06T00:00:00Z",
            "RADAR_API_WRITER_INSTANCE_ID": "00000000-0000-4000-8000-000000000001",
            "RADAR_POSTGRES_PASSWORD": "postgres-password-for-test",
            "RADAR_JWT_SECRET": "jwt-secret-for-test",
            "RADAR_ADMIN_PASSWORD": "admin-password-for-test",
            "RADAR_CONTEXT_SIGNING_KEY": "context-signing-key-for-test",
            "RADAR_EVIDENCE_HASH_KEY": "evidence-hash-key-for-test",
            "RADAR_MINIO_ROOT_USER": "radaradmin",
            "RADAR_MINIO_ROOT_PASSWORD": "minio-password-for-test",
            "RADAR_SYNTHETIC_UPSTREAM_API_KEY": "synthetic-api-key-for-test",
            "RADAR_RUNNER_WORKER_TOKEN": "r" * 40,
            "RADAR_GRADER_WORKER_TOKEN": "g" * 40,
            "RADAR_STATISTICS_WORKER_TOKEN": "s" * 40,
            "RADAR_RELIABILITY_UID": "10001",
            "RADAR_RELIABILITY_GID": "10001",
            "RADAR_LOADGEN_WORKER_TOKEN": "l" * 40,
            "RADAR_LOADGEN_EVALUATION_API_KEY": "loadgen-api-key-for-test",
            "RADAR_LOAD_PLAN_ID": "00000000-0000-4000-8000-000000000010",
            "RADAR_LOAD_RUN_ID": "00000000-0000-4000-8000-000000000011",
            "RADAR_LOADGEN_IMAGE_DIGEST": worker_digest,
            "RADAR_CHAOS_CONTROLLER_TOKEN": "c" * 40,
            "RADAR_CHAOS_AUTO_ROLLBACK_SECONDS": "15",
            "RADAR_FAULT_EXPERIMENT_ID": "00000000-0000-4000-8000-000000000012",
            "RADAR_CHAOS_TARGET_WORKER_ID": "00000000-0000-4000-8000-000000000013",
            "RADAR_RECOVERY_VERIFIER_TOKEN": "v" * 40,
            "RADAR_RECOVERY_EVIDENCE_ID": "00000000-0000-4000-8000-000000000014",
            "RADAR_RELIABILITY_EVIDENCE_DIR": "/tmp",
            "RADAR_RELIABILITY_REPORT_DIR": "/tmp",
        }

        with tempfile.TemporaryDirectory(prefix="radar-compose-config-") as temp_dir:
            env_file = Path(temp_dir) / "candidate.env"
            env_file.write_text(
                "".join(f"{key}={value}\n" for key, value in env.items()),
                encoding="utf-8",
            )
            env_file.chmod(0o600)
            command = [
                "docker",
                "compose",
                "--env-file",
                str(env_file),
                "-f",
                str(COMPOSE_FILE),
            ]
            if include_reliability:
                command.extend(
                    [
                        "-f",
                        str(RELIABILITY_COMPOSE_FILE),
                        "--profile",
                        "reliability",
                        "--profile",
                        "chaos",
                    ]
                )
            command.extend(["config", "--format", "json"])
            result = subprocess.run(
                command,
                cwd=REPO_ROOT,
                env={**os.environ, **env},
                capture_output=True,
                text=True,
                check=False,
            )
        if result.returncode != 0:
            self.fail(f"docker compose config failed: {result.stderr.strip()}")
        return json.loads(result.stdout)

    def test_candidate_images_are_digest_pinned_and_workers_are_private(self) -> None:
        config = self._render()
        services = config["services"]
        control = services["sub2api-staging"]
        worker_names = [
            "radar-synthetic-upstream",
            "radar-runner",
            "radar-grader",
            "radar-statistics",
        ]

        self.assertEqual(
            "registry.example.invalid/sub2api/radar-control-plane@sha256:" + "a" * 64,
            control["image"],
        )
        self.assertEqual("always", control["pull_policy"])
        for name in worker_names:
            service = services[name]
            self.assertEqual(
                "registry.example.invalid/sub2api/radar-worker@sha256:" + "b" * 64,
                service["image"],
                name,
            )
            self.assertEqual("always", service["pull_policy"], name)
            self.assertNotIn("ports", service, name)
            self.assertTrue(service.get("read_only"), name)
            self.assertIn("ALL", service.get("cap_drop", []), name)

        self.assertTrue(config["networks"]["radar_worker_internal"]["internal"])
        self.assertRegex(control["image"].split("@", 1)[1], IMAGE_DIGEST_RE)

    def test_reliability_workers_are_digest_pinned(self) -> None:
        config = self._render(include_reliability=True)
        services = config["services"]
        expected = "registry.example.invalid/sub2api/radar-worker@sha256:" + "b" * 64

        for name in ["radar-loadgen", "radar-chaos-controller", "radar-recovery-verifier"]:
            service = services[name]
            self.assertEqual(expected, service["image"], name)
            self.assertEqual("always", service["pull_policy"], name)
            self.assertNotIn("ports", service, name)
            self.assertTrue(service.get("read_only"), name)
            self.assertIn("ALL", service.get("cap_drop", []), name)


if __name__ == "__main__":
    unittest.main()
