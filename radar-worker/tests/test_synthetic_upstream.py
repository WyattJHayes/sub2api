from __future__ import annotations

from threading import Thread

import httpx
import pytest

from sub2api_radar.synthetic_upstream import create_server, scenario_output

EXPECTED_MODEL_DIMENSION_OUTPUTS = {
    "radar-synthetic-baseline": {
        "knowledge_freshness": "Paris",
        "model_fingerprint": "Paris",
        "reasoning_stability": "Paris",
        "structure_compliance": "Paris",
        "parameter_fidelity": "Paris",
        "instruction_hierarchy": "Paris",
        "protocol_schema": "Paris",
        "stream_completeness": "Paris",
    },
    "radar-synthetic-healthy": {
        "knowledge_freshness": "Paris",
        "model_fingerprint": "Paris",
        "reasoning_stability": "Paris",
        "structure_compliance": "Paris",
        "parameter_fidelity": "Paris",
        "instruction_hierarchy": "Paris",
        "protocol_schema": "Paris",
        "stream_completeness": "Paris",
    },
    "radar-synthetic-watered": {
        "knowledge_freshness": "Paris",
        "model_fingerprint": "Lyon",
        "reasoning_stability": "Paris",
        "structure_compliance": "Lyon",
        "parameter_fidelity": "Paris",
        "instruction_hierarchy": "Paris",
        "protocol_schema": "Paris",
        "stream_completeness": "Paris",
    },
    "radar-synthetic-degraded": {
        "knowledge_freshness": "Lyon",
        "model_fingerprint": "Paris",
        "reasoning_stability": "Paris",
        "structure_compliance": "Paris",
        "parameter_fidelity": "Paris",
        "instruction_hierarchy": "Lyon",
        "protocol_schema": "Paris",
        "stream_completeness": "Paris",
    },
    "radar-synthetic-candidate": {
        "knowledge_freshness": "Lyon",
        "model_fingerprint": "Lyon",
        "reasoning_stability": "Lyon",
        "structure_compliance": "Lyon",
        "parameter_fidelity": "Lyon",
        "instruction_hierarchy": "Lyon",
        "protocol_schema": "Lyon",
        "stream_completeness": "Lyon",
    },
}


@pytest.mark.parametrize(
    ("model", "dimension", "expected"),
    tuple(
        (model, dimension, expected)
        for model, dimensions in EXPECTED_MODEL_DIMENSION_OUTPUTS.items()
        for dimension, expected in dimensions.items()
    ),
)
def test_complete_five_model_by_eight_dimension_matrix(
    model: str,
    dimension: str,
    expected: str,
) -> None:
    messages = [{"role": "user", "content": f"RADAR_QUALITY_DIMENSION={dimension}"}]

    assert scenario_output(model, messages) == expected


@pytest.mark.parametrize(
    ("model", "dimension", "expected"),
    (
        ("radar-synthetic-baseline", "model_fingerprint", "Paris"),
        ("radar-synthetic-healthy", "model_fingerprint", "Paris"),
        ("radar-synthetic-watered", "model_fingerprint", "Lyon"),
        ("radar-synthetic-watered", "knowledge_freshness", "Paris"),
        ("radar-synthetic-degraded", "knowledge_freshness", "Lyon"),
        ("radar-synthetic-degraded", "structure_compliance", "Paris"),
    ),
)
def test_quality_scenario_output(model: str, dimension: str, expected: str) -> None:
    messages = [
        {
            "role": "user",
            "content": f"RADAR_QUALITY_DIMENSION={dimension}\nReturn the reference answer",
        }
    ]

    assert scenario_output(model, messages) == expected


def test_scenario_output_rejects_unknown_model() -> None:
    messages = [{"role": "user", "content": "RADAR_QUALITY_DIMENSION=model_fingerprint"}]

    with pytest.raises(ValueError, match="unsupported synthetic quality request"):
        scenario_output("radar-synthetic-unknown", messages)


def test_scenario_output_rejects_malformed_marker() -> None:
    messages = [{"role": "user", "content": "Return the reference answer"}]

    with pytest.raises(ValueError, match="synthetic quality dimension marker is required"):
        scenario_output("radar-synthetic-baseline", messages)


def test_controlled_malformed_request_returns_invalid_quality_fixture() -> None:
    server = create_server("127.0.0.1", 0, "test-key")
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        host, port = server.server_address
        response = httpx.post(
            f"http://{host}:{port}/v1/chat/completions",
            headers={"Authorization": "Bearer test-key"},
            json={
                "model": "radar-synthetic-baseline",
                "messages": [
                    {
                        "role": "user",
                        "content": "RADAR_QUALITY_DIMENSION=unknown_dimension",
                    }
                ],
            },
        )
    finally:
        server.shutdown()
        server.server_close()
        thread.join()

    assert response.status_code == 400
    assert response.json() == {"error": {"code": "invalid_quality_fixture"}}


def test_controlled_unknown_model_returns_invalid_quality_fixture() -> None:
    server = create_server("127.0.0.1", 0, "test-key")
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        host, port = server.server_address
        response = httpx.post(
            f"http://{host}:{port}/v1/chat/completions",
            headers={"Authorization": "Bearer test-key"},
            json={
                "model": "radar-synthetic-unknown",
                "messages": [
                    {
                        "role": "user",
                        "content": "RADAR_QUALITY_DIMENSION=model_fingerprint",
                    }
                ],
            },
        )
    finally:
        server.shutdown()
        server.server_close()
        thread.join()

    assert response.status_code == 400
    assert response.json() == {"error": {"code": "invalid_quality_fixture"}}
