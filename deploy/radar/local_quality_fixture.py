#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import os
import socket
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable


MAX_RESPONSE_BYTES = 1024 * 1024
MANIFEST_SCHEMA_VERSION = "radar-local-quality-fixture-v1"
DIMENSIONS = (
    "knowledge_freshness",
    "model_fingerprint",
    "reasoning_stability",
    "structure_compliance",
    "parameter_fidelity",
    "instruction_hierarchy",
    "protocol_schema",
    "stream_completeness",
)
EVENT_CLASS = {
    "knowledge_freshness": "response_shape",
    "model_fingerprint": "fingerprint",
    "reasoning_stability": "response_shape",
    "structure_compliance": "response_shape",
    "parameter_fidelity": "parameter_echo",
    "instruction_hierarchy": "request_shape",
    "protocol_schema": "response_shape",
    "stream_completeness": "stream_integrity",
}
CANDIDATES = (
    {"display_name": "Radar Synthetic Reference", "confidence": 0.90},
    {"display_name": "Radar Synthetic Alternate", "confidence": 0.70},
)
EXPECTED_SCENARIOS = {
    "healthy": {
        "overall_conclusion": "no_significant_anomaly",
        "adulteration_risk": "no_significant_anomaly",
        "degradation_risk": "no_significant_anomaly",
        "source_state": "inferred",
    },
    "watered": {
        "overall_conclusion": "high_risk",
        "adulteration_risk": "high_risk",
        "degradation_risk": "no_significant_anomaly",
        "source_state": "inferred",
    },
    "degraded": {
        "overall_conclusion": "high_risk",
        "adulteration_risk": "no_significant_anomaly",
        "degradation_risk": "high_risk",
        "source_state": "inferred",
    },
    "insufficient": {
        "overall_conclusion": "insufficient_coverage",
        "adulteration_risk": "insufficient_coverage",
        "degradation_risk": "insufficient_coverage",
        "source_state": "insufficient_evidence",
    },
}
ALIASES = {scenario: f"radar-quality-{scenario}" for scenario in EXPECTED_SCENARIOS}
UPSTREAM_MODELS = {
    "healthy": "radar-synthetic-healthy",
    "watered": "radar-synthetic-watered",
    "degraded": "radar-synthetic-degraded",
    "insufficient": "radar-synthetic-healthy",
}
MODEL_MAPPING = {
    "radar-synthetic-baseline": "radar-synthetic-baseline",
    "radar-synthetic-healthy": "radar-synthetic-healthy",
    "radar-synthetic-watered": "radar-synthetic-watered",
    "radar-synthetic-degraded": "radar-synthetic-degraded",
}
RADAR_ROLES = ("platform_admin", "quality_admin", "test_operator", "release_manager")
FORBIDDEN_MANIFEST_TERMS = (
    "password",
    "token",
    "api_key",
    "prompt",
    "completion",
    "account",
    "channel",
    "route_trace_id",
    "probe_spec_hash",
    "observation",
)


class FixtureError(RuntimeError):
    pass


class RejectRedirects(urllib.request.HTTPRedirectHandler):
    def redirect_request(
        self,
        req: urllib.request.Request,
        fp: Any,
        code: int,
        msg: str,
        headers: Any,
        newurl: str,
    ) -> None:
        return None


def _loopback_origin(origin: str) -> str:
    parsed = urllib.parse.urlparse(origin)
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
        raise FixtureError("origin must be a plain HTTP loopback origin")
    try:
        if parsed.port is None:
            raise FixtureError("origin must include an explicit port")
    except ValueError as error:
        raise FixtureError("origin has an invalid port") from error
    return origin.rstrip("/")


