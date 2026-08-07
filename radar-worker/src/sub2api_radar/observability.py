from __future__ import annotations

import asyncio
import hashlib
import math
import re
from collections import defaultdict
from collections.abc import Iterator
from contextlib import contextmanager
from contextvars import ContextVar
from decimal import Decimal
from typing import Self, cast

_LABEL_RE = re.compile(r"^[A-Za-z0-9_.:-]{1,64}$")
_OUTCOMES = {"success", "error", "timeout", "client_failure"}
_LATENCY_BUCKETS = (50, 100, 250, 500, 1000, 2500, 5000, 10000)
_TRACEPARENT_RE = re.compile(r"^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$")
_TRACEPARENT: ContextVar[str | None] = ContextVar("radar_traceparent", default=None)


def _label(value: str) -> str:
    if not isinstance(value, str) or _LABEL_RE.fullmatch(value) is None:
        raise ValueError("metric label must contain only bounded ASCII characters")
    return value


def _number(value: float) -> str:
    if not isinstance(value, int | float) or isinstance(value, bool) or not math.isfinite(value):
        raise ValueError("metric value must be finite")
    rendered = format(float(value), ".15g")
    if isinstance(value, float) and "e" not in rendered.lower() and "." not in rendered:
        rendered += ".0"
    return rendered


def traceparent_for(seed: str) -> str:
    """Derive a deterministic W3C traceparent without exposing request data."""
    if _TRACEPARENT_RE.fullmatch(seed):
        return seed
    digest = hashlib.sha256(seed.encode("utf-8")).hexdigest()
    return f"00-{digest[:32]}-{digest[32:48]}-01"


def current_traceparent() -> str | None:
    return _TRACEPARENT.get()


@contextmanager
def trace_scope(value: str) -> Iterator[None]:
    token = _TRACEPARENT.set(traceparent_for(value))
    try:
        yield
    finally:
        _TRACEPARENT.reset(token)


