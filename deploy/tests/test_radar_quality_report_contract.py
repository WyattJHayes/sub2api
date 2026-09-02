from __future__ import annotations

import json
import os
import stat
import subprocess
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from threading import Thread
from typing import Iterator

import pytest


REPO_ROOT = Path(__file__).resolve().parents[2]
E2E_SCRIPT = REPO_ROOT / "deploy" / "tests" / "radar-quality-report-e2e.sh"
QUALITY_DIMENSIONS = (
    "knowledge_freshness",
    "model_fingerprint",
    "reasoning_stability",
    "structure_compliance",
    "parameter_fidelity",
    "instruction_hierarchy",
    "protocol_schema",
    "stream_completeness",
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
SENSITIVE_KEY_VARIANTS = (
    ("credentials", 0),
    ("apiKey", 1),
    ("Password", 2),
    ("access-token", 0),
    ("prompt.spec", 1),
    ("completion_value", 2),
    ("routeTraceId", 0),
    ("accountId", 1),
    ("channel/id", 2),
    ("rawArtifact", 0),
    ("ProbeSpecHash", 1),
    ("observation", 2),
)
FORBIDDEN_FIELDS = (
    "route_trace_id",
    "prompt",
    "completion",
    "api_key",
    "account_ref",
    "channel_ref",
    "probe_spec_hash",
)


def valid_manifest() -> dict[str, object]:
    return {
        "schema_version": "radar-local-quality-fixture-v1",
        "run_identifier": "quality-contract-001",
        "fixture_user_email": "fixture@example.invalid",
        "setup_administrator_email": "setup@example.invalid",
        "route_snapshot_path": "/api/v1/admin/groups/7/composite-routes",
        "scenarios": {
            scenario: {
                "model_alias": alias,
                "run_id": f"run-{scenario}",
                "expected": dict(EXPECTED_SCENARIOS[scenario]),
            }
            for scenario, alias in ALIASES.items()
        },
    }


def write_manifest(path: Path, document: dict[str, object], mode: int = 0o600) -> None:
    path.write_text(json.dumps(document), encoding="utf-8")
    path.chmod(mode)


def synthetic_quality_report(scenario: str, *, mismatch: bool = False) -> dict[str, object]:
    expected = EXPECTED_SCENARIOS[scenario]
    overall = "observe" if mismatch and scenario == "watered" else expected["overall_conclusion"]
    return {
        "model_alias": ALIASES[scenario],
        "overall_conclusion": overall,
        "adulteration_risk": expected["adulteration_risk"],
        "degradation_risk": expected["degradation_risk"],
        "generated_at": "2026-08-11T00:00:00Z",
        "fresh_until": "2026-08-11T06:00:00Z",
        "dimension_results": [
            {
                "key": key,
                "score": 0.9,
                "status": expected["overall_conclusion"],
                "sample_count": 3,
                "confidence": 0.9,
                "checked_at": "2026-08-11T00:00:00Z",
                "evidence_code": "within_policy_bounds",
            }
            for key in QUALITY_DIMENSIONS
        ],
        "source_attribution": {
            "state": expected["source_state"],
            "display_name": "Radar Synthetic Reference",
            "evidence_code": "source_inferred",
        },
        "evidence": [{"code": "within_policy_bounds"}],
    }


@contextmanager
def synthetic_radar_server(
    *,
    mismatch: bool = False,
    sensitive_report_key: str | None = None,
    sensitive_report_depth: int = 0,
) -> Iterator[str]:
    class Handler(BaseHTTPRequestHandler):
        def do_GET(self) -> None:  # noqa: N802
            if self.path == "/api/v1/admin/groups/7/composite-routes":
                data: object = [{"id": "route-1", "model": "synthetic"}]
            elif self.path == "/api/v1/radar/health":
                data = [{"model_alias": alias} for alias in ALIASES.values()]
            else:
                prefix = "/api/v1/radar/models/"
                suffix = "/quality-report"
                if not self.path.startswith(prefix) or not self.path.endswith(suffix):
                    self.send_error(404)
                    return
                alias = self.path[len(prefix) : -len(suffix)]
                scenario = next((name for name, value in ALIASES.items() if value == alias), None)
                if scenario is None:
                    self.send_error(404)
                    return
                data = synthetic_quality_report(scenario, mismatch=mismatch)
                if sensitive_report_key is not None and scenario == "healthy":
                    sensitive: object = {sensitive_report_key: "redacted"}
                    for level in range(sensitive_report_depth):
                        sensitive = {f"safe_level_{level}": sensitive}
                    data["nested_evidence"] = sensitive
            body = json.dumps({"code": 0, "message": "success", "data": data}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, _format: str, *_args: object) -> None:
            return

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}"
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


