from __future__ import annotations

import json
import os
import stat
import tempfile
import unittest
from datetime import datetime, timedelta, timezone
from pathlib import Path

from deploy.radar.rehearsal_isolation_gate import (
    RunnerIdentity,
    load_runner_identity,
    validate_runtime_topology,
    validate_static_compose,
    verify_local_docker_context,
    write_runtime_report,
)


RUN_ID = "radar-local-20260814-rehearsal"
CONTROL_IMAGE = "registry.invalid/control@sha256:" + "a" * 64
WORKER_IMAGE = "registry.invalid/worker@sha256:" + "b" * 64
MACHINE_ID_SHA256 = "c" * 64


def runner_identity_document() -> dict[str, str]:
    now = datetime.now(timezone.utc)
    return {
        "schema_version": "radar-rehearsal-runner-identity-v1",
        "run_id": RUN_ID,
        "instance_id": "ins-radar-rehearsal",
        "public_ip": "101.43.35.235",
        "machine_id_sha256": MACHINE_ID_SHA256,
        "issued_at": now.isoformat().replace("+00:00", "Z"),
        "expires_at": (now + timedelta(minutes=30)).isoformat().replace("+00:00", "Z"),
    }


def valid_compose() -> dict[str, object]:
    return {
        "name": RUN_ID,
        "services": {
            "sub2api-staging": {
                "container_name": f"{RUN_ID}-sub2api-rehearsal",
                "image": CONTROL_IMAGE,
                "networks": {"control_plane": None},
                "ports": [{"host_ip": "127.0.0.1", "published": "18080", "target": 8080}],
                "volumes": [
                    {
                        "type": "bind",
                        "source": "/evidence/private/database-password",
                        "target": "/run/secrets/radar-database-password",
                        "read_only": True,
                    }
                ],
            },
            "postgres-staging": {
                "container_name": f"{RUN_ID}-postgres-rehearsal",
                "image": WORKER_IMAGE,
                "networks": {"control_plane": None},
                "volumes": [
                    {
                        "type": "bind",
                        "source": "/evidence/private/postgres-password",
                        "target": "/run/secrets/radar-postgres-password",
                        "read_only": True,
                    },
                    {
                        "type": "bind",
                        "source": "/evidence/private/database.pgpass",
                        "target": "/run/secrets/radar-database.pgpass",
                        "read_only": True,
                    },
                ],
            },
            "radar-runner": {
                "container_name": f"{RUN_ID}-runner-rehearsal",
                "image": WORKER_IMAGE,
                "networks": {"control_plane": None, "radar_worker_internal": None},
            },
        },
        "networks": {
            "control_plane": {"name": f"{RUN_ID}-control-rehearsal", "internal": True},
            "radar_worker_internal": {"name": f"{RUN_ID}-internal-rehearsal", "internal": True},
        },
        "volumes": {
            "state": {"name": f"{RUN_ID}-state-rehearsal"},
        },
    }


def runtime_container(
    *,
    service: str = "sub2api-staging",
    image: str = CONTROL_IMAGE,
    privileged: bool = False,
    mounts: list[dict[str, object]] | None = None,
    ports: dict[str, object] | None = None,
    networks: dict[str, object] | None = None,
) -> dict[str, object]:
    return {
        "Name": f"/{RUN_ID}-{service}-rehearsal",
        "Config": {
            "Image": image,
            "Labels": {
                "com.docker.compose.project": RUN_ID,
                "com.docker.compose.service": service,
            },
        },
        "RepoDigests": [image],
        "HostConfig": {
            "NetworkMode": f"{RUN_ID}-control-rehearsal",
            "Privileged": privileged,
            "PortBindings": ports or {"8080/tcp": [{"HostIp": "127.0.0.1", "HostPort": "18080"}]},
        },
        "Mounts": mounts or [],
        "NetworkSettings": {
            "Networks": networks or {f"{RUN_ID}-control-rehearsal": {"NetworkID": "network-1"}},
        },
    }


