from __future__ import annotations

import json
from urllib.parse import quote

from ..models import AssignmentLease, ExecutionEvidence
from .base import BaseExecutor, ProtocolError


class GeminiExecutor(BaseExecutor):
    async def execute(self, lease: AssignmentLease) -> ExecutionEvidence:
        spec = lease.case.execution_spec
        body = (
            dict(lease.case.prompt_spec or {})
            if isinstance(lease.case.prompt_spec, dict)
            else {"contents": [{"parts": [{"text": str(lease.case.prompt_spec or "")}]}]}
        )
        model = self.gateway_model(lease)
        endpoint_template = str(
            spec.get("url", "/v1beta/models/{model}:generateContent")
        )
        if "{model}" not in endpoint_template:
            raise ProtocolError(
                "invalid_gateway_path",
                "Gemini execution URL must contain the frozen {model} placeholder",
            )
        endpoint = endpoint_template.replace("{model}", quote(model, safe=""))
        response = await self.request(
            lease,
            url=endpoint,
            body=body,
            headers={
                "x-goog-api-key": lease.gateway_api_key,
                "X-Sub2API-Evaluation-Token": lease.gateway_evaluation_token,
                "content-type": "application/json",
            },
        )
        if response.status_code >= 400:
            raise ProtocolError(
                f"upstream_{response.status_code}", "Gemini endpoint returned an error"
            )
        try:
            parsed = json.loads(response.body)
            final_output = "".join(
                str(part.get("text", ""))
                for part in parsed["candidates"][0]["content"]["parts"]
                if "text" in part
            )
        except (ValueError, TypeError, AttributeError, KeyError, IndexError) as exc:
            raise ProtocolError(
                "malformed_gemini_response", "Gemini response could not be parsed"
            ) from exc
        return self.evidence_from_response(lease, body, response, final_output=final_output)