class RadarMetrics:
    """Small dependency-free Prometheus registry for the controlled Worker path."""

    def __init__(self) -> None:
        self._requests: defaultdict[tuple[str, str, str], int] = defaultdict(int)
        self._latency: defaultdict[str, list[float]] = defaultdict(lambda: [0.0, 0.0])
        self._latency_buckets: defaultdict[str, list[int]] = defaultdict(
            lambda: [0] * len(_LATENCY_BUCKETS)
        )
        self._queue_lag = 0.0
        self._lease_age = 0.0
        self._analysis_lag = 0.0
        self._recovery_duration = 0.0
        self._cost: defaultdict[str, float] = defaultdict(float)
        self._billing_failures = 0
        self._heartbeat: dict[str, float] = {}
        self._gpu: dict[tuple[str, str], float] = {}

    def inc_request(self, *, model: str, region: str, outcome: str) -> None:
        if outcome not in _OUTCOMES:
            raise ValueError("metric request outcome is invalid")
        self._requests[(_label(model), _label(region), _label(outcome))] += 1

    def record_gateway_request(
        self,
        *,
        model: str,
        region: str,
        outcome: str,
        latency_ms: int,
        ttft_ms: int | None = None,
        cost: Decimal = Decimal("0"),
        billing_idempotency_failure: bool = False,
    ) -> None:
        self.inc_request(model=model, region=region, outcome=outcome)
        self.observe_latency("gateway", latency_ms)
        if ttft_ms is not None:
            self.observe_latency("ttft", ttft_ms)
        if cost:
            self.observe_cost(region, float(cost))
        if billing_idempotency_failure:
            self.inc_billing_idempotency_failure()

    def observe_latency(self, stage: str, milliseconds: float) -> None:
        stage = _label(stage)
        value = float(_number(milliseconds))
        total, count = self._latency[stage]
        self._latency[stage] = [total + value, count + 1]
        buckets = self._latency_buckets[stage]
        for index, bound in enumerate(_LATENCY_BUCKETS):
            if value <= bound:
                buckets[index] += 1

    def observe_queue_lag(self, seconds: float) -> None:
        self._queue_lag = float(_number(seconds))

    def observe_lease_age(self, seconds: float) -> None:
        self._lease_age = float(_number(seconds))

    def observe_analysis_lag(self, seconds: float) -> None:
        self._analysis_lag = float(_number(seconds))

    def observe_recovery_duration(self, seconds: float) -> None:
        self._recovery_duration = float(_number(seconds))

    def observe_cost(self, region: str, dollars: float) -> None:
        region = _label(region)
        self._cost[region] += float(_number(dollars))

    def inc_billing_idempotency_failure(self) -> None:
        self._billing_failures += 1

    def observe_worker_heartbeat(self, worker_kind: str, age_seconds: float) -> None:
        self._heartbeat[_label(worker_kind)] = float(_number(age_seconds))

    def observe_gpu_utilization(self, *, worker_kind: str, region: str, ratio: float) -> None:
        if ratio < 0 or ratio > 1:
            raise ValueError("GPU utilization ratio must be between zero and one")
        key = (_label(worker_kind), _label(region))
        self._gpu[key] = float(_number(ratio))

    def render(self) -> str:
        lines = [
            "# TYPE radar_gateway_requests_total counter",
            "# TYPE radar_gateway_request_latency_ms histogram",
            "# TYPE radar_queue_lag_seconds gauge",
            "# TYPE radar_lease_age_seconds gauge",
            "# TYPE radar_analysis_lag_seconds gauge",
            "# TYPE radar_recovery_duration_seconds gauge",
            "# TYPE radar_cost_usd_total counter",
            "# TYPE radar_billing_idempotency_failures_total counter",
            "# TYPE radar_worker_heartbeat_age_seconds gauge",
        ]
        for (model, region, outcome), request_count in sorted(self._requests.items()):
            lines.append(
                f'radar_gateway_requests_total{{model="{model}",outcome="{outcome}",'
                f'region="{region}"}} {request_count}'
            )
        for stage, (total, sample_count) in sorted(self._latency.items()):
            for bound, bucket_count in zip(
                _LATENCY_BUCKETS, self._latency_buckets[stage], strict=True
            ):
                lines.append(
                    f'radar_gateway_request_latency_ms_bucket{{stage="{stage}",le="{bound}"}} '
                    f'{bucket_count}'
                )
            lines.append(
                f'radar_gateway_request_latency_ms_bucket{{stage="{stage}",le="+Inf"}} '
                f'{int(sample_count)}'
            )
            lines.append(
                f'radar_gateway_request_latency_ms_sum{{stage="{stage}"}} '
                f'{_number(total)}'
            )
            lines.append(
                f'radar_gateway_request_latency_ms_count{{stage="{stage}"}} '
                f'{int(sample_count)}'
            )
        lines.extend(
            [
                f"radar_queue_lag_seconds {_number(self._queue_lag)}",
                f"radar_lease_age_seconds {_number(self._lease_age)}",
                f"radar_analysis_lag_seconds {_number(self._analysis_lag)}",
                f"radar_recovery_duration_seconds {_number(self._recovery_duration)}",
                f"radar_billing_idempotency_failures_total {self._billing_failures}",
            ]
        )
        for region, value in sorted(self._cost.items()):
            lines.append(f'radar_cost_usd_total{{region="{region}"}} {_number(value)}')
        for worker_kind, value in sorted(self._heartbeat.items()):
            lines.append(
                f'radar_worker_heartbeat_age_seconds{{worker_kind="{worker_kind}"}} '
                f'{_number(value)}'
            )
        lines.append("# TYPE radar_gpu_utilization_ratio gauge")
        for (worker_kind, region), value in sorted(self._gpu.items()):
            lines.append(
                f'radar_gpu_utilization_ratio{{region="{region}",worker_kind="{worker_kind}"}} '
                f'{_number(value)}'
            )
        return "\n".join(lines) + "\n"


class MetricsServer:
    """Minimal authenticated-process-local endpoint for Prometheus scraping."""

    def __init__(self, metrics: RadarMetrics, *, host: str, port: int) -> None:
        if not host.strip():
            raise ValueError("metrics host is required")
        if port < 0 or port > 65535:
            raise ValueError("metrics port is out of range")
        self.metrics = metrics
        self.host = host
        self._requested_port = port
        self._server: asyncio.AbstractServer | None = None

    @property
    def port(self) -> int | None:
        if self._server is None:
            return None
        server = cast(asyncio.Server, self._server)
        if not server.sockets:
            return None
        address = server.sockets[0].getsockname()
        return int(address[1])

    async def start(self) -> Self:
        if self._server is None:
            self._server = await asyncio.start_server(
                self._handle, self.host, self._requested_port
            )
        return self

    async def close(self) -> None:
        if self._server is not None:
            self._server.close()
            await self._server.wait_closed()
            self._server = None

    async def _handle(
        self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter
    ) -> None:
        try:
            request = await reader.read(4096)
            first_line = request.split(b"\r\n", 1)[0].split()
            if len(first_line) < 2:
                status, body = "400 Bad Request", b"bad request\n"
            elif first_line[0] != b"GET" or first_line[1] != b"/metrics":
                status, body = "404 Not Found", b"not found\n"
            else:
                status = "200 OK"
                body = self.metrics.render().encode("utf-8")
            headers = (
                f"HTTP/1.1 {status}\r\n"
                "Content-Type: text/plain; version=0.0.4; charset=utf-8\r\n"
                f"Content-Length: {len(body)}\r\n"
                "Connection: close\r\n\r\n"
            ).encode("ascii")
            writer.write(headers + body)
            await writer.drain()
        finally:
            writer.close()
            await writer.wait_closed()
