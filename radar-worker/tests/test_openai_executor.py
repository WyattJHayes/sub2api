from __future__ import annotations

from datetime import UTC, datetime, timedelta
from uuid import uuid4

import httpx
import pytest

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
