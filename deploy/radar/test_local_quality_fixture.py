from __future__ import annotations

import json
import os
import stat
import subprocess
import sys
import tempfile
import time
import unittest
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from threading import Thread
from typing import Iterator
from urllib.error import URLError

from deploy.radar.local_quality_fixture import (
    CANDIDATES,
    DIMENSIONS,
    EVENT_CLASS,
    EXPECTED_SCENARIOS,
    FixtureConfig,
    FixtureError,
    JSONClient,
    build_scenario_cases,
    provision_fixture,
    quality_case,
)


ALIASES = {
    "healthy": "radar-quality-healthy",
    "watered": "radar-quality-watered",
    "degraded": "radar-quality-degraded",
    "insufficient": "radar-quality-insufficient",
}
RUN_IDS = {scenario: f"run-{scenario}" for scenario in ALIASES}
ROLE_BINDING_IDS = {
    "platform_admin": "00000000-0000-4000-8000-000000000001",
    "quality_admin": "00000000-0000-4000-8000-000000000002",
    "test_operator": "00000000-0000-4000-8000-000000000003",
    "release_manager": "00000000-0000-4000-8000-000000000004",
}
FORBIDDEN_EVIDENCE_TERMS = (
    "password",
    "token",
    "api_key",
    "prompt",
    "completion",
    "account",
    "channel",
    "route_trace_id",
    "probe_spec_hash",
)


def envelope(data: object) -> bytes:
    return json.dumps({"code": 0, "message": "success", "data": data}).encode()


