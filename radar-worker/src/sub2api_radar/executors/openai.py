from __future__ import annotations

from typing import Any

from ..models import AssignmentLease, ExecutionEvidence
from .base import BaseExecutor, ProtocolError


class OpenAIExecutor(BaseExecutor):
    async def execute(self, lease: AssignmentLease) -> ExecutionEvidence:
        spec = lease.case.execution_spec
        body = (
            dict(lease.case.prompt_spec or {})
            if isinstance(lease.case.prompt_spec, dict)
            else {"input": lease.case.prompt_spec}
        )
        body["model"] = self.gateway_model(lease)
        headers = {
            "Authorization": f"Bearer {lease.gateway_api_key}",
            "X-Sub2API-Evaluation-Token": lease.gateway_evaluation_token,
            "Content-Type": "application/json",
        }
        endpoint = str(spec.get("url", "/v1/responses"))
        response = await self.request(lease, url=endpoint, body=body, headers=headers)
        if response.status_code >= 400:
            raise ProtocolError(
                f"upstream_{response.status_code}", "OpenAI endpoint returned an error"
            )
        final_output = None
        try:
            parsed: Any = __import__("json").loads(response.body)
            final_output = parsed.get("output_text") or parsed.get("choices", [{}])[0].get(
                "message", {}
            ).get("content")
        except (ValueError, IndexError, AttributeError, TypeError) as exc:
            raise ProtocolError(
                "malformed_openai_response", "OpenAI response could not be parsed"
            ) from exc
        return self.evidence_from_response(lease, body, response, final_output=final_output)
