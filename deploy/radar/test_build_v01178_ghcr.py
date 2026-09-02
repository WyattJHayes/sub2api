from __future__ import annotations

import importlib.util
import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import call, patch


RADAR_DIR = Path(__file__).resolve().parent
REPO_ROOT = RADAR_DIR.parents[1]
SHA_A = "sha256:" + "a" * 64
SHA_B = "sha256:" + "b" * 64
SHA_C = "sha256:" + "c" * 64


def load_builder():
    path = RADAR_DIR / "build_v01178_ghcr.py"
    spec = importlib.util.spec_from_file_location("radar_build_v01178_test", path)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


class BuildV01178GHCRTests(unittest.TestCase):
    def test_current_source_rejects_missing_worker_test_suite(self) -> None:
        builder = load_builder()
        with tempfile.TemporaryDirectory() as directory:
            source_root = Path(directory)
            backend = source_root / "backend"
            worker = source_root / "radar-worker"
            backend.mkdir()
            worker.mkdir()
            (backend / "go.mod").write_text(
                "module example.invalid/sub2api\n\ngo 1.26.6\n",
                encoding="utf-8",
            )
            (worker / "pyproject.toml").write_text(
                '[project]\nname = "sub2api-radar-worker"\nversion = "0.1.178"\n',
                encoding="utf-8",
            )
            inputs = builder.BuildInputs(
                version="0.1.178",
                image_tag="0.1.178-radar-v16-20260820T101140Z",
                commit="e0c48a19ed794a565e3858662520afe0a1f9f0ba",
                source_sha256=builder.source_tree_sha256(source_root),
                date="2026-08-20T10:11:40Z",
                node_image="node:24-alpine@sha256:" + "1" * 64,
                golang_image="golang:1.26.6-alpine@sha256:" + "2" * 64,
                alpine_image="alpine:3.20@sha256:" + "3" * 64,
                worker_python_base_image="python:3.14-slim@sha256:" + "4" * 64,
                push=True,
            )

            with self.assertRaisesRegex(ValueError, "worker test suite is missing"):
                builder.validate_current_source(inputs, source_root)

    def test_control_plane_probe_verifies_the_recorded_config_digest(self) -> None:
        builder = load_builder()
        reference = builder.CONTROL_REPOSITORY + "@" + SHA_A

        with patch.object(
            builder,
            "run_checked",
            side_effect=["", "sub2api 0.1.178\n"],
        ) as run, patch.object(
            builder,
            "resolve_config_digest",
            return_value=SHA_B,
        ):
            result = builder.verify_control_plane(
                builder.CONTROL_REPOSITORY,
                SHA_A,
                SHA_B,
                "0.1.178",
            )

        self.assertEqual("0.1.178", result)
        self.assertEqual(
            [
                call(["docker", "pull", reference]),
                call(
                    [
                        "docker",
                        "run",
                        "--rm",
                        "--entrypoint",
                        "/bin/sh",
                        reference,
                        "-c",
                        "/app/sub2api -version 2>&1",
                    ]
                ),
            ],
            run.call_args_list,
        )

    def test_current_source_rejects_golang_image_below_go_mod_version(self) -> None:
        builder = load_builder()
        inputs = builder.BuildInputs(
            version="0.1.178",
            image_tag="0.1.178-radar-v16-20260820T101140Z",
            commit="e0c48a19ed794a565e3858662520afe0a1f9f0ba",
            source_sha256=builder.source_tree_sha256(REPO_ROOT),
            date="2026-08-20T10:11:40Z",
            node_image="node:24-alpine@sha256:" + "1" * 64,
            golang_image="golang:1.26.5-alpine@sha256:" + "2" * 64,
            alpine_image="alpine:3.20@sha256:" + "3" * 64,
            worker_python_base_image="python:3.14-slim@sha256:" + "4" * 64,
            push=True,
        )

        with self.assertRaisesRegex(ValueError, "golang image must match backend go.mod"):
            builder.validate_current_source(inputs, REPO_ROOT)

    def test_provenance_accepts_oci_index_with_matching_platform_config(self) -> None:
        builder = load_builder()
        inputs = builder.BuildInputs(
            version="0.1.178",
            image_tag="0.1.178-radar-v16-20260820T101140Z",
            commit="e0c48a19ed794a565e3858662520afe0a1f9f0ba",
            source_sha256="d" * 64,
            date="2026-08-20T10:11:40Z",
            node_image="node:24-alpine@sha256:" + "1" * 64,
            golang_image="golang:1.26.6-alpine@sha256:" + "2" * 64,
            alpine_image="alpine:3.21@sha256:" + "3" * 64,
            worker_python_base_image="python:3.14-slim@sha256:" + "4" * 64,
            push=True,
        )
        reference = builder.CONTROL_REPOSITORY + "@" + SHA_A
        index = json.dumps(
            {
                "mediaType": "application/vnd.oci.image.index.v1+json",
                "manifests": [
                    {
                        "digest": SHA_C,
                        "platform": {"os": "linux", "architecture": "amd64"},
                    }
                ],
            }
        )
        manifest = json.dumps(
            {
                "mediaType": "application/vnd.oci.image.manifest.v1+json",
                "config": {"digest": SHA_B},
            }
        )
        inspection = json.dumps(
            {
                "Id": SHA_A,
                "Config": {
                    "Labels": {
                        "org.opencontainers.image.revision": inputs.commit,
                        "io.sub2api.radar.source-sha256": inputs.source_sha256,
                    }
                },
            }
        )

        with patch.object(
            builder,
            "run_checked",
            side_effect=["", index, manifest, inspection],
        ) as run:
            builder.verify_image_provenance(
                builder.CONTROL_REPOSITORY,
                SHA_A,
                SHA_B,
                inputs,
            )

        self.assertEqual(
            [
                call(["docker", "pull", reference]),
                call(
                    [
                        "docker",
                        "buildx",
                        "imagetools",
                        "inspect",
                        reference,
                        "--raw",
                    ]
                ),
                call(
                    [
                        "docker",
                        "buildx",
                        "imagetools",
                        "inspect",
                        builder.CONTROL_REPOSITORY + "@" + SHA_C,
                        "--raw",
                    ]
                ),
                call(
                    [
                        "docker",
                        "image",
                        "inspect",
                        "--format",
                        "{{json .}}",
                        reference,
                    ]
                ),
            ],
            run.call_args_list,
        )


if __name__ == "__main__":
    unittest.main()
