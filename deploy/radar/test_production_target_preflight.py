from __future__ import annotations

import importlib.util
import hashlib
import inspect
import json
import sys
import tempfile
import unittest
from unittest.mock import patch
from pathlib import Path
from typing import Any


RADAR_DIR = Path(__file__).resolve().parent
sys.path.insert(0, str(RADAR_DIR))


def load_script(name: str, filename: str) -> Any:
    spec = importlib.util.spec_from_file_location(name, RADAR_DIR / filename)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load {filename}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


preflight = load_script("radar_production_target_preflight", "production_target_preflight.py")
CONTROL_REPOSITORY = "ghcr.io/wyattjhayes/sub2api-radar-control-plane"
WORKER_REPOSITORY = "ghcr.io/wyattjhayes/sub2api-radar-worker"
CONTROL_DIGEST = "sha256:" + "1" * 64
WORKER_DIGEST = "sha256:" + "2" * 64


def canonical_sha256(value: object) -> str:
    body = json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    return hashlib.sha256(body.encode("utf-8")).hexdigest()


def target_identity() -> tuple[dict[str, object], str, str]:
    machine_id_sha256 = "a" * 64
    host_fingerprint = canonical_sha256({"machine_id_sha256": machine_id_sha256})
    descriptor = {
        "host_fingerprint": host_fingerprint,
        "target_dir": "/opt/sub2api",
        "target_dir_device": 1,
        "target_dir_inode": 2,
        "project_name": "sub2api",
        "app_service": "sub2api",
    }
    return descriptor, host_fingerprint, canonical_sha256(descriptor)


def snapshot(
    *,
    compose_projects: list[dict[str, Any]] | None = None,
    containers: list[dict[str, Any]] | None = None,
    env_mode: str = "600",
    listeners: list[dict[str, Any]] | None = None,
    dgc_aliases: list[str] | None = None,
) -> dict[str, Any]:
    descriptor, host_fingerprint, target_id = target_identity()
    return {
        "schema_version": preflight.SCHEMA_VERSION,
        "target_dir": "/opt/sub2api",
        "compose_projects": compose_projects if compose_projects is not None else [],
        "production_containers": containers if containers is not None else [],
        "env": {"mode": env_mode, "owner": "root:root", "path": "/opt/sub2api/.env"},
        "listeners": listeners if listeners is not None else [],
        "dgc_aliases": dgc_aliases if dgc_aliases is not None else [],
        "host_fingerprint": host_fingerprint,
        "target_descriptor": descriptor,
        "target_id": target_id,
    }


def project(name: str = "sub2api", status: str = "running(3)") -> dict[str, Any]:
    return {"name": name, "status": status}


def container(
    name: str,
    *,
    service: str,
    running: bool = True,
    health: str = "healthy",
    image_id: str = "sha256:" + "a" * 64,
    configured_image: str = "",
    repo_digests: list[str] | None = None,
) -> dict[str, Any]:
    return {
        "name": name,
        "service": service,
        "running": running,
        "health": health,
        "image_id": image_id,
        "configured_image": configured_image,
        "repo_digests": repo_digests if repo_digests is not None else [],
    }


def listener(port: int = 8080, local_address: str = "0.0.0.0") -> dict[str, Any]:
    return {"local_address": local_address, "port": port, "process": "docker-proxy"}


