from __future__ import annotations

import hashlib
import json
import time
from collections.abc import Mapping
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from typing import Any
from urllib.parse import urlsplit, urlunsplit

import httpx

from ..models import AssignmentLease, ExecutionEvidence
from ..observability import traceparent_for
from ..runner import environment_fingerprint


@dataclass(frozen=True)
class ProtocolResponse:
    status_code: int
    headers: dict[str, str]
    body: bytes
    events: tuple[dict[str, Any], ...]
    ttft_ms: int | None
    latency_ms: int


class ProtocolError(RuntimeError):
    def __init__(self, code: str, message: str) -> None:
        super().__init__(message)
        self.code = code


class BaseExecutor:
    allowed_response_headers = frozenset(
        {
            "content-type",
            "x-request-id",
            "openai-request-id",
            "request-id",
            "retry-after",
            "server-timing",
        }
    )

    def __init__(
        self,
        client: httpx.AsyncClient,
        *,
        max_response_bytes: int = 8 * 1024 * 1024,
        max_events: int = 10_000,
    ) -> None:
        self.client = client
        self.max_response_bytes = max_response_bytes
        self.max_events = max_events

    @staticmethod
    def gateway_model(lease: AssignmentLease) -> str:
        for key in ("route", "model", "model_route", "id"):
            value = lease.route_config.get(key)
            if isinstance(value, str) and value.strip():
                return value.strip()
        return lease.model_route

    @staticmethod
    def gateway_path(value: str) -> str:
        parsed = urlsplit(value.strip())
        if (
            parsed.scheme
            or parsed.netloc
            or not parsed.path.startswith("/")
            or parsed.path.startswith("//")
            or parsed.fragment
        ):
            raise ProtocolError(
                "invalid_gateway_path",
                "execution URL must be a relative gateway path",
            )
        return urlunsplit(("", "", parsed.path, parsed.query, ""))

    async def request(
        self,
        lease: AssignmentLease,
        *,
        url: str,
        body: Mapping[str, Any],
        headers: Mapping[str, str],
    ) -> ProtocolResponse:
        started = time.monotonic()
        raw_request = json.dumps(body, ensure_ascii=False, separators=(",", ":")).encode()
        request_headers = dict(headers)
        request_headers.setdefault("traceparent", traceparent_for(lease.route_trace_id))
        response = await self.client.post(
            self.gateway_path(url), content=raw_request, headers=request_headers
        )
        body_bytes = response.content
        if len(body_bytes) > self.max_response_bytes:
            raise ProtocolError("response_too_large", "upstream response exceeded the byte limit")
        events, ttft = (
            parse_sse(body_bytes, self.max_events)
            if "text/event-stream" in response.headers.get("content-type", "")
            else ((), None)
        )
        return ProtocolResponse(
            response.status_code,
            {
                key.lower(): value
                for key, value in response.headers.items()
                if key.lower() in self.allowed_response_headers
            },
            body_bytes,
            events,
            ttft,
            max(0, int((time.monotonic() - started) * 1000)),
        )

    def evidence_from_response(
        self,
        lease: AssignmentLease,
        body: Mapping[str, Any],
        response: ProtocolResponse,
        *,
        final_output: str | None,
        finish_reason: str | None = None,
    ) -> ExecutionEvidence:
        request_bytes = json.dumps(body, ensure_ascii=False, separators=(",", ":")).encode()
        finished = datetime.now(UTC)
        response_hash = hashlib.sha256(response.body).hexdigest()
        started = finished - timedelta(milliseconds=response.latency_ms)
        return ExecutionEvidence(
            assignment_id=lease.id,
            sample_id=lease.sample_id,
            case_content_sha256=lease.case.content_sha256,
            execution_image_digest=str(
                lease.route_config.get("execution_image_digest", "worker@sha256:" + "0" * 64)
            ),
            request_sha256=hashlib.sha256(request_bytes).hexdigest(),
            response_sha256=response_hash,
            route_trace_id=lease.route_trace_id,
            started_at=started,
            finished_at=finished,
            latency_ms=response.latency_ms,
            ttft_ms=response.ttft_ms,
            transport_status=str(response.status_code),
            finish_reason=finish_reason,
            response_headers=response.headers,
            final_output=final_output,
            protocol_events=response.events,
            environment_fingerprint=environment_fingerprint(),
        )


def parse_sse(body: bytes, max_events: int) -> tuple[tuple[dict[str, Any], ...], int | None]:
    events: list[dict[str, Any]] = []
    first_offset: int | None = None
    started = time.monotonic()
    for block in body.decode("utf-8", errors="strict").replace("\r\n", "\n").split("\n\n"):
        data_lines = [line[5:] for line in block.split("\n") if line.startswith("data:")]
        if not data_lines:
            continue
        payload = "\n".join(data_lines).strip()
        if payload == "[DONE]":
            events.append({"type": "done", "offset_ms": int((time.monotonic() - started) * 1000)})
        else:
            try:
                parsed = json.loads(payload)
            except json.JSONDecodeError as exc:
                raise ProtocolError("invalid_sse_json", "SSE data is not valid JSON") from exc
            if not isinstance(parsed, dict):
                raise ProtocolError("invalid_sse_event", "SSE data must be an object")
            if first_offset is None:
                first_offset = int((time.monotonic() - started) * 1000)
            events.append(
                {"payload": parsed, "offset_ms": int((time.monotonic() - started) * 1000)}
            )
        if len(events) > max_events:
            raise ProtocolError("too_many_sse_events", "SSE event limit exceeded")
    return tuple(events), first_offset
