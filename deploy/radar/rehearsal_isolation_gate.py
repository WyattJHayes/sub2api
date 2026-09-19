#!/usr/bin/env python3
"""Fail-closed isolation checks for the Tencent Cloud Radar rehearsal runner."""

from __future__ import annotations

import ipaddress
import json
import re
import stat
import subprocess
import sys
import argparse
import hashlib
import os
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Iterable


IDENTITY_SCHEMA = "radar-rehearsal-runner-identity-v1"
IDENTITY_KEYS = frozenset(
    {
        "schema_version",
        "run_id",
        "instance_id",
        "public_ip",
        "machine_id_sha256",
        "issued_at",
        "expires_at",
    }
)
SHA256_PATTERN = re.compile(r"^[0-9a-f]{64}$")
IMAGE_PATTERN = re.compile(r"^.+@sha256:[0-9a-f]{64}$")
ALLOWED_NETWORKS = frozenset({"control_plane", "radar_worker_internal"})
SECRET_MOUNTS = {
    "postgres-password": "/run/secrets/radar-postgres-password",
    "database-password": "/run/secrets/radar-database-password",
    "database.pgpass": "/run/secrets/radar-database.pgpass",
}
PRODUCTION_TARGETS = frozenset({"192.255.134.229", "sub2api.weihub.cloud"})


@dataclass(frozen=True)
class RunnerIdentity:
    machine_id_sha256: str
    expected_public_ip: str
    instance_id: str


def _parse_timestamp(value: object, field: str) -> datetime:
    if not isinstance(value, str) or not value.endswith("Z"):
        raise ValueError(f"runner identity {field} must be RFC3339 UTC")
    try:
        parsed = datetime.fromisoformat(value[:-1] + "+00:00")
    except ValueError as error:
        raise ValueError(f"runner identity {field} must be RFC3339 UTC") from error
    if parsed.tzinfo is None:
        raise ValueError(f"runner identity {field} must be RFC3339 UTC")
    return parsed.astimezone(timezone.utc)


def load_runner_identity(path: Path, *, run_id: str, now: datetime | None = None) -> RunnerIdentity:
    if path.is_symlink() or not path.is_file() or stat.S_IMODE(path.stat().st_mode) != 0o600:
        raise ValueError("runner identity must be a non-symlink regular file with mode 0600")
    try:
        document = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise ValueError("runner identity is not valid JSON") from error
    if not isinstance(document, dict) or frozenset(document) != IDENTITY_KEYS:
        raise ValueError("runner identity has an invalid schema")
    if document["schema_version"] != IDENTITY_SCHEMA or document["run_id"] != run_id:
        raise ValueError("runner identity does not bind this rehearsal run")
    if not isinstance(document["instance_id"], str) or not document["instance_id"]:
        raise ValueError("runner identity instance_id is invalid")
    if not isinstance(document["machine_id_sha256"], str) or SHA256_PATTERN.fullmatch(document["machine_id_sha256"]) is None:
        raise ValueError("runner identity machine_id_sha256 is invalid")
    try:
        public_ip = ipaddress.ip_address(document["public_ip"])
    except ValueError as error:
        raise ValueError("runner identity public_ip is invalid") from error
    if public_ip.version != 4 or public_ip.is_loopback or public_ip.is_private:
        raise ValueError("runner identity public_ip is invalid")
    issued_at = _parse_timestamp(document["issued_at"], "issued_at")
    expires_at = _parse_timestamp(document["expires_at"], "expires_at")
    current = (now or datetime.now(timezone.utc)).astimezone(timezone.utc)
    if issued_at > current or expires_at <= current or expires_at <= issued_at:
        raise ValueError("runner identity is expired or not yet valid")
    return RunnerIdentity(
        machine_id_sha256=document["machine_id_sha256"],
        expected_public_ip=str(public_ip),
        instance_id=document["instance_id"],
    )


def verify_runner_identity(
    identity: RunnerIdentity,
    *,
    machine_id_sha256: str,
    expected_public_ip: str,
) -> None:
    if SHA256_PATTERN.fullmatch(machine_id_sha256) is None:
        raise ValueError("runner machine fingerprint is invalid")
    if identity.machine_id_sha256 != machine_id_sha256:
        raise ValueError("runner machine fingerprint does not match its identity record")
    if identity.expected_public_ip != expected_public_ip:
        raise ValueError("runner identity public IP does not match the isolated runner")


