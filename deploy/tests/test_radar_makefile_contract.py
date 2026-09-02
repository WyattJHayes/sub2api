"""Keep Radar unit coverage in the repository frontend CI target."""

from __future__ import annotations

import subprocess
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
RADAR_VITEST_FILES = (
    "src/views/user/__tests__/ModelHealthView.spec.ts",
    "src/views/user/__tests__/ModelQualityReportView.spec.ts",
    "src/views/admin/radar/__tests__/RadarModelsView.spec.ts",
    "src/views/admin/radar/__tests__/RadarManagementViews.spec.ts",
    "src/views/admin/radar/__tests__/RadarShellNavigation.spec.ts",
    "src/views/admin/radar/__tests__/RadarLocalizedViews.spec.ts",
)


class RadarMakefileContractTests(unittest.TestCase):
    def test_frontend_target_includes_all_radar_vitest_files_and_e2e_is_opt_in(self) -> None:
        frontend_target = subprocess.run(
            ["make", "--dry-run", "test-frontend-radar"],
            cwd=ROOT,
            check=True,
            capture_output=True,
            text=True,
        ).stdout
        for path in RADAR_VITEST_FILES:
            self.assertIn(path, frontend_target)

        full_frontend_target = subprocess.run(
            ["make", "--dry-run", "test-frontend"],
            cwd=ROOT,
            check=True,
            capture_output=True,
            text=True,
        ).stdout
        for path in RADAR_VITEST_FILES:
            self.assertIn(path, full_frontend_target)
        self.assertNotIn("pnpm test:e2e", full_frontend_target)

        e2e_target = subprocess.run(
            ["make", "--dry-run", "test-radar-e2e"],
            cwd=ROOT,
            check=True,
            capture_output=True,
            text=True,
        ).stdout
        self.assertEqual("cd frontend && pnpm test:e2e\n", e2e_target)


if __name__ == "__main__":
    unittest.main()