class JSONClient:
    def __init__(self, origin: str, timeout_seconds: float, opener: Any | None = None) -> None:
        self.origin = _loopback_origin(origin)
        if timeout_seconds <= 0:
            raise FixtureError("timeout_seconds must be positive")
        self.timeout_seconds = timeout_seconds
        self.opener = opener or urllib.request.build_opener(RejectRedirects())

    def request(
        self,
        method: str,
        path: str,
        *,
        expected_status: int,
        payload: dict[str, object] | None = None,
        bearer_token: str | None = None,
        idempotency_key: str | None = None,
        not_ready_status: int | None = None,
        timeout_seconds: float | None = None,
    ) -> dict[str, object] | None:
        if not path.startswith("/") or path.startswith("//"):
            raise FixtureError("request path must be origin-relative")
        headers = {"Accept": "application/json"}
        body = None
        if payload is not None:
            body = json.dumps(payload, ensure_ascii=True, separators=(",", ":")).encode()
            headers["Content-Type"] = "application/json"
        if bearer_token:
            headers["Authorization"] = f"Bearer {bearer_token}"
        if idempotency_key:
            headers["Idempotency-Key"] = idempotency_key
        request = urllib.request.Request(
            self.origin + path,
            data=body,
            headers=headers,
            method=method,
        )
        request_timeout = self.timeout_seconds
        if timeout_seconds is not None:
            if timeout_seconds <= 0:
                raise FixtureError(f"{method} {path} request deadline expired")
            request_timeout = min(request_timeout, timeout_seconds)
        try:
            with self.opener.open(request, timeout=request_timeout) as response:
                status = response.getcode()
                if status != expected_status:
                    raise FixtureError(f"{method} {path} returned HTTP {status}, expected {expected_status}")
                content_length = response.headers.get("Content-Length")
                if content_length is not None:
                    try:
                        if int(content_length) > MAX_RESPONSE_BYTES:
                            raise FixtureError(f"{method} {path} response exceeds 1 MiB")
                    except ValueError as error:
                        raise FixtureError(f"{method} {path} has invalid Content-Length") from error
                encoded = response.read(MAX_RESPONSE_BYTES + 1)
        except urllib.error.HTTPError as error:
            error.close()
            if error.code == not_ready_status:
                return None
            if 300 <= error.code < 400:
                raise FixtureError(f"{method} {path} redirect rejected") from error
            raise FixtureError(f"{method} {path} returned HTTP {error.code}") from error
        except (urllib.error.URLError, TimeoutError, socket.timeout, OSError) as error:
            raise FixtureError(f"{method} {path} request failed") from error
        if len(encoded) > MAX_RESPONSE_BYTES:
            raise FixtureError(f"{method} {path} response exceeds 1 MiB")
        try:
            document = json.loads(encoded)
        except (json.JSONDecodeError, UnicodeDecodeError) as error:
            raise FixtureError(f"{method} {path} returned invalid JSON") from error
        if not isinstance(document, dict):
            raise FixtureError(f"{method} {path} must return a JSON object")
        return document


def quality_case(
    dimension: str,
    scenario: str,
    sample_count: int,
    candidate: dict[str, object] | None,
) -> dict[str, object]:
    if dimension not in EVENT_CLASS:
        raise FixtureError(f"unknown quality dimension: {dimension}")
    if scenario not in EXPECTED_SCENARIOS:
        raise FixtureError(f"unknown quality scenario: {scenario}")
    probe: dict[str, object] = {
        "schema_version": "quality-v1",
        "quality_dimension": dimension,
        "event_class": EVENT_CLASS[dimension],
        "minimum_samples": 3,
    }
    if candidate is not None:
        probe["source_candidate"] = candidate
    candidate_suffix = "primary"
    if candidate is not None:
        candidate_suffix = "reference" if candidate["confidence"] == 0.90 else "alternate"
    return {
        "case_key": f"{scenario}-{dimension}-{candidate_suffix}",
        "capability_domain": "reasoning",
        "priority": "P1",
        "weight": "1",
        "sample_count": sample_count,
        "prompt_spec": {
            "messages": [
                {
                    "role": "user",
                    "content": f"RADAR_QUALITY_DIMENSION={dimension}\nReturn the reference answer",
                }
            ]
        },
        "expected_spec": "Paris",
        "execution_spec": {"url": "/v1/chat/completions"},
        "grader_id": "exact",
        "grader_version": "v1",
        "confidentiality": "synthetic",
        "estimated_cost": "0.01",
        "quality_dimension": dimension,
        "quality_probe_spec": probe,
    }


def build_scenario_cases(scenario: str) -> list[dict[str, object]]:
    if scenario not in EXPECTED_SCENARIOS:
        raise FixtureError(f"unknown quality scenario: {scenario}")
    sample_count = 2 if scenario == "insufficient" else 3
    cases: list[dict[str, object]] = []
    for dimension in DIMENSIONS:
        candidates: tuple[dict[str, object] | None, ...]
        candidates = CANDIDATES if dimension == "model_fingerprint" else (None,)
        for candidate in candidates:
            cases.append(quality_case(dimension, scenario, sample_count, candidate))
    return cases


