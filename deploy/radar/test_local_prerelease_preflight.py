from __future__ import annotations

import hashlib
import json
import os
import stat
import subprocess
import sys
import tempfile
import unittest
from argparse import Namespace
from datetime import datetime, timedelta, timezone
from pathlib import Path
from unittest.mock import patch

from deploy.radar.local_prerelease_preflight import (
    ImmutableBindings,
    PreflightInputs,
    _compose_config,
    _docker_version,
    _file_sha256,
    _publish_staged_outputs,
    compose_environment,
    inspect_local_images,
    parse_inputs,
    run_preflight,
    source_tree_sha256,
    validate_compose_config,
    validate_endpoint,
    validate_inputs,
    write_rehearsal_environment,
)


DIGEST = "sha256:" + "a" * 64
OTHER_DIGEST = "sha256:" + "b" * 64
THIRD_DIGEST = "sha256:" + "c" * 64
RUN_ID = "radar-local-20260811-rehearsal"
CONTROL_IMAGE = f"registry.invalid/control@{DIGEST}"
WORKER_IMAGE = f"registry.invalid/worker@{OTHER_DIGEST}"


class LocalPrereleasePreflightTests(unittest.TestCase):
    def test_publish_preflight_replaces_empty_private_directory_created_by_runner(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            staging = root / "staging"
            private = staging / "private"
            private.mkdir(parents=True)
            (private / "postgres-password").write_text("fixture\n", encoding="utf-8")
            for name in ("rehearsal.env", "bindings.json", "compose-config.json"):
                (staging / name).write_text("{}\n", encoding="utf-8")
            evidence = root / "evidence"
            (evidence / "private").mkdir(parents=True)

            _publish_staged_outputs(staging, evidence)

            self.assertTrue((evidence / "private" / "postgres-password").is_file())

    def test_direct_preflight_script_execution_is_importable(self) -> None:
        script = Path(__file__).with_name("local_prerelease_preflight.py")
        with tempfile.TemporaryDirectory() as directory:
            completed = subprocess.run(
                [sys.executable, "-B", str(script), "--help"],
                cwd=directory,
                capture_output=True,
                text=True,
                check=False,
            )
        self.assertEqual(0, completed.returncode, completed.stderr)

    def make_inputs(self, root: Path, evidence: Path, backup: Path) -> PreflightInputs:
        return PreflightInputs(
            candidate_root=root,
            run_id=RUN_ID,
            browser_origin="http://127.0.0.1:18080",
            backup_path=backup,
            backup_sha256=hashlib.sha256(backup.read_bytes()).hexdigest(),
            control_plane_image=CONTROL_IMAGE,
            worker_image=WORKER_IMAGE,
            rollback_control_plane_image=f"registry.invalid/control-previous@{DIGEST}",
            rollback_worker_image=f"registry.invalid/worker-previous@{OTHER_DIGEST}",
            dependency_images=tuple(f"registry.invalid/dependency-{index}@{DIGEST}" for index in range(9)),
            evidence_dir=evidence,
            expected_runner_ip="1.13.161.130",
            minimum_free_bytes=1,
        )

    def make_valid_config(self) -> dict[str, object]:
        return {
            "name": RUN_ID,
            "services": {
                "sub2api-staging": {
                    "container_name": f"{RUN_ID}-sub2api-rehearsal",
                    "image": CONTROL_IMAGE,
                    "ports": ["127.0.0.1:18080:8080"],
                    "networks": {"control_plane": None},
                },
                "worker": {
                    "container_name": f"{RUN_ID}-worker-rehearsal",
                    "image": WORKER_IMAGE,
                    "networks": {"control_plane": None, "radar_worker_internal": None},
                    "healthcheck": {
                        "test": [
                            "CMD",
                            "python",
                            "-c",
                            "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8090/health')",
                        ]
                    },
                },
            },
            "networks": {
                "control_plane": {"name": f"{RUN_ID}-control-rehearsal", "internal": True},
                "radar_worker_internal": {"name": f"{RUN_ID}-internal-rehearsal", "internal": True},
            },
            "volumes": {"data": {"name": f"{RUN_ID}-data-rehearsal"}},
        }

    def test_parse_inputs_requires_explicit_runner_ip(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            environment = {
                "RADAR_LOCAL_RUN_ID": RUN_ID,
                "RADAR_LOCAL_BROWSER_ORIGIN": "http://127.0.0.1:18080",
                "RADAR_LOCAL_BACKUP_PATH": str(root / "backup.dump"),
                "RADAR_LOCAL_BACKUP_SHA256": "d" * 64,
                "RADAR_LOCAL_EVIDENCE_DIR": str(root / "evidence"),
                "RADAR_LOCAL_RUNNER_IDENTITY": str(root / "runner-identity.json"),
                "RADAR_CONTROL_PLANE_IMAGE": CONTROL_IMAGE,
                "RADAR_WORKER_IMAGE": WORKER_IMAGE,
                "RADAR_ROLLBACK_CONTROL_PLANE_IMAGE": f"registry.invalid/control-previous@{DIGEST}",
                "RADAR_ROLLBACK_WORKER_IMAGE": f"registry.invalid/worker-previous@{OTHER_DIGEST}",
                "RADAR_NODE_BASE_IMAGE": f"registry.invalid/node@{DIGEST}",
                "RADAR_GOLANG_BASE_IMAGE": f"registry.invalid/golang@{DIGEST}",
                "RADAR_ALPINE_BASE_IMAGE": f"registry.invalid/alpine@{DIGEST}",
                "RADAR_WORKER_BASE_IMAGE": f"registry.invalid/python@{DIGEST}",
                "RADAR_POSTGRES_IMAGE": f"registry.invalid/postgres@{DIGEST}",
                "RADAR_REDIS_IMAGE": f"registry.invalid/redis@{DIGEST}",
                "RADAR_MINIO_IMAGE": f"registry.invalid/minio@{DIGEST}",
                "RADAR_MINIO_MC_IMAGE": f"registry.invalid/minio-mc@{DIGEST}",
                "RADAR_CLAMAV_IMAGE": f"registry.invalid/clamav@{DIGEST}",
            }
            with patch.dict(os.environ, environment, clear=True):
                with self.assertRaisesRegex(ValueError, "RADAR_LOCAL_EXPECTED_RUNNER_IP is required"):
                    parse_inputs(Namespace(candidate_root=str(root)))
            environment["RADAR_LOCAL_EXPECTED_RUNNER_IP"] = "1.13.161.130"
            with patch.dict(os.environ, environment, clear=True):
                inputs = parse_inputs(Namespace(candidate_root=str(root)))
            self.assertEqual("1.13.161.130", inputs.expected_runner_ip)

    def test_production_targets_are_rejected(self) -> None:
        for value in ("192.255.134.229", "sub2api.weihub.cloud"):
            with self.assertRaisesRegex(ValueError, "production target"):
                validate_endpoint(value)

    def test_rehearsal_override_publishes_loopback_control_plane_port(self) -> None:
        override = Path(__file__).parents[1] / "docker-compose.radar-rehearsal.yml"
        text = override.read_text(encoding="utf-8")
        self.assertIn('127.0.0.1:${RADAR_CONTROL_PLANE_PORT:-18080}:8080', text)

    def test_source_digest_changes_with_tracked_source(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "backend" / "main.go"
            source.parent.mkdir()
            source.write_text("package main\n", encoding="utf-8")
            before = source_tree_sha256(root)
            source.write_text("package changed\n", encoding="utf-8")
            self.assertNotEqual(source_tree_sha256(root), before)

    def test_file_and_source_hashing_use_bounded_streaming_reads(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            payload = (b"radar-streaming-hash\n" * 120_000) + b"tail"
            backup = root / "backup.dump"
            backup.write_bytes(payload)
            source = root / "backend" / "large.bin"
            source.parent.mkdir()
            source.write_bytes(payload)
            expected_backup = hashlib.sha256(payload).hexdigest()
            with patch.object(Path, "read_bytes", side_effect=AssertionError("whole-file read is prohibited")):
                self.assertEqual(_file_sha256(backup), expected_backup)
                source_digest = source_tree_sha256(root)
            self.assertRegex(source_digest, r"^[0-9a-f]{64}$")

    def test_source_binding_exact_inclusion_and_exclusion_contract(self) -> None:
        excluded_paths = (
            ".git/config",
            "nested/.git/config",
            ".superpowers/brainstorm/session/state.json",
            "nested/.superpowers/session.log",
            ".pytest_cache/v/cache/nodeids",
            "backend/.pytest_cache/v/cache/lastfailed",
            ".mypy_cache/3.12/cache.db",
            "radar-worker/.mypy_cache/missing_stubs",
            ".ruff_cache/0.12.12/cache",
            "radar-worker/.ruff_cache/0.12.12/cache",
            "frontend/tsconfig.tsbuildinfo",
            "frontend/tsconfig.node.tsbuildinfo",
            "node_modules/pkg/cache.js",
            "frontend/node_modules/pkg/cache.js",
            ".venv/cache.bin",
            "backend/.venv/cache.bin",
            "__pycache__/cache.pyc",
            "backend/__pycache__/cache.pyc",
            "dist/generated.js",
            "frontend/dist/generated.js",
            "test-results/output.json",
            "frontend/test-results/output.json",
            "release-evidence/result.json",
            "nested/release-evidence/result.json",
            "docs/superpowers/sdd/2026-08-11-local-isolated-radar-release-readiness/task-9-report.md",
            "docs/superpowers/sdd/2026-08-11-local-isolated-radar-release-readiness/task-9-environment_spec.diff",
            "docs/superpowers/sdd/2026-08-11-local-isolated-radar-release-readiness/task-9-specification.diff",
            "docs/superpowers/sdd/2026-08-11-local-isolated-radar-release-readiness/task-9-fix-round-1-before/backend/main.go",
        )
        included_paths = (
            "main.go",
            "backend/main.go",
            "test_main.py",
            "backend/tests/test_main.py",
            "docker-compose.yml",
            "deploy/docker-compose.test.yml",
            "Dockerfile",
            "deploy/Dockerfile.test",
            "Makefile",
            "tools/Makefile",
            "runbook.md",
            "deploy/radar/operator-runbook.md",
            "plan.md",
            "docs/superpowers/plans/release-plan.md",
            "specification.md",
            "docs/superpowers/specs/release-specification.md",
            "docs/superpowers/sdd/2026-08-11-local-isolated-radar-release-readiness/task-9-brief.md",
            "docs/superpowers/sdd/2026-08-11-local-isolated-radar-release-readiness/task-9-plan.md",
            "docs/superpowers/sdd/2026-08-11-local-isolated-radar-release-readiness/task-9-specification.md",
        )
        for relative in excluded_paths:
            with self.subTest(excluded=relative), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                before = source_tree_sha256(root)
                path = root / relative
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("generated\n", encoding="utf-8")
                self.assertEqual(source_tree_sha256(root), before)
        for relative in included_paths:
            with self.subTest(included=relative), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                before = source_tree_sha256(root)
                path = root / relative
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("canonical\n", encoding="utf-8")
                self.assertNotEqual(source_tree_sha256(root), before)

    def test_rejects_invalid_run_names_public_origins_and_insecure_backup(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            candidate = root / "candidate"
            candidate.mkdir()
            evidence = root / "evidence"
            evidence.mkdir()
            backup = root / "backup.sql"
            backup.write_text("synthetic backup", encoding="utf-8")
            os.chmod(backup, 0o644)
            inputs = self.make_inputs(candidate, evidence, backup)
            with self.assertRaisesRegex(ValueError, "mode 0600"):
                validate_inputs(inputs)
            os.chmod(backup, 0o600)
            with self.assertRaisesRegex(ValueError, "run_id"):
                validate_inputs(inputs.__class__(**{**inputs.__dict__, "run_id": "bad name"}))
            with self.assertRaisesRegex(ValueError, "HTTP loopback"):
                validate_inputs(inputs.__class__(**{**inputs.__dict__, "browser_origin": "https://example.invalid"}))

    def test_rejects_checksum_mutable_images_low_disk_and_unavailable_docker(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            evidence = root / "evidence"
            evidence.mkdir()
            backup = root / "backup.sql"
            backup.write_text("synthetic backup", encoding="utf-8")
            os.chmod(backup, 0o600)
            inputs = self.make_inputs(root, evidence, backup)
            with self.assertRaisesRegex(ValueError, "checksum"):
                validate_inputs(inputs.__class__(**{**inputs.__dict__, "backup_sha256": "0" * 64}))
            with self.assertRaisesRegex(ValueError, "immutable sha256"):
                validate_inputs(inputs.__class__(**{**inputs.__dict__, "worker_image": "worker:latest"}))
            with self.assertRaisesRegex(ValueError, "10 GiB"):
                validate_inputs(inputs.__class__(**{**inputs.__dict__, "minimum_free_bytes": 10 * 1024**3}), disk_free=lambda _path: 0)

    def test_rejects_evidence_directory_inside_candidate_tree(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            evidence = root / "evidence"
            evidence.mkdir()
            backup = root / "backup.sql"
            backup.write_text("synthetic backup", encoding="utf-8")
            os.chmod(backup, 0o600)
            with self.assertRaisesRegex(ValueError, "outside candidate_root"):
                validate_inputs(self.make_inputs(root, evidence, backup))

    def test_rejects_missing_local_images(self) -> None:
        images = (f"registry.invalid/control@{DIGEST}", f"registry.invalid/worker@{OTHER_DIGEST}")
        with self.assertRaisesRegex(ValueError, "not present locally"):
            inspect_local_images(images, lambda _image: None)
        self.assertEqual(
            inspect_local_images(images, lambda image: image.rsplit("@", 1)[1]),
            (DIGEST, OTHER_DIGEST),
        )

    def test_compose_rejects_host_binding_production_hosts_and_external_networks(self) -> None:
        config = self.make_valid_config()
        validate_compose_config(config, RUN_ID)
        unsafe = json.loads(json.dumps(config))
        unsafe["services"]["worker"]["ports"] = ["0.0.0.0:8081:8081"]
        with self.assertRaisesRegex(ValueError, "loopback"):
            validate_compose_config(unsafe, RUN_ID)
        unsafe = json.loads(json.dumps(config))
        unsafe["services"]["worker"]["environment"] = {"UPSTREAM": "https://sub2api.weihub.cloud"}
        with self.assertRaisesRegex(ValueError, "internal target"):
            validate_compose_config(unsafe, RUN_ID)
        unsafe = json.loads(json.dumps(config))
        unsafe["networks"]["control_plane"]["external"] = True
        with self.assertRaisesRegex(ValueError, "external"):
            validate_compose_config(unsafe, RUN_ID)

    def test_compose_accepts_internal_python_healthcheck(self) -> None:
        try:
            validate_compose_config(self.make_valid_config(), RUN_ID)
        except ValueError as error:
            self.fail(f"internal Python healthcheck was rejected: {error}")

    def test_compose_rejects_wildcard_address_as_an_outbound_target(self) -> None:
        for key, value in (
            ("UPSTREAM_HOST", "0.0.0.0"),
            ("UPSTREAM_URL", "http://0.0.0.0:8080/v1"),
        ):
            with self.subTest(key=key):
                unsafe = self.make_valid_config()
                unsafe["services"]["worker"]["environment"] = {key: value}
                with self.assertRaisesRegex(ValueError, "internal target"):
                    validate_compose_config(unsafe, RUN_ID)

    def test_compose_rejects_bare_public_host_after_client_options(self) -> None:
        unsafe = self.make_valid_config()
        unsafe["services"]["worker"]["command"] = ["curl", "-fsS", "example.com"]
        with self.assertRaisesRegex(ValueError, "internal target"):
            validate_compose_config(unsafe, RUN_ID)

    def test_compose_accepts_bind_fields_and_base_healthcheck_forms(self) -> None:
        config = self.make_valid_config()
        config["services"]["sub2api-staging"]["environment"] = {
            "SERVER_HOST": "0.0.0.0",
            "RADAR_SYNTHETIC_HOST": "0.0.0.0",
            "RADAR_METRICS_HOST": "0.0.0.0",
        }
        config["services"]["sub2api-staging"]["healthcheck"] = {
            "test": ["CMD", "wget", "-q", "-T", "5", "-O", "/dev/null", "http://localhost:8080/health"]
        }
        config["services"]["worker"]["command"] = [
            "sh",
            "-c",
            "curl -fsS --user-agent example.com http://sub2api-staging:8080/health >/dev/null",
        ]
        try:
            validate_compose_config(config, RUN_ID)
        except ValueError as error:
            self.fail(f"base Compose target form was rejected: {error}")

    def test_compose_rejects_unapproved_networking_and_public_targets(self) -> None:
        mutations = (
            ("public URL", lambda value: value["services"]["worker"].update(environment={"UPSTREAM_URL": "https://example.com/v1"}), "internal target"),
            ("embedded production URL", lambda value: value["services"]["worker"].update(command=["sh", "-c", "curl https://sub2api.weihub.cloud/health"]), "internal target"),
            ("embedded production host", lambda value: value["services"]["worker"].update(command=["curl", "192.255.134.229:8080/health"]), "internal target"),
            ("third network", lambda value: value["networks"].update(public={"name": f"{RUN_ID}-public-rehearsal", "internal": False}), "allowed network"),
            ("host networking", lambda value: value["services"]["worker"].update(network_mode="host"), "host network"),
            ("extra hosts", lambda value: value["services"]["worker"].update(extra_hosts=["example.com:203.0.113.1"]), "extra_hosts"),
            ("external links", lambda value: value["services"]["worker"].update(external_links=["public"]), "external_links"),
            ("implicit default", lambda value: value["services"]["worker"].pop("networks"), "approved network"),
        )
        for label, mutate, message in mutations:
            with self.subTest(label=label):
                unsafe = self.make_valid_config()
                mutate(unsafe)
                with self.assertRaisesRegex(ValueError, message):
                    validate_compose_config(unsafe, RUN_ID)

    def test_compose_requires_explicit_project_and_container_names(self) -> None:
        for label, mutate in (
            ("missing project", lambda value: value.pop("name")),
            ("malformed project", lambda value: value.update(name="radar-staging")),
            ("missing container", lambda value: value["services"]["worker"].pop("container_name")),
            ("malformed container", lambda value: value["services"]["worker"].update(container_name="worker-1")),
        ):
            with self.subTest(label=label):
                unsafe = self.make_valid_config()
                mutate(unsafe)
                with self.assertRaisesRegex(ValueError, "named resource"):
                    validate_compose_config(unsafe, RUN_ID)

    def test_compose_rejects_unapproved_rendered_image(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            candidate = root / "candidate"
            evidence = root / "evidence"
            candidate.mkdir()
            evidence.mkdir()
            backup = root / "backup.sql"
            backup.write_text("synthetic backup", encoding="utf-8")
            os.chmod(backup, 0o600)
            inputs = self.make_inputs(candidate, evidence, backup)
            config = self.make_valid_config()
            config["services"]["extra"] = {
                "container_name": f"{RUN_ID}-extra-rehearsal",
                "image": f"registry.invalid/extra@{THIRD_DIGEST}",
                "networks": {"control_plane": None},
            }
            with (
                patch("deploy.radar.local_prerelease_preflight._docker_version", return_value="{}"),
                patch("deploy.radar.local_prerelease_preflight._local_image_id", side_effect=lambda image: image.rsplit("@", 1)[1]),
                patch("deploy.radar.local_prerelease_preflight._compose_config", return_value=config),
            ):
                with self.assertRaisesRegex(ValueError, "approved immutable input"):
                    run_preflight(inputs)

    def test_rendered_images_are_inspected_after_compose_expansion(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            candidate = root / "candidate"
            evidence = root / "evidence"
            candidate.mkdir()
            evidence.mkdir()
            backup = root / "backup.sql"
            backup.write_text("synthetic backup", encoding="utf-8")
            os.chmod(backup, 0o600)
            inputs = self.make_inputs(candidate, evidence, backup)
            rendered_image = inputs.dependency_images[0]
            config = self.make_valid_config()
            config["services"]["extra"] = {
                "container_name": f"{RUN_ID}-extra-rehearsal",
                "image": rendered_image,
                "networks": {"control_plane": None},
            }
            compose_completed = False

            def inspect(image: str) -> str | None:
                if image == rendered_image and compose_completed:
                    return None
                return image.rsplit("@", 1)[1]

            def render(_inputs: PreflightInputs, _environment: Path) -> dict[str, object]:
                nonlocal compose_completed
                compose_completed = True
                return config

            with (
                patch("deploy.radar.local_prerelease_preflight._docker_version", return_value="{}"),
                patch("deploy.radar.local_prerelease_preflight._local_image_id", side_effect=inspect),
                patch("deploy.radar.local_prerelease_preflight._compose_config", side_effect=render),
            ):
                with self.assertRaisesRegex(ValueError, "not present locally"):
                    run_preflight(inputs)

    def test_docker_version_and_compose_timeouts_fail_closed(self) -> None:
        timeout = subprocess.TimeoutExpired(["docker"], 30)
        with patch("deploy.radar.local_prerelease_preflight.subprocess.run", side_effect=timeout):
            try:
                _docker_version()
            except Exception as error:
                self.assertIsInstance(error, RuntimeError)
                self.assertRegex(str(error), "timed out")
            else:
                self.fail("Docker version timeout was accepted")
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            environment = root / "rehearsal.env"
            environment.write_text("SYNTHETIC=1\n", encoding="utf-8")
            os.chmod(environment, 0o600)
            inputs = self.make_inputs(root, root.parent, environment)
            with patch("deploy.radar.local_prerelease_preflight.subprocess.run", side_effect=timeout):
                try:
                    _compose_config(inputs, environment)
                except Exception as error:
                    self.assertIsInstance(error, RuntimeError)
                    self.assertRegex(str(error), "timed out")
                else:
                    self.fail("Compose timeout was accepted")

    def test_unavailable_docker_fails_closed(self) -> None:
        with patch("deploy.radar.local_prerelease_preflight.subprocess.run", side_effect=OSError("absent")):
            try:
                _docker_version()
            except Exception as error:
                self.assertIsInstance(error, RuntimeError)
                self.assertRegex(str(error), "Docker is unavailable")
            else:
                self.fail("unavailable Docker was accepted")

    def test_failed_preflight_publishes_no_partial_artifacts(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            candidate = root / "candidate"
            evidence = root / "evidence"
            candidate.mkdir()
            evidence.mkdir()
            backup = root / "backup.sql"
            backup.write_text("synthetic backup", encoding="utf-8")
            os.chmod(backup, 0o600)
            inputs = self.make_inputs(candidate, evidence, backup)
            with (
                patch("deploy.radar.local_prerelease_preflight._docker_version", return_value="{}"),
                patch("deploy.radar.local_prerelease_preflight._local_image_id", side_effect=lambda image: image.rsplit("@", 1)[1]),
                patch("deploy.radar.local_prerelease_preflight._compose_config", side_effect=ValueError("synthetic compose failure")),
            ):
                with self.assertRaisesRegex(ValueError, "synthetic compose failure"):
                    run_preflight(inputs)
            self.assertEqual(tuple(evidence.iterdir()), ())

    def test_successful_preflight_publishes_complete_private_binding_set(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            candidate = root / "candidate"
            evidence = root / "evidence"
            candidate.mkdir()
            evidence.mkdir()
            backup = root / "backup.sql"
            backup.write_text("synthetic backup", encoding="utf-8")
            os.chmod(backup, 0o600)
            inputs = self.make_inputs(candidate, evidence, backup)
            with (
                patch("deploy.radar.local_prerelease_preflight._docker_version", return_value="{}"),
                patch("deploy.radar.local_prerelease_preflight._local_image_id", side_effect=lambda image: image.rsplit("@", 1)[1]),
                patch("deploy.radar.local_prerelease_preflight._compose_config", return_value=self.make_valid_config()),
            ):
                run_preflight(inputs)
            self.assertEqual(
                {path.name for path in evidence.iterdir()},
                {"rehearsal.env", "bindings.json", "compose-config.json", "private"},
            )
            for path in evidence.iterdir():
                if path.name == "private":
                    self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o700)
                    self.assertEqual(
                        {secret.name for secret in path.iterdir()},
                        {"postgres-password", "database-password", "database.pgpass"},
                    )
                    continue
                self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
            public_environment = compose_environment(evidence / "rehearsal.env")
            self.assertEqual(
                public_environment["RADAR_DATABASE_PASSWORD_FILE"],
                str(evidence / "private" / "database-password"),
            )
            bindings = json.loads((evidence / "bindings.json").read_text(encoding="utf-8"))
            self.assertEqual(
                bindings["source_exclusions"].get("generated_task_artifact_root"),
                "docs/superpowers/sdd/2026-08-11-local-isolated-radar-release-readiness",
            )
            self.assertEqual(bindings["source_exclusions"].get("file_suffixes"), [".tsbuildinfo"])

    def test_invalid_input_does_not_contact_docker(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            candidate = root / "candidate"
            evidence = root / "evidence"
            candidate.mkdir()
            evidence.mkdir()
            backup = root / "backup.sql"
            backup.write_text("synthetic backup", encoding="utf-8")
            os.chmod(backup, 0o600)
            inputs = self.make_inputs(candidate, evidence, backup)
            invalid = inputs.__class__(**{**inputs.__dict__, "run_id": "bad name"})
            with patch("deploy.radar.local_prerelease_preflight._docker_version", return_value="{}") as docker_version:
                with self.assertRaisesRegex(ValueError, "run_id"):
                    run_preflight(invalid)
                docker_version.assert_not_called()

    def test_mismatched_runner_identity_is_rejected_before_docker(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            candidate = root / "candidate"
            candidate.mkdir()
            evidence = root / "evidence"
            evidence.mkdir()
            backup = root / "backup.sql"
            backup.write_text("synthetic backup", encoding="utf-8")
            backup.chmod(0o600)
            identity = root / "runner-identity.json"
            now = datetime.now(timezone.utc)
            identity.write_text(
                json.dumps(
                    {
                        "schema_version": "radar-rehearsal-runner-identity-v1",
                        "run_id": RUN_ID,
                        "instance_id": "ins-test",
                        "public_ip": "101.43.35.235",
                        "machine_id_sha256": "0" * 64,
                        "issued_at": now.isoformat().replace("+00:00", "Z"),
                        "expires_at": (now + timedelta(minutes=15)).isoformat().replace("+00:00", "Z"),
                    }
                ),
                encoding="utf-8",
            )
            identity.chmod(0o600)
            machine_id = root / "machine-id"
            machine_id.write_text("runner-machine\n", encoding="utf-8")
            inputs = self.make_inputs(candidate, evidence, backup)
            inputs = inputs.__class__(
                **{
                    **inputs.__dict__,
                    "runner_identity_path": identity,
                    "runner_machine_id_path": machine_id,
                }
            )
            with patch("deploy.radar.local_prerelease_preflight._docker_version") as docker_version:
                with self.assertRaisesRegex(ValueError, "machine fingerprint"):
                    run_preflight(inputs)
                docker_version.assert_not_called()

    def test_docker_preflight_process_does_not_inherit_database_password_variables(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            environment = root / "rehearsal.env"
            environment.write_text("SYNTHETIC=1\n", encoding="utf-8")
            environment.chmod(0o600)
            inputs = self.make_inputs(root / "candidate", root / "evidence", environment)
            inputs.candidate_root.mkdir()
            inputs.evidence_dir.mkdir()
            seen: dict[str, str] = {}

            def fake_run(command: list[str], **kwargs: object) -> subprocess.CompletedProcess[str]:
                seen.update(kwargs.get("env") or {})
                return subprocess.CompletedProcess(command, 0, "{}", "")

            with patch.dict(os.environ, {"DATABASE_PASSWORD": "preflight-sentinel"}, clear=False):
                with patch("deploy.radar.local_prerelease_preflight.subprocess.run", side_effect=fake_run):
                    _compose_config(inputs, environment)
            self.assertNotIn("DATABASE_PASSWORD", seen)

    def test_writes_mode_0600_environment_without_exposing_credentials_in_bindings(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            output = root / "rehearsal.env"
            private = root / "private"
            bindings = ImmutableBindings(
                run_id="radar-local-20260811-rehearsal",
                source_sha256="c" * 64,
                backup_sha256="d" * 64,
                control_plane_digest=DIGEST,
                worker_digest=OTHER_DIGEST,
                rollback_control_plane_digest=DIGEST,
                rollback_worker_digest=OTHER_DIGEST,
                dependency_digests=(DIGEST, OTHER_DIGEST),
                environment_fingerprint="e" * 64,
            )
            with patch(
                "deploy.radar.local_prerelease_preflight.secrets.token_urlsafe",
                return_value="-generated-secret-with-leading-option-marker",
            ):
                values = write_rehearsal_environment(
                    output,
                    bindings,
                    "http://127.0.0.1:18080",
                    private_dir=private,
                )
            self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)
            self.assertGreaterEqual(len(values["RADAR_RUNNER_WORKER_TOKEN"]), 32)
            self.assertRegex(values["RADAR_MINIO_ROOT_PASSWORD"], r"^[A-Za-z0-9]")
            environment = compose_environment(output)
            self.assertEqual(environment["RADAR_COMPOSE_PROJECT_NAME"], bindings.run_id)
            self.assertEqual(environment["RADAR_CONTROL_PLANE_PORT"], "18080")
            self.assertEqual(environment["RADAR_RELEASE_VERSION"], "0.1.178-local-rehearsal")
            self.assertEqual(environment["RADAR_RELEASE_COMMIT"], bindings.source_sha256)
            self.assertEqual(environment["RADAR_RELEASE_DATE"], "1970-01-01T00:00:00Z")
            self.assertNotIn("RADAR_POSTGRES_PASSWORD", environment)
            self.assertEqual(
                environment["RADAR_POSTGRES_PASSWORD_FILE"],
                str(private / "postgres-password"),
            )
            self.assertEqual(
                environment["RADAR_DATABASE_PASSWORD_FILE"],
                str(private / "database-password"),
            )
            self.assertEqual(
                environment["RADAR_DATABASE_PGPASS_FILE"],
                str(private / "database.pgpass"),
            )
            self.assertEqual(
                (private / "database.pgpass").read_text(encoding="utf-8"),
                "*:*:*:*:-generated-secret-with-leading-option-marker\n",
            )
            for secret_path in private.iterdir():
                self.assertEqual(0o600, stat.S_IMODE(secret_path.stat().st_mode))
            self.assertNotIn("RADAR_POSTGRES_PASSWORD=", output.read_text(encoding="utf-8"))
            self.assertNotIn(values["RADAR_ADMIN_PASSWORD"], json.dumps(bindings.__dict__))


if __name__ == "__main__":
    unittest.main()
