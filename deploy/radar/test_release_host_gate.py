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


gate = load_script("radar_release_host_gate", "release_host_gate.py")


GIB = 1024 * 1024 * 1024


def container(
    name: str,
    *,
    container_id: str = "4d8b0b5e",
    started_at: str = "2026-08-02T15:00:00.000000000Z",
    restart_count: int = 5,
    running: bool = True,
    health: str = "healthy",
) -> dict[str, Any]:
    return {
        "name": name,
        "container_id": container_id,
        "started_at": started_at,
        "restart_count": restart_count,
        "running": running,
        "health": health,
    }


def state(*containers: dict[str, Any]) -> dict[str, Any]:
    return {
        "schema_version": gate.SCHEMA_VERSION,
        "captured_at": "2026-08-02T15:01:00Z",
        "containers": list(containers),
    }


def disk(*, used_percent: float = 70.0, free_bytes: int = 12 * GIB) -> dict[str, Any]:
    return {
        "path": "/",
        "total_bytes": 50 * GIB,
        "free_bytes": free_bytes,
        "used_percent": used_percent,
    }


class ReleaseHostGateTests(unittest.TestCase):
    def test_unchanged_historical_restart_counts_pass(self) -> None:
        baseline = state(container("radar-control", restart_count=5))
        current = state(container("radar-control", restart_count=5))

        result = gate.verify_release_state(
            baseline,
            current,
            disk(),
            max_used_percent=85.0,
            min_free_bytes=10 * GIB,
        )

        self.assertTrue(result["ok"], result)
        restart = next(check for check in result["checks"] if check["name"] == "restart_count")
        self.assertEqual(5, restart["baseline_restart_count"])
        self.assertEqual(5, restart["current_restart_count"])

    def test_restart_count_increase_fails(self) -> None:
        baseline = state(container("radar-control", restart_count=5))
        current = state(container("radar-control", restart_count=6))

        result = gate.verify_release_state(baseline, current, disk())

        self.assertFalse(result["ok"])
        self.assertTrue(any("restart_count" in failure for failure in result["failures"]))

    def test_moved_start_timestamp_fails(self) -> None:
        baseline = state(container("radar-control", started_at="2026-08-02T15:00:00Z"))
        current = state(container("radar-control", started_at="2026-08-02T15:02:00Z"))

        result = gate.verify_release_state(baseline, current, disk())

        self.assertFalse(result["ok"])
        self.assertTrue(any("started_at" in failure for failure in result["failures"]))

    def test_unhealthy_container_fails(self) -> None:
        baseline = state(container("radar-control"))
        current = state(container("radar-control", health="unhealthy"))

        result = gate.verify_release_state(baseline, current, disk())

        self.assertFalse(result["ok"])
        self.assertTrue(any("health" in failure for failure in result["failures"]))

    def test_absent_container_fails(self) -> None:
        baseline = state(container("radar-control"), container("radar-worker"))
        current = state(container("radar-control"))

        result = gate.verify_release_state(baseline, current, disk())

        self.assertFalse(result["ok"])
        self.assertTrue(any("radar-worker" in failure for failure in result["failures"]))

    def test_disk_used_percent_above_threshold_fails(self) -> None:
        baseline = state(container("radar-control"))
        current = state(container("radar-control"))

        result = gate.verify_release_state(baseline, current, disk(used_percent=85.1))

        self.assertFalse(result["ok"])
        self.assertTrue(any("used_percent" in failure for failure in result["failures"]))

    def test_disk_free_bytes_below_threshold_fails(self) -> None:
        baseline = state(container("radar-control"))
        current = state(container("radar-control"))

        result = gate.verify_release_state(baseline, current, disk(free_bytes=(10 * GIB) - 1))

        self.assertFalse(result["ok"])
        self.assertTrue(any("free_bytes" in failure for failure in result["failures"]))


if __name__ == "__main__":
    unittest.main()
