from __future__ import annotations

import importlib.util
import json
import stat
import sys
import tempfile
import unittest
from dataclasses import replace
from pathlib import Path
from typing import Any
from unittest.mock import call, patch


RADAR_DIR = Path(__file__).resolve().parent


def load_script(name: str, filename: str) -> Any:
    spec = importlib.util.spec_from_file_location(name, RADAR_DIR / filename)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load {filename}")
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


build_tool = load_script("radar_build_v01171_ghcr", "build_v01171_ghcr.py")


SHA_A = "sha256:" + "a" * 64
SHA_B = "sha256:" + "b" * 64


def valid_inputs() -> Any:
    return build_tool.BuildInputs(
        version="0.1.171-radar-v11-20260807T010203Z",
        commit="c" * 40,
        date="2026-08-07T01:02:03Z",
        node_image="node:24-alpine@sha256:" + "1" * 64,
        golang_image="golang:1.26.5-alpine@sha256:" + "2" * 64,
        alpine_image="alpine:3.20@sha256:" + "3" * 64,
        worker_python_base_image="python:3.14-slim@sha256:" + "4" * 64,
        push=True,
    )


class BuildV01171GhcrTests(unittest.TestCase):
    def test_constructs_exact_control_plane_and_worker_commands(self) -> None:
        inputs = valid_inputs()
        control = build_tool.control_plane_command(inputs, Path("control-metadata.json"))
        worker = build_tool.worker_command(inputs, Path("worker-metadata.json"))

        self.assertEqual("deploy/Dockerfile.radar-control-staging", control[control.index("-f") + 1])
        self.assertIn("linux/amd64", control)
        self.assertIn("--push", control)
        for argument in ("VERSION", "COMMIT", "DATE", "NODE_IMAGE", "GOLANG_IMAGE", "ALPINE_IMAGE"):
            self.assertIn(f"{argument}=", "\n".join(control))

        self.assertEqual("radar-worker/Dockerfile", worker[worker.index("-f") + 1])
        self.assertEqual("radar-worker", worker[-1])
        self.assertIn("RADAR_WORKER_PYTHON_BASE_IMAGE=" + inputs.worker_python_base_image, worker)

    def test_rejects_tag_only_base_image(self) -> None:
        with self.assertRaisesRegex(ValueError, "node_image must be digest-qualified"):
            build_tool.validate_inputs(replace(valid_inputs(), node_image="node:24-alpine"))

    def test_rejects_non_ghcr_target_repository(self) -> None:
        with self.assertRaisesRegex(ValueError, "repository must be under ghcr.io"):
            build_tool.digest_reference("docker.io/example/radar", SHA_A)

    def test_rejects_malformed_commit_non_utc_date_and_missing_push(self) -> None:
        cases = (
            (replace(valid_inputs(), commit="abc"), "commit must be 40 lowercase"),
            (replace(valid_inputs(), date="2026-08-07T01:02:03+08:00"), "date must be UTC"),
            (replace(valid_inputs(), push=False), "--push is required"),
        )
        for inputs, message in cases:
            with self.subTest(message=message), self.assertRaisesRegex(ValueError, message):
                build_tool.validate_inputs(inputs)

    def test_metadata_requires_manifest_and_config_digests(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            metadata = Path(directory) / "metadata.json"
            metadata.write_text(
                json.dumps({"containerimage.config.digest": SHA_B}), encoding="utf-8"
            )

            with self.assertRaisesRegex(ValueError, "containerimage.digest"):
                build_tool.parse_metadata(metadata)

            metadata.write_text(
                json.dumps({"containerimage.digest": SHA_A}), encoding="utf-8"
            )
            with self.assertRaisesRegex(ValueError, "containerimage.config.digest"):
                build_tool.parse_metadata(metadata)

    def test_runtime_version_must_match_expected_value(self) -> None:
        self.assertEqual(
            "sub2api 0.1.171-radar-v11-20260807T010203Z",
            build_tool.validate_version_output(
                "sub2api 0.1.171-radar-v11-20260807T010203Z\n",
                "0.1.171-radar-v11-20260807T010203Z",
                "control-plane",
            ),
        )
        with self.assertRaisesRegex(ValueError, "Worker version output"):
            build_tool.validate_version_output("0.0.9\n", "0.1.0", "Worker")

    def test_runtime_probes_pull_digest_references_and_override_entrypoints(self) -> None:
        control_ref = build_tool.CONTROL_REPOSITORY + "@" + SHA_A
        worker_ref = build_tool.WORKER_REPOSITORY + "@" + SHA_B
        with patch.object(
            build_tool,
            "run_checked",
            side_effect=["", "Sub2API release-v11\n", "", "0.1.0\n"],
        ) as run:
            control_output = build_tool.verify_control_plane(
                build_tool.CONTROL_REPOSITORY, SHA_A, "release-v11"
            )
            worker_output = build_tool.verify_worker(
                build_tool.WORKER_REPOSITORY, SHA_B, "0.1.0"
            )

        self.assertEqual("Sub2API release-v11", control_output)
        self.assertEqual("0.1.0", worker_output)
        self.assertEqual(
            [
                call(["docker", "pull", control_ref]),
                call(
                    [
                        "docker",
                        "run",
                        "--rm",
                        "--entrypoint",
                        "/bin/sh",
                        control_ref,
                        "-c",
                        "/app/sub2api -version 2>&1",
                    ]
                ),
                call(["docker", "pull", worker_ref]),
                call(
                    [
                        "docker",
                        "run",
                        "--rm",
                        "--entrypoint",
                        "python",
                        worker_ref,
                        "-c",
                        (
                            "import importlib.metadata as m; "
                            "print(m.version('sub2api-radar-worker'))"
                        ),
                    ]
                ),
            ],
            run.call_args_list,
        )

    def test_writes_redacted_mode_0600_dual_image_record(self) -> None:
        inputs = valid_inputs()
        record = build_tool.build_record(
            inputs,
            control_manifest=SHA_A,
            control_config=SHA_B,
            control_version="sub2api " + inputs.version,
            worker_manifest=SHA_B,
            worker_config=SHA_A,
            worker_version="0.1.0",
        )
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "image-record.json"
            build_tool.write_record(output, record)

            stored = json.loads(output.read_text(encoding="utf-8"))
            self.assertEqual("radar-v01171-image-record-v1", stored["schema_version"])
            self.assertEqual(build_tool.CONTROL_REPOSITORY, stored["control_plane"]["repository"])
            self.assertEqual(build_tool.WORKER_REPOSITORY, stored["worker"]["repository"])
            self.assertEqual("linux/amd64", stored["platform"])
            self.assertEqual(0o600, stat.S_IMODE(output.stat().st_mode))
            self.assertNotIn("environment", stored)
            self.assertNotIn("credentials", stored)


if __name__ == "__main__":
    unittest.main()