@dataclass(frozen=True)
class FixtureConfig:
    origin: str
    setup_administrator_email: str
    setup_administrator_password: str
    fixture_password: str
    synthetic_upstream_key: str
    manifest_path: Path
    run_identifier: str
    timeout_seconds: float = 120.0
    poll_interval_seconds: float = 1.0

    def validate(self) -> None:
        _loopback_origin(self.origin)
        for name, value in (
            ("setup_administrator_email", self.setup_administrator_email),
            ("setup_administrator_password", self.setup_administrator_password),
            ("fixture_password", self.fixture_password),
            ("synthetic_upstream_key", self.synthetic_upstream_key),
            ("run_identifier", self.run_identifier),
        ):
            if not value or value.strip() != value:
                raise FixtureError(f"{name} must be non-empty and trimmed")
        if any(character not in "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-" for character in self.run_identifier):
            raise FixtureError("run_identifier contains unsupported characters")
        if self.timeout_seconds <= 0 or self.poll_interval_seconds <= 0:
            raise FixtureError("timeouts must be positive")


def _data_object(document: dict[str, object], endpoint: str) -> dict[str, object]:
    data = document.get("data")
    if not isinstance(data, dict):
        raise FixtureError(f"{endpoint} response is missing an object data field")
    return data


def _poll_timeout(deadline: float) -> float:
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise FixtureError("timed out waiting for quality fixture conclusions")
    return remaining


def _required(data: dict[str, object], field: str, endpoint: str, expected_type: type) -> Any:
    value = data.get(field)
    if not isinstance(value, expected_type) or isinstance(value, bool) and expected_type is int:
        raise FixtureError(f"{endpoint} response is missing {field}")
    return value


def _login(client: JSONClient, email: str, password: str) -> tuple[str, dict[str, object]]:
    endpoint = "/api/v1/auth/login"
    document = client.request(
        "POST",
        endpoint,
        expected_status=200,
        payload={"email": email, "password": password},
    )
    data = _data_object(document, endpoint)
    token = _required(data, "access_token", endpoint, str)
    user = _required(data, "user", endpoint, dict)
    return token, user


def _ensure_admin_compliance(client: JSONClient, token: str) -> None:
    endpoint = "/api/v1/admin/compliance"
    status = _data_object(
        client.request("GET", endpoint, expected_status=200, bearer_token=token),
        endpoint,
    )
    required = status.get("required")
    if required is False:
        return
    if required is not True:
        raise FixtureError("administrator compliance status is malformed")
    phrase = status.get("ack_phrase_en")
    if not isinstance(phrase, str) or not phrase.strip():
        raise FixtureError("administrator compliance phrase is unavailable")
    accept_endpoint = "/api/v1/admin/compliance/accept"
    accepted = _data_object(
        client.request(
            "POST",
            accept_endpoint,
            expected_status=200,
            payload={"language": "en", "phrase": phrase},
            bearer_token=token,
        ),
        accept_endpoint,
    )
    if accepted.get("required") is not False:
        raise FixtureError("administrator compliance acknowledgement did not persist")


def _mutation_key(run_identifier: str, action: str) -> str:
    return f"radar-quality-{run_identifier}-{action}"


def _recover_fixture_user_id(
    client: JSONClient,
    setup_token: str,
    fixture_email: str,
) -> int:
    endpoint = "/api/v1/admin/users?" + urllib.parse.urlencode({
        "search": fixture_email,
        "page": 1,
        "page_size": 10,
    })
    page = _data_object(
        client.request("GET", endpoint, expected_status=200, bearer_token=setup_token),
        endpoint,
    )
    items = page.get("items")
    if not isinstance(items, list):
        raise FixtureError("temporary administrator recovery response is malformed")
    matches = [
        item for item in items
        if isinstance(item, dict) and item.get("email") == fixture_email
    ]
    if len(matches) != 1:
        raise FixtureError(
            f"temporary administrator recovery found {len(matches)} exact fixture users"
        )
    return _required(matches[0], "id", endpoint, int)


def _validate_report(data: dict[str, object], scenario: str) -> None:
    alias = ALIASES[scenario]
    expected = EXPECTED_SCENARIOS[scenario]
    if data.get("model_alias") != alias:
        raise FixtureError(f"quality report alias mismatch for {scenario}")
    for field in ("overall_conclusion", "adulteration_risk", "degradation_risk"):
        if data.get(field) != expected[field]:
            raise FixtureError(f"quality report {field} mismatch for {scenario}")
    source = data.get("source_attribution")
    if not isinstance(source, dict) or source.get("state") != expected["source_state"]:
        raise FixtureError(f"quality report source state mismatch for {scenario}")


