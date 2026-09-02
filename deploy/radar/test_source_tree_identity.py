from __future__ import annotations

import tempfile
import unittest
import os
from pathlib import Path

from deploy.radar.local_prerelease_preflight import source_tree_sha256 as preflight_source_tree_sha256
from deploy.radar.source_tree_identity import create_readonly_source_snapshot, source_tree_sha256


class SourceTreeIdentityTests(unittest.TestCase):
    def test_builder_and_preflight_share_the_canonical_source_hash(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "backend" / "main.go"
            source.parent.mkdir()
            source.write_text("package main\n", encoding="utf-8")

            self.assertEqual(source_tree_sha256(root), preflight_source_tree_sha256(root))

    def test_source_hash_binds_executable_semantics(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            script = root / "run.sh"
            script.write_text("#!/usr/bin/env bash\n", encoding="utf-8")
            script.chmod(0o600)
            non_executable_sha256 = source_tree_sha256(root)

            script.chmod(0o700)

            self.assertNotEqual(non_executable_sha256, source_tree_sha256(root))

    def test_snapshot_excludes_local_secrets_and_remains_stable_after_source_changes(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "source"
            root.mkdir()
            tracked = root / "backend" / "main.go"
            tracked.parent.mkdir()
            tracked.write_text("package main\n", encoding="utf-8")
            (root / ".env").write_text("secret=value\n", encoding="utf-8")
            (root / ".env.example").write_text("SAFE_VALUE=example\n", encoding="utf-8")
            (root / "config.yaml").write_text("password: secret\n", encoding="utf-8")

            snapshot = Path(directory) / "snapshot"
            snapshot_sha256 = create_readonly_source_snapshot(root, snapshot)

            self.assertEqual(snapshot_sha256, source_tree_sha256(snapshot))
            self.assertFalse((snapshot / ".env").exists())
            self.assertFalse((snapshot / "config.yaml").exists())
            self.assertEqual("SAFE_VALUE=example\n", (snapshot / ".env.example").read_text())
            self.assertEqual("package main\n", (snapshot / "backend" / "main.go").read_text())

            tracked.write_text("package changed\n", encoding="utf-8")
            self.assertEqual(snapshot_sha256, source_tree_sha256(snapshot))

    def test_snapshot_preserves_executable_semantics_while_removing_write_access(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "source"
            root.mkdir()
            script = root / "run.sh"
            script.write_text("#!/usr/bin/env bash\n", encoding="utf-8")
            script.chmod(0o700)
            regular = root / "README.md"
            regular.write_text("read only\n", encoding="utf-8")
            regular.chmod(0o600)

            snapshot = Path(directory) / "snapshot"
            create_readonly_source_snapshot(root, snapshot)

            self.assertEqual(0o500, (snapshot / "run.sh").stat().st_mode & 0o777)
            self.assertEqual(0o400, (snapshot / "README.md").stat().st_mode & 0o777)

    def test_nested_worktree_does_not_change_source_identity_or_snapshot(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "source"
            root.mkdir()
            tracked = root / "backend" / "main.go"
            tracked.parent.mkdir()
            tracked.write_text("package main\n", encoding="utf-8")
            nested_worktree_file = root / "work" / "previous-candidate" / "metadata.txt"
            nested_worktree_file.parent.mkdir(parents=True)
            nested_worktree_file.write_text("first revision\n", encoding="utf-8")

            before = source_tree_sha256(root)
            nested_worktree_file.write_text("second revision\n", encoding="utf-8")

            self.assertEqual(before, source_tree_sha256(root))

            snapshot = Path(directory) / "snapshot"
            create_readonly_source_snapshot(root, snapshot)
            self.assertFalse((snapshot / "work").exists())

    def test_snapshot_ignores_symlinks_only_inside_excluded_directories(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "source"
            root.mkdir()
            target = root / "target.txt"
            target.write_text("safe\n", encoding="utf-8")
            excluded = root / "node_modules"
            excluded.mkdir()
            os.symlink(target, excluded / "linked-package")

            snapshot = Path(directory) / "snapshot"
            create_readonly_source_snapshot(root, snapshot)
            self.assertFalse((snapshot / "node_modules").exists())

            os.symlink(target, root / "unexpected-link")
            with self.assertRaisesRegex(ValueError, "refuses symlink"):
                source_tree_sha256(root)
            with self.assertRaisesRegex(ValueError, "refuses symlink"):
                create_readonly_source_snapshot(root, Path(directory) / "second-snapshot")


if __name__ == "__main__":
    unittest.main()