class RehearsalIsolationGateTests(unittest.TestCase):
    def test_loads_only_mode_0600_unexpired_runner_identity(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            record = root / "identity.json"
            record.write_text(json.dumps(runner_identity_document()), encoding="utf-8")
            record.chmod(0o600)
            identity = load_runner_identity(record, run_id=RUN_ID, now=datetime.now(timezone.utc))
            self.assertEqual(
                identity,
                RunnerIdentity(
                    machine_id_sha256=MACHINE_ID_SHA256,
                    expected_public_ip="101.43.35.235",
                    instance_id="ins-radar-rehearsal",
                ),
            )
            record.chmod(0o644)
            with self.assertRaisesRegex(ValueError, "mode 0600"):
                load_runner_identity(record, run_id=RUN_ID, now=datetime.now(timezone.utc))

    def test_remote_docker_context_is_rejected_before_compose(self) -> None:
        with self.assertRaisesRegex(ValueError, "local Unix Docker"):
            verify_local_docker_context(
                docker_host="tcp://127.0.0.1:2375",
                context_endpoint="tcp://127.0.0.1:2375",
            )
        with self.assertRaisesRegex(ValueError, "local Unix Docker"):
            verify_local_docker_context(docker_host=None, context_endpoint="ssh://runner")
        verify_local_docker_context(docker_host=None, context_endpoint="unix:///var/run/docker.sock")

    def test_static_compose_rejects_privileged_socket_mount_extra_network_and_public_port(self) -> None:
        allowed_images = {CONTROL_IMAGE, WORKER_IMAGE}
        validate_static_compose(valid_compose(), release_id=RUN_ID, allowed_images=allowed_images)
        mutations = (
            ("privileged", lambda config: config["services"]["radar-runner"].update(privileged=True), "privileged"),
            (
                "docker socket",
                lambda config: config["services"]["radar-runner"].update(
                    volumes=["/var/run/docker.sock:/var/run/docker.sock"]
                ),
                "Docker socket",
            ),
            (
                "unapproved host secret",
                lambda config: config["services"]["radar-runner"].update(
                    volumes=[
                        {
                            "type": "bind",
                            "source": "/tmp/database-password",
                            "target": "/run/secrets/radar-database-password",
                            "read_only": True,
                        }
                    ]
                ),
                "approved read-only secret",
            ),
            (
                "extra network",
                lambda config: config["services"]["radar-runner"].update(networks={"public": None}),
                "approved network",
            ),
            (
                "public port",
                lambda config: config["services"]["sub2api-staging"].update(ports=["0.0.0.0:18080:8080"]),
                "loopback",
            ),
        )
        for label, mutate, message in mutations:
            with self.subTest(label=label):
                config = json.loads(json.dumps(valid_compose()))
                mutate(config)
                with self.assertRaisesRegex(ValueError, message):
                    validate_static_compose(config, release_id=RUN_ID, allowed_images=allowed_images)

    def test_runtime_topology_rejects_extra_network_privilege_and_socket(self) -> None:
        allowed_images = {CONTROL_IMAGE, WORKER_IMAGE}
        valid = runtime_container()
        validate_runtime_topology([valid], release_id=RUN_ID, allowed_images=allowed_images)
        cases = (
            (
                runtime_container(privileged=True),
                "privileged",
            ),
            (
                runtime_container(
                    mounts=[
                        {"Source": "/var/run/docker.sock", "Destination": "/var/run/docker.sock", "RW": False}
                    ]
                ),
                "Docker socket",
            ),
            (
                runtime_container(networks={"bridge": {}}),
                "approved network",
            ),
        )
        for document, message in cases:
            with self.subTest(message=message):
                with self.assertRaisesRegex(ValueError, message):
                    validate_runtime_topology([document], release_id=RUN_ID, allowed_images=allowed_images)

    def test_runtime_topology_accepts_repository_alias_with_same_manifest_digest(self) -> None:
        digest = "sha256:" + "a" * 64
        configured_image = "postgres:18-alpine@" + digest
        document = runtime_container(image=configured_image)
        document["RepoDigests"] = ["postgres@" + digest]

        validate_runtime_topology(
            [document],
            release_id=RUN_ID,
            allowed_images={configured_image},
        )

    def test_runtime_report_is_private_and_content_addressed(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "private" / "runtime-isolation.json"
            write_runtime_report(path, release_id=RUN_ID, documents=[runtime_container()])
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
            report = json.loads(path.read_text(encoding="utf-8"))
            self.assertEqual(report["schema_version"], "radar-rehearsal-runtime-isolation-v1")
            self.assertEqual(report["release_id"], RUN_ID)
            self.assertRegex(report["topology_sha256"], r"^[0-9a-f]{64}$")


if __name__ == "__main__":
    unittest.main()