def _manifest(
    config: FixtureConfig,
    fixture_email: str,
    run_ids: dict[str, str],
    route_snapshot_path: str,
) -> dict[str, object]:
    document: dict[str, object] = {
        "schema_version": MANIFEST_SCHEMA_VERSION,
        "run_identifier": config.run_identifier,
        "fixture_user_email": fixture_email,
        "setup_administrator_email": config.setup_administrator_email,
        "route_snapshot_path": route_snapshot_path,
        "scenarios": {
            scenario: {
                "model_alias": ALIASES[scenario],
                "run_id": run_ids[scenario],
                "expected": dict(EXPECTED_SCENARIOS[scenario]),
            }
            for scenario in EXPECTED_SCENARIOS
        },
    }
    encoded = json.dumps(document, ensure_ascii=True, sort_keys=True).lower()
    for term in FORBIDDEN_MANIFEST_TERMS:
        if term in encoded:
            raise FixtureError(f"redacted manifest contains forbidden evidence field: {term}")
    return document


def _write_manifest(path: Path, document: dict[str, object]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    temporary_path = Path(temporary_name)
    try:
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            json.dump(document, stream, ensure_ascii=True, sort_keys=True, separators=(",", ":"))
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary_path, path)
        os.chmod(path, 0o600)
    except BaseException:
        try:
            os.close(descriptor)
        except OSError:
            pass
        temporary_path.unlink(missing_ok=True)
        raise


class _TemporaryAdministratorLease:
    def __init__(self, config: FixtureConfig) -> None:
        self.config = config
        self.client: JSONClient | None = None
        self.setup_token = ""
        self.fixture_token = ""
        self.fixture_user_id: int | None = None
        self.role_bindings: list[tuple[str, str]] = []
        self.demoted = False
        self.disabled = False

    def start(
        self,
        client: JSONClient,
        setup_token: str,
        fixture_user_id: int,
    ) -> None:
        self.client = client
        self.setup_token = setup_token
        self.fixture_user_id = fixture_user_id

    def authenticate(self, fixture_token: str) -> None:
        self.fixture_token = fixture_token

    def record_binding(self, role: str, binding_id: str) -> None:
        self.role_bindings.append((role, binding_id))

    def _reconcile_bindings(self) -> None:
        if (
            self.demoted
            or self.client is None
            or not self.fixture_token
            or self.fixture_user_id is None
        ):
            return
        endpoint = f"/api/v1/admin/radar/rbac/role-bindings?actor_id={self.fixture_user_id}"
        document = self.client.request(
            "GET",
            endpoint,
            expected_status=200,
            bearer_token=self.fixture_token,
        )
        bindings = document.get("data")
        if not isinstance(bindings, list) or not all(isinstance(item, dict) for item in bindings):
            raise FixtureError("temporary administrator role reconciliation is malformed")
        known = set(self.role_bindings)
        for binding in bindings:
            binding_id = binding.get("ID")
            actor_id = binding.get("ActorID")
            role = binding.get("Role")
            enabled = binding.get("Enabled")
            if (
                not isinstance(binding_id, str)
                or not isinstance(actor_id, int)
                or isinstance(actor_id, bool)
                or actor_id != self.fixture_user_id
                or not isinstance(role, str)
                or role not in RADAR_ROLES
                or not isinstance(enabled, bool)
            ):
                raise FixtureError("temporary administrator role reconciliation is malformed")
            identity = (role, binding_id)
            if enabled and identity not in known:
                self.role_bindings.append(identity)
                known.add(identity)

    def _disable_binding(self, role: str, binding_id: str) -> None:
        if self.client is None or not self.fixture_token:
            raise FixtureError("temporary administrator role cleanup is unavailable")
        endpoint = f"/api/v1/admin/radar/rbac/role-bindings/{binding_id}"
        disabled = _data_object(
            self.client.request(
                "DELETE",
                endpoint,
                expected_status=200,
                bearer_token=self.fixture_token,
            ),
            endpoint,
        )
        if disabled.get("id") != binding_id or disabled.get("enabled") is not False:
            raise FixtureError(f"temporary administrator {role} role cleanup was not confirmed")
        self.role_bindings.remove((role, binding_id))

    def _cleanup(self, *, disable_user: bool) -> None:
        if self.demoted and not self.role_bindings and (not disable_user or self.disabled):
            return
        errors: list[str] = []
        try:
            self._reconcile_bindings()
        except Exception as error:
            errors.append(f"reconcile temporary administrator roles: {error}")
        for role, binding_id in tuple(self.role_bindings):
            if role != "platform_admin":
                try:
                    self._disable_binding(role, binding_id)
                except Exception as error:
                    errors.append(f"disable {role} binding: {error}")
        for role, binding_id in tuple(self.role_bindings):
            if role == "platform_admin":
                try:
                    self._disable_binding(role, binding_id)
                except Exception as error:
                    errors.append(f"disable {role} binding: {error}")
        if self.fixture_user_id is not None and not self.demoted:
            try:
                if self.client is None or not self.setup_token:
                    raise FixtureError("temporary administrator demotion is unavailable")
                endpoint = f"/api/v1/admin/users/{self.fixture_user_id}"
                demoted = _data_object(
                    self.client.request(
                        "PUT",
                        endpoint,
                        expected_status=200,
                        payload={"role": "user"},
                        bearer_token=self.setup_token,
                        idempotency_key=_mutation_key(self.config.run_identifier, "demote-user"),
                    ),
                    endpoint,
                )
                if demoted.get("role") != "user":
                    raise FixtureError("temporary administrator demotion was not confirmed")
                self.demoted = True
            except Exception as error:
                errors.append(f"demote temporary administrator: {error}")
        if disable_user and self.fixture_user_id is not None and not self.disabled:
            try:
                if self.client is None or not self.setup_token:
                    raise FixtureError("temporary fixture user disable is unavailable")
                endpoint = f"/api/v1/admin/users/{self.fixture_user_id}"
                disabled = _data_object(
                    self.client.request(
                        "PUT",
                        endpoint,
                        expected_status=200,
                        payload={"status": "disabled"},
                        bearer_token=self.setup_token,
                        idempotency_key=_mutation_key(self.config.run_identifier, "disable-user"),
                    ),
                    endpoint,
                )
                if disabled.get("role") != "user" or disabled.get("status") != "disabled":
                    raise FixtureError("temporary fixture user disable was not confirmed")
                self.disabled = True
            except Exception as error:
                errors.append(f"disable temporary fixture user: {error}")
        if errors:
            raise FixtureError("; ".join(errors))

    def revoke_privileges(self) -> None:
        self._cleanup(disable_user=False)

    def disable_user(self) -> None:
        self._cleanup(disable_user=True)