def verify_local_docker_context(*, docker_host: str | None, context_endpoint: str) -> None:
    if docker_host not in (None, "") and not docker_host.startswith("unix://"):
        raise ValueError("Tencent Cloud rehearsal requires a local Unix Docker context")
    if not isinstance(context_endpoint, str) or not context_endpoint.startswith("unix://"):
        raise ValueError("Tencent Cloud rehearsal requires a local Unix Docker context")


def _resource_name(value: object, release_id: str) -> None:
    if not isinstance(value, str) or release_id not in value or not value.endswith("-rehearsal"):
        raise ValueError("rehearsal resource names must bind the release ID")


def _port_is_loopback(value: object) -> bool:
    if isinstance(value, str):
        return value.startswith("127.0.0.1:") or value.startswith("[::1]:")
    if isinstance(value, dict):
        return value.get("host_ip") in {"127.0.0.1", "::1"}
    return False


def _service_networks(service: dict[str, object]) -> frozenset[str]:
    networks = service.get("networks")
    if isinstance(networks, dict):
        return frozenset(str(name) for name in networks)
    if isinstance(networks, list):
        return frozenset(str(name) for name in networks)
    return frozenset()


def _contains_production_target(value: object) -> bool:
    return any(target in text.lower() for text in _walk_strings(value) for target in PRODUCTION_TARGETS)


def _walk_strings(value: object) -> Iterable[str]:
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for nested in value.values():
            yield from _walk_strings(nested)
    elif isinstance(value, list):
        for nested in value:
            yield from _walk_strings(nested)


def _validate_mount(mount: object, *, allowed_named_volumes: frozenset[str]) -> None:
    if isinstance(mount, str):
        parts = mount.split(":")
        if any("docker.sock" in part for part in parts):
            raise ValueError("Docker socket mounts are prohibited")
        if len(parts) < 2:
            raise ValueError("rehearsal mounts must be explicit")
        source, target = parts[:2]
        read_only = parts[-1] == "ro"
        if source.startswith("/"):
            expected_target = SECRET_MOUNTS.get(Path(source).name)
            if expected_target != target or Path(source).parent.name != "private" or not read_only:
                raise ValueError("only approved read-only secret file mounts are allowed")
        elif source not in allowed_named_volumes:
            raise ValueError("rehearsal mount uses an unapproved volume")
        return
    if not isinstance(mount, dict):
        raise ValueError("rehearsal mount is invalid")
    source = mount.get("source")
    target = mount.get("target")
    mount_type = mount.get("type", "volume")
    if not isinstance(source, str) or not isinstance(target, str):
        raise ValueError("rehearsal mount is invalid")
    if "docker.sock" in source or "docker.sock" in target:
        raise ValueError("Docker socket mounts are prohibited")
    if mount_type == "bind":
        expected_target = SECRET_MOUNTS.get(Path(source).name)
        if (
            expected_target != target
            or Path(source).parent.name != "private"
            or mount.get("read_only") is not True
        ):
            raise ValueError("only approved read-only secret file mounts are allowed")
        return
    if mount_type != "volume" or source not in allowed_named_volumes:
        raise ValueError("rehearsal mount uses an unapproved volume")


def validate_static_compose(
    config: dict[str, object],
    *,
    release_id: str,
    allowed_images: set[str],
) -> None:
    _resource_name(config.get("name"), release_id)
    if _contains_production_target(config):
        raise ValueError("Compose must not reference a production endpoint")
    services = config.get("services")
    if not isinstance(services, dict) or not services:
        raise ValueError("Compose must define services")
    networks = config.get("networks")
    if not isinstance(networks, dict) or frozenset(networks) != ALLOWED_NETWORKS:
        raise ValueError("Compose must use the exact approved network set")
    for network in networks.values():
        if not isinstance(network, dict) or network.get("external") or network.get("internal") is not True:
            raise ValueError("Compose networks must be internal and non-external")
        _resource_name(network.get("name"), release_id)
    named_volumes: set[str] = set()
    volumes = config.get("volumes", {})
    if not isinstance(volumes, dict):
        raise ValueError("Compose volumes are invalid")
    for key, volume in volumes.items():
        named_volumes.add(str(key))
        if isinstance(volume, dict):
            if volume.get("external"):
                raise ValueError("external volumes are prohibited")
            name = volume.get("name")
            if name is not None:
                _resource_name(name, release_id)
                named_volumes.add(name)
    for resource_type in ("configs", "secrets"):
        if config.get(resource_type):
            raise ValueError(f"Compose {resource_type} are not permitted in isolated rehearsal")
    allowed_volumes = frozenset(named_volumes)
    for service_name, service in services.items():
        if not isinstance(service_name, str) or not isinstance(service, dict):
            raise ValueError("Compose service is invalid")
        _resource_name(service.get("container_name"), release_id)
        image = service.get("image")
        if not isinstance(image, str) or IMAGE_PATTERN.fullmatch(image) is None or image not in allowed_images:
            raise ValueError("Compose service image is not approved")
        if service.get("network_mode") or service.get("privileged"):
            raise ValueError("host network mode and privileged containers are prohibited")
        if service.get("extra_hosts") or service.get("external_links") or service.get("devices"):
            raise ValueError("extra host links or devices are prohibited")
        attached = _service_networks(service)
        if not attached or not attached.issubset(ALLOWED_NETWORKS):
            raise ValueError("every service must attach only to an approved network")
        ports = service.get("ports", [])
        if ports and (service_name != "sub2api-staging" or len(ports) != 1 or not _port_is_loopback(ports[0])):
            raise ValueError("only the control plane may publish one loopback port")
        for mount in service.get("volumes", []) or []:
            _validate_mount(mount, allowed_named_volumes=allowed_volumes)


