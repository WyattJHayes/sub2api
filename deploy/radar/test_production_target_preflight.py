from __future__ import annotations

import importlib.util
import sys
import unittest
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


def snapshot(
    *,
    compose_projects: list[dict[str, Any]] | None = None,
    containers: list[dict[str, Any]] | None = None,
    env_mode: str = "600",
    listeners: list[dict[str, Any]] | None = None,
    dgc_aliases: list[str] | None = None,
) -> dict[str, Any]:
    return {
        "schema_version": preflight.SCHEMA_VERSION,
        "target_dir": "/opt/sub2api",
        "compose_projects": compose_projects if compose_projects is not None else [],
        "production_containers": containers if containers is not None else [],
        "env": {"mode": env_mode, "owner": "root:root", "path": "/opt/sub2api/.env"},
        "listeners": listeners if listeners is not None else [],
        "dgc_aliases": dgc_aliases if dgc_aliases is not None else [],
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
) -> dict[str, Any]:
    return {
        "name": name,
        "service": service,
        "running": running,
        "health": health,
        "image_id": image_id,
    }


def listener(port: int = 8080, local_address: str = "0.0.0.0") -> dict[str, Any]:
    return {"local_address": local_address, "port": port, "process": "docker-proxy"}


class ProductionTargetPreflightTests(unittest.TestCase):
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
