from __future__ import annotations

import json
import os
import stat
import tempfile
import threading
import unittest
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import deploy.radar.local_prerelease_closure as closure_module

from deploy.radar.local_prerelease_closure import (
    ClosureError,
    REQUIRED_GATES,
    audit_closure,
    close_evidence,
    register_workers,
    verify_clock_sanity,
    write_gate,
    write_runtime_environment,
)


def passing_bindings() -> dict[str, object]:
    digest = "sha256:" + "a" * 64
    return {
        "run_id": "task7-test-rehearsal",
        "source_sha256": "b" * 64,
        "backup_sha256": "c" * 64,
        "control_plane_digest": digest,
        "worker_digest": "sha256:" + "d" * 64,
        "rollback_control_plane_digest": "sha256:" + "e" * 64,
        "rollback_worker_digest": "sha256:" + "f" * 64,
        "policy_version": "quality-v1",
        "fixture_version": "local-quality-fixture-v1",
        "dependency_digests": ["sha256:" + "1" * 64],
        "environment_fingerprint": "2" * 64,
    }


def gate_document(gate: str, bindings: dict[str, object] | None = None) -> dict[str, object]:
    checksum = "9" * 64
    checks: dict[str, object]
    summary: dict[str, object]
    artifacts: list[str]
    if gate == REQUIRED_GATES[0]:
        checks = {
            "exit_code": 0,
            "preflight_passed": True,
            "backend_tests_passed": True,
            "worker_tests_passed": True,
            "frontend_checks_passed": True,
            "frontend_build_passed": True,
        }
        summary = {"result": "passed", "immutable_inputs_bound": True, "code_checks_passed": True}
        artifacts = []
    elif gate == REQUIRED_GATES[1]:
        checks = {"exit_code": 0, "migration_validator_passed": True}
        summary = {
            "result": "passed",
            "migration_224_checksum": checksum,
            "migration_225_checksum": "8" * 64,
            "expected_schema_migrations": 307,
            "baseline_schema_migrations": 285,
            "actual_migration_count": 307,
            "migration_ledger_ok": True,
            "candidate_pending_migrations": [],
            "legacy_entries": [
                "202_add_radar_tracked_models.sql",
                "207_scope_radar_tracked_models_by_tenant.sql",
            ],
            "checksum_mismatches": [],
            "baseline_ledger_sha256": "1" * 64,
            "candidate_ledger_sha256": "2" * 64,
            "expected_candidate_ledger_sha256": "2" * 64,
            "expected_runtime_ledger_sha256": "3" * 64,
            "runtime_ledger_sha256": "3" * 64,
            "migration_224_semantics_ok": True,
            "migration_225_semantics_ok": True,
            "rollback_database_clone_used": True,
        }
        artifacts = ["private/gate-2-migration/summary.json"]
    elif gate == REQUIRED_GATES[2]:
        checks = {
            "exit_code": 0,
            "worker_registration_passed": True,
            "fixture_provisioned": True,
            "api_verifier_passed": True,
            "playwright_passed": True,
        }
        summary = {
            "result": "passed",
            "registrations": [
                {"identity": "reasoning-runner", "worker_kind": "runner", "capability": "reasoning"},
                {"identity": "exact-grader", "worker_kind": "grader", "capability": "exact"},
                {"identity": "reasoning-statistics", "worker_kind": "statistics", "capability": "reasoning"},
            ],
            "browser_workflows_passed": True,
            "api_verifier_passed": True,
        }
        artifacts = ["private/worker-registration.json"]
    elif gate == REQUIRED_GATES[3]:
        checks = {
            "exit_code": 0,
            "candidate_restart_passed": True,
            "browser_smoke_passed": True,
            "api_smoke_passed": True,
            "rollback_health_passed": True,
            "candidate_primary_restored": True,
            "repeated_quality_smoke_passed": True,
            "migration_replay_passed": True,
        }
        summary = {
            "result": "passed",
            "migration_224_checksum": checksum,
            "migration_225_checksum": "8" * 64,
            "expected_schema_migrations": 307,
            "baseline_schema_migrations": 285,
            "actual_migration_count": 307,
            "migration_ledger_ok": True,
            "candidate_pending_migrations": [],
            "legacy_entries": [
                "202_add_radar_tracked_models.sql",
                "207_scope_radar_tracked_models_by_tenant.sql",
            ],
            "checksum_mismatches": [],
            "baseline_ledger_sha256": "1" * 64,
            "candidate_ledger_sha256": "2" * 64,
            "expected_candidate_ledger_sha256": "2" * 64,
            "expected_runtime_ledger_sha256": "3" * 64,
            "runtime_ledger_sha256": "3" * 64,
            "migration_224_semantics_ok": True,
            "migration_225_semantics_ok": True,
            "rollback_database_clone_used": True,
            "migration_replay_matched": True,
            "primary_database_clone_used": True,
            "rollback_control_plane_clone_only": True,
        }
        artifacts = [
            "private/gate-4-primary-rollback/summary.json",
            "private/gate-4-migration/summary.json",
        ]
    else:
        checks = {"exit_code": 0, "prior_gate_count": 4}
        summary = {"result": "passed", "closure_input_validated": True}
        artifacts = []
    gate_offset = REQUIRED_GATES.index(gate) * 2
    return {
        "schema_version": "radar-local-prerelease-gate-v1",
        "gate": gate,
        "status": "passed",
        "run_id": "task7-test-rehearsal",
        "started_at": f"2026-08-11T00:00:{gate_offset:02d}Z",
        "finished_at": f"2026-08-11T00:00:{gate_offset + 1:02d}Z",
        "bindings": bindings or passing_bindings(),
        "checks": checks,
        "summary": summary,
        "artifacts": artifacts,
    }