def _runtime_networks(document: dict[str, object]) -> frozenset[str]:
    settings = document.get("NetworkSettings")
    if not isinstance(settings, dict):
        return frozenset()
    networks = settings.get("Networks")
    if not isinstance(networks, dict):
        return frozenset()
    return frozenset(str(name) for name in networks)


def _runtime_mount_is_socket(mount: object) -> bool:
    if not isinstance(mount, dict):
        return True
    return "docker.sock" in str(mount.get("Source", "")) or "docker.sock" in str(mount.get("Destination", ""))


def _repository_digest_matches(image: str, repo_digests: object) -> bool:
    configured = IMAGE_PATTERN.fullmatch(image)
    if configured is None or not isinstance(repo_digests, list):
        return False
    configured_digest = configured.group(0).rsplit("@", 1)[1]
    return any(
        isinstance(value, str)
        and (match := IMAGE_PATTERN.fullmatch(value)) is not None
        and match.group(0).rsplit("@", 1)[1] == configured_digest
        for value in repo_digests
    )


def validate_runtime_topology(
    inspect_documents: list[dict[str, object]],
    *,
    release_id: str,
    allowed_images: set[str],
) -> None:
    if not inspect_documents:
        raise ValueError("runtime topology must include release containers")
    allowed_network_names = frozenset(
        {f"{release_id}-control-rehearsal", f"{release_id}-internal-rehearsal"}
    )
    for document in inspect_documents:
        config = document.get("Config")
        host_config = document.get("HostConfig")
        if not isinstance(config, dict) or not isinstance(host_config, dict):
            raise ValueError("runtime inspect document is invalid")
        labels = config.get("Labels")
        if not isinstance(labels, dict) or labels.get("com.docker.compose.project") != release_id:
            raise ValueError("runtime container is not bound to this release")
        service = labels.get("com.docker.compose.service")
        if not isinstance(service, str):
            raise ValueError("runtime container service label is invalid")
        _resource_name(document.get("Name", "").lstrip("/") if isinstance(document.get("Name"), str) else None, release_id)
        image = config.get("Image")
        if not isinstance(image, str) or image not in allowed_images:
            raise ValueError("runtime container image is not approved")
        repo_digests = document.get("RepoDigests")
        if not _repository_digest_matches(image, repo_digests):
            raise ValueError("runtime container repository digest is not bound to its image")
        if host_config.get("Privileged") is True:
            raise ValueError("privileged containers are prohibited")
        if host_config.get("NetworkMode") not in allowed_network_names:
            raise ValueError("runtime container must use an approved network")
        attached = _runtime_networks(document)
        if not attached or not attached.issubset(allowed_network_names):
            raise ValueError("runtime container must attach only to an approved network")
        if any(_runtime_mount_is_socket(mount) for mount in document.get("Mounts", []) or []):
            raise ValueError("Docker socket mounts are prohibited")
        bindings = host_config.get("PortBindings") or {}
        if bindings:
            values = bindings.get("8080/tcp") if isinstance(bindings, dict) else None
            if service != "sub2api-staging" or not isinstance(values, list) or len(values) != 1:
                raise ValueError("only the control plane may publish one loopback port")
            host_ip = values[0].get("HostIp") if isinstance(values[0], dict) else None
            if host_ip not in {"127.0.0.1", "::1"}:
                raise ValueError("runtime ingress must be loopback only")