@contextmanager
def fixture_server(
    *,
    never_ready: bool = False,
    report_not_found_rounds: int = 0,
    poll_delay_seconds: float = 0.0,
    failed_role_cleanup: str | None = None,
    malformed_role_response: str | None = None,
    malformed_user_create_response: bool = False,
) -> Iterator[tuple[str, list[dict[str, object]]]]:
    requests: list[dict[str, object]] = []
    datasets: dict[str, str] = {}
    plans: dict[str, str] = {}
    report_attempts: dict[str, int] = {}
    active_role_bindings: dict[str, str] = {}
    created_fixture_email: str | None = None

    class Handler(BaseHTTPRequestHandler):
        def _reply(self, status: int, data: object) -> None:
            body = envelope(data)
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            try:
                self.wfile.write(body)
            except (BrokenPipeError, ConnectionResetError):
                return

        def _capture(self) -> dict[str, object]:
            length = int(self.headers.get("Content-Length", "0"))
            payload = json.loads(self.rfile.read(length)) if length else None
            item = {
                "method": self.command,
                "path": self.path,
                "payload": payload,
                "authorization": self.headers.get("Authorization"),
                "idempotency_key": self.headers.get("Idempotency-Key"),
                "captured_at": time.monotonic(),
            }
            requests.append(item)
            return item

        def do_POST(self) -> None:  # noqa: N802
            item = self._capture()
            payload = item["payload"]
            assert isinstance(payload, dict)
            path = self.path
            if path == "/api/v1/auth/login":
                email = payload["email"]
                token = "setup-secret" if email == "setup@example.invalid" else "fixture-secret"
                role = "admin" if token == "setup-secret" else "admin"
                self._reply(200, {"access_token": token, "user": {"id": 1, "email": email, "role": role}})
                return
            if path == "/api/v1/admin/compliance/accept":
                self._reply(200, {"required": False})
                return
            if path == "/api/v1/admin/users":
                nonlocal created_fixture_email
                created_fixture_email = str(payload["email"])
                if malformed_user_create_response:
                    self._reply(200, {})
                    return
                self._reply(200, {"id": 101, "email": payload["email"], "role": "admin"})
                return
            if path == "/api/v1/admin/radar/rbac/role-bindings":
                role = str(payload["role"])
                active_role_bindings[role] = ROLE_BINDING_IDS[role]
                if role == malformed_role_response:
                    self._reply(201, {})
                    return
                self._reply(
                    201,
                    {
                        "ID": ROLE_BINDING_IDS[role],
                        "ActorID": 101,
                        "Role": role,
                        "Scope": {},
                        "Enabled": True,
                        "CreatedBy": 101,
                        "CreatedAt": "2026-08-13T00:00:00Z",
                        "DisabledAt": None,
                    },
                )
                return
            if path == "/api/v1/admin/radar/workers":
                self._reply(201, {
                    "name": payload["name"],
                    "worker_kind": payload["worker_kind"],
                    "status": "active",
                    "capabilities": payload["capabilities"],
                    "image_digest": payload["image_digest"],
                    "region": payload["region"],
                    "max_concurrency": payload["max_concurrency"],
                })
                return
            if path == "/api/v1/admin/groups":
                group_id = 203 if payload.get("platform") == "composite" else 202
                self._reply(200, {"id": group_id, "name": payload["name"]})
                return
            if path == "/api/v1/admin/accounts":
                self._reply(200, {"id": 303, "name": payload["name"]})
                return
            if path == "/api/v1/keys":
                self._reply(200, {"id": 404, "name": payload["name"]})
                return
            if path == "/api/v1/admin/radar/evaluation-keys/404/enable":
                self._reply(200, {"id": 404, "enabled": True})
                return
            if path == "/api/v1/admin/radar/models":
                self._reply(201, {"model_alias": payload["model_alias"]})
                return
            if path == "/api/v1/admin/radar/datasets":
                scenario = str(payload["dataset_key"]).split("-")[-1]
                dataset_id = f"dataset-{scenario}"
                datasets[dataset_id] = scenario
                self._reply(201, {"id": dataset_id, "status": "draft"})
                return
            if path.startswith("/api/v1/admin/radar/datasets/") and path.endswith("/publish"):
                dataset_id = path.split("/")[-2]
                self._reply(200, {"id": dataset_id, "status": "published"})
                return
            if path == "/api/v1/admin/radar/plans":
                dataset_id = str(payload["dataset_version_id"])
                scenario = datasets[dataset_id]
                plan_id = f"plan-{scenario}"
                plans[plan_id] = scenario
                self._reply(201, {"id": plan_id})
                return
            if path == "/api/v1/admin/radar/runs":
                scenario = plans[str(payload["plan_id"])]
                self._reply(202, {"id": RUN_IDS[scenario], "status": "queued"})
                return
            self._reply(404, {"error": "unexpected endpoint"})

        def do_PUT(self) -> None:  # noqa: N802
            item = self._capture()
            payload = item["payload"]
            if self.path == "/api/v1/admin/users/101" and payload == {"allowed_groups": [202]}:
                self._reply(200, {"id": 101, "role": "admin", "allowed_groups": [202]})
                return
            if self.path == "/api/v1/admin/users/101" and payload == {"role": "user"}:
                self._reply(200, {"id": 101, "role": "user"})
                return
            if self.path == "/api/v1/admin/users/101" and payload == {"status": "disabled"}:
                self._reply(200, {"id": 101, "role": "user", "status": "disabled"})
                return
            self._reply(404, {"error": "unexpected endpoint"})

        def do_GET(self) -> None:  # noqa: N802
            self._capture()
            if self.path.startswith("/api/v1/admin/users?"):
                if created_fixture_email is None:
                    self._reply(200, {"items": [], "total": 0, "page": 1, "page_size": 1, "pages": 1})
                    return
                self._reply(200, {
                    "items": [{"id": 101, "email": created_fixture_email, "role": "admin"}],
                    "total": 1,
                    "page": 1,
                    "page_size": 1,
                    "pages": 1,
                })
                return
            if self.path == "/api/v1/admin/radar/rbac/role-bindings?actor_id=101":
                self._reply(
                    200,
                    [
                        {
                            "ID": binding_id,
                            "ActorID": 101,
                            "Role": role,
                            "Scope": {},
                            "Enabled": True,
                            "CreatedBy": 101,
                            "CreatedAt": "2026-08-13T00:00:00Z",
                            "DisabledAt": None,
                        }
                        for role, binding_id in active_role_bindings.items()
                    ],
                )
                return
            if self.path == "/api/v1/admin/compliance":
                self._reply(200, {"required": True, "ack_phrase_en": "I acknowledge"})
                return
            if self.path == "/api/v1/admin/radar/runs":
                if poll_delay_seconds:
                    time.sleep(poll_delay_seconds)
                status = "running" if never_ready else "completed"
                self._reply(200, [{"id": run_id, "status": status} for run_id in RUN_IDS.values()])
                return
            if self.path == "/api/v1/auth/me":
                self._reply(200, {"id": 101, "email": "fixture@example.invalid", "role": "user"})
                return
            if self.path == "/api/v1/radar/health":
                self._reply(200, [{"model_alias": alias} for alias in ALIASES.values()])
                return
            prefix = "/api/v1/radar/models/"
            suffix = "/quality-report"
            if self.path.startswith(prefix) and self.path.endswith(suffix):
                if poll_delay_seconds:
                    time.sleep(poll_delay_seconds)
                alias = self.path[len(prefix) : -len(suffix)]
                attempts = report_attempts.get(alias, 0)
                report_attempts[alias] = attempts + 1
                if attempts < report_not_found_rounds:
                    self._reply(404, {"message": "quality report not published"})
                    return
                scenario = next(name for name, candidate in ALIASES.items() if candidate == alias)
                expected = EXPECTED_SCENARIOS[scenario]
                report = {
                    "model_alias": alias,
                    "overall_conclusion": expected["overall_conclusion"],
                    "adulteration_risk": expected["adulteration_risk"],
                    "degradation_risk": expected["degradation_risk"],
                    "source_attribution": {"state": expected["source_state"]},
                    "dimension_results": [{"key": key} for key in DIMENSIONS],
                }
                if never_ready:
                    report["overall_conclusion"] = "observe"
                self._reply(200, report)
                return
            self._reply(404, {"error": "unexpected endpoint"})

        def do_DELETE(self) -> None:  # noqa: N802
            self._capture()
            prefix = "/api/v1/admin/radar/rbac/role-bindings/"
            failed_binding_id = ROLE_BINDING_IDS.get(failed_role_cleanup or "")
            if self.path == prefix + str(failed_binding_id):
                self._reply(500, {"error": "injected cleanup failure"})
                return
            if self.path.startswith(prefix) and self.path[len(prefix) :] in ROLE_BINDING_IDS.values():
                binding_id = self.path[len(prefix) :]
                for role, active_id in tuple(active_role_bindings.items()):
                    if active_id == binding_id:
                        del active_role_bindings[role]
                self._reply(200, {"id": self.path[len(prefix) :], "enabled": False})
                return
            self._reply(404, {"error": "unexpected endpoint"})

        def log_message(self, _format: str, *_args: object) -> None:
            return

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}", requests
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