class ProductionTargetPreflightTests(unittest.TestCase):
    def test_target_identity_changes_when_host_or_directory_identity_changes(self) -> None:
        self.assertTrue(hasattr(preflight, "target_descriptor"))
        self.assertTrue(hasattr(preflight, "target_id"))
        if hasattr(preflight, "target_descriptor") and hasattr(preflight, "target_id"):
            original = preflight.target_descriptor(
                machine_id_sha256="a" * 64,
                target_dir="/opt/sub2api",
                target_dir_device=1,
                target_dir_inode=2,
                project_name="sub2api",
                app_service="sub2api",
            )
            changed = preflight.target_descriptor(
                machine_id_sha256="b" * 64,
                target_dir="/opt/sub2api",
                target_dir_device=1,
                target_dir_inode=2,
                project_name="sub2api",
                app_service="sub2api",
            )
            self.assertNotEqual(preflight.target_id(original), preflight.target_id(changed))

    def test_capture_snapshot_binds_host_directory_and_snapshot_hash(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            with (
                patch.object(preflight, "_capture_compose_projects", return_value=[]),
                patch.object(preflight, "_capture_production_containers", return_value=[]),
                patch.object(preflight, "_capture_hashes", return_value={}),
                patch.object(preflight, "_capture_compose_images", return_value=[]),
                patch.object(preflight, "_capture_listeners", return_value=[]),
                patch.object(preflight, "_capture_network_aliases", return_value=[]),
                patch.object(preflight, "_machine_id_sha256", return_value="a" * 64, create=True),
            ):
                captured = preflight.capture_snapshot(root)
        self.assertIn("host_fingerprint", captured)
        self.assertIn("target_descriptor", captured)
        self.assertIn("target_id", captured)
        self.assertIn("snapshot_sha256", captured)
        self.assertEqual(captured["target_id"], canonical_sha256(captured["target_descriptor"]))

    def test_candidate_manifest_must_match_runtime_repository_digest(self) -> None:
        self.assertIn("candidate_image_record", inspect.signature(preflight.evaluate_snapshot).parameters)
        if "candidate_image_record" not in inspect.signature(preflight.evaluate_snapshot).parameters:
            return
        candidate = {
            "control_plane": {"repository": CONTROL_REPOSITORY, "manifest_digest": CONTROL_DIGEST},
            "worker": {"repository": WORKER_REPOSITORY, "manifest_digest": WORKER_DIGEST},
        }
        healthy = snapshot(
            compose_projects=[project()],
            containers=[
                container("sub2api-postgres-1", service="postgres"),
                container("sub2api-redis-1", service="redis"),
                container(
                    "sub2api-sub2api-1",
                    service="sub2api",
                    repo_digests=[f"{CONTROL_REPOSITORY}@{CONTROL_DIGEST}"],
                ),
                container(
                    "sub2api-runner-1",
                    service="radar-runner",
                    repo_digests=[f"{WORKER_REPOSITORY}@{WORKER_DIGEST}"],
                ),
                container(
                    "sub2api-grader-1",
                    service="radar-grader",
                    repo_digests=[f"{WORKER_REPOSITORY}@{WORKER_DIGEST}"],
                ),
                container(
                    "sub2api-statistics-1",
                    service="radar-statistics",
                    repo_digests=[f"{WORKER_REPOSITORY}@{WORKER_DIGEST}"],
                ),
            ],
            listeners=[listener()],
            dgc_aliases=["sub2api-sub2api-1 sub2api"],
        )
        passed = preflight.evaluate_snapshot(healthy, candidate_image_record=candidate)
        self.assertTrue(passed["ok"], passed)

        healthy["production_containers"][2]["repo_digests"] = [
            f"{CONTROL_REPOSITORY}@sha256:{'9' * 64}"
        ]
        failed = preflight.evaluate_snapshot(healthy, candidate_image_record=candidate)
        self.assertFalse(failed["ok"], failed)
        self.assertIn("control_plane_repo_digest", failed["blockers"])

    def test_inactive_production_target_fails_closed_and_names_required_authorizations(self) -> None:
        result = preflight.evaluate_snapshot(snapshot(env_mode="644"))

        self.assertFalse(result["ok"], result)
        self.assertFalse(result["promotion_ready"], result)
        self.assertIn("production_compose_project_running", result["blockers"])
        self.assertIn("production_target_container_present", result["blockers"])
        self.assertIn("production_env_mode_0600", result["blockers"])
        self.assertEqual(
            [
                "confirm_target_dir",
                "authorize_inactive_stack_start",
                "authorize_env_chmod_0600",
                "authorize_fresh_backup",
                "authorize_digest_promotion",
                "authorize_rollback_drill",
            ],
            result["required_authorizations"],
        )
        self.assertTrue(result["production_exposure_event"])

    def test_active_healthy_target_with_safe_env_passes_target_preflight(self) -> None:
        result = preflight.evaluate_snapshot(
            snapshot(
                compose_projects=[project()],
                containers=[
                    container("sub2api-postgres-1", service="postgres"),
                    container("sub2api-redis-1", service="redis"),
                    container("sub2api-sub2api-1", service="sub2api"),
                ],
                listeners=[listener()],
                dgc_aliases=["sub2api-sub2api-1 sub2api"],
            )
        )

        self.assertTrue(result["ok"], result)
        self.assertTrue(result["promotion_ready"], result)
        self.assertFalse(result["production_exposure_event"])
        self.assertEqual([], result["blockers"])
        self.assertEqual([], result["required_authorizations"])

    def test_active_target_with_unsafe_env_mode_fails_until_env_is_tightened(self) -> None:
        result = preflight.evaluate_snapshot(
            snapshot(
                compose_projects=[project()],
                containers=[container("sub2api-sub2api-1", service="sub2api")],
                env_mode="0644",
                listeners=[listener()],
                dgc_aliases=["sub2api-sub2api-1 sub2api"],
            )
        )

        self.assertFalse(result["ok"], result)
        self.assertIn("production_env_mode_0600", result["blockers"])
        self.assertIn("authorize_env_chmod_0600", result["required_authorizations"])

    def test_created_or_unhealthy_production_container_is_not_promotion_ready(self) -> None:
        result = preflight.evaluate_snapshot(
            snapshot(
                compose_projects=[project()],
                containers=[
                    container(
                        "sub2api-sub2api-1",
                        service="sub2api",
                        running=False,
                        health="created",
                    )
                ],
                listeners=[listener()],
                dgc_aliases=["sub2api-sub2api-1 sub2api"],
            )
        )

        self.assertFalse(result["ok"], result)
        self.assertIn("production_target_container_running", result["blockers"])
        self.assertIn("production_target_container_healthy", result["blockers"])

    def test_missing_port_and_dgc_alias_fail_for_active_target(self) -> None:
        result = preflight.evaluate_snapshot(
            snapshot(
                compose_projects=[project()],
                containers=[container("sub2api-sub2api-1", service="sub2api")],
                listeners=[],
                dgc_aliases=[],
            )
        )

        self.assertFalse(result["ok"], result)
        self.assertIn("production_port_8080_listening", result["blockers"])
        self.assertIn("production_dgc_alias_present", result["blockers"])


if __name__ == "__main__":
    unittest.main()