def _provision_fixture(
    config: FixtureConfig,
    lease: _TemporaryAdministratorLease,
    *,
    worker_registrar: Callable[[str], None] | None = None,
) -> dict[str, object]:
    config.validate()
    client = JSONClient(config.origin, config.timeout_seconds)
    setup_token, _setup_user = _login(
        client,
        config.setup_administrator_email,
        config.setup_administrator_password,
    )
    _ensure_admin_compliance(client, setup_token)
    fixture_email = f"radar-quality-{config.run_identifier}@example.invalid"
    create_user_endpoint = "/api/v1/admin/users"
    try:
        created_user = _data_object(
            client.request(
                "POST",
                create_user_endpoint,
                expected_status=200,
                payload={
                    "email": fixture_email,
                    "password": config.fixture_password,
                    "username": f"radar-quality-{config.run_identifier}",
                    "notes": f"local isolated quality fixture {config.run_identifier}",
                    "role": "admin",
                    "balance": 5,
                },
                bearer_token=setup_token,
                idempotency_key=_mutation_key(config.run_identifier, "create-user"),
            ),
            create_user_endpoint,
        )
        fixture_user_id = _required(created_user, "id", create_user_endpoint, int)
    except FixtureError:
        fixture_user_id = _recover_fixture_user_id(client, setup_token, fixture_email)
        lease.start(client, setup_token, fixture_user_id)
        lease.disable_user()
        raise
    lease.start(client, setup_token, fixture_user_id)
    fixture_token, _fixture_user = _login(client, fixture_email, config.fixture_password)
    lease.authenticate(fixture_token)
    _ensure_admin_compliance(client, fixture_token)

    role_endpoint = "/api/v1/admin/radar/rbac/role-bindings"
    for role in RADAR_ROLES:
        binding = _data_object(
            client.request(
                "POST",
                role_endpoint,
                expected_status=201,
                payload={"actor_id": fixture_user_id, "role": role, "scope": {}},
                bearer_token=fixture_token,
                idempotency_key=_mutation_key(config.run_identifier, f"bind-{role}"),
            ),
            role_endpoint,
        )
        lease.record_binding(role, _required(binding, "ID", role_endpoint, str))

    if worker_registrar is not None:
        worker_registrar(fixture_token)

    group_endpoint = "/api/v1/admin/groups"
    group = _data_object(
        client.request(
            "POST",
            group_endpoint,
            expected_status=200,
            payload={
                "name": f"radar-quality-{config.run_identifier}",
                "description": f"local isolated quality fixture {config.run_identifier}",
                "platform": "openai",
                "rate_multiplier": 1,
                "is_exclusive": True,
                "subscription_type": "standard",
            },
            bearer_token=fixture_token,
            idempotency_key=_mutation_key(config.run_identifier, "create-group"),
        ),
        group_endpoint,
    )
    group_id = _required(group, "id", group_endpoint, int)

    route_snapshot_group = _data_object(
        client.request(
            "POST",
            group_endpoint,
            expected_status=200,
            payload={
                "name": f"radar-quality-route-snapshot-{config.run_identifier}",
                "description": f"local isolated route snapshot {config.run_identifier}",
                "platform": "composite",
                "rate_multiplier": 1,
                "is_exclusive": True,
                "subscription_type": "standard",
            },
            bearer_token=fixture_token,
            idempotency_key=_mutation_key(config.run_identifier, "create-route-snapshot-group"),
        ),
        group_endpoint,
    )
    route_snapshot_group_id = _required(route_snapshot_group, "id", group_endpoint, int)
    if route_snapshot_group_id <= 0:
        raise FixtureError("route snapshot group response has an invalid id")
    route_snapshot_path = f"/api/v1/admin/groups/{route_snapshot_group_id}/composite-routes"

    authorize_group_endpoint = f"/api/v1/admin/users/{fixture_user_id}"
    authorized_user = _data_object(
        client.request(
            "PUT",
            authorize_group_endpoint,
            expected_status=200,
            payload={"allowed_groups": [group_id]},
            bearer_token=setup_token,
            idempotency_key=_mutation_key(config.run_identifier, "authorize-group"),
        ),
        authorize_group_endpoint,
    )
    if authorized_user.get("allowed_groups") != [group_id]:
        raise FixtureError("temporary administrator group authorization did not persist")

    account_endpoint = "/api/v1/admin/accounts"
    _data_object(
        client.request(
            "POST",
            account_endpoint,
            expected_status=200,
            payload={
                "name": f"radar-quality-{config.run_identifier}",
                "platform": "openai",
                "type": "apikey",
                "credentials": {
                    "api_key": config.synthetic_upstream_key,
                    "base_url": "http://radar-synthetic-upstream:8090",
                    "model_mapping": dict(MODEL_MAPPING),
                },
                "concurrency": 4,
                "priority": 1,
                "group_ids": [group_id],
                "upstream_billing_probe_enabled": False,
            },
            bearer_token=fixture_token,
            idempotency_key=_mutation_key(config.run_identifier, "create-account"),
        ),
        account_endpoint,
    )

    key_endpoint = "/api/v1/keys"
    gateway_key = _data_object(
        client.request(
            "POST",
            key_endpoint,
            expected_status=200,
            payload={
                "name": f"radar-quality-{config.run_identifier}",
                "group_id": group_id,
                "quota": 5,
                "expires_in_days": 1,
            },
            bearer_token=fixture_token,
            idempotency_key=_mutation_key(config.run_identifier, "create-key"),
        ),
        key_endpoint,
    )
    gateway_key_id = _required(gateway_key, "id", key_endpoint, int)
    enable_endpoint = f"/api/v1/admin/radar/evaluation-keys/{gateway_key_id}/enable"
    _data_object(
        client.request(
            "POST",
            enable_endpoint,
            expected_status=200,
            payload={"run_identifier": config.run_identifier},
            bearer_token=fixture_token,
            idempotency_key=_mutation_key(config.run_identifier, "enable-key"),
        ),
        enable_endpoint,
    )

    model_endpoint = "/api/v1/admin/radar/models"
    for scenario, alias in ALIASES.items():
        _data_object(
            client.request(
                "POST",
                model_endpoint,
                expected_status=201,
                payload={"model_alias": alias},
                bearer_token=fixture_token,
                idempotency_key=_mutation_key(config.run_identifier, f"register-{scenario}"),
            ),
            model_endpoint,
        )

    run_ids: dict[str, str] = {}
    for scenario, alias in ALIASES.items():
        dataset_endpoint = "/api/v1/admin/radar/datasets"
        dataset = _data_object(
            client.request(
                "POST",
                dataset_endpoint,
                expected_status=201,
                payload={
                    "dataset_key": f"radar-quality-{config.run_identifier}-{scenario}",
                    "version": config.run_identifier,
                    "source_type": "synthetic",
                    "cases": build_scenario_cases(scenario),
                },
                bearer_token=fixture_token,
                idempotency_key=_mutation_key(config.run_identifier, f"dataset-{scenario}"),
            ),
            dataset_endpoint,
        )
        dataset_id = _required(dataset, "id", dataset_endpoint, str)
        publish_endpoint = f"/api/v1/admin/radar/datasets/{dataset_id}/publish"
        _data_object(
            client.request(
                "POST",
                publish_endpoint,
                expected_status=200,
                payload={"run_identifier": config.run_identifier},
                bearer_token=fixture_token,
                idempotency_key=_mutation_key(config.run_identifier, f"publish-{scenario}"),
            ),
            publish_endpoint,
        )
        plan_endpoint = "/api/v1/admin/radar/plans"
        plan = _data_object(
            client.request(
                "POST",
                plan_endpoint,
                expected_status=201,
                payload={
                    "name": f"radar-quality-{config.run_identifier}-{scenario}",
                    "dataset_version_id": dataset_id,
                    "gateway_api_key_id": gateway_key_id,
                    "trigger_type": "manual",
                    "model_matrix": [
                        {
                            "route": alias,
                            "baseline": {"model": "radar-synthetic-baseline"},
                            "candidate": {"model": UPSTREAM_MODELS[scenario]},
                        }
                    ],
                    "max_run_cost": "1",
                    "daily_cost_limit": "4",
                    "max_concurrency": 1,
                },
                bearer_token=fixture_token,
                idempotency_key=_mutation_key(config.run_identifier, f"plan-{scenario}"),
            ),
            plan_endpoint,
        )
        plan_id = _required(plan, "id", plan_endpoint, str)
        run_endpoint = "/api/v1/admin/radar/runs"
        run = _data_object(
            client.request(
                "POST",
                run_endpoint,
                expected_status=202,
                payload={
                    "plan_id": plan_id,
                    "trigger_source": "manual",
                    "baseline_ref": {"model_alias": "radar-synthetic-baseline"},
                    "candidate_ref": {"model_alias": UPSTREAM_MODELS[scenario]},
                },
                bearer_token=fixture_token,
                idempotency_key=_mutation_key(config.run_identifier, f"run-{scenario}"),
            ),
            run_endpoint,
        )
        run_ids[scenario] = _required(run, "id", run_endpoint, str)

    deadline = time.monotonic() + config.timeout_seconds
    while True:
        runs_document = client.request(
            "GET",
            "/api/v1/admin/radar/runs",
            expected_status=200,
            bearer_token=fixture_token,
            timeout_seconds=_poll_timeout(deadline),
        )
        if runs_document is None:
            raise FixtureError("run polling returned an unexpected not-ready response")
        runs = runs_document.get("data")
        if not isinstance(runs, list) or not all(isinstance(item, dict) for item in runs):
            raise FixtureError("run polling response is missing a data list")
        status_by_id = {item.get("id"): item.get("status") for item in runs}
        reports_ready = True
        for scenario, alias in ALIASES.items():
            endpoint = f"/api/v1/radar/models/{alias}/quality-report"
            report_document = client.request(
                "GET",
                endpoint,
                expected_status=200,
                bearer_token=fixture_token,
                not_ready_status=404,
                timeout_seconds=_poll_timeout(deadline),
            )
            if report_document is None:
                reports_ready = False
                continue
            report = _data_object(report_document, endpoint)
            try:
                _validate_report(report, scenario)
            except FixtureError:
                reports_ready = False
        runs_ready = all(status_by_id.get(run_id) == "completed" for run_id in run_ids.values())
        if runs_ready and reports_ready and time.monotonic() <= deadline:
            break
        if time.monotonic() >= deadline:
            raise FixtureError("timed out waiting for quality fixture conclusions")
        time.sleep(min(config.poll_interval_seconds, max(0.0, deadline - time.monotonic())))

    lease.revoke_privileges()

    fixture_token, _fixture_user = _login(client, fixture_email, config.fixture_password)
    me_endpoint = "/api/v1/auth/me"
    me = _data_object(
        client.request("GET", me_endpoint, expected_status=200, bearer_token=fixture_token),
        me_endpoint,
    )
    if me.get("role") != "user":
        raise FixtureError("demoted fixture user still has administrator role")
    health_endpoint = "/api/v1/radar/health"
    health_document = client.request(
        "GET",
        health_endpoint,
        expected_status=200,
        bearer_token=fixture_token,
    )
    health = health_document.get("data")
    if not isinstance(health, list):
        raise FixtureError("health response is missing a data list")
    health_aliases = {
        item.get("model_alias") for item in health if isinstance(item, dict)
    }
    if health_aliases != set(ALIASES.values()):
        raise FixtureError("health response does not contain exactly the fixture aliases")
    for scenario, alias in ALIASES.items():
        endpoint = f"/api/v1/radar/models/{alias}/quality-report"
        report = _data_object(
            client.request("GET", endpoint, expected_status=200, bearer_token=fixture_token),
            endpoint,
        )
        _validate_report(report, scenario)

    manifest = _manifest(config, fixture_email, run_ids, route_snapshot_path)
    _write_manifest(config.manifest_path, manifest)
    return manifest