def script_env(url: str, manifest: Path) -> dict[str, str]:
    return {
        **os.environ,
        "RADAR_QUALITY_STAGING_E2E": "1",
        "RADAR_QUALITY_STAGING_URL": url,
        "RADAR_QUALITY_FIXTURE_MANIFEST": str(manifest),
        "RADAR_QUALITY_STAGING_ADMIN_TOKEN": "admin-test-secret",
        "RADAR_QUALITY_STAGING_USER_TOKEN": "user-test-secret",
        "RADAR_QUALITY_STAGING_MAX_ATTEMPTS": "1",
        "RADAR_QUALITY_STAGING_POLL_SECONDS": "1",
    }


def test_four_scenario_contract_exposes_eight_dimensions_and_no_sensitive_fields() -> None:
    for scenario in EXPECTED_SCENARIOS:
        payload = synthetic_quality_report(scenario)
        assert payload["model_alias"] == ALIASES[scenario]
        assert payload["overall_conclusion"] == EXPECTED_SCENARIOS[scenario]["overall_conclusion"]
        dimensions = payload["dimension_results"]
        assert isinstance(dimensions, list)
        assert {item["key"] for item in dimensions if isinstance(item, dict)} == set(QUALITY_DIMENSIONS)
        assert len(dimensions) == 8
        source = payload["source_attribution"]
        assert isinstance(source, dict)
        assert source["state"] == EXPECTED_SCENARIOS[scenario]["source_state"]
        encoded = json.dumps(payload, sort_keys=True)
        for forbidden in FORBIDDEN_FIELDS:
            assert forbidden not in encoded


def test_staging_e2e_requires_a_mode_0600_manifest(tmp_path: Path) -> None:
    manifest = tmp_path / "fixture-manifest.json"
    write_manifest(manifest, valid_manifest(), mode=0o640)
    result = subprocess.run(
        [str(E2E_SCRIPT)],
        cwd=REPO_ROOT,
        env=script_env("http://127.0.0.1:1", manifest),
        capture_output=True,
        text=True,
        check=False,
    )

    assert result.returncode != 0
    assert "mode 0600" in result.stderr
    assert stat.S_IMODE(manifest.stat().st_mode) == 0o640


def test_staging_e2e_rejects_incomplete_scenarios_before_network(tmp_path: Path) -> None:
    manifest = tmp_path / "fixture-manifest.json"
    document = valid_manifest()
    del document["scenarios"]["degraded"]
    write_manifest(manifest, document)
    result = subprocess.run(
        [str(E2E_SCRIPT)],
        cwd=REPO_ROOT,
        env=script_env("http://127.0.0.1:1", manifest),
        capture_output=True,
        text=True,
        check=False,
    )

    assert result.returncode != 0
    assert "four exact scenarios" in result.stderr


def test_staging_e2e_rejects_extra_top_level_manifest_fields(tmp_path: Path) -> None:
    manifest = tmp_path / "fixture-manifest.json"
    document = valid_manifest()
    document["note"] = "unexpected"
    write_manifest(manifest, document)
    result = subprocess.run(
        [str(E2E_SCRIPT)],
        cwd=REPO_ROOT,
        env=script_env("http://127.0.0.1:1", manifest),
        capture_output=True,
        text=True,
        check=False,
    )

    assert result.returncode != 0
    assert "exact top-level schema" in result.stderr


