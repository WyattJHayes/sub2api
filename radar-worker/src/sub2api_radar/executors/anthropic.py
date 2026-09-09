from __future__ import annotations

import json
from typing import Any

from ..models import AssignmentLease, ExecutionEvidence
from .base import BaseExecutor, ProtocolError


class AnthropicExecutor(BaseExecutor):
    async def execute(self, lease: AssignmentLease) -> ExecutionEvidence:
        spec = lease.case.execution_spec
        body: dict[str, Any] = (
            dict(lease.case.prompt_spec or {})
            if isinstance(lease.case.prompt_spec, dict)
            else {"messages": [{"role": "user", "content": str(lease.case.prompt_spec or "")}]}
        )
        body["model"] = self.gateway_model(lease)
        response = await self.request(
            lease,
            url=str(spec.get("url", "/v1/messages")),
            body=body,
            headers={
                "x-api-key": lease.gateway_api_key,
                "X-Sub2API-Evaluation-Token": lease.gateway_evaluation_token,
                "anthropic-version": "2023-06-01",
                "content-type": "application/json",
            },
        )
        if response.status_code >= 400:
            raise ProtocolError(
                f"upstream_{response.status_code}", "Anthropic endpoint returned an error"
            )
        try:
            parsed = json.loads(response.body)
            content = parsed.get("content", [])
            final_output = "".join(
                str(item.get("text", "")) for item in content if item.get("type") == "text"
            )
        except (ValueError, TypeError, AttributeError) as exc:
            raise ProtocolError(
                "malformed_anthropic_response", "Anthropic response could not be parsed"
            ) from exc
        return self.evidence_from_response(
            lease,
            body,
            response,
            final_output=final_output,
            finish_reason=parsed.get("stop_reason"),
        )
