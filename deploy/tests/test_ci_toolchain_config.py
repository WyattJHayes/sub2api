from __future__ import annotations

import json
import re
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
WORKFLOW_PATHS = (
    ".github/workflows/backend-ci.yml",
    ".github/workflows/security-scan.yml",
    ".github/workflows/release.yml",
)
PNPM_SETUP_VERSION_RE = re.compile(
    r"(?ms)^\s+- name: Set(?:up| up) pnpm\n"
    r"\s+uses: pnpm/action-setup@v6\n"
    r"\s+with:\n"
    r"\s+version: ['\"]?([^\s'\"#]+)"
)
NODE_SETUP_VERSION_RE = re.compile(
    r"(?ms)^\s+- name: Set(?:up| up) Node\.js\n"
    r"\s+uses: actions/setup-node@v6\n"
    r"\s+with:\n"
    r"\s+node-version: ['\"]?([^\s'\"#]+)"
)
NODE_MAJOR_VERSION = "24"


class CIToolchainConfigTest(unittest.TestCase):
    def test_workflows_use_frontend_package_manager_version(self) -> None:
        package = json.loads(
            (REPO_ROOT / "frontend" / "package.json").read_text(encoding="utf-8")
        )
        package_manager = package["packageManager"]
        self.assertTrue(package_manager.startswith("pnpm@"))
        expected_version = package_manager.removeprefix("pnpm@")

        for relative_path in WORKFLOW_PATHS:
            workflow = (REPO_ROOT / relative_path).read_text(encoding="utf-8")
            match = PNPM_SETUP_VERSION_RE.search(workflow)
            self.assertIsNotNone(match, relative_path)
            self.assertEqual(expected_version, match.group(1), relative_path)

    def test_workflows_use_v01171_node_major(self) -> None:
        for relative_path in WORKFLOW_PATHS:
            workflow = (REPO_ROOT / relative_path).read_text(encoding="utf-8")
            match = NODE_SETUP_VERSION_RE.search(workflow)
            self.assertIsNotNone(match, relative_path)
            self.assertEqual(NODE_MAJOR_VERSION, match.group(1), relative_path)


if __name__ == "__main__":
    unittest.main()
