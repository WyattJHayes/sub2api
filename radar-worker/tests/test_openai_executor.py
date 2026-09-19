from __future__ import annotations

import json
from datetime import UTC, datetime, timedelta
from uuid import uuid4

import httpx
import pytest

from sub2api_radar.executors.base import ProtocolError
from sub2api_radar.executors.openai import OpenAIExecutor, extract_final_output
from sub2api_radar.models import AssignmentLease, CaseSpec


def responses_lease() -> AssignmentLease:
    return AssignmentLease(
        id=uuid4(),
        sample_id=uuid4(),
        run_id=uuid4(),
        case=CaseSpec(
            case_id=uuid4(),
            case_key="responses-output-text",
            capability_domain="reasoning",
            priority="P1",
            weight=1,
            prompt_spec={"input": "Return answer"},
            expected_spec="answer",
            execution_spec={"url": "/v1/responses"},
            grader_id="exact",
            grader_version="v1",
            content_sha256="a" * 64,
            confidentiality="public",
        ),
        model_route="gpt-test",
        model_config={"execution_image_digest": "worker@sha256:" + "b" * 64},
        attempt=1,
        lease_token="lease-token-123456",
        lease_expires_at=datetime.now(UTC) + timedelta(minutes=1),
        gateway_api_key="gateway-key",
        gateway_evaluation_token="gateway-token",
        route_trace_id="trace-1",
    )


@pytest.mark.asyncio
async def test_openai_executor_extracts_native_responses_output_text() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        assert request.url.path == "/v1/responses"
        return httpx.Response(
            200,
            headers={"content-type": "application/json"},
            json={
                "id": "resp_test",
                "object": "response",
                "status": "completed",
                "output": [
                    {
                        "id": "msg_test",
                        "type": "message",
                        "status": "completed",
                        "role": "assistant",
                        "content": [
                            {
                                "type": "output_text",
                                "annotations": [],
                                "text": "answer",
                            }
                        ],
                    }
                ],
            },
        )

    async with httpx.AsyncClient(
        transport=httpx.MockTransport(handler), base_url="https://gateway.example.test"
    ) as client:
        evidence = await OpenAIExecutor(client).execute(responses_lease())

    assert evidence.transport_status == "200"
    assert evidence.final_output == "answer"


def test_extract_final_output_supports_responses_message_content() -> None:
    assert extract_final_output(
        {
            "output": [
                {"type": "reasoning", "summary": []},
                {
                    "type": "message",
                    "content": [
                        {"type": "output_text", "text": "first"},
                        {"type": "output_text", "text": " second"},
                    ],
                },
            ]
        }
    ) == "first second"


@pytest.mark.asyncio
@pytest.mark.parametrize("config", [{"max_tokens": 256}, {"temperature": 0}, {"top_p": None}])
async def test_openai_executor_rejects_unfrozen_parameters_before_http(config: dict) -> None:
    lease = responses_lease()
    lease = lease.model_copy(update={
        "case": lease.case.model_copy(
            update={"prompt_spec": {"input": "Return answer", "max_output_tokens": 64}}
        ),
        "route_config": {**lease.route_config, **config},
    })
    requests: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        return httpx.Response(200, json={"output_text": "answer"})

    async with httpx.AsyncClient(
        transport=httpx.MockTransport(handler), base_url="https://gateway.example.test"
    ) as client:
        with pytest.raises(ProtocolError) as error:
            await OpenAIExecutor(client).execute(lease)
    assert error.value.code == "unfrozen_request_parameters"
    assert requests == []


@pytest.mark.asyncio
@pytest.mark.parametrize("endpoint", ["/v1/responses", "/v1/responses?trace=1", "/v1/responses/"])
async def test_openai_executor_uses_case_output_limit_without_mutation(endpoint: str) -> None:
    lease = responses_lease()
    original = {"input": "Return answer", "max_output_tokens": 64}
    lease = lease.model_copy(update={
        "case": lease.case.model_copy(update={
            "prompt_spec": original.copy(),
            "execution_spec": {"url": endpoint},
        }),
        "route_config": {**lease.route_config, "route": "gpt-6-astra", "max_tokens": 64},
    })
    requests: list[dict] = []

    def handler(request: httpx.Request) -> httpx.Response:
        requests.append(json.loads(request.content))
        return httpx.Response(200, json={"output_text": "answer"})

    async with httpx.AsyncClient(
        transport=httpx.MockTransport(handler), base_url="https://gateway.example.test"
    ) as client:
        evidence = await OpenAIExecutor(client).execute(lease)
    assert evidence.final_output == "answer"
    assert requests == [{"input": "Return answer", "max_output_tokens": 64, "model": "gpt-6-astra"}]
    assert lease.case.prompt_spec == original


@pytest.mark.asyncio
@pytest.mark.parametrize("config_value,frozen_value", [(True, 1), (0, False), (0, 0.0)])
async def test_openai_executor_rejects_parameter_type_mismatch(
    config_value: object, frozen_value: object
) -> None:
    lease = responses_lease()
    lease = lease.model_copy(update={
        "case": lease.case.model_copy(
            update={"prompt_spec": {"input": "Return answer", "seed": frozen_value}}
        ),
        "route_config": {**lease.route_config, "seed": config_value},
    })
    requests: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        return httpx.Response(200, json={"output_text": "answer"})

    async with httpx.AsyncClient(
        transport=httpx.MockTransport(handler), base_url="https://gateway.example.test"
    ) as client:
        with pytest.raises(ProtocolError) as error:
            await OpenAIExecutor(client).execute(lease)
    assert error.value.code == "unfrozen_request_parameters"
    assert requests == []