def passing_bundle() -> dict[str, dict[str, object]]:
    return {gate: gate_document(gate) for gate in REQUIRED_GATES}


class LocalPrereleaseClosureTests(unittest.TestCase):
    def test_v01183_rejects_legacy_fixed_count_without_manifest_ledger(self) -> None:
        evidence = passing_bundle()
        for gate in (REQUIRED_GATES[1], REQUIRED_GATES[3]):
            summary = evidence[gate]["summary"]
            summary["actual_migration_count"] = 286  # type: ignore[index]
            for key in (
                "expected_schema_migrations",
                "migration_ledger_ok",
                "candidate_pending_migrations",
                "checksum_mismatches",
            ):
                summary.pop(key)  # type: ignore[union-attr]
        result = audit_closure(evidence, passing_bindings())

        self.assertIs(result["ok"], False, result)
        self.assertIn("expected migration count", " ".join(result["errors"]))

    def test_v01183_accepts_manifest_bound_migration_summary(self) -> None:
        result = audit_closure(passing_bundle(), passing_bindings())
        self.assertIs(result["ok"], True, result)

    def test_v01183_rejects_missing_or_inconsistent_ledger_identity(self) -> None:
        for field, value in (
            ("baseline_ledger_sha256", None),
            ("candidate_ledger_sha256", "4" * 64),
            ("expected_runtime_ledger_sha256", "5" * 64),
            ("legacy_entries", ["207_scope_radar_tracked_models_by_tenant.sql", "202_add_radar_tracked_models.sql"]),
        ):
            evidence = passing_bundle()
            for gate in (REQUIRED_GATES[1], REQUIRED_GATES[3]):
                if value is None:
                    evidence[gate]["summary"].pop(field)  # type: ignore[union-attr]
                else:
                    evidence[gate]["summary"][field] = value  # type: ignore[index]
            with self.subTest(field=field):
                self.assertIs(audit_closure(evidence, passing_bindings())["ok"], False)

    def test_v01176_requires_the_migration_225_gate(self) -> None:
        self.assertEqual(REQUIRED_GATES[1], "migration-225")

    def test_clock_sanity_requires_independent_utc_with_bounded_skew(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "clock-sanity.json"
            result = verify_clock_sanity(
                "2026-08-11T00:00:00Z",
                output,
                max_skew_seconds=300,
                observed_now=datetime(2026, 8, 11, 0, 0, 30, tzinfo=timezone.utc),
            )
            self.assertIs(result["passed"], True)
            self.assertEqual(result["absolute_skew_seconds"], 30)
            self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)
            with self.assertRaisesRegex(ClosureError, "clock skew exceeds"):
                verify_clock_sanity(
                    "2026-08-11T00:00:00Z",
                    output,
                    max_skew_seconds=300,
                    observed_now=datetime(2026, 8, 11, 0, 5, 1, tzinfo=timezone.utc),
                )

    def test_passing_bundle_requires_exact_canonical_bindings(self) -> None:
        evidence = passing_bundle()
        evidence[REQUIRED_GATES[2]]["bindings"] = dict(reversed(list(passing_bindings().items())))
        result = audit_closure(evidence, passing_bindings())
        self.assertIs(result["ok"], True)
        self.assertEqual(result["status"], "local-isolated-prerelease-passed")
        self.assertEqual(result["gate_count"], 5)

    def test_missing_gate_fails_closed(self) -> None:
        evidence = passing_bundle()
        evidence.pop("radar-browser-workflows")
        result = audit_closure(evidence, passing_bindings())
        self.assertIs(result["ok"], False)
        self.assertEqual(result["status"], "local-isolated-prerelease-failed")

    def test_duplicate_gate_fails_closed(self) -> None:
        documents = list(passing_bundle().values())
        documents.append(gate_document(REQUIRED_GATES[0]))
        result = audit_closure(documents, passing_bindings())
        self.assertIs(result["ok"], False)
        self.assertIn("duplicate", " ".join(result["errors"]))

    def test_malformed_inconsistent_and_failed_evidence_fail_closed(self) -> None:
        mutations = []
        malformed = passing_bundle()
        malformed[REQUIRED_GATES[0]] = []  # type: ignore[assignment]
        mutations.append(malformed)
        inconsistent = passing_bundle()
        inconsistent[REQUIRED_GATES[1]]["bindings"] = {**passing_bindings(), "policy_version": "other"}
        mutations.append(inconsistent)
        failed = passing_bundle()
        failed[REQUIRED_GATES[3]]["status"] = "failed"
        mutations.append(failed)
        wrong_run = passing_bundle()
        wrong_run[REQUIRED_GATES[4]]["run_id"] = "other-rehearsal"
        mutations.append(wrong_run)
        for evidence in mutations:
            with self.subTest(evidence=evidence):
                self.assertIs(audit_closure(evidence, passing_bindings())["ok"], False)

    def test_unknown_fields_and_forbidden_nested_keys_fail_closed(self) -> None:
        unknown = passing_bundle()
        unknown[REQUIRED_GATES[0]]["unexpected"] = "value"
        forbidden = passing_bundle()
        forbidden[REQUIRED_GATES[0]]["summary"] = {"nested": {"prompt": "secret"}}
        unknown_binding = passing_bundle()
        unknown_binding[REQUIRED_GATES[0]]["bindings"] = {**passing_bindings(), "extra": "value"}
        for evidence in (unknown, forbidden, unknown_binding):
            with self.subTest(evidence=evidence):
                self.assertIs(audit_closure(evidence, passing_bindings())["ok"], False)

    def test_passed_gate_requires_zero_exit_and_exact_gate_specific_facts(self) -> None:
        mutations: list[dict[str, dict[str, object]]] = []
        nonzero = passing_bundle()
        nonzero[REQUIRED_GATES[0]]["checks"]["exit_code"] = 99  # type: ignore[index]
        mutations.append(nonzero)
        false_semantics = passing_bundle()
        false_semantics[REQUIRED_GATES[1]]["summary"]["migration_224_semantics_ok"] = False  # type: ignore[index]
        mutations.append(false_semantics)
        false_pricing_semantics = passing_bundle()
        false_pricing_semantics[REQUIRED_GATES[1]]["summary"]["migration_225_semantics_ok"] = False  # type: ignore[index]
        mutations.append(false_pricing_semantics)
        false_clone = passing_bundle()
        false_clone[REQUIRED_GATES[1]]["summary"]["rollback_database_clone_used"] = False  # type: ignore[index]
        mutations.append(false_clone)
        missing_registration = passing_bundle()
        missing_registration[REQUIRED_GATES[2]]["summary"].pop("registrations")  # type: ignore[union-attr]
        mutations.append(missing_registration)
        false_browser = passing_bundle()
        false_browser[REQUIRED_GATES[2]]["checks"]["playwright_passed"] = False  # type: ignore[index]
        mutations.append(false_browser)
        false_restart = passing_bundle()
        false_restart[REQUIRED_GATES[3]]["checks"]["candidate_restart_passed"] = False  # type: ignore[index]
        mutations.append(false_restart)
        false_replay = passing_bundle()
        false_replay[REQUIRED_GATES[3]]["summary"]["migration_replay_matched"] = False  # type: ignore[index]
        mutations.append(false_replay)
        bad_gate5 = passing_bundle()
        bad_gate5[REQUIRED_GATES[4]]["checks"]["prior_gate_count"] = 5  # type: ignore[index]
        mutations.append(bad_gate5)
        for evidence in mutations:
            with self.subTest(evidence=evidence):
                self.assertIs(audit_closure(evidence, passing_bindings())["ok"], False)

    def test_gate2_and_gate4_migration_identity_must_match(self) -> None:
        for field, value in (
            ("migration_224_checksum", "7" * 64),
            ("migration_225_checksum", "9" * 64),
            ("actual_migration_count", 285),
        ):
            evidence = passing_bundle()
            evidence[REQUIRED_GATES[3]]["summary"][field] = value  # type: ignore[index]
            with self.subTest(field=field):
                self.assertIs(audit_closure(evidence, passing_bindings())["ok"], False)

    def test_gate_timestamps_require_rfc3339_utc_monotonic_order_and_bounded_future(self) -> None:
        now = datetime(2026, 8, 11, 0, 20, tzinfo=timezone.utc)
        mutations: list[dict[str, dict[str, object]]] = []
        malformed = passing_bundle()
        malformed[REQUIRED_GATES[0]]["started_at"] = "not-a-timestamp"
        mutations.append(malformed)
        offset_timezone = passing_bundle()
        offset_timezone[REQUIRED_GATES[0]]["started_at"] = "2026-08-11T00:00:00+00:00"
        mutations.append(offset_timezone)
        reversed_gate = passing_bundle()
        reversed_gate[REQUIRED_GATES[1]]["started_at"] = "2026-08-11T00:00:04Z"
        reversed_gate[REQUIRED_GATES[1]]["finished_at"] = "2026-08-11T00:00:03Z"
        mutations.append(reversed_gate)
        overlapping = passing_bundle()
        overlapping[REQUIRED_GATES[1]]["started_at"] = "2026-08-11T00:00:00Z"
        mutations.append(overlapping)
        future = passing_bundle()
        future[REQUIRED_GATES[4]]["finished_at"] = "2026-08-11T00:26:00Z"
        mutations.append(future)
        for evidence in mutations:
            with self.subTest(evidence=evidence):
                result = audit_closure(evidence, passing_bindings(), now=now)
                self.assertIs(result["ok"], False)

    def test_gate_bindings_reject_task6_metadata_field(self) -> None:
        evidence = passing_bundle()
        evidence[REQUIRED_GATES[0]]["bindings"] = {**passing_bindings(), "source_exclusions": {}}
        self.assertIs(audit_closure(evidence, {**passing_bindings(), "source_exclusions": {}})["ok"], False)

    def test_close_evidence_publishes_only_allowlisted_private_files(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            bindings_path = root / "bindings.json"
            bindings_path.write_text(json.dumps({**passing_bindings(), "source_exclusions": {"metadata": True}}))
            os.chmod(bindings_path, 0o600)
            for index, gate in enumerate(REQUIRED_GATES, start=1):
                name = f"gate-{index}-{'code' if index == 1 else 'closure-input' if index == 5 else 'evidence'}.json"
                if index == 2:
                    name = "gate-2-migration.json"
                elif index == 3:
                    name = "gate-3-radar.json"
                elif index == 4:
                    name = "gate-4-restart-rollback.json"
                (root / name).write_text(json.dumps(gate_document(gate)))
            result = close_evidence(root, bindings_path)
            self.assertIs(result["ok"], True)
            closure = json.loads((root / "public" / "closure.json").read_text())
            self.assertEqual(closure["status"], "local-isolated-prerelease-passed")
            self.assertNotIn("ok", closure)
            self.assertEqual(stat.S_IMODE((root / "public" / "closure.json").stat().st_mode), 0o600)
            self.assertEqual(stat.S_IMODE((root / "private").stat().st_mode), 0o700)

    def test_extra_gate_file_fails_and_fresh_public_replaces_old_files(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            bindings_path = root / "bindings.json"
            bindings_path.write_text(json.dumps({**passing_bindings(), "source_exclusions": {}}))
            for index, gate in enumerate(REQUIRED_GATES):
                (root / ("gate-1-code.json", "gate-2-migration.json", "gate-3-radar.json", "gate-4-restart-rollback.json", "gate-5-closure-input.json")[index]).write_text(json.dumps(gate_document(gate)))
            public = root / "public"
            public.mkdir()
            (public / "old-gate.json").write_text("stale")
            (root / "gate-6-unknown.json").write_text(json.dumps(gate_document(REQUIRED_GATES[0])))
            failed = close_evidence(root, bindings_path)
            self.assertIs(failed["ok"], False)
            self.assertEqual({path.name for path in public.iterdir()}, {"closure.json"})
            (root / "gate-6-unknown.json").unlink()
            passed = close_evidence(root, bindings_path)
            self.assertIs(passed["ok"], True)
            self.assertEqual({path.name for path in public.iterdir()}, {
                "closure.json", "gate-1-code.json", "gate-2-migration.json", "gate-3-radar.json",
                "gate-4-restart-rollback.json", "gate-5-closure-input.json",
            })

    def test_gate_summaries_capture_safe_migration_and_worker_order_evidence(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            bindings_path = root / "bindings.json"
            bindings_path.write_text(json.dumps(passing_bindings()))
            (root / "private" / "gate-2-migration").mkdir(parents=True)
            (root / "private" / "gate-2-migration" / "summary.json").write_text(json.dumps({
                "migration_224_checksum": "a" * 64,
                "migration_225_checksum": "b" * 64,
                "migration_count": 307,
                "baseline_schema_migrations": 285,
                "actual_schema_migrations": 307,
                "expected_schema_migrations": 307,
                "migration_ledger_ok": True,
                "candidate_pending_migrations": [],
                "legacy_entries": [
                    "202_add_radar_tracked_models.sql",
                    "207_scope_radar_tracked_models_by_tenant.sql",
                ],
                "checksum_mismatches": [],
                "baseline_ledger_sha256": "1" * 64,
                "candidate_ledger_sha256": "2" * 64,
                "expected_candidate_ledger_sha256": "2" * 64,
                "expected_runtime_ledger_sha256": "3" * 64,
                "runtime_ledger_sha256": "3" * 64,
                "migration_224_semantics_ok": True,
                "migration_225_semantics_ok": True,
                "rollback_database_clone_used": True,
            }))
            write_gate(
                root,
                bindings_path,
                REQUIRED_GATES[1],
                "passed",
                0,
                "2026-08-11T00:00:00Z",
                "2026-08-11T00:00:01Z",
            )
            migration = json.loads((root / "gate-2-migration.json").read_text())
            self.assertEqual(migration["summary"]["actual_migration_count"], 307)
            self.assertEqual(migration["summary"]["migration_224_checksum"], "a" * 64)
            self.assertEqual(migration["summary"]["migration_225_checksum"], "b" * 64)

            (root / "private" / "worker-registration.json").write_text(json.dumps({
                "registrations": [
                    {"identity": "reasoning-runner", "worker_kind": "runner", "capability": "reasoning"},
                    {"identity": "exact-grader", "worker_kind": "grader", "capability": "exact"},
                    {"identity": "reasoning-statistics", "worker_kind": "statistics", "capability": "reasoning"},
                ],
                "registration_order": ["reasoning-runner", "exact-grader", "reasoning-statistics"],
                "status": "passed",
            }))
            write_gate(
                root,
                bindings_path,
                REQUIRED_GATES[2],
                "passed",
                0,
                "2026-08-11T00:00:02Z",
                "2026-08-11T00:00:03Z",
            )
            workers = json.loads((root / "gate-3-radar.json").read_text())
            self.assertEqual(workers["summary"]["registrations"], [
                {"identity": "reasoning-runner", "worker_kind": "runner", "capability": "reasoning"},
                {"identity": "exact-grader", "worker_kind": "grader", "capability": "exact"},
                {"identity": "reasoning-statistics", "worker_kind": "statistics", "capability": "reasoning"},
            ])

    def test_gate4_requires_primary_compose_clone_before_fresh_migration_replay(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            bindings_path = root / "bindings.json"
            bindings_path.write_text(json.dumps(passing_bindings()))
            migration_directory = root / "private" / "gate-4-migration"
            migration_directory.mkdir(parents=True)
            (migration_directory / "summary.json").write_text(json.dumps({
                "migration_224_checksum": "a" * 64,
                "migration_225_checksum": "b" * 64,
                "migration_count": 307,
                "baseline_schema_migrations": 285,
                "actual_schema_migrations": 307,
                "expected_schema_migrations": 307,
                "migration_ledger_ok": True,
                "candidate_pending_migrations": [],
                "legacy_entries": [
                    "202_add_radar_tracked_models.sql",
                    "207_scope_radar_tracked_models_by_tenant.sql",
                ],
                "checksum_mismatches": [],
                "baseline_ledger_sha256": "1" * 64,
                "candidate_ledger_sha256": "2" * 64,
                "expected_candidate_ledger_sha256": "2" * 64,
                "expected_runtime_ledger_sha256": "3" * 64,
                "runtime_ledger_sha256": "3" * 64,
                "migration_224_semantics_ok": True,
                "migration_225_semantics_ok": True,
                "rollback_database_clone_used": True,
            }))
            primary_directory = root / "private" / "gate-4-primary-rollback"
            primary_directory.mkdir()
            (primary_directory / "summary.json").write_text(json.dumps({
                "schema_version": "radar-local-compose-rollback-v1",
                "primary_database_clone_used": True,
                "rollback_control_plane_clone_only": True,
                "rollback_health_passed": True,
            }))

            write_gate(
                root,
                bindings_path,
                REQUIRED_GATES[3],
                "passed",
                0,
                "2026-08-11T00:00:00Z",
                "2026-08-11T00:00:01Z",
            )

            gate = json.loads((root / "gate-4-restart-rollback.json").read_text())
            self.assertIs(gate["summary"]["primary_database_clone_used"], True)
            self.assertIs(gate["summary"]["rollback_control_plane_clone_only"], True)
            self.assertEqual(gate["artifacts"], [
                "private/gate-4-primary-rollback/summary.json",
                "private/gate-4-migration/summary.json",
            ])


class _WorkerHandler(BaseHTTPRequestHandler):
    requests: list[dict[str, object]] = []
    mismatch: str | None = None
    redirect_location: str | None = None
    oversized_login = False
    compliance_required = True
    compliance_status_failure = False
    compliance_accept_failure = False
    role_binding_failure = False

    def do_GET(self) -> None:  # noqa: N802
        self.__class__.requests.append({
            "method": "GET",
            "path": self.path,
            "payload": None,
            "authorization": self.headers.get("Authorization"),
        })
        if self.path != "/api/v1/admin/compliance":
            self.send_error(404)
            return
        if self.__class__.compliance_status_failure:
            self.send_error(500)
            return
        self._send_json({"data": {
            "required": self.__class__.compliance_required,
            "version": "v2026.06.10",
            "ack_phrase_en": (
                "I have read, understood, and agree to the Sub2API Deployment and "
                "Operation Compliance Commitment"
            ),
        }})

    def do_POST(self) -> None:  # noqa: N802
        size = int(self.headers.get("Content-Length", "0"))
        payload = json.loads(self.rfile.read(size) or b"{}")
        self.__class__.requests.append({
            "method": "POST",
            "path": self.path,
            "payload": payload,
            "authorization": self.headers.get("Authorization"),
        })
        if self.path == "/api/v1/auth/login":
            body = {"data": {"access_token": "local-test-access"}}
            status = 200
            if self.__class__.oversized_login:
                body["padding"] = "x" * (1024 * 1024)
        elif self.path == "/api/v1/admin/compliance/accept":
            if self.__class__.compliance_accept_failure:
                self.send_error(500)
                return
            expected = (
                "I have read, understood, and agree to the Sub2API Deployment and "
                "Operation Compliance Commitment"
            )
            if payload != {"language": "en", "phrase": expected}:
                self.send_error(400)
                return
            self.__class__.compliance_required = False
            body = {"data": {"required": False, "version": "v2026.06.10"}}
            status = 200
        elif self.path == "/api/v1/admin/radar/rbac/role-bindings":
            if self.__class__.role_binding_failure:
                self.send_error(403)
                return
            if payload not in (
                {"role": "platform_admin", "scope": {}},
                {"role": "test_operator", "scope": {}},
            ):
                self.send_error(400)
                return
            body = {"data": {
                "ID": "00000000-0000-4000-8000-000000000001",
                "ActorID": 1,
                "Role": payload["role"],
                "Scope": {},
                "Enabled": True,
                "CreatedBy": 1,
                "CreatedAt": "2026-08-12T00:00:00Z",
                "DisabledAt": None,
            }}
            status = 201
        else:
            if self.__class__.compliance_required:
                self.send_error(423)
                return
            if self.__class__.redirect_location is not None:
                self.send_response(302)
                self.send_header("Location", self.__class__.redirect_location)
                self.send_header("Content-Length", "0")
                self.end_headers()
                return
            capability = payload["capabilities"][0]
            if self.__class__.mismatch == "capability":
                capability = "wrong"
            max_concurrency: object = payload["max_concurrency"]
            if self.__class__.mismatch == "concurrency":
                max_concurrency = 2
            elif self.__class__.mismatch == "concurrency-bool":
                max_concurrency = True
            elif self.__class__.mismatch == "concurrency-float":
                max_concurrency = 1.0
            body = {"data": {
                "name": payload["name"],
                "worker_kind": payload["worker_kind"],
                "capabilities": [capability],
                "status": "active",
                "image_digest": "sha256:" + "0" * 64 if self.__class__.mismatch == "image" else payload["image_digest"],
                "region": "wrong" if self.__class__.mismatch == "region" else payload["region"],
                "max_concurrency": max_concurrency,
            }}
            status = 201
        self._send_json(body, status)

    def _send_json(self, body: dict[str, object], status: int = 200) -> None:
        encoded = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(encoded)))
        self.end_headers()
        try:
            self.wfile.write(encoded)
        except (BrokenPipeError, ConnectionResetError):
            return

    def log_message(self, format: str, *args: object) -> None:
        return


class WorkerRegistrationTests(unittest.TestCase):
    def setUp(self) -> None:
        _WorkerHandler.requests = []
        _WorkerHandler.mismatch = None
        _WorkerHandler.redirect_location = None
        _WorkerHandler.oversized_login = False
        _WorkerHandler.compliance_required = True
        _WorkerHandler.compliance_status_failure = False
        _WorkerHandler.compliance_accept_failure = False
        _WorkerHandler.role_binding_failure = False
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), _WorkerHandler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()

    def test_workers_register_before_fixture_in_required_order(self) -> None:
        origin = f"http://127.0.0.1:{self.server.server_port}"
        result = register_workers(
            origin,
            "task7-test-rehearsal",
            "sha256:" + "d" * 64,
            "radar-admin@staging.local",
            "admin-local-password",
            {
                "RADAR_RUNNER_WORKER_TOKEN": "r" * 32,
                "RADAR_GRADER_WORKER_TOKEN": "g" * 32,
                "RADAR_STATISTICS_WORKER_TOKEN": "s" * 32,
            },
        )
        paths = [(request["method"], request["path"]) for request in _WorkerHandler.requests]
        self.assertEqual(paths, [
            ("POST", "/api/v1/auth/login"),
            ("GET", "/api/v1/admin/compliance"),
            ("POST", "/api/v1/admin/compliance/accept"),
            ("POST", "/api/v1/admin/radar/rbac/role-bindings"),
            ("POST", "/api/v1/admin/radar/rbac/role-bindings"),
            ("POST", "/api/v1/admin/radar/workers"),
            ("POST", "/api/v1/admin/radar/workers"),
            ("POST", "/api/v1/admin/radar/workers"),
        ])
        payloads = [
            request["payload"]
            for request in _WorkerHandler.requests
            if request["path"] == "/api/v1/admin/radar/workers"
        ]
        self.assertEqual([(item["worker_kind"], item["capabilities"]) for item in payloads], [
            ("runner", ["reasoning"]),
            ("grader", ["exact"]),
            ("statistics", ["reasoning"]),
        ])
        self.assertEqual(result["registration_order"], ["reasoning-runner", "exact-grader", "reasoning-statistics"])
        serialized_result = json.dumps(result)
        self.assertNotIn("local-test-access", serialized_result)
        self.assertNotIn("I have read", serialized_result)
        self.assertNotIn("r" * 32, serialized_result)

    def test_authenticated_fixture_token_registers_workers_without_setup_admin_login(self) -> None:
        self.assertTrue(
            hasattr(closure_module, "register_workers_with_token"),
            "fixture-scoped Worker registration entry point is missing",
        )
        _WorkerHandler.compliance_required = False
        origin = f"http://127.0.0.1:{self.server.server_port}"
        result = closure_module.register_workers_with_token(
            origin,
            "task7-test-rehearsal",
            "sha256:" + "d" * 64,
            "fixture-tenant-access",
            {
                "RADAR_RUNNER_WORKER_TOKEN": "r" * 32,
                "RADAR_GRADER_WORKER_TOKEN": "g" * 32,
                "RADAR_STATISTICS_WORKER_TOKEN": "s" * 32,
            },
        )

        paths = [(request["method"], request["path"]) for request in _WorkerHandler.requests]
        self.assertEqual(paths, [
            ("POST", "/api/v1/admin/radar/workers"),
            ("POST", "/api/v1/admin/radar/workers"),
            ("POST", "/api/v1/admin/radar/workers"),
        ])
        self.assertTrue(all(
            request["authorization"] == "Bearer fixture-tenant-access"
            for request in _WorkerHandler.requests
        ))
        self.assertEqual(
            result["registration_order"],
            ["reasoning-runner", "exact-grader", "reasoning-statistics"],
        )

    def test_workers_skip_accept_when_compliance_is_current(self) -> None:
        _WorkerHandler.compliance_required = False
        origin = f"http://127.0.0.1:{self.server.server_port}"
        register_workers(
            origin,
            "task7-test-rehearsal",
            "sha256:" + "d" * 64,
            "radar-admin@staging.local",
            "admin-local-password",
            {
                "RADAR_RUNNER_WORKER_TOKEN": "r" * 32,
                "RADAR_GRADER_WORKER_TOKEN": "g" * 32,
                "RADAR_STATISTICS_WORKER_TOKEN": "s" * 32,
            },
        )
        paths = [(request["method"], request["path"]) for request in _WorkerHandler.requests]
        self.assertEqual(paths[:2], [
            ("POST", "/api/v1/auth/login"),
            ("GET", "/api/v1/admin/compliance"),
        ])
        self.assertNotIn(("POST", "/api/v1/admin/compliance/accept"), paths)

    def test_required_radar_roles_are_bound_before_worker_registration(self) -> None:
        origin = f"http://127.0.0.1:{self.server.server_port}"
        register_workers(
            origin,
            "task7-test-rehearsal",
            "sha256:" + "d" * 64,
            "radar-admin@staging.local",
            "admin-local-password",
            {
                "RADAR_RUNNER_WORKER_TOKEN": "r" * 32,
                "RADAR_GRADER_WORKER_TOKEN": "g" * 32,
                "RADAR_STATISTICS_WORKER_TOKEN": "s" * 32,
            },
        )
        roles = [
            request["payload"]["role"]
            for request in _WorkerHandler.requests
            if request["path"] == "/api/v1/admin/radar/rbac/role-bindings"
        ]
        self.assertEqual(roles, ["platform_admin", "test_operator"])
        first_worker = next(
            index for index, request in enumerate(_WorkerHandler.requests)
            if request["path"] == "/api/v1/admin/radar/workers"
        )
        last_role = max(
            index for index, request in enumerate(_WorkerHandler.requests)
            if request["path"] == "/api/v1/admin/radar/rbac/role-bindings"
        )
        self.assertLess(last_role, first_worker)

    def test_role_binding_failure_prevents_worker_registration(self) -> None:
        _WorkerHandler.role_binding_failure = True
        origin = f"http://127.0.0.1:{self.server.server_port}"
        with self.assertRaises(ClosureError):
            register_workers(
                origin,
                "task7-test-rehearsal",
                "sha256:" + "d" * 64,
                "radar-admin@staging.local",
                "admin-local-password",
                {
                    "RADAR_RUNNER_WORKER_TOKEN": "r" * 32,
                    "RADAR_GRADER_WORKER_TOKEN": "g" * 32,
                    "RADAR_STATISTICS_WORKER_TOKEN": "s" * 32,
                },
            )
        self.assertFalse(any(
            request["path"] == "/api/v1/admin/radar/workers"
            for request in _WorkerHandler.requests
        ))

    def test_compliance_status_failure_prevents_worker_registration(self) -> None:
        _WorkerHandler.compliance_status_failure = True
        origin = f"http://127.0.0.1:{self.server.server_port}"
        with self.assertRaises(ClosureError):
            register_workers(
                origin,
                "task7-test-rehearsal",
                "sha256:" + "d" * 64,
                "radar-admin@staging.local",
                "admin-local-password",
                {
                    "RADAR_RUNNER_WORKER_TOKEN": "r" * 32,
                    "RADAR_GRADER_WORKER_TOKEN": "g" * 32,
                    "RADAR_STATISTICS_WORKER_TOKEN": "s" * 32,
                },
            )
        self.assertFalse(any(
            request["path"] == "/api/v1/admin/radar/workers"
            for request in _WorkerHandler.requests
        ))

    def test_compliance_accept_failure_prevents_worker_registration(self) -> None:
        _WorkerHandler.compliance_accept_failure = True
        origin = f"http://127.0.0.1:{self.server.server_port}"
        with self.assertRaises(ClosureError):
            register_workers(
                origin,
                "task7-test-rehearsal",
                "sha256:" + "d" * 64,
                "radar-admin@staging.local",
                "admin-local-password",
                {
                    "RADAR_RUNNER_WORKER_TOKEN": "r" * 32,
                    "RADAR_GRADER_WORKER_TOKEN": "g" * 32,
                    "RADAR_STATISTICS_WORKER_TOKEN": "s" * 32,
                },
            )
        self.assertFalse(any(
            request["path"] == "/api/v1/admin/radar/workers"
            for request in _WorkerHandler.requests
        ))

    def test_capability_mismatch_fails_closed(self) -> None:
        _WorkerHandler.mismatch = "capability"
        origin = f"http://127.0.0.1:{self.server.server_port}"
        with self.assertRaises(ClosureError):
            register_workers(
                origin,
                "task7-test-rehearsal",
                "sha256:" + "d" * 64,
                "radar-admin@staging.local",
                "admin-local-password",
                {
                    "RADAR_RUNNER_WORKER_TOKEN": "r" * 32,
                    "RADAR_GRADER_WORKER_TOKEN": "g" * 32,
                    "RADAR_STATISTICS_WORKER_TOKEN": "s" * 32,
                },
            )

    def test_worker_image_region_and_concurrency_mismatch_fail_closed(self) -> None:
        origin = f"http://127.0.0.1:{self.server.server_port}"
        for mismatch in ("image", "region", "concurrency"):
            _WorkerHandler.mismatch = mismatch
            with self.subTest(mismatch=mismatch), self.assertRaises(ClosureError):
                register_workers(
                    origin,
                    "task7-test-rehearsal",
                    "sha256:" + "d" * 64,
                    "radar-admin@staging.local",
                    "admin-local-password",
                    {
                        "RADAR_RUNNER_WORKER_TOKEN": "r" * 32,
                        "RADAR_GRADER_WORKER_TOKEN": "g" * 32,
                        "RADAR_STATISTICS_WORKER_TOKEN": "s" * 32,
                    },
                )

    def test_worker_concurrency_requires_json_integer_one(self) -> None:
        origin = f"http://127.0.0.1:{self.server.server_port}"
        for mismatch in ("concurrency-bool", "concurrency-float"):
            _WorkerHandler.mismatch = mismatch
            with self.subTest(mismatch=mismatch), self.assertRaises(ClosureError):
                register_workers(
                    origin,
                    "task7-test-rehearsal",
                    "sha256:" + "d" * 64,
                    "radar-admin@staging.local",
                    "admin-local-password",
                    {
                        "RADAR_RUNNER_WORKER_TOKEN": "r" * 32,
                        "RADAR_GRADER_WORKER_TOKEN": "g" * 32,
                        "RADAR_STATISTICS_WORKER_TOKEN": "s" * 32,
                    },
                )

    def test_worker_registration_rejects_redirect_without_forwarding_bearer(self) -> None:
        redirected_requests: list[dict[str, object]] = []

        class RedirectTargetHandler(BaseHTTPRequestHandler):
            def do_GET(self) -> None:  # noqa: N802
                redirected_requests.append({
                    "path": self.path,
                    "authorization": self.headers.get("Authorization"),
                })
                body = json.dumps({"data": {}}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, format: str, *args: object) -> None:
                return

        redirect_server = ThreadingHTTPServer(("127.0.0.1", 0), RedirectTargetHandler)
        redirect_thread = threading.Thread(target=redirect_server.serve_forever, daemon=True)
        redirect_thread.start()
        try:
            _WorkerHandler.redirect_location = (
                f"http://127.0.0.1:{redirect_server.server_port}/redirected"
            )
            origin = f"http://127.0.0.1:{self.server.server_port}"
            with self.assertRaises(ClosureError):
                register_workers(
                    origin,
                    "task7-test-rehearsal",
                    "sha256:" + "d" * 64,
                    "radar-admin@staging.local",
                    "admin-local-password",
                    {
                        "RADAR_RUNNER_WORKER_TOKEN": "r" * 32,
                        "RADAR_GRADER_WORKER_TOKEN": "g" * 32,
                        "RADAR_STATISTICS_WORKER_TOKEN": "s" * 32,
                    },
                )
        finally:
            redirect_server.shutdown()
            redirect_server.server_close()
            redirect_thread.join()
        self.assertEqual(redirected_requests, [])

    def test_worker_registration_rejects_response_larger_than_one_mib(self) -> None:
        _WorkerHandler.oversized_login = True
        origin = f"http://127.0.0.1:{self.server.server_port}"
        with self.assertRaisesRegex(ClosureError, "response exceeds 1 MiB"):
            register_workers(
                origin,
                "task7-test-rehearsal",
                "sha256:" + "d" * 64,
                "radar-admin@staging.local",
                "admin-local-password",
                {
                    "RADAR_RUNNER_WORKER_TOKEN": "r" * 32,
                    "RADAR_GRADER_WORKER_TOKEN": "g" * 32,
                    "RADAR_STATISTICS_WORKER_TOKEN": "s" * 32,
                },
            )

    def test_runtime_authentication_is_written_only_to_mode_0600_private_environment(self) -> None:
        origin = f"http://127.0.0.1:{self.server.server_port}"
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "runtime.env"
            write_runtime_environment(
                origin,
                "task7-test-rehearsal",
                "radar-admin@staging.local",
                "admin-local-password",
                "fixture-local-password",
                output,
            )
            self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)
            values = dict(line.split("=", 1) for line in output.read_text().splitlines())
            self.assertEqual(values["RADAR_QUALITY_STAGING_ADMIN_TOKEN"], "local-test-access")
            self.assertEqual(values["RADAR_QUALITY_STAGING_USER_TOKEN"], "local-test-access")
            self.assertEqual(values["RADAR_E2E_USER_EMAIL"], "radar-quality-task7-test-rehearsal@example.invalid")


if __name__ == "__main__":
    unittest.main()