def provision_fixture(
    config: FixtureConfig,
    *,
    worker_registrar: Callable[[str], None] | None = None,
) -> dict[str, object]:
    lease = _TemporaryAdministratorLease(config)
    try:
        manifest = _provision_fixture(config, lease, worker_registrar=worker_registrar)
    except BaseException as operation_error:
        try:
            lease.disable_user()
        except Exception as cleanup_error:
            if isinstance(operation_error, Exception):
                raise FixtureError(
                    f"{operation_error}; temporary administrator cleanup failed: {cleanup_error}"
                ) from operation_error
            operation_error.add_note(f"temporary administrator cleanup failed: {cleanup_error}")
        raise
    lease.revoke_privileges()
    return manifest


def _arguments() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Provision a deterministic local Quality Radar fixture")
    parser.add_argument("--origin", required=True)
    parser.add_argument("--setup-administrator-email", required=True)
    parser.add_argument("--run-identifier", required=True)
    parser.add_argument("--manifest", required=True, type=Path)
    parser.add_argument("--worker-bindings", type=Path)
    parser.add_argument("--worker-environment", type=Path)
    parser.add_argument("--worker-registration-output", type=Path)
    parser.add_argument("--timeout-seconds", type=float, default=120.0)
    parser.add_argument("--poll-interval-seconds", type=float, default=1.0)
    return parser.parse_args()


