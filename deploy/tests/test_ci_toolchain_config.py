from __future__ import annotations

import json
import re
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
PNPM_SETUP_VERSION_RE = re.compile(
    r"(?ms)^\s+- name: Set(?:up| up) pnpm\n"
    r"\s+uses: pnpm/action-setup@v6\n"
    r"\s+with:\n"
    r"\s+version: ['\"]?([^\s'\"#]+)"
)


class CIToolchainConfigTest(unittest.TestCase):
    def test_workflows_use_frontend_package_manager_version(self) -> None:
        package = json.loads(
            (REPO_ROOT / "frontend" / "package.json").read_text(encoding="utf-8")
        )
        package_manager = package["packageManager"]
        self.assertTrue(package_manager.startswith("pnpm@"))
        expected_version = package_manager.removeprefix("pnpm@")

        for relative_path in (
            ".github/workflows/backend-ci.yml",
            ".github/workflows/security-scan.yml",
        ):
            workflow = (REPO_ROOT / relative_path).read_text(encoding="utf-8")
            match = PNPM_SETUP_VERSION_RE.search(workflow)
            self.assertIsNotNone(match, relative_path)
            self.assertEqual(expected_version, match.group(1), relative_path)


if __name__ == "__main__":
    unittest.main()