def collect_runtime_topology(*, docker_bin: str, release_id: str) -> list[dict[str, object]]:
    try:
        listed = subprocess.run(
            [docker_bin, "ps", "-aq", "--filter", f"label=com.docker.compose.project={release_id}"],
            text=True,
            capture_output=True,
            check=False,
            timeout=30,
        )
    except (OSError, subprocess.TimeoutExpired) as error:
        raise RuntimeError("runtime topology inspection could not start") from error
    if listed.returncode != 0:
        raise RuntimeError("runtime topology inspection failed")
    container_ids = [value for value in listed.stdout.splitlines() if value]
    if not container_ids:
        raise ValueError("runtime topology has no release containers")
    try:
        inspected = subprocess.run(
            [docker_bin, "inspect", *container_ids],
            text=True,
            capture_output=True,
            check=False,
            timeout=30,
        )
    except (OSError, subprocess.TimeoutExpired) as error:
        raise RuntimeError("runtime topology inspection could not complete") from error
    if inspected.returncode != 0:
        raise RuntimeError("runtime topology inspection failed")
    try:
        documents = json.loads(inspected.stdout)
    except json.JSONDecodeError as error:
        raise ValueError("runtime topology inspect output is invalid") from error
    if not isinstance(documents, list) or not all(isinstance(document, dict) for document in documents):
        raise ValueError("runtime topology inspect output is invalid")
    for document in documents:
        config = document.get("Config")
        image = config.get("Image") if isinstance(config, dict) else None
        if not isinstance(image, str):
            raise ValueError("runtime topology container image is invalid")
        try:
            image_inspect = subprocess.run(
                [docker_bin, "image", "inspect", image, "--format", "{{json .RepoDigests}}"],
                text=True,
                capture_output=True,
                check=False,
                timeout=30,
            )
        except (OSError, subprocess.TimeoutExpired) as error:
            raise RuntimeError("runtime image digest inspection could not complete") from error
        if image_inspect.returncode != 0:
            raise RuntimeError("runtime image digest inspection failed")
        try:
            repo_digests = json.loads(image_inspect.stdout)
        except json.JSONDecodeError as error:
            raise ValueError("runtime image digest inspection is invalid") from error
        document["RepoDigests"] = repo_digests
    return documents


def write_runtime_report(path: Path, *, release_id: str, documents: list[dict[str, object]]) -> None:
    summary = {
        "schema_version": "radar-rehearsal-runtime-isolation-v1",
        "release_id": release_id,
        "container_count": len(documents),
        "containers": sorted(
            (
                {
                    "name": str(document.get("Name", "")),
                    "service": str(
                        document.get("Config", {})
                        .get("Labels", {})
                        .get("com.docker.compose.service", "")
                    )
                    if isinstance(document.get("Config"), dict)
                    else "",
                    "image": str(document.get("Config", {}).get("Image", ""))
                    if isinstance(document.get("Config"), dict)
                    else "",
                    "repo_digests": sorted(
                        str(value)
                        for value in document.get("RepoDigests", [])
                        if isinstance(value, str)
                    ),
                }
                for document in documents
            ),
            key=lambda value: value["name"],
        ),
    }
    canonical = json.dumps(summary, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    summary["topology_sha256"] = hashlib.sha256(canonical.encode("utf-8")).hexdigest()
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            descriptor = -1
            json.dump(summary, stream, ensure_ascii=False, indent=2, sort_keys=True)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
    finally:
        if descriptor >= 0:
            os.close(descriptor)
    path.chmod(0o600)


def main() -> int:
    parser = argparse.ArgumentParser(description="Validate isolated Radar rehearsal runtime topology")
    subcommands = parser.add_subparsers(dest="command", required=True)
    runtime = subcommands.add_parser("runtime")
    runtime.add_argument("--docker-bin", default="docker")
    runtime.add_argument("--run-id", required=True)
    runtime.add_argument("--image", action="append", required=True)
    runtime.add_argument("--output", type=Path)
    arguments = parser.parse_args()
    try:
        if arguments.command == "runtime":
            documents = collect_runtime_topology(docker_bin=arguments.docker_bin, release_id=arguments.run_id)
            validate_runtime_topology(documents, release_id=arguments.run_id, allowed_images=set(arguments.image))
            if arguments.output is not None:
                write_runtime_report(arguments.output, release_id=arguments.run_id, documents=documents)
    except (RuntimeError, ValueError) as error:
        print(f"ERROR: {error}", file=sys.stderr)
        return 1
    print("runtime isolation topology verified")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
