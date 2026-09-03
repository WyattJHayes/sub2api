from __future__ import annotations

import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from deploy.radar.test_v01181_release_metadata import load_builder


class V020WorkerWheelhouseTests(unittest.TestCase):
    def test_build_workspace_is_outside_source_and_below_its_shared_parent(self) -> None:
        builder = load_builder("build_v020_ghcr.py")

        with tempfile.TemporaryDirectory() as directory:
            source_root = Path(directory) / "source"
            source_root.mkdir()

            with builder.temporary_build_workspace(source_root) as workspace:
                self.assertTrue(workspace.is_dir())
                self.assertEqual(source_root.resolve().parent, workspace.parent)
                self.assertFalse(workspace.is_relative_to(source_root))

    def test_prepares_derived_worker_context_without_mutating_sealed_source(self) -> None:
        builder = load_builder("build_v020_ghcr.py")
        python_image = "python:3.14-slim@sha256:" + "4" * 64

        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            snapshot = root / "source"
            worker_source = snapshot / "radar-worker"
            worker_source.mkdir(parents=True)
            (worker_source / "requirements.lock").write_text(
                "example==1.0 --hash=sha256:" + "a" * 64 + "\n",
                encoding="utf-8",
            )
            (worker_source / "Dockerfile").write_text("COPY wheelhouse /wheelhouse\n")
            (worker_source / "src").mkdir()
            (worker_source / "src" / "worker.py").write_text("VALUE = 1\n")
            for path in sorted(snapshot.rglob("*"), reverse=True):
                path.chmod(0o400 if path.is_file() else 0o500)
            snapshot.chmod(0o500)

            def fake_docker(command: list[str], *, cwd: Path = builder.REPO_ROOT) -> str:
                self.assertEqual("docker", command[0])
                self.assertIn("linux/amd64", command)
                self.assertIn(python_image, command)
                self.assertIn("--require-hashes", command)
                self.assertIn("--only-binary=:all:", command)
                wheelhouse_mount = next(
                    item for item in command if item.endswith(":/wheelhouse")
                )
                wheelhouse = Path(wheelhouse_mount.removesuffix(":/wheelhouse"))
                (wheelhouse / "example-1.0-py3-none-any.whl").write_bytes(b"wheel")
                return ""

            with patch.object(builder, "run_checked", side_effect=fake_docker):
                context = builder.prepare_worker_build_context(
                    snapshot,
                    root / "worker-build-context",
                    python_image,
                )

            self.assertEqual("VALUE = 1\n", (context / "src" / "worker.py").read_text())
            self.assertEqual(
                ["example-1.0-py3-none-any.whl"],
                [path.name for path in (context / "wheelhouse").iterdir()],
            )
            self.assertFalse((worker_source / "wheelhouse").exists())

    def test_rejects_an_empty_downloaded_wheelhouse(self) -> None:
        builder = load_builder("build_v020_ghcr.py")
        python_image = "python:3.14-slim@sha256:" + "4" * 64

        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            worker_source = root / "source" / "radar-worker"
            worker_source.mkdir(parents=True)
            (worker_source / "requirements.lock").write_text(
                "example==1.0 --hash=sha256:" + "a" * 64 + "\n",
                encoding="utf-8",
            )

            with patch.object(builder, "run_checked", return_value=""):
                with self.assertRaisesRegex(ValueError, "wheelhouse is empty"):
                    builder.prepare_worker_build_context(
                        root / "source",
                        root / "worker-build-context",
                        python_image,
                    )


if __name__ == "__main__":
    unittest.main()
