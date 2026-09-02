#!/usr/bin/env python3
"""Create immutable local rehearsal inputs without starting containers."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import platform
import re
import secrets
import shlex
import shutil
import stat
import subprocess
import sys
import tempfile
import uuid
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Any, Callable, Iterable
from urllib.parse import urlparse

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from deploy.radar.rehearsal_isolation_gate import (
    load_runner_identity,
    validate_static_compose,
    verify_local_docker_context,
    verify_runner_identity,
)
from deploy.radar.source_tree_identity import (
    HASH_CHUNK_BYTES,
    SOURCE_EXCLUSIONS,
    source_tree_sha256,
)


PRODUCTION_TARGETS = frozenset({"192.255.134.229", "sub2api.weihub.cloud"})
DEPENDENCY_ENVIRONMENT_KEYS = (
    "RADAR_POSTGRES_IMAGE",
    "RADAR_REDIS_IMAGE",
    "RADAR_MINIO_IMAGE",
    "RADAR_MINIO_MC_IMAGE",
    "RADAR_CLAMAV_IMAGE",
    "RADAR_NODE_BASE_IMAGE",
    "RADAR_GOLANG_BASE_IMAGE",
    "RADAR_ALPINE_BASE_IMAGE",
    "RADAR_WORKER_BASE_IMAGE",
)
IMAGE_PATTERN = re.compile(r"^.+@(?P<digest>sha256:[0-9a-f]{64})$")
RUN_ID_PATTERN = re.compile(r"^[a-z0-9][a-z0-9_-]{2,62}-rehearsal$")
URL_PATTERN = re.compile(r"https?://[^\s'\"<>]+", re.IGNORECASE)
BARE_TARGET_PATTERN = re.compile(
    r"((?:\d{1,3}\.){3}\d{1,3}|(?:[a-z0-9-]+\.)+[a-z]{2,})(?::\d+)?(?:/[^\s]*)?",
    re.IGNORECASE,
)
ALLOWED_NETWORKS = frozenset({"control_plane", "radar_worker_internal"})
LOOPBACK_TARGET_HOSTS = frozenset({"127.0.0.1", "localhost", "::1"})
BIND_ONLY_WILDCARD_FIELDS = frozenset({"SERVER_HOST", "RADAR_SYNTHETIC_HOST", "RADAR_METRICS_HOST"})
NETWORK_CLIENTS = frozenset({"curl", "wget", "nc", "netcat", "ping", "ping6", "telnet", "host"})
CLIENT_OPTIONS_WITH_VALUE = {
    "curl": frozenset(
        {
            "-A",
            "-H",
            "-X",
            "-d",
            "-e",
            "-F",
            "-o",
            "-u",
            "--cacert",
            "--cert",
            "--connect-timeout",
            "--data",
            "--data-raw",
            "--form",
            "--header",
            "--key",
            "--max-time",
            "--output",
            "--proxy",
            "--referer",
            "--request",
            "--resolve",
            "--user",
            "--user-agent",
        }
    ),
    "wget": frozenset(
        {
            "-O",
            "-T",
            "--body-data",
            "--ca-certificate",
            "--header",
            "--method",
            "--output-document",
            "--post-data",
            "--timeout",
            "--user-agent",
        }
    ),
    "nc": frozenset({"-i", "-p", "-s", "-w"}),
    "netcat": frozenset({"-i", "-p", "-s", "-w"}),
    "ping": frozenset({"-c", "-i", "-s", "-t", "-W"}),
    "ping6": frozenset({"-c", "-i", "-s", "-t", "-W"}),
}
DOCKER_TIMEOUT_SECONDS = 30.0


@dataclass(frozen=True)
class PreflightInputs:
    candidate_root: Path
    run_id: str
    browser_origin: str
    backup_path: Path
    backup_sha256: str
    control_plane_image: str
    worker_image: str
    rollback_control_plane_image: str
    rollback_worker_image: str
    dependency_images: tuple[str, ...]
    evidence_dir: Path
    expected_runner_ip: str
    minimum_free_bytes: int = 10 * 1024**3
    runner_identity_path: Path | None = None
    runner_machine_id_path: Path = Path("/etc/machine-id")


@dataclass(frozen=True)
class ImmutableBindings:
    run_id: str
    source_sha256: str
    backup_sha256: str
    control_plane_digest: str
    worker_digest: str
    rollback_control_plane_digest: str
    rollback_worker_digest: str
    dependency_digests: tuple[str, ...]
    environment_fingerprint: str
    policy_version: str = "quality-v1"
    fixture_version: str = "local-quality-fixture-v1"


def _image_digest(image: str) -> str:
    matched = IMAGE_PATTERN.fullmatch(image)
    if matched is None:
        raise ValueError("image must use an immutable sha256 identity")
    return matched.group("digest")


def _require_mode_0600(path: Path, label: str) -> None:
    if not path.is_file() or stat.S_IMODE(path.stat().st_mode) != 0o600:
        raise ValueError(f"{label} must have mode 0600")


def validate_endpoint(value: str) -> None:
    parsed = urlparse(value if "://" in value else "//" + value)
    host = (parsed.hostname or "").lower().rstrip(".")
    if host in PRODUCTION_TARGETS:
        raise ValueError("production target is prohibited")


def _validate_browser_origin(origin: str) -> None:
    validate_endpoint(origin)
    parsed = urlparse(origin)
    if (
        parsed.scheme != "http"
        or parsed.hostname not in {"127.0.0.1", "localhost", "::1"}
        or parsed.username
        or parsed.password
        or parsed.path not in {"", "/"}
        or parsed.params
        or parsed.query
        or parsed.fragment
    ):
        raise ValueError("browser origin must be a plain HTTP loopback origin")
    try:
        if parsed.port is None:
            raise ValueError("browser origin must include an explicit port")
    except ValueError as error:
        if str(error).startswith("browser origin"):
            raise
        raise ValueError("browser origin has an invalid port") from error


def _file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        while chunk := stream.read(HASH_CHUNK_BYTES):
            digest.update(chunk)
    return digest.hexdigest()


def validate_inputs(
    inputs: PreflightInputs,
    *,
    disk_free: Callable[[Path], int] | None = None,
) -> None:
    if not RUN_ID_PATTERN.fullmatch(inputs.run_id):
        raise ValueError("run_id must be a lowercase unique name ending in -rehearsal")
    _validate_browser_origin(inputs.browser_origin)
    if not inputs.candidate_root.is_dir():
        raise ValueError("candidate_root must be an existing directory")
    _require_mode_0600(inputs.backup_path, "backup")
    if _file_sha256(inputs.backup_path) != inputs.backup_sha256:
        raise ValueError("backup checksum does not match")
    for image in (
        inputs.control_plane_image,
        inputs.worker_image,
        inputs.rollback_control_plane_image,
        inputs.rollback_worker_image,
        *inputs.dependency_images,
    ):
        _image_digest(image)
    free = (disk_free or (lambda path: shutil.disk_usage(path).free))(inputs.evidence_dir)
    if free < inputs.minimum_free_bytes:
        raise ValueError("evidence filesystem requires at least 10 GiB free")
    try:
        inputs.evidence_dir.resolve().relative_to(inputs.candidate_root.resolve())
    except ValueError:
        pass
    else:
        raise ValueError("evidence_dir must be outside candidate_root to preserve immutable source")
    for name in ("rehearsal.env", "bindings.json", "compose-config.json"):
        if (inputs.evidence_dir / name).exists():
            raise ValueError("preflight output already exists")


def inspect_local_images(images: Iterable[str], inspect: Callable[[str], str | None]) -> tuple[str, ...]:
    identities: list[str] = []
    for image in images:
        expected = _image_digest(image)
        local = inspect(image)
        if local is None or not re.fullmatch(r"sha256:[0-9a-f]{64}", local.strip()):
            raise ValueError("immutable image is not present locally")
        identities.append(expected)
    return tuple(identities)


def environment_fingerprint(docker_server: str) -> str:
    payload = {
        "docker_server": docker_server,
        "platform_machine": platform.machine(),
        "platform_release": platform.release(),
        "platform_system": platform.system(),
        "python": list(os.sys.version_info[:2]),
    }
    return hashlib.sha256(json.dumps(payload, sort_keys=True, separators=(",", ":")).encode("utf-8")).hexdigest()


def build_bindings(inputs: PreflightInputs, docker_server: str) -> ImmutableBindings:
    return ImmutableBindings(
        run_id=inputs.run_id,
        source_sha256=source_tree_sha256(inputs.candidate_root),
        backup_sha256=inputs.backup_sha256,
        control_plane_digest=_image_digest(inputs.control_plane_image),
        worker_digest=_image_digest(inputs.worker_image),
        rollback_control_plane_digest=_image_digest(inputs.rollback_control_plane_image),
        rollback_worker_digest=_image_digest(inputs.rollback_worker_image),
        dependency_digests=tuple(sorted(_image_digest(image) for image in inputs.dependency_images)),
        environment_fingerprint=environment_fingerprint(docker_server),
    )


def _atomic_write(path: Path, content: str, mode: int = 0o600) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(prefix=".tmp-", dir=path.parent, text=True)
    temporary = Path(temporary_name)
    try:
        os.fchmod(descriptor, mode)
        with os.fdopen(descriptor, "w", encoding="utf-8", newline="\n") as handle:
            handle.write(content)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        os.chmod(path, mode)
    finally:
        if temporary.exists():
            temporary.unlink()


def _writer_id(run_id: str, kind: str) -> str:
    return str(uuid.uuid5(uuid.NAMESPACE_URL, f"radar-local-rehearsal/{run_id}/{kind}"))


def write_rehearsal_environment(
    path: Path,
    bindings: ImmutableBindings,
    browser_origin: str,
    image_references: dict[str, str] | None = None,
    *,
    private_dir: Path | None = None,
) -> dict[str, str]:
    _validate_browser_origin(browser_origin)
    browser_port = urlparse(browser_origin).port
    if browser_port is None:
        raise ValueError("browser origin must include an explicit port")
    images = image_references or {
        "RADAR_CONTROL_PLANE_IMAGE": f"local/control@{bindings.control_plane_digest}",
        "RADAR_WORKER_IMAGE": f"local/worker@{bindings.worker_digest}",
    }
    secrets_dir = private_dir or path.parent / "private"
    secrets_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(secrets_dir, 0o700)
    postgres_password = secrets.token_urlsafe(32)
    postgres_password_file = secrets_dir / "postgres-password"
    database_password_file = secrets_dir / "database-password"
    database_pgpass_file = secrets_dir / "database.pgpass"
    _atomic_write(postgres_password_file, postgres_password + "\n")
    _atomic_write(database_password_file, postgres_password + "\n")
    _atomic_write(
        database_pgpass_file,
        f"*:*:*:*:{postgres_password}\n",
    )
    release_version = os.environ.get("RADAR_RELEASE_VERSION", "0.1.178")
    if not release_version.endswith("-local-rehearsal"):
        release_version += "-local-rehearsal"
    values = {
        "RADAR_COMPOSE_PROJECT_NAME": bindings.run_id,
        "RADAR_COMPOSE_RESOURCE_PREFIX": bindings.run_id,
        "RADAR_IMAGE_PULL_POLICY": "never",
        "RADAR_LOCAL_BROWSER_ORIGIN": browser_origin.rstrip("/"),
        "RADAR_CONTROL_PLANE_PORT": str(browser_port),
        "RADAR_POSTGRES_PASSWORD_FILE": str(postgres_password_file),
        "RADAR_DATABASE_PASSWORD_FILE": str(database_password_file),
        "RADAR_DATABASE_PGPASS_FILE": str(database_pgpass_file),
        "RADAR_ADMIN_PASSWORD": secrets.token_urlsafe(32),
        "RADAR_JWT_SECRET": secrets.token_urlsafe(32),
        "RADAR_CONTEXT_SIGNING_KEY": secrets.token_urlsafe(32),
        "RADAR_EVIDENCE_HASH_KEY": secrets.token_urlsafe(32),
        "RADAR_MINIO_ROOT_USER": "radar-rehearsal",
        "RADAR_MINIO_ROOT_PASSWORD": "local-" + secrets.token_urlsafe(32),
        "RADAR_SYNTHETIC_UPSTREAM_API_KEY": secrets.token_urlsafe(32),
        "RADAR_RUNNER_WORKER_TOKEN": secrets.token_urlsafe(32),
        "RADAR_GRADER_WORKER_TOKEN": secrets.token_urlsafe(32),
        "RADAR_STATISTICS_WORKER_TOKEN": secrets.token_urlsafe(32),
        "RADAR_RELEASE_VERSION": release_version,
        "RADAR_RELEASE_COMMIT": bindings.source_sha256,
        "RADAR_RELEASE_DATE": "1970-01-01T00:00:00Z",
        "RADAR_API_WRITER_INSTANCE_ID": _writer_id(bindings.run_id, "api"),
        "RADAR_RUNNER_WRITER_INSTANCE_ID": _writer_id(bindings.run_id, "runner"),
        "RADAR_GRADER_WRITER_INSTANCE_ID": _writer_id(bindings.run_id, "grader"),
        "RADAR_STATISTICS_WRITER_INSTANCE_ID": _writer_id(bindings.run_id, "statistics"),
        **images,
    }
    _atomic_write(path, "".join(f"{key}={value}\n" for key, value in sorted(values.items())))
    return values


def compose_environment(path: Path) -> dict[str, str]:
    _require_mode_0600(path, "rehearsal environment")
    values: dict[str, str] = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        key, value = line.split("=", 1)
        values[key] = value
    return values


def _walk_strings(value: object) -> Iterable[str]:
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for nested in value.values():
            yield from _walk_strings(nested)
    elif isinstance(value, list):
        for nested in value:
            yield from _walk_strings(nested)


def _port_is_loopback(port: object) -> bool:
    if isinstance(port, str):
        return port.startswith("127.0.0.1:") or port.startswith("[::1]:")
    if isinstance(port, dict):
        return port.get("host_ip") in {"127.0.0.1", "::1"}
    return False


def _assert_rehearsal_name(name: object, run_id: str) -> None:
    if not isinstance(name, str) or run_id not in name or not name.endswith("-rehearsal"):
        raise ValueError("every named resource must include the run_id and end in -rehearsal")


def _internal_target_host(value: str) -> str:
    parsed = urlparse(value if "://" in value else "//" + value)
    return (parsed.hostname or "").lower().rstrip(".")


def _command_target_candidates(tokens: list[str]) -> Iterable[str]:
    active_client: str | None = None
    skip_option_value = False
    for token in tokens:
        if token in {";", "&&", "||", "|"}:
            active_client = None
            skip_option_value = False
            continue
        candidate = token.strip("'\"();,")
        command = Path(candidate).name
        if command in NETWORK_CLIENTS:
            active_client = command
            skip_option_value = False
            continue
        if active_client is None:
            continue
        if skip_option_value:
            skip_option_value = False
            continue
        if candidate.startswith("-"):
            option = candidate.split("=", 1)[0]
            if "=" not in candidate and option in CLIENT_OPTIONS_WITH_VALUE.get(active_client, frozenset()):
                skip_option_value = True
            continue
        yield candidate


def _validate_internal_target(value: str, service_names: frozenset[str], *, scan_bare: bool = False) -> None:
    allowed_hosts = service_names | LOOPBACK_TARGET_HOSTS
    urls = URL_PATTERN.findall(value)
    for url in urls:
        if _internal_target_host(url) not in allowed_hosts:
            raise ValueError("Compose target must use loopback or a known internal target")
    if scan_bare:
        tokens = shlex.split(value)
        for candidate in _command_target_candidates(tokens):
            if not BARE_TARGET_PATTERN.fullmatch(candidate):
                continue
            host = _internal_target_host(candidate)
            if host not in allowed_hosts:
                raise ValueError("Compose target must use loopback or a known internal target")


def _validate_service_targets(service: dict[str, object], service_names: frozenset[str]) -> None:
    environment = service.get("environment", {})
    environment_items: list[tuple[str, str]] = []
    if isinstance(environment, dict):
        environment_items = [(str(key), str(value)) for key, value in environment.items() if value is not None]
    elif isinstance(environment, list):
        for item in environment:
            if isinstance(item, str) and "=" in item:
                environment_items.append(tuple(item.split("=", 1)))
    for key, value in environment_items:
        _validate_internal_target(value, service_names)
        upper_key = key.upper()
        is_secret = any(marker in upper_key for marker in ("KEY", "TOKEN", "PASSWORD", "SECRET"))
        is_target = any(marker in upper_key for marker in ("URL", "HOST", "ENDPOINT", "ADDRESS", "DOMAINS"))
        if is_target and not is_secret and not URL_PATTERN.search(value):
            for token in value.split(","):
                target = token.strip()
                host = _internal_target_host(target)
                if host == "0.0.0.0" and upper_key in BIND_ONLY_WILDCARD_FIELDS and target == "0.0.0.0":
                    continue
                if host not in service_names | LOOPBACK_TARGET_HOSTS:
                    raise ValueError("Compose target must use loopback or a known internal target")
    for field in ("command", "entrypoint", "healthcheck"):
        command = " ".join(_walk_strings(service.get(field)))
        _validate_internal_target(command, service_names, scan_bare=True)


def _service_networks(service: dict[str, object]) -> frozenset[str]:
    networks = service.get("networks")
    if isinstance(networks, dict):
        return frozenset(str(name) for name in networks)
    if isinstance(networks, list):
        return frozenset(str(name) for name in networks)
    return frozenset()


def validate_compose_config(
    config: dict[str, object],
    run_id: str,
    approved_images: Iterable[str] | None = None,
) -> tuple[str, ...]:
    _assert_rehearsal_name(config.get("name"), run_id)
    services = config.get("services")
    if not isinstance(services, dict) or not services:
        raise ValueError("compose config must define services")
    service_names = frozenset(str(name) for name in services)
    rendered_images: set[str] = set()
    approved = frozenset(approved_images) if approved_images is not None else None
    for service_name, service in services.items():
        if not isinstance(service_name, str) or not isinstance(service, dict):
            raise ValueError("compose service is invalid")
        _assert_rehearsal_name(service.get("container_name"), run_id)
        if service.get("network_mode") == "host":
            raise ValueError("host network mode is prohibited")
        for field in ("external_links", "extra_hosts"):
            if service.get(field):
                raise ValueError(f"{field} is prohibited")
        attached_networks = _service_networks(service)
        if not attached_networks or not attached_networks.issubset(ALLOWED_NETWORKS):
            raise ValueError("every service must attach only to an approved network")
        image = service.get("image")
        if not isinstance(image, str):
            raise ValueError("every service requires an immutable image")
        _image_digest(image)
        if approved is not None and image not in approved:
            raise ValueError("rendered image is not an approved immutable input")
        rendered_images.add(image)
        ports = service.get("ports", [])
        if ports:
            if service_name != "sub2api-staging" or len(ports) != 1 or not _port_is_loopback(ports[0]):
                raise ValueError("only sub2api-staging may publish one loopback port")
        _validate_service_targets(service, service_names)
    networks = config.get("networks")
    if not isinstance(networks, dict):
        raise ValueError("compose config must define networks")
    if frozenset(networks) != ALLOWED_NETWORKS:
        raise ValueError("Compose must define the exact allowed network set")
    for network in networks.values():
        if not isinstance(network, dict):
            raise ValueError("compose network is invalid")
        if network.get("external"):
            raise ValueError("external network is prohibited")
        if network.get("internal") is not True:
            raise ValueError("every Compose network must be internal")
        _assert_rehearsal_name(network.get("name"), run_id)
    for resource_type in ("volumes", "configs", "secrets"):
        resources = config.get(resource_type, {})
        if not isinstance(resources, dict):
            raise ValueError(f"compose {resource_type} are invalid")
        for resource in resources.values():
            if isinstance(resource, dict) and resource.get("external"):
                raise ValueError("external named resource is prohibited")
            if isinstance(resource, dict) and "name" in resource:
                _assert_rehearsal_name(resource["name"], run_id)
    return tuple(sorted(rendered_images))


def _docker_output(command: list[str], cwd: Path | None = None, env: dict[str, str] | None = None) -> str:
    if env is None:
        env = os.environ.copy()
    else:
        env = dict(env)
    for key in ("PGPASSWORD", "POSTGRES_PASSWORD", "DATABASE_PASSWORD", "RADAR_POSTGRES_PASSWORD"):
        env.pop(key, None)
    try:
        completed = subprocess.run(
            command,
            cwd=cwd,
            env=env,
            text=True,
            capture_output=True,
            check=False,
            timeout=DOCKER_TIMEOUT_SECONDS,
        )
    except subprocess.TimeoutExpired as error:
        raise RuntimeError(f"Docker command timed out after {DOCKER_TIMEOUT_SECONDS:g} seconds") from error
    except OSError as error:
        raise RuntimeError("Docker is unavailable") from error
    if completed.returncode != 0:
        raise RuntimeError(f"Docker command failed: {completed.stderr.strip()}")
    return completed.stdout.strip()


def _docker_version() -> str:
    return _docker_output(["docker", "version", "--format", "{{json .Server}}"])


def _docker_context_endpoint() -> str:
    return _docker_output(
        ["docker", "context", "inspect", "--format", '{{ (index .Endpoints "docker").Host }}']
    )


def _runner_machine_id_sha256(path: Path) -> str:
    if path.is_symlink() or not path.is_file():
        raise ValueError("runner machine ID file is unavailable")
    value = path.read_text(encoding="utf-8").strip()
    if not value:
        raise ValueError("runner machine ID is empty")
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def _local_image_id(image: str) -> str | None:
    try:
        return _docker_output(["docker", "image", "inspect", image, "--format", "{{.Id}}"])
    except RuntimeError as error:
        if "timed out" in str(error):
            raise
        return None


def _compose_config(inputs: PreflightInputs, environment_file: Path) -> dict[str, object]:
    environment = os.environ.copy()
    environment["RADAR_LOCAL_ENV_FILE"] = str(environment_file)
    output = _docker_output(
        [
            "docker",
            "compose",
            "--env-file",
            str(environment_file),
            "-f",
            "deploy/docker-compose.radar-staging.yml",
            "-f",
            "deploy/docker-compose.radar-rehearsal.yml",
            "config",
            "--format",
            "json",
        ],
        cwd=inputs.candidate_root,
        env=environment,
    )
    parsed = json.loads(output)
    if not isinstance(parsed, dict):
        raise ValueError("compose config must be a JSON object")
    return parsed


def _required_environment(name: str) -> str:
    value = os.environ.get(name, "")
    if not value:
        raise ValueError(f"{name} is required")
    return value


def _publish_staged_outputs(staging_dir: Path, evidence_dir: Path) -> None:
    published: list[Path] = []
    try:
        private_source = staging_dir / "private"
        private_destination = evidence_dir / "private"
        if private_destination.exists():
            if not private_destination.is_dir() or any(private_destination.iterdir()):
                raise ValueError("preflight private output already exists")
            private_destination.rmdir()
        environment_source = staging_dir / "rehearsal.env"
        environment_source.write_text(
            environment_source.read_text(encoding="utf-8").replace(
                str(private_source), str(private_destination)
            ),
            encoding="utf-8",
        )
        for name in ("rehearsal.env", "bindings.json", "compose-config.json"):
            destination = evidence_dir / name
            if destination.exists():
                raise ValueError("preflight output already exists")
            os.replace(staging_dir / name, destination)
            os.chmod(destination, 0o600)
            published.append(destination)
        os.replace(private_source, private_destination)
        os.chmod(private_destination, 0o700)
        published.append(private_destination)
    except Exception:
        for path in published:
            if path.is_dir():
                for child in path.iterdir():
                    child.unlink(missing_ok=True)
                path.rmdir()
            else:
                path.unlink(missing_ok=True)
        raise


def run_preflight(inputs: PreflightInputs) -> ImmutableBindings:
    validate_inputs(inputs)
    if inputs.runner_identity_path is not None:
        identity = load_runner_identity(inputs.runner_identity_path, run_id=inputs.run_id)
        verify_runner_identity(
            identity,
            machine_id_sha256=_runner_machine_id_sha256(inputs.runner_machine_id_path),
            expected_public_ip=inputs.expected_runner_ip,
        )
        verify_local_docker_context(
            docker_host=os.environ.get("DOCKER_HOST"),
            context_endpoint=_docker_context_endpoint(),
        )
    docker_server = _docker_version()
    all_images = (
        inputs.control_plane_image,
        inputs.worker_image,
        inputs.rollback_control_plane_image,
        inputs.rollback_worker_image,
        *inputs.dependency_images,
    )
    bindings = build_bindings(inputs, docker_server)
    images = {
        "RADAR_CONTROL_PLANE_IMAGE": inputs.control_plane_image,
        "RADAR_WORKER_IMAGE": inputs.worker_image,
        "RADAR_ROLLBACK_CONTROL_PLANE_IMAGE": inputs.rollback_control_plane_image,
        "RADAR_ROLLBACK_WORKER_IMAGE": inputs.rollback_worker_image,
    }
    images.update(dict(zip(DEPENDENCY_ENVIRONMENT_KEYS, inputs.dependency_images, strict=True)))
    with tempfile.TemporaryDirectory(prefix=".radar-preflight-", dir=inputs.evidence_dir) as directory:
        staging_dir = Path(directory)
        os.chmod(staging_dir, 0o700)
        environment_file = staging_dir / "rehearsal.env"
        write_rehearsal_environment(
            environment_file,
            bindings,
            inputs.browser_origin,
            images,
            private_dir=staging_dir / "private",
        )
        _atomic_write(
            staging_dir / "bindings.json",
            json.dumps({**asdict(bindings), "source_exclusions": SOURCE_EXCLUSIONS}, sort_keys=True, indent=2) + "\n",
        )
        config = _compose_config(inputs, environment_file)
        rendered_images = validate_compose_config(config, inputs.run_id, all_images)
        validate_static_compose(config, release_id=inputs.run_id, allowed_images=set(all_images))
        inspect_local_images(rendered_images, _local_image_id)
        remaining_images = tuple(sorted(set(all_images) - set(rendered_images)))
        inspect_local_images(remaining_images, _local_image_id)
        if source_tree_sha256(inputs.candidate_root) != bindings.source_sha256:
            raise RuntimeError("candidate source tree changed during preflight")
        _atomic_write(staging_dir / "compose-config.json", json.dumps(config, sort_keys=True, indent=2) + "\n")
        _publish_staged_outputs(staging_dir, inputs.evidence_dir)
    return bindings


def parse_inputs(arguments: argparse.Namespace) -> PreflightInputs:
    dependency_images = tuple(_required_environment(name) for name in DEPENDENCY_ENVIRONMENT_KEYS)
    return PreflightInputs(
        candidate_root=Path(arguments.candidate_root).resolve(),
        run_id=_required_environment("RADAR_LOCAL_RUN_ID"),
        browser_origin=_required_environment("RADAR_LOCAL_BROWSER_ORIGIN"),
        backup_path=Path(_required_environment("RADAR_LOCAL_BACKUP_PATH")).resolve(),
        backup_sha256=_required_environment("RADAR_LOCAL_BACKUP_SHA256"),
        control_plane_image=_required_environment("RADAR_CONTROL_PLANE_IMAGE"),
        worker_image=_required_environment("RADAR_WORKER_IMAGE"),
        rollback_control_plane_image=_required_environment("RADAR_ROLLBACK_CONTROL_PLANE_IMAGE"),
        rollback_worker_image=_required_environment("RADAR_ROLLBACK_WORKER_IMAGE"),
        dependency_images=dependency_images,
        evidence_dir=Path(_required_environment("RADAR_LOCAL_EVIDENCE_DIR")).resolve(),
        expected_runner_ip=_required_environment("RADAR_LOCAL_EXPECTED_RUNNER_IP"),
        runner_identity_path=Path(_required_environment("RADAR_LOCAL_RUNNER_IDENTITY")).resolve(),
    )


def main() -> int:
    parser = argparse.ArgumentParser(description="Validate immutable local Radar rehearsal inputs without starting services.")
    parser.add_argument("--candidate-root", default=".", help="candidate source root")
    arguments = parser.parse_args()
    inputs = parse_inputs(arguments)
    inputs.evidence_dir.mkdir(parents=True, exist_ok=True)
    run_preflight(inputs)
    print("local prerelease preflight completed without starting services")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