@pytest.mark.parametrize(
    "route_snapshot_path",
    (
        "/api/v1/admin/groups/0/composite-routes",
        "/api/v1/admin/groups/7/composite-routes/extra",
        "https://example.invalid/api/v1/admin/groups/7/composite-routes",
    ),
)
def test_staging_e2e_rejects_invalid_manifest_route_snapshot_path_before_network(
    tmp_path: Path, route_snapshot_path: str
) -> None:
    manifest = tmp_path / "fixture-manifest.json"
    document = valid_manifest()
    document["route_snapshot_path"] = route_snapshot_path
    write_manifest(manifest, document)
    result = subprocess.run(
        [str(E2E_SCRIPT)],
        cwd=REPO_ROOT,
        env=script_env("http://127.0.0.1:1", manifest),
        capture_output=True,
        text=True,
        check=False,
    )

    assert result.returncode != 0
    assert "route snapshot path" in result.stderr


@pytest.mark.parametrize(("sensitive_key", "depth"), SENSITIVE_KEY_VARIANTS)
def test_staging_e2e_rejects_each_recursive_manifest_sensitive_key_family(
    tmp_path: Path, sensitive_key: str, depth: int
) -> None:
    manifest = tmp_path / "fixture-manifest.json"
    document = valid_manifest()
    sensitive: object = {sensitive_key: "redacted"}
    for level in range(depth):
        sensitive = {f"safe_level_{level}": sensitive}
    document["nested_evidence"] = sensitive
    write_manifest(manifest, document)
    result = subprocess.run(
        [str(E2E_SCRIPT)],
        cwd=REPO_ROOT,
        env=script_env("http://127.0.0.1:1", manifest),
        capture_output=True,
        text=True,
        check=False,
    )

    assert result.returncode != 0
    assert "forbidden evidence fields" in result.stderr


def test_staging_e2e_rejects_production_hosts_before_network_access(tmp_path: Path) -> None:
    manifest = tmp_path / "fixture-manifest.json"
    write_manifest(manifest, valid_manifest())
    result = subprocess.run(
        [str(E2E_SCRIPT)],
        cwd=REPO_ROOT,
        env=script_env("https://sub2api.weihub.cloud", manifest),
        capture_output=True,
        text=True,
        check=False,
    )

    assert result.returncode != 0
    assert "HTTP loopback" in result.stderr


def test_staging_e2e_validates_all_scenarios_and_preserves_routes(tmp_path: Path) -> None:
    manifest = tmp_path / "fixture-manifest.json"
    write_manifest(manifest, valid_manifest())
    with synthetic_radar_server() as url:
        result = subprocess.run(
            [str(E2E_SCRIPT)],
            cwd=REPO_ROOT,
            env=script_env(url, manifest),
            capture_output=True,
            text=True,
            check=False,
        )

    assert result.returncode == 0, result.stderr
    assert "four fixture scenarios" in result.stdout


def test_staging_e2e_fails_closed_on_conclusion_mismatch(tmp_path: Path) -> None:
    manifest = tmp_path / "fixture-manifest.json"
    write_manifest(manifest, valid_manifest())
    with synthetic_radar_server(mismatch=True) as url:
        result = subprocess.run(
            [str(E2E_SCRIPT)],
            cwd=REPO_ROOT,
            env=script_env(url, manifest),
            capture_output=True,
            text=True,
            check=False,
        )

    assert result.returncode != 0
    assert "does not match fixture manifest" in result.stderr


@pytest.mark.parametrize(("sensitive_key", "depth"), SENSITIVE_KEY_VARIANTS)
def test_staging_e2e_rejects_each_recursive_public_report_sensitive_key_family(
    tmp_path: Path, sensitive_key: str, depth: int
) -> None:
    manifest = tmp_path / "fixture-manifest.json"
    write_manifest(manifest, valid_manifest())
    with synthetic_radar_server(
        sensitive_report_key=sensitive_key,
        sensitive_report_depth=depth,
    ) as url:
        result = subprocess.run(
            [str(E2E_SCRIPT)],
            cwd=REPO_ROOT,
            env=script_env(url, manifest),
            capture_output=True,
            text=True,
            check=False,
        )

    assert result.returncode != 0
    assert "forbidden field" in result.stderr
