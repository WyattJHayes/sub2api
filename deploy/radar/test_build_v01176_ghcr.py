from __future__ import annotations

import importlib.util
import io
import json
import stat
import sys
import tempfile
import unittest
from contextlib import redirect_stderr
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


build_tool = load_script("radar_build_v01176_ghcr", "build_v01176_ghcr.py")


SHA_A = "sha256:" + "a" * 64
SHA_B = "sha256:" + "b" * 64


def valid_inputs() -> Any:
    return build_tool.BuildInputs(
        version="0.1.176",
        image_tag="0.1.176-radar-v14-20260809T010203Z",
        commit="c" * 40,
        source_sha256="c" * 64,
        date="2026-08-09T01:02:03Z",
        node_image="node:24-alpine@sha256:" + "1" * 64,
        golang_image="golang:1.26.5-alpine@sha256:" + "2" * 64,
        alpine_image="alpine:3.20@sha256:" + "3" * 64,
        worker_python_base_image="python:3.14-slim@sha256:" + "4" * 64,
        push=True,
    )


class BuildV01176GhcrTests(unittest.TestCase):
    def test_control_plane_dockerfile_uses_reachable_verified_dependency_mirrors(self) -> None:
        dockerfile = (
            RADAR_DIR.parents[1] / "deploy" / "Dockerfile.radar-control-staging"
        ).read_text(encoding="utf-8")

        self.assertIn(
            "ARG APK_REPOSITORY=https://mirrors.aliyun.com/alpine", dockerfile
        )
        self.assertIn("ARG GOPROXY=https://goproxy.cn,direct", dockerfile)
        self.assertEqual(
            2,
            dockerfile.count(
                'sed -i "s|https://dl-cdn.alpinelinux.org/alpine|${APK_REPOSITORY}|g" '
                "/etc/apk/repositories"
            ),
        )

    def test_constructs_exact_control_plane_and_worker_commands(self) -> None:
        inputs = valid_inputs()
        control = build_tool.control_plane_command(inputs, Path("control-metadata.json"))
        worker = build_tool.worker_command(inputs, Path("worker-metadata.json"))

        self.assertEqual(
            "deploy/Dockerfile.radar-control-staging", control[control.index("-f") + 1]
        )
        self.assertIn("linux/amd64", control)
        self.assertIn("--push", control)
        for argument in ("VERSION", "COMMIT", "DATE", "NODE_IMAGE", "GOLANG_IMAGE", "ALPINE_IMAGE"):
            self.assertIn(f"{argument}=", "\n".join(control))
        self.assertIn("VERSION=0.1.176", control)
        self.assertIn(build_tool.CONTROL_REPOSITORY + ":" + inputs.image_tag, control)
        self.assertIn("--label", control)
        self.assertIn(
            "org.opencontainers.image.revision=" + inputs.commit,
            control,
        )
        self.assertIn(
            "io.sub2api.radar.source-sha256=" + inputs.source_sha256,
            control,
        )

        self.assertEqual("radar-worker/Dockerfile", worker[worker.index("-f") + 1])
        self.assertEqual("radar-worker", worker[-1])
        self.assertIn("RADAR_WORKER_PYTHON_BASE_IMAGE=" + inputs.worker_python_base_image, worker)
        self.assertIn("--label", worker)
        self.assertIn(
            "org.opencontainers.image.revision=" + inputs.commit,
            worker,
        )
        self.assertIn(
            "io.sub2api.radar.source-sha256=" + inputs.source_sha256,
            worker,
        )

    def test_rejects_source_sha256_that_does_not_match_the_current_source_tree(self) -> None:
        with patch.object(build_tool, "source_tree_sha256", return_value="d" * 64):
            with self.assertRaisesRegex(ValueError, "current source tree"):
                build_tool.validate_current_source(valid_inputs())

        with (
            patch.object(build_tool, "source_tree_sha256", return_value="c" * 64),
            patch.object(build_tool, "worker_package_version", return_value="0.1.176"),
        ):
            build_tool.validate_current_source(valid_inputs())

    def test_rejects_tag_only_base_image(self) -> None:
        with self.assertRaisesRegex(ValueError, "node_image must be digest-qualified"):
            build_tool.validate_inputs(replace(valid_inputs(), node_image="node:24-alpine"))

    def test_rejects_non_ghcr_target_repository(self) -> None:
        with self.assertRaisesRegex(ValueError, "repository must be under ghcr.io"):
            build_tool.digest_reference("docker.io/example/radar", SHA_A)

    def test_rejects_malformed_commit_non_utc_date_and_missing_push(self) -> None:
        cases = (
            (replace(valid_inputs(), commit="abc"), "commit must be 40 lowercase"),
            (replace(valid_inputs(), date="2026-08-09T01:02:03+08:00"), "date must be UTC"),
            (replace(valid_inputs(), push=False), "--push is required"),
        )
        for inputs, message in cases:
            with self.subTest(message=message), self.assertRaisesRegex(ValueError, message):
                build_tool.validate_inputs(inputs)

    def test_source_sha256_is_required_and_must_match_revision_prefix(self) -> None:
        command = [
            "--version", "0.1.176",
            "--image-tag", "0.1.176-radar-v14-20260809T010203Z",
            "--commit", "c" * 40,
            "--date", "2026-08-09T01:02:03Z",
            "--node-image", "node:24-alpine@sha256:" + "1" * 64,
            "--golang-image", "golang:1.26.5-alpine@sha256:" + "2" * 64,
            "--alpine-image", "alpine:3.20@sha256:" + "3" * 64,
            "--worker-python-base-image", "python:3.14-slim@sha256:" + "4" * 64,
            "--push",
            "--output", "image-record.json",
        ]
        with redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            build_tool.parse_args(command)

        fields = build_tool.BuildInputs.__dataclass_fields__
        self.assertIn("source_sha256", fields)
        if "source_sha256" in fields:
            with self.assertRaisesRegex(ValueError, "commit must equal source_sha256 prefix"):
                build_tool.validate_inputs(
                    replace(valid_inputs(), source_sha256="d" * 64)
                )

    def test_requires_exact_app_version_and_timestamped_v14_image_tag(self) -> None:
        cases = (
            (replace(valid_inputs(), version="0.1.173"), "version must equal 0.1.176"),
            (replace(valid_inputs(), version="0.1.176-radar-v14"), "version must equal 0.1.176"),
            (replace(valid_inputs(), image_tag="0.1.176"), "image_tag must use"),
            (
                replace(valid_inputs(), image_tag="0.1.176-radar-v14-20260809T010204Z"),
                "image_tag timestamp must match",
            ),
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

    def test_metadata_resolves_missing_config_digest_from_amd64_manifest(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            metadata = Path(directory) / "metadata.json"
            metadata.write_text(
                json.dumps(
                    {
                        "containerimage.digest": SHA_A,
                        "image.name": "ghcr.io/example/radar:v13",
                    }
                ),
                encoding="utf-8",
            )
            index = json.dumps(
                {
                    "mediaType": "application/vnd.oci.image.index.v1+json",
                    "manifests": [
                        {
                            "digest": SHA_B,
                            "platform": {"os": "linux", "architecture": "amd64"},
                        }
                    ],
                }
            )
            manifest = json.dumps(
                {
                    "mediaType": "application/vnd.oci.image.manifest.v1+json",
                    "config": {"digest": SHA_A},
                }
            )
            with patch.object(build_tool, "run_checked", side_effect=[index, manifest]) as run:
                self.assertEqual((SHA_A, SHA_A), build_tool.parse_metadata(metadata))
            self.assertEqual(2, run.call_count)

    def test_runtime_version_must_match_expected_value(self) -> None:
        self.assertEqual(
            "0.1.176",
            build_tool.validate_version_output(
                "sub2api 0.1.176\n",
                "0.1.176",
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
            side_effect=["", "Sub2API release-v13\n", "", "0.1.0\n"],
        ) as run:
            control_output = build_tool.verify_control_plane(
                build_tool.CONTROL_REPOSITORY, SHA_A, "release-v13"
            )
            worker_output = build_tool.verify_worker(
                build_tool.WORKER_REPOSITORY, SHA_B, "0.1.0"
            )

        self.assertEqual("release-v13", control_output)
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

    def test_image_provenance_requires_exact_digest_labels(self) -> None:
        inputs = valid_inputs()
        reference = build_tool.CONTROL_REPOSITORY + "@" + SHA_A
        labels = json.dumps(
            {
                "org.opencontainers.image.revision": inputs.commit,
                "io.sub2api.radar.source-sha256": inputs.source_sha256,
            }
        )
        inspected = json.dumps(
            {
                "Id": SHA_B,
                "Config": {"Labels": json.loads(labels)},
            }
        )
        with patch.object(build_tool, "run_checked", side_effect=["", inspected]) as run:
            build_tool.verify_image_provenance(
                build_tool.CONTROL_REPOSITORY,
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

        with patch.object(
            build_tool,
            "run_checked",
            side_effect=[
                "",
                json.dumps(
                    {
                        "Id": SHA_B,
                        "Config": {"Labels": {"org.opencontainers.image.revision": inputs.commit}},
                    }
                ),
            ],
        ):
            with self.assertRaisesRegex(ValueError, "source hash label"):
                build_tool.verify_image_provenance(
                    build_tool.CONTROL_REPOSITORY,
                    SHA_A,
                    SHA_B,
                    inputs,
                )

        with patch.object(
            build_tool,
            "run_checked",
            side_effect=["", json.dumps({"Id": SHA_A, "Config": {"Labels": json.loads(labels)}})],
        ):
            with self.assertRaisesRegex(ValueError, "config digest"):
                build_tool.verify_image_provenance(
                    build_tool.CONTROL_REPOSITORY,
                    SHA_A,
                    SHA_B,
                    inputs,
                )

    def test_candidate_build_runbook_requires_source_sha_and_correct_tag_timestamp(self) -> None:
        runbook = (RADAR_DIR / "v01176-upgrade-runbook.md").read_text(encoding="utf-8")
        self.assertIn("--source-sha256 <64 lowercase hex>", runbook)
        self.assertRegex(runbook, r"timestamp embedded in\s+`--image-tag`")

    def test_v01176_runbook_uses_private_credential_files_and_current_baseline(self) -> None:
        runbook = (RADAR_DIR / "v01176-upgrade-runbook.md").read_text(encoding="utf-8")
        self.assertIn(
            "RADAR_MIGRATION_REHEARSAL_POSTGRES_PASSWORD_FILE=/secure/rehearsal/postgres-password",
            runbook,
        )
        self.assertIn(
            "RADAR_MIGRATION_REHEARSAL_DATABASE_PASSWORD_FILE=/secure/rehearsal/database-password",
            runbook,
        )
        self.assertIn(
            "RADAR_MIGRATION_REHEARSAL_PGPASS_FILE=/secure/rehearsal/database.pgpass",
            runbook,
        )
        self.assertNotIn("RADAR_MIGRATION_REHEARSAL_DATABASE_PASSWORD=", runbook)
        self.assertIn("current v0.1.173 production rollback digests", runbook)
        self.assertIn("284 source migration SQL files", runbook)
        self.assertRegex(runbook, r"must produce 286\s+runtime schema_migrations records")
        self.assertIn("deploy/radar/rehearse-v01176-migrations.sh", runbook)
        readme = (RADAR_DIR / "README.md").read_text(encoding="utf-8")
        self.assertIn("deploy/radar/v01176-upgrade-runbook.md", readme)
        self.assertIn("deploy/radar/rehearse-v01176-migrations.sh", readme)

    def test_writes_redacted_mode_0600_dual_image_record(self) -> None:
        inputs = valid_inputs()
        record = build_tool.build_record(
            inputs,
            control_manifest=SHA_A,
            control_config=SHA_B,
            control_version="sub2api " + inputs.version,
            worker_manifest=SHA_B,
            worker_config=SHA_A,
            worker_version="0.1.176",
        )
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "image-record.json"
            build_tool.write_record(output, record)

            stored = json.loads(output.read_text(encoding="utf-8"))
            self.assertEqual("radar-v01176-image-record-v1", stored["schema_version"])
            self.assertEqual("0.1.176", stored["version"])
            self.assertEqual(inputs.image_tag, stored["image_tag"])
            self.assertEqual(build_tool.CONTROL_REPOSITORY, stored["control_plane"]["repository"])
            self.assertEqual(build_tool.WORKER_REPOSITORY, stored["worker"]["repository"])
            self.assertEqual("linux/amd64", stored["platform"])
            self.assertEqual(inputs.version, stored["control_plane"]["version_output"])
            self.assertEqual(inputs.version, stored["worker"]["version_output"])
            self.assertIn("source_sha256", stored)
            self.assertIn("revision", stored)
            if "source_sha256" in stored and "revision" in stored:
                self.assertEqual("c" * 64, stored["source_sha256"])
                self.assertEqual(inputs.commit, stored["revision"])
            self.assertEqual(0o600, stat.S_IMODE(output.stat().st_mode))
            self.assertNotIn("environment", stored)
            self.assertNotIn("credentials", stored)


if __name__ == "__main__":
    unittest.main()