class QualityCaseTests(unittest.TestCase):
    def test_builds_all_dimensions_with_exact_event_classes(self) -> None:
        for dimension in DIMENSIONS:
            with self.subTest(dimension=dimension):
                case = quality_case(dimension, "healthy", 3, None)
                self.assertEqual(case["case_key"], f"healthy-{dimension}-primary")
                self.assertEqual(case["sample_count"], 3)
                self.assertEqual(
                    case["quality_probe_spec"],
                    {
                        "schema_version": "quality-v1",
                        "quality_dimension": dimension,
                        "event_class": EVENT_CLASS[dimension],
                        "minimum_samples": 3,
                    },
                )

    def test_fingerprint_uses_only_the_two_exact_candidates(self) -> None:
        cases = build_scenario_cases("watered")
        fingerprint = [case for case in cases if case["quality_dimension"] == "model_fingerprint"]
        self.assertEqual(len(fingerprint), 2)
        self.assertEqual(
            [case["quality_probe_spec"]["source_candidate"] for case in fingerprint],
            list(CANDIDATES),
        )
        self.assertEqual(
            [case["case_key"] for case in fingerprint],
            ["watered-model_fingerprint-reference", "watered-model_fingerprint-alternate"],
        )

    def test_scenario_sample_counts_are_deterministic(self) -> None:
        for scenario in EXPECTED_SCENARIOS:
            with self.subTest(scenario=scenario):
                cases = build_scenario_cases(scenario)
                self.assertEqual(len(cases), 9)
                expected_count = 2 if scenario == "insufficient" else 3
                self.assertEqual({case["sample_count"] for case in cases}, {expected_count})


class JSONClientTests(unittest.TestCase):
    def test_rejects_non_loopback_origins(self) -> None:
        for origin in ("https://example.com", "http://10.0.0.4", "http://sub2api-staging"):
            with self.subTest(origin=origin), self.assertRaises(FixtureError):
                JSONClient(origin, timeout_seconds=1)

    def test_rejects_redirect_non_object_and_oversized_responses(self) -> None:
        bodies = {
            "/redirect": (302, b"{}", {"Location": "/object"}),
            "/array": (200, b"[]", {}),
            "/large": (200, b"{" + b" " * (1024 * 1024) + b"}", {}),
            "/error": (500, b"{}", {}),
            "/object": (200, b"{}", {}),
        }

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self) -> None:  # noqa: N802
                status, body, headers = bodies[self.path]
                self.send_response(status)
                for key, value in headers.items():
                    self.send_header(key, value)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                try:
                    self.wfile.write(body)
                except (BrokenPipeError, ConnectionResetError):
                    return

            def log_message(self, _format: str, *_args: object) -> None:
                return

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            client = JSONClient(f"http://127.0.0.1:{server.server_port}", timeout_seconds=1)
            for path in ("/redirect", "/array", "/large", "/error"):
                with self.subTest(path=path), self.assertRaises(FixtureError):
                    client.request("GET", path, expected_status=200)
        finally:
            server.shutdown()
            server.server_close()
            thread.join()

    def test_wraps_transport_timeout_as_fixture_error(self) -> None:
        class TimeoutOpener:
            def open(self, _request: object, timeout: float) -> object:
                raise URLError(TimeoutError(f"timed out after {timeout}"))

        client = JSONClient("http://127.0.0.1:1", timeout_seconds=0.01, opener=TimeoutOpener())
        with self.assertRaisesRegex(FixtureError, "request failed"):
            client.request("GET", "/timeout", expected_status=200)


