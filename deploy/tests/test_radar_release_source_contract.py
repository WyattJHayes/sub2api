from __future__ import annotations

import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]


class RadarReleaseSourceContractTests(unittest.TestCase):
    def test_release_source_contains_control_plane_and_worker_contracts(self) -> None:
        required_paths = (
            "backend/internal/server/routes/radar_worker.go",
            "backend/internal/server/router_radar_worker_test.go",
            "radar-worker/Dockerfile",
            "radar-worker/src/sub2api_radar/control_plane.py",
            "radar-worker/tests/test_control_plane.py",
        )

        for relative_path in required_paths:
            self.assertTrue(
                (REPO_ROOT / relative_path).is_file(),
                f"release source is missing {relative_path}",
            )


if __name__ == "__main__":
    unittest.main()