def main() -> int:
    arguments = _arguments()
    try:
        config = FixtureConfig(
            origin=arguments.origin,
            setup_administrator_email=arguments.setup_administrator_email,
            setup_administrator_password=os.environ.get("RADAR_SETUP_ADMINISTRATOR_PASSWORD", ""),
            fixture_password=os.environ.get("RADAR_FIXTURE_PASSWORD", ""),
            synthetic_upstream_key=os.environ.get("RADAR_SYNTHETIC_UPSTREAM_KEY", ""),
            manifest_path=arguments.manifest,
            run_identifier=arguments.run_identifier,
            timeout_seconds=arguments.timeout_seconds,
            poll_interval_seconds=arguments.poll_interval_seconds,
        )
        registration_arguments = (
            arguments.worker_bindings,
            arguments.worker_environment,
            arguments.worker_registration_output,
        )
        if any(value is not None for value in registration_arguments) and not all(
            value is not None for value in registration_arguments
        ):
            raise FixtureError("Worker registration requires bindings, environment, and output")

        worker_registrar: Callable[[str], None] | None = None
        registration_result: dict[str, object] | None = None
        if all(value is not None for value in registration_arguments):
            try:
                if __package__:
                    from deploy.radar.local_prerelease_closure import (
                        _input_binding_projection,
                        read_environment,
                        register_workers_with_token,
                        write_worker_registration_evidence,
                    )
                else:
                    from local_prerelease_closure import (  # type: ignore[no-redef]
                        _input_binding_projection,
                        read_environment,
                        register_workers_with_token,
                        write_worker_registration_evidence,
                    )

                bindings_path = arguments.worker_bindings
                environment_path = arguments.worker_environment
                registration_output = arguments.worker_registration_output
                assert bindings_path is not None
                assert environment_path is not None
                assert registration_output is not None
                bindings = _input_binding_projection(json.loads(bindings_path.read_text(encoding="utf-8")))
                worker_environment = read_environment(environment_path)

                def register_for_fixture(access_token: str) -> None:
                    nonlocal registration_result
                    try:
                        registration_result = register_workers_with_token(
                            config.origin,
                            config.run_identifier,
                            str(bindings["worker_digest"]),
                            access_token,
                            worker_environment,
                        )
                    except ValueError as error:
                        raise FixtureError(str(error)) from error

                worker_registrar = register_for_fixture
            except (OSError, ValueError, json.JSONDecodeError) as error:
                raise FixtureError("Worker registration inputs are invalid") from error

        provision_fixture(config, worker_registrar=worker_registrar)
        if registration_result is not None:
            write_worker_registration_evidence(registration_output, registration_result)
    except (FixtureError, OSError, json.JSONDecodeError) as error:
        print(f"ERROR: {error}", file=os.sys.stderr)
        return 1
    print(f"Fixture manifest written to {arguments.manifest}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