class ProvisionLifecycleTests(unittest.TestCase):
    def config(self, origin: str, manifest_path: Path, timeout_seconds: float = 1.0) -> FixtureConfig:
        return FixtureConfig(
            origin=origin,
            setup_administrator_email="setup@example.invalid",
            setup_administrator_password="setup-password-secret",
            fixture_password="fixture-password-secret",
            synthetic_upstream_key="synthetic-upstream-secret",
            manifest_path=manifest_path,
            run_identifier="quality-run-001",
            timeout_seconds=timeout_seconds,
            poll_interval_seconds=0.01,
        )

    def test_provisions_in_order_demotes_admin_and_writes_redacted_manifest(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir, fixture_server() as (origin, requests):
            manifest_path = Path(temp_dir) / "fixture-manifest.json"
            manifest = provision_fixture(self.config(origin, manifest_path))

            expected_prefix = [
                ("POST", "/api/v1/auth/login"),
                ("GET", "/api/v1/admin/compliance"),
                ("POST", "/api/v1/admin/compliance/accept"),
                ("POST", "/api/v1/admin/users"),
                ("POST", "/api/v1/auth/login"),
                ("GET", "/api/v1/admin/compliance"),
                ("POST", "/api/v1/admin/compliance/accept"),
                *(("POST", "/api/v1/admin/radar/rbac/role-bindings") for _ in range(4)),
                ("POST", "/api/v1/admin/groups"),
                ("POST", "/api/v1/admin/groups"),
                ("PUT", "/api/v1/admin/users/101"),
                ("POST", "/api/v1/admin/accounts"),
                ("POST", "/api/v1/keys"),
                ("POST", "/api/v1/admin/radar/evaluation-keys/404/enable"),
                *(("POST", "/api/v1/admin/radar/models") for _ in range(4)),
            ]
            observed = [(str(item["method"]), str(item["path"])) for item in requests]
            self.assertEqual(observed[: len(expected_prefix)], expected_prefix)
            compliance_accepts = [
                item
                for item in requests
                if item["path"] == "/api/v1/admin/compliance/accept"
            ]
            self.assertEqual(
                [item["authorization"] for item in compliance_accepts],
                ["Bearer setup-secret", "Bearer fixture-secret"],
            )
            create_user_request = next(
                item for item in requests if item["path"] == "/api/v1/admin/users"
            )
            fixture_balance = create_user_request["payload"].get("balance", 0)
            self.assertGreater(fixture_balance, 0)
            self.assertLessEqual(fixture_balance, 5)
            expected_middle: list[tuple[str, str]] = []
            for scenario in EXPECTED_SCENARIOS:
                expected_middle.extend(
                    [
                        ("POST", "/api/v1/admin/radar/datasets"),
                        ("POST", f"/api/v1/admin/radar/datasets/dataset-{scenario}/publish"),
                        ("POST", "/api/v1/admin/radar/plans"),
                        ("POST", "/api/v1/admin/radar/runs"),
                    ]
                )
            self.assertEqual(
                observed[len(expected_prefix) : len(expected_prefix) + len(expected_middle)],
                expected_middle,
            )
            run_requests = [
                item
                for item in requests
                if item["method"] == "POST" and item["path"] == "/api/v1/admin/radar/runs"
            ]
            self.assertEqual(
                [item["payload"]["trigger_source"] for item in run_requests],
                ["manual"] * len(EXPECTED_SCENARIOS),
            )
            poll_start = len(expected_prefix) + len(expected_middle)
            self.assertEqual(
                observed[poll_start : poll_start + 5],
                [
                    ("GET", "/api/v1/admin/radar/runs"),
                    *(("GET", f"/api/v1/radar/models/{alias}/quality-report") for alias in ALIASES.values()),
                ],
            )

            mutation_requests = [item for item in requests if item["method"] in {"POST", "PUT"}]
            for item in mutation_requests:
                if item["path"] not in {"/api/v1/auth/login", "/api/v1/admin/compliance/accept"}:
                    encoded = json.dumps(item["payload"], sort_keys=True)
                    self.assertTrue(
                        "quality-run-001" in encoded or "quality-run-001" in str(item["idempotency_key"]),
                        item,
                    )

            role_payloads = [
                item["payload"]
                for item in requests
                if item["path"] == "/api/v1/admin/radar/rbac/role-bindings"
            ]
            self.assertEqual(
                [payload["role"] for payload in role_payloads if isinstance(payload, dict)],
                ["platform_admin", "quality_admin", "test_operator", "release_manager"],
            )
            role_deletes = [
                item
                for item in requests
                if item["method"] == "DELETE"
                and str(item["path"]).startswith("/api/v1/admin/radar/rbac/role-bindings/")
            ]
            self.assertEqual(
                [str(item["path"]).rsplit("/", 1)[-1] for item in role_deletes],
                [
                    ROLE_BINDING_IDS["quality_admin"],
                    ROLE_BINDING_IDS["test_operator"],
                    ROLE_BINDING_IDS["release_manager"],
                    ROLE_BINDING_IDS["platform_admin"],
                ],
            )
            self.assertTrue(
                all(item["authorization"] == "Bearer fixture-secret" for item in role_deletes)
            )

            account_request = next(item for item in requests if item["path"] == "/api/v1/admin/accounts")
            credentials = account_request["payload"]["credentials"]
            self.assertEqual(credentials["base_url"], "http://radar-synthetic-upstream:8090")
            self.assertEqual(
                credentials["model_mapping"],
                {
                    "radar-synthetic-baseline": "radar-synthetic-baseline",
                    "radar-synthetic-healthy": "radar-synthetic-healthy",
                    "radar-synthetic-watered": "radar-synthetic-watered",
                    "radar-synthetic-degraded": "radar-synthetic-degraded",
                },
            )

            group_requests = [
                item for item in requests if item["path"] == "/api/v1/admin/groups"
            ]
            self.assertEqual(
                [item["payload"]["platform"] for item in group_requests],
                ["openai", "composite"],
            )

            demotion_index = next(
                index
                for index, item in enumerate(requests)
                if item["path"] == "/api/v1/admin/users/101" and item["payload"] == {"role": "user"}
            )
            self.assertEqual(requests[demotion_index]["payload"], {"role": "user"})
            self.assertEqual(
                observed[demotion_index + 1 :],
                [
                    ("POST", "/api/v1/auth/login"),
                    ("GET", "/api/v1/auth/me"),
                    ("GET", "/api/v1/radar/health"),
                    *(("GET", f"/api/v1/radar/models/{alias}/quality-report") for alias in ALIASES.values()),
                ],
            )

            disk_manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
            self.assertEqual(manifest, disk_manifest)
            self.assertEqual(stat.S_IMODE(os.stat(manifest_path).st_mode), 0o600)
            self.assertEqual(manifest["schema_version"], "radar-local-quality-fixture-v1")
            self.assertEqual(manifest["run_identifier"], "quality-run-001")
            self.assertEqual(manifest["setup_administrator_email"], "setup@example.invalid")
            self.assertEqual(
                manifest["route_snapshot_path"],
                "/api/v1/admin/groups/203/composite-routes",
            )
            self.assertEqual(set(manifest["scenarios"]), set(EXPECTED_SCENARIOS))
            for scenario, expected in EXPECTED_SCENARIOS.items():
                entry = manifest["scenarios"][scenario]
                self.assertEqual(entry["model_alias"], ALIASES[scenario])
                self.assertEqual(entry["run_id"], RUN_IDS[scenario])
                self.assertEqual(entry["expected"], expected)
            encoded_manifest = json.dumps(manifest, sort_keys=True).lower()
            for forbidden in FORBIDDEN_EVIDENCE_TERMS:
                self.assertNotIn(forbidden, encoded_manifest)

    def test_registers_workers_as_fixture_tenant_before_creating_tenant_resources(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir, fixture_server() as (origin, requests):
            manifest_path = Path(temp_dir) / "fixture-manifest.json"
            observed_tokens: list[str] = []

            def register_workers_for_fixture(access_token: str) -> None:
                observed_tokens.append(access_token)
                requests.append({
                    "method": "REGISTER_WORKERS",
                    "path": "/fixture-tenant/workers",
                    "payload": None,
                    "authorization": f"Bearer {access_token}",
                    "idempotency_key": None,
                    "captured_at": time.monotonic(),
                })

            provision_fixture(
                self.config(origin, manifest_path),
                worker_registrar=register_workers_for_fixture,
            )

            self.assertEqual(observed_tokens, ["fixture-secret"])
            paths = [(str(item["method"]), str(item["path"])) for item in requests]
            registration_index = paths.index(("REGISTER_WORKERS", "/fixture-tenant/workers"))
            role_indices = [
                index
                for index, path in enumerate(paths)
                if path == ("POST", "/api/v1/admin/radar/rbac/role-bindings")
            ]
            first_tenant_resource = paths.index(("POST", "/api/v1/admin/groups"))
            self.assertEqual(len(role_indices), 4)
            self.assertLess(max(role_indices), registration_index)
            self.assertLess(registration_index, first_tenant_resource)

    def test_cli_writes_fixture_scoped_worker_registration_evidence(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir, fixture_server() as (origin, requests):
            root = Path(temp_dir)
            manifest_path = root / "fixture-manifest.json"
            registration_path = root / "worker-registration.json"
            bindings_path = root / "bindings.json"
            environment_path = root / "rehearsal.env"
            bindings_path.write_text(json.dumps({
                "run_id": "quality-run-001-rehearsal",
                "source_sha256": "b" * 64,
                "backup_sha256": "c" * 64,
                "control_plane_digest": "sha256:" + "a" * 64,
                "worker_digest": "sha256:" + "d" * 64,
                "rollback_control_plane_digest": "sha256:" + "e" * 64,
                "rollback_worker_digest": "sha256:" + "f" * 64,
                "policy_version": "quality-v1",
                "fixture_version": "local-quality-fixture-v1",
                "dependency_digests": ["sha256:" + "1" * 64],
                "environment_fingerprint": "2" * 64,
                "source_exclusions": {"metadata": True},
            }), encoding="utf-8")
            environment_path.write_text(
                "RADAR_RUNNER_WORKER_TOKEN=" + "r" * 32 + "\n"
                "RADAR_GRADER_WORKER_TOKEN=" + "g" * 32 + "\n"
                "RADAR_STATISTICS_WORKER_TOKEN=" + "s" * 32 + "\n",
                encoding="utf-8",
            )
            bindings_path.chmod(0o600)
            environment_path.chmod(0o600)

            completed = subprocess.run(
                [
                    sys.executable,
                    str(Path(__file__).with_name("local_quality_fixture.py")),
                    "--origin", origin,
                    "--setup-administrator-email", "setup@example.invalid",
                    "--run-identifier", "quality-run-001-rehearsal",
                    "--manifest", str(manifest_path),
                    "--worker-bindings", str(bindings_path),
                    "--worker-environment", str(environment_path),
                    "--worker-registration-output", str(registration_path),
                    "--timeout-seconds", "1",
                    "--poll-interval-seconds", "0.01",
                ],
                cwd=Path(__file__).resolve().parents[2],
                env=os.environ | {
                    "RADAR_SETUP_ADMINISTRATOR_PASSWORD": "setup-password-secret",
                    "RADAR_FIXTURE_PASSWORD": "fixture-password-secret",
                    "RADAR_SYNTHETIC_UPSTREAM_KEY": "synthetic-upstream-secret",
                },
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertEqual(completed.returncode, 0, completed.stderr)
            registration = json.loads(registration_path.read_text(encoding="utf-8"))
            self.assertEqual(
                registration["registration_order"],
                ["reasoning-runner", "exact-grader", "reasoning-statistics"],
            )
            self.assertEqual(stat.S_IMODE(registration_path.stat().st_mode), 0o600)
            worker_requests = [
                item for item in requests if item["path"] == "/api/v1/admin/radar/workers"
            ]
            self.assertEqual(len(worker_requests), 3)
            self.assertTrue(all(
                item["authorization"] == "Bearer fixture-secret"
                for item in worker_requests
            ))
            first_worker = requests.index(worker_requests[0])
            first_group = next(
                index for index, item in enumerate(requests)
                if item["path"] == "/api/v1/admin/groups"
            )
            self.assertLess(first_worker, first_group)

    def test_worker_registration_failure_is_reported_without_a_traceback(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir, fixture_server() as (origin, _requests):
            root = Path(temp_dir)
            bindings_path = root / "bindings.json"
            environment_path = root / "rehearsal.env"
            bindings_path.write_text(json.dumps({
                "run_id": "quality-run-001-rehearsal",
                "source_sha256": "b" * 64,
                "backup_sha256": "c" * 64,
                "control_plane_digest": "sha256:" + "a" * 64,
                "worker_digest": "sha256:" + "d" * 64,
                "rollback_control_plane_digest": "sha256:" + "e" * 64,
                "rollback_worker_digest": "sha256:" + "f" * 64,
                "policy_version": "quality-v1",
                "fixture_version": "local-quality-fixture-v1",
                "dependency_digests": ["sha256:" + "1" * 64],
                "environment_fingerprint": "2" * 64,
                "source_exclusions": {"metadata": True},
            }), encoding="utf-8")
            environment_path.write_text(
                "RADAR_RUNNER_WORKER_TOKEN=short\n"
                "RADAR_GRADER_WORKER_TOKEN=" + "g" * 32 + "\n"
                "RADAR_STATISTICS_WORKER_TOKEN=" + "s" * 32 + "\n",
                encoding="utf-8",
            )
            bindings_path.chmod(0o600)
            environment_path.chmod(0o600)

            completed = subprocess.run(
                [
                    sys.executable,
                    str(Path(__file__).with_name("local_quality_fixture.py")),
                    "--origin", origin,
                    "--setup-administrator-email", "setup@example.invalid",
                    "--run-identifier", "quality-run-001-rehearsal",
                    "--manifest", str(root / "fixture-manifest.json"),
                    "--worker-bindings", str(bindings_path),
                    "--worker-environment", str(environment_path),
                    "--worker-registration-output", str(root / "worker-registration.json"),
                    "--timeout-seconds", "1",
                    "--poll-interval-seconds", "0.01",
                ],
                cwd=Path(__file__).resolve().parents[2],
                env=os.environ | {
                    "RADAR_SETUP_ADMINISTRATOR_PASSWORD": "setup-password-secret",
                    "RADAR_FIXTURE_PASSWORD": "fixture-password-secret",
                    "RADAR_SYNTHETIC_UPSTREAM_KEY": "synthetic-upstream-secret",
                },
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertEqual(completed.returncode, 1)
            self.assertIn("ERROR: required local Worker token is unavailable", completed.stderr)
            self.assertNotIn("Traceback", completed.stderr)

    def test_polling_timeout_fails_without_manifest(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir, fixture_server(never_ready=True) as (origin, requests):
            manifest_path = Path(temp_dir) / "fixture-manifest.json"
            with self.assertRaisesRegex(FixtureError, "timed out"):
                provision_fixture(self.config(origin, manifest_path, timeout_seconds=0.04))
            self.assertFalse(manifest_path.exists())
            cleanup = [
                (item["method"], item["path"], item["payload"])
                for item in requests
                if item["method"] == "DELETE"
                or item["path"] == "/api/v1/admin/users/101"
                and item["payload"] in ({"role": "user"}, {"status": "disabled"})
            ]
            self.assertEqual(
                cleanup,
                [
                    ("DELETE", f"/api/v1/admin/radar/rbac/role-bindings/{ROLE_BINDING_IDS['quality_admin']}", None),
                    ("DELETE", f"/api/v1/admin/radar/rbac/role-bindings/{ROLE_BINDING_IDS['test_operator']}", None),
                    ("DELETE", f"/api/v1/admin/radar/rbac/role-bindings/{ROLE_BINDING_IDS['release_manager']}", None),
                    ("DELETE", f"/api/v1/admin/radar/rbac/role-bindings/{ROLE_BINDING_IDS['platform_admin']}", None),
                    ("PUT", "/api/v1/admin/users/101", {"role": "user"}),
                    ("PUT", "/api/v1/admin/users/101", {"status": "disabled"}),
                ],
            )
            self.assertTrue(
                all(
                    item["authorization"]
                    == (
                        "Bearer fixture-secret"
                        if item["method"] == "DELETE"
                        else "Bearer setup-secret"
                    )
                    for item in requests
                    if item["method"] == "DELETE"
                    or item["path"] == "/api/v1/admin/users/101"
                    and item["payload"] in ({"role": "user"}, {"status": "disabled"})
                )
            )

    def test_cleanup_failure_preserves_original_error_and_continues_disabling_identity(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir, fixture_server(
            never_ready=True,
            failed_role_cleanup="quality_admin",
        ) as (origin, requests):
            manifest_path = Path(temp_dir) / "fixture-manifest.json"
            with self.assertRaises(FixtureError) as raised:
                provision_fixture(self.config(origin, manifest_path, timeout_seconds=0.04))

        message = str(raised.exception)
        self.assertIn("timed out waiting for quality fixture conclusions", message)
        self.assertIn("temporary administrator cleanup failed", message)
        cleanup = [
            (item["method"], item["path"], item["payload"])
            for item in requests
            if item["method"] == "DELETE"
            or item["path"] == "/api/v1/admin/users/101"
            and item["payload"] in ({"role": "user"}, {"status": "disabled"})
        ]
        self.assertEqual(
            cleanup[-6:],
            [
                ("DELETE", f"/api/v1/admin/radar/rbac/role-bindings/{ROLE_BINDING_IDS['quality_admin']}", None),
                ("DELETE", f"/api/v1/admin/radar/rbac/role-bindings/{ROLE_BINDING_IDS['test_operator']}", None),
                ("DELETE", f"/api/v1/admin/radar/rbac/role-bindings/{ROLE_BINDING_IDS['release_manager']}", None),
                ("DELETE", f"/api/v1/admin/radar/rbac/role-bindings/{ROLE_BINDING_IDS['platform_admin']}", None),
                ("PUT", "/api/v1/admin/users/101", {"role": "user"}),
                ("PUT", "/api/v1/admin/users/101", {"status": "disabled"}),
            ],
        )
        self.assertFalse(manifest_path.exists())

    def test_malformed_binding_response_reconciles_and_revokes_server_side_binding(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir, fixture_server(
            malformed_role_response="quality_admin",
        ) as (origin, requests):
            manifest_path = Path(temp_dir) / "fixture-manifest.json"
            with self.assertRaisesRegex(FixtureError, "response is missing ID"):
                provision_fixture(self.config(origin, manifest_path))

        cleanup_paths = [
            str(item["path"])
            for item in requests
            if item["method"] == "DELETE"
            or item["path"] == "/api/v1/admin/users/101"
            and item["payload"] in ({"role": "user"}, {"status": "disabled"})
        ]
        self.assertIn(
            "/api/v1/admin/radar/rbac/role-bindings?actor_id=101",
            [str(item["path"]) for item in requests],
        )
        self.assertEqual(
            cleanup_paths,
            [
                f"/api/v1/admin/radar/rbac/role-bindings/{ROLE_BINDING_IDS['quality_admin']}",
                f"/api/v1/admin/radar/rbac/role-bindings/{ROLE_BINDING_IDS['platform_admin']}",
                "/api/v1/admin/users/101",
                "/api/v1/admin/users/101",
            ],
        )
        self.assertFalse(manifest_path.exists())

    def test_malformed_create_response_recovers_and_disables_temporary_administrator(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir, fixture_server(
            malformed_user_create_response=True,
        ) as (origin, requests):
            manifest_path = Path(temp_dir) / "fixture-manifest.json"
            with self.assertRaisesRegex(FixtureError, "response is missing id"):
                provision_fixture(self.config(origin, manifest_path))

        paths = [(item["method"], item["path"], item["payload"]) for item in requests]
        self.assertIn(
            ("GET", "/api/v1/admin/users?search=radar-quality-quality-run-001%40example.invalid&page=1&page_size=10", None),
            paths,
        )
        self.assertEqual(
            [
                (item["method"], item["path"], item["payload"])
                for item in requests
                if item["path"] == "/api/v1/admin/users/101"
            ],
            [
                ("PUT", "/api/v1/admin/users/101", {"role": "user"}),
                ("PUT", "/api/v1/admin/users/101", {"status": "disabled"}),
            ],
        )
        self.assertFalse(manifest_path.exists())

    def test_transient_report_not_found_is_retried_until_success(self) -> None:
        with (
            tempfile.TemporaryDirectory() as temp_dir,
            fixture_server(report_not_found_rounds=1) as (origin, requests),
        ):
            manifest_path = Path(temp_dir) / "fixture-manifest.json"
            manifest = provision_fixture(self.config(origin, manifest_path))

        self.assertEqual(set(manifest["scenarios"]), set(EXPECTED_SCENARIOS))
        for alias in ALIASES.values():
            path = f"/api/v1/radar/models/{alias}/quality-report"
            self.assertEqual(sum(item["path"] == path for item in requests), 3)

    def test_polling_deadline_bounds_the_whole_request_sequence(self) -> None:
        deadline_seconds = 0.20
        scheduling_margin_seconds = 0.03
        with tempfile.TemporaryDirectory() as temp_dir, fixture_server(
            poll_delay_seconds=0.12
        ) as (origin, requests):
            manifest_path = Path(temp_dir) / "fixture-manifest.json"
            with self.assertRaises(FixtureError):
                provision_fixture(
                    self.config(origin, manifest_path, timeout_seconds=deadline_seconds)
                )
            poll_started = next(
                item["captured_at"]
                for item in requests
                if item["path"] == "/api/v1/admin/radar/runs" and item["method"] == "GET"
            )
            poll_requests = [
                item
                for item in requests
                if item["method"] == "GET"
                and (
                    item["path"] == "/api/v1/admin/radar/runs"
                    or str(item["path"]).endswith("/quality-report")
                )
            ]

        poll_elapsed = max(float(item["captured_at"]) for item in poll_requests) - float(poll_started)
        self.assertLessEqual(poll_elapsed, deadline_seconds + scheduling_margin_seconds)
        self.assertEqual(
            [item["path"] for item in poll_requests],
            [
                "/api/v1/admin/radar/runs",
                "/api/v1/radar/models/radar-quality-healthy/quality-report",
            ],
        )
        self.assertFalse(manifest_path.exists())


if __name__ == "__main__":
    unittest.main()
