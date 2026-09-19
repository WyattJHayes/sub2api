from __future__ import annotations

import json
from typing import Any
from urllib.parse import urlsplit

from ..models import AssignmentLease, ExecutionEvidence
from .base import BaseExecutor, ProtocolError


def extract_final_output(parsed: Any) -> str | None:
    """Extract assistant text from Responses API and Chat Completions payloads."""
    output_text = parsed.get("output_text") if isinstance(parsed, dict) else None
    if isinstance(output_text, str) and output_text:
        return output_text

    if isinstance(parsed, dict):
        output = parsed.get("output")
        if isinstance(output, list):
            chunks: list[str] = []
            for item in output:
                if not isinstance(item, dict) or item.get("type") != "message":
                    continue
                content = item.get("content")
                if not isinstance(content, list):
                    continue
                for part in content:
                    if isinstance(part, dict) and part.get("type") in {"output_text", "text"}:
                        text = part.get("text")
                        if isinstance(text, str):
                            chunks.append(text)
            if chunks:
                return "".join(chunks)

        choices = parsed.get("choices")
        if isinstance(choices, list) and choices and isinstance(choices[0], dict):
            message = choices[0].get("message")
            if isinstance(message, dict):
                content = message.get("content")
                if isinstance(content, str):
                    return content
    return None


class OpenAIExecutor(BaseExecutor):
    request_parameter_keys = frozenset(
        {
            "temperature",
            "top_p",
            "top_k",
            "seed",
            "max_tokens",
            "max_output_tokens",
            "max_completion_tokens",
            "reasoning_effort",
            "reasoning",
            "response_format",
            "text",
            "stop",
            "tools",
            "tool_choice",
            "functions",
            "function_call",
            "tool_config",
            "parallel_tool_calls",
        }
    )

    async def execute(self, lease: AssignmentLease) -> ExecutionEvidence:
        spec = lease.case.execution_spec
        body = (
            dict(lease.case.prompt_spec or {})
            if isinstance(lease.case.prompt_spec, dict)
            else {"input": lease.case.prompt_spec}
        )
        endpoint = str(spec.get("url", "/v1/responses"))
        is_responses_endpoint = urlsplit(endpoint.strip()).path.rstrip("/").endswith("/responses")
        for key in self.request_parameter_keys & lease.route_config.keys():
            request_key = (
                "max_output_tokens" if key == "max_tokens" and is_responses_endpoint else key
            )
            actual = json.dumps(lease.route_config[key], sort_keys=True)
            expected = json.dumps(body.get(request_key), sort_keys=True)
            if request_key not in body or actual != expected:
                raise ProtocolError(
                    "unfrozen_request_parameters",
                    "Route request parameters must match the frozen case prompt_spec",
                )
        body["model"] = self.gateway_model(lease)
        headers = {
            "Authorization": f"Bearer {lease.gateway_api_key}",
            "X-Sub2API-Evaluation-Token": lease.gateway_evaluation_token,
            "Content-Type": "application/json",
        }
        response = await self.request(lease, url=endpoint, body=body, headers=headers)
        if response.status_code >= 400:
            raise ProtocolError(
                f"upstream_{response.status_code}", "OpenAI endpoint returned an error"
            )
        final_output = None
        try:
            parsed: Any = json.loads(response.body)
            final_output = extract_final_output(parsed)
        except (ValueError, IndexError, AttributeError, TypeError) as exc:
            raise ProtocolError(
                "malformed_openai_response", "OpenAI response could not be parsed"
            ) from exc
        return self.evidence_from_response(lease, body, response, final_output=final_output)
