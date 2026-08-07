from __future__ import annotations

import argparse
import asyncio
import hashlib
import json
import os
import time
from collections.abc import Awaitable, Callable, Mapping, Sequence
from datetime import UTC, datetime, timedelta
from decimal import Decimal, InvalidOperation
from itertools import product
from pathlib import Path
from typing import Any
from uuid import UUID, uuid4

import httpx

from ..config import Settings, get_settings
from ..control_plane import ControlPlaneClient
from ..observability import MetricsServer, RadarMetrics, traceparent_for
from .models import LoadCell, ReliabilityWindow, RequestMeasurement
from .publisher import build_reliability_snapshot
from .sse import SSEDecoder, SSEProtocolError

Requester = Callable[[LoadCell], Awaitable[RequestMeasurement]]


def write_reliability_report(
    directory: str | Path,
    *,
    run_id: UUID,
    load_plan_id: UUID,
    worker_image_digest: str,
    profile_id: str,
    query_version: str,
    cells: Sequence[LoadCell],
    results: Mapping[str, ReliabilityWindow],
    published_snapshots: Sequence[Mapping[str, Any]],
    started_at: datetime | None = None,
    finished_at: datetime | None = None,
) -> Path:
    """Persist a replayable load result bound to immutable run identities."""
    output_dir = Path(directory)
    output_dir.mkdir(parents=True, exist_ok=True)
    started = started_at or datetime.now(UTC)
    finished = finished_at or datetime.now(UTC)
    if started.tzinfo is None or finished.tzinfo is None:
        raise ValueError("load report timestamps must include a timezone")
    cell_records: list[dict[str, Any]] = []
    for cell in cells:
        window = results.get(cell.load_cell_id)
        if window is None:
            raise ValueError(f"missing result for load cell {cell.load_cell_id}")
        cell_records.append(
            {
                "load_cell_id": cell.load_cell_id,
                "model_alias": cell.model_alias,
                "region": cell.region,
                "concurrency": cell.concurrency,
                "input_tokens": cell.input_tokens,
                "output_tokens": cell.output_tokens,
                "streaming": cell.streaming,
                "request_count": window.request_count,
                "success_count": window.success_count,
                "error_count": window.error_count + window.client_failure_count,
                "timeout_count": window.timeout_count,
                "retry_count": window.retry_count,
                "protocol_error_count": window.protocol_error_count,
                "billing_idempotency_failures": window.billing_idempotency_failures,
                "cost_amount": str(window.cost_amount),
                "p99_latency_ms": window.latency_histogram.percentile(0.99),
                "ttft_histogram": window.ttft_histogram.canonical_object(),
                "latency_histogram": window.latency_histogram.canonical_object(),
            }
        )
    document = {
        "schema_version": "radar-loadgen-report-v1",
        "run_id": str(run_id),
        "load_plan_id": str(load_plan_id),
        "worker_image_digest": worker_image_digest,
        "profile_id": profile_id,
        "query_version": query_version,
        "started_at": started.astimezone(UTC).isoformat().replace("+00:00", "Z"),
        "finished_at": finished.astimezone(UTC).isoformat().replace("+00:00", "Z"),
        "cells": cell_records,
        "published_snapshots": [dict(item) for item in published_snapshots],
    }
    target = output_dir / f"loadgen-{run_id}.json"
    temporary = target.with_suffix(".json.tmp")
    temporary.write_text(
        json.dumps(document, ensure_ascii=False, sort_keys=True, separators=(",", ":")),
        encoding="utf-8",
    )
    temporary.replace(target)
    return target


def build_load_cells(plan_id: UUID, document: Mapping[str, Any]) -> list[LoadCell]:
    """Expand a published plan into deterministic, bounded load cells."""
    models = _positive_strings(document.get("model_aliases"), "model_aliases")
    regions = _positive_strings(document.get("regions"), "regions")
    concurrency = _positive_ints(document.get("concurrency_levels"), "concurrency_levels")
    inputs = _positive_ints(document.get("input_token_buckets"), "input_token_buckets")
    outputs = _positive_ints(document.get("output_token_buckets"), "output_token_buckets")
    max_concurrency = document.get("max_concurrency")
    if (
        not isinstance(max_concurrency, int)
        or isinstance(max_concurrency, bool)
        or max_concurrency < 1
    ):
        raise ValueError("published load plan max_concurrency is invalid")
    if any(level > max_concurrency for level in concurrency):
        raise ValueError("published load plan concurrency exceeds its maximum")
    combinations = len(models) * len(regions) * len(concurrency) * len(inputs) * len(outputs)
    if combinations < 1 or combinations > 10_000:
        raise ValueError("published load plan expands to an unsafe number of load cells")
    streaming = document.get("streaming", False)
    if not isinstance(streaming, bool):
        raise ValueError("published load plan streaming flag is invalid")
    cells: list[LoadCell] = []
    for model, region, level, input_tokens, output_tokens in product(
        models, regions, concurrency, inputs, outputs
    ):
        identity = ":".join(
            (
                str(plan_id),
                model,
                region,
                str(level),
                str(input_tokens),
                str(output_tokens),
                str(streaming).lower(),
            )
        )
        load_cell_id = hashlib.sha256(identity.encode()).hexdigest()[:32]
        cells.append(
            LoadCell(
                load_cell_id=load_cell_id,
                model_alias=model,
                region=region,
                concurrency=level,
                input_tokens=input_tokens,
                output_tokens=output_tokens,
                streaming=streaming,
            )
        )
    return cells


def _positive_strings(value: Any, field: str) -> list[str]:
    if not isinstance(value, list | tuple) or not value:
        raise ValueError(f"published load plan {field} is required")
    result = [str(item).strip() for item in value]
    if any(not item for item in result) or len(set(result)) != len(result):
        raise ValueError(f"published load plan {field} contains invalid values")
    return result


def _positive_ints(value: Any, field: str) -> list[int]:
    if not isinstance(value, list | tuple) or not value:
        raise ValueError(f"published load plan {field} is required")
    result = list(value)
    if any(not isinstance(item, int) or isinstance(item, bool) or item <= 0 for item in result):
        raise ValueError(f"published load plan {field} contains invalid values")
    if len(set(result)) != len(result):
        raise ValueError(f"published load plan {field} contains duplicate values")
    return result


class _GatewayRequester:
    def __init__(
        self,
        settings: Settings,
        *,
        client: httpx.AsyncClient | None = None,
        metrics: RadarMetrics | None = None,
    ) -> None:
        if not settings.gateway_url:
            raise RuntimeError("RADAR_GATEWAY_URL is required for an unmanaged loadgen run")
        if settings.loadgen_evaluation_api_key is None:
            raise RuntimeError(
                "RADAR_LOADGEN_EVALUATION_API_KEY is required for an unmanaged loadgen run"
            )
        if settings.gateway_url is None:
            raise RuntimeError("RADAR_GATEWAY_URL is required for an unmanaged loadgen run")
        self._settings = settings
        self._api_key = settings.loadgen_evaluation_api_key.get_secret_value()
        self._gateway_url = settings.gateway_url
        self._worker_id = settings.worker_id
        self._run_id = settings.load_run_id
        self._max_retries = settings.max_request_retries
        self.metrics = metrics or RadarMetrics()
        self._client = client or httpx.AsyncClient(
            timeout=httpx.Timeout(
                settings.request_timeout_seconds, connect=settings.connect_timeout_seconds
            )
        )

    async def close(self) -> None:
        await self._client.aclose()

    async def __call__(self, cell: LoadCell) -> RequestMeasurement:
        started = time.monotonic()
        request_id = f"{self._worker_id}:{self._run_id}:{cell.load_cell_id}:{uuid4()}"
        body = {
            "model": cell.model_alias,
            "messages": [{"role": "user", "content": "radar " * cell.input_tokens}],
            "max_tokens": cell.output_tokens,
            "stream": cell.streaming,
        }
        headers = {
            "Authorization": f"Bearer {self._api_key}",
            "X-Radar-Run-ID": str(self._settings.load_run_id),
            "X-Radar-Load-Cell-ID": cell.load_cell_id,
            "X-Radar-Region": cell.region,
            "X-Radar-Route-Profile": self._settings.route_profile_version,
            "X-Radar-Request-ID": request_id,
            "Idempotency-Key": request_id,
            "traceparent": traceparent_for(request_id),
        }
        retries = 0
        billing_failed = False
        while True:
            try:
                async with self._client.stream(
                    "POST",
                    self._gateway_url.rstrip("/") + "/v1/chat/completions",
                    headers=headers,
                    json=body,
                ) as response:
                    billing_failed = billing_failed or _billing_idempotency_failed(response)
                    if response.status_code in {408, 429} or response.status_code >= 500:
                        if retries < self._max_retries:
                            retries += 1
                            await response.aread()
                            continue
                        latency_ms = _elapsed_ms(started)
                        cost, _ = _response_cost(response)
                        return self._record(
                            cell,
                            RequestMeasurement(
                                "error",
                                latency_ms=latency_ms,
                                cost=cost,
                                retry_count=retries,
                                billing_idempotency_failure=billing_failed,
                            ),
                        )
                    if response.status_code >= 400:
                        cost, _ = _response_cost(response)
                        return self._record(
                            cell,
                            RequestMeasurement(
                                "error",
                                latency_ms=_elapsed_ms(started),
                                cost=cost,
                                retry_count=retries,
                                billing_idempotency_failure=billing_failed,
                            ),
                        )
                    if cell.streaming:
                        return self._record(
                            cell,
                            await _measure_stream_response(
                                response,
                                started=started,
                                retry_count=retries,
                                billing_idempotency_failure=billing_failed,
                            ),
                        )
                    await response.aread()
                    return self._record(
                        cell,
                        _measure_json_response(
                            response,
                            started=started,
                            retry_count=retries,
                            billing_idempotency_failure=billing_failed,
                        ),
                    )
            except httpx.TimeoutException:
                if retries < self._max_retries:
                    retries += 1
                    continue
                return self._record(
                    cell,
                    RequestMeasurement.timeout(
                        latency_ms=_elapsed_ms(started), retry_count=retries
                    ),
                )
            except httpx.HTTPError:
                if retries < self._max_retries:
                    retries += 1
                    continue
                return self._record(
                    cell,
                    RequestMeasurement(
                        "client_failure",
                        latency_ms=_elapsed_ms(started),
                        retry_count=retries,
                        billing_idempotency_failure=billing_failed,
                    ),
                )

    def _record(self, cell: LoadCell, measurement: RequestMeasurement) -> RequestMeasurement:
        self.metrics.record_gateway_request(
            model=cell.model_alias,
            region=cell.region,
            outcome=measurement.outcome,
            latency_ms=measurement.latency_ms,
            ttft_ms=measurement.ttft_ms,
            cost=measurement.cost,
            billing_idempotency_failure=measurement.billing_idempotency_failure,
        )
        return measurement


def _billing_idempotency_failed(response: httpx.Response) -> bool:
    value = response.headers.get("X-Radar-Billing-Idempotency-Failure", "")
    return str(value).lower() == "true"


def _response_cost(response: httpx.Response) -> tuple[Decimal, bool]:
    try:
        return Decimal(response.headers.get("X-Radar-Cost-USD", "0")), False
    except InvalidOperation:
        return Decimal("0"), True


def _measure_json_response(
    response: httpx.Response,
    *,
    started: float,
    retry_count: int,
    billing_idempotency_failure: bool,
) -> RequestMeasurement:
    latency_ms = _elapsed_ms(started)
    cost, cost_error = _response_cost(response)
    protocol_error = cost_error
    try:
        payload = response.json()
        protocol_error = bool(
            protocol_error
            or not isinstance(payload, Mapping)
            or "choices" not in payload
        )
    except (ValueError, json.JSONDecodeError):
        protocol_error = True
    if protocol_error:
        return RequestMeasurement(
            "error",
            latency_ms=latency_ms,
            protocol_error=True,
            cost=cost,
            retry_count=retry_count,
            billing_idempotency_failure=billing_idempotency_failure,
        )
    return RequestMeasurement.success(
        latency_ms=latency_ms,
        ttft_ms=_header_int(response, "X-Radar-TTFT-MS"),
        cost=cost,
        retry_count=max(retry_count, _header_int(response, "X-Radar-Retry-Count") or 0),
        billing_idempotency_failure=billing_idempotency_failure,
    )


async def _measure_stream_response(
    response: httpx.Response,
    *,
    started: float,
    retry_count: int,
    billing_idempotency_failure: bool,
) -> RequestMeasurement:
    decoder = SSEDecoder()
    first_token_ms: int | None = None
    terminal = False
    protocol_error = False
    try:
        async for chunk in response.aiter_bytes():
            for event in decoder.feed(chunk):
                data = event.data.strip()
                if data == "[DONE]" or event.event.lower() in {"done", "terminal"}:
                    terminal = True
                    continue
                try:
                    payload = json.loads(data)
                except (TypeError, ValueError, json.JSONDecodeError):
                    protocol_error = True
                    continue
                if not isinstance(payload, Mapping):
                    protocol_error = True
                    continue
                if payload.get("error") is not None:
                    protocol_error = True
                    continue
                choices = payload.get("choices")
                if not isinstance(choices, list) or not choices:
                    protocol_error = True
                    continue
                for choice in choices:
                    if not isinstance(choice, Mapping):
                        protocol_error = True
                        continue
                    if choice.get("finish_reason") is not None:
                        terminal = True
                    delta = choice.get("delta")
                    if isinstance(delta, Mapping):
                        content = delta.get("content")
                        if isinstance(content, str) and content and first_token_ms is None:
                            first_token_ms = _elapsed_ms(started)
        decoder.finish()
    except SSEProtocolError:
        protocol_error = True
    cost, cost_error = _response_cost(response)
    protocol_error = protocol_error or cost_error or not terminal or first_token_ms is None
    if protocol_error:
        return RequestMeasurement(
            "error",
            latency_ms=_elapsed_ms(started),
            ttft_ms=first_token_ms,
            protocol_error=True,
            cost=cost,
            retry_count=max(retry_count, _header_int(response, "X-Radar-Retry-Count") or 0),
            billing_idempotency_failure=billing_idempotency_failure,
        )
    return RequestMeasurement.success(
        latency_ms=_elapsed_ms(started),
        ttft_ms=first_token_ms,
        cost=cost,
        retry_count=max(retry_count, _header_int(response, "X-Radar-Retry-Count") or 0),
        billing_idempotency_failure=billing_idempotency_failure,
    )


def _elapsed_ms(started: float) -> int:
    return max(0, int((time.monotonic() - started) * 1000))


def _header_int(response: httpx.Response, name: str) -> int | None:
    value = response.headers.get(name)
    if value is None:
        return None
    try:
        parsed = int(value)
    except ValueError:
        return None
    return parsed if parsed >= 0 else None


def _credential(name: str) -> str:
    value = os.environ.get(name, "").strip()
    if len(value) < 32 or value.lower().startswith(("change-me", "placeholder", "test-")):
        raise SystemExit(f"credential {name} is required and must be a dedicated secret")
    return value


class LoadGenerator:
    def __init__(
        self,
        requester: Requester,
        *,
        clock: Callable[[], float] = time.monotonic,
        sleeper: Callable[[float], Awaitable[None]] = asyncio.sleep,
        metrics: RadarMetrics | None = None,
    ) -> None:
        self.requester = requester
        self.clock = clock
        self.sleeper = sleeper
        self.metrics = metrics or RadarMetrics()

    async def measure_cell(
        self,
        cell: LoadCell,
        *,
        window: ReliabilityWindow | None,
        warmup_seconds: float,
        measurement_seconds: float,
        minimum_valid_requests: int,
    ) -> ReliabilityWindow:
        if warmup_seconds < 0 or measurement_seconds < 0 or minimum_valid_requests < 0:
            raise ValueError("load window values must be non-negative")
        result = window or ReliabilityWindow()
        if warmup_seconds:
            await self.sleeper(warmup_seconds)
        deadline = self.clock() + measurement_seconds
        stopped = asyncio.Event()

        async def worker() -> None:
            while not stopped.is_set():
                enough_requests = result.request_count >= minimum_valid_requests
                if enough_requests and (measurement_seconds == 0 or self.clock() >= deadline):
                    stopped.set()
                    return
                if measurement_seconds > 0 and self.clock() >= deadline:
                    stopped.set()
                    return
                try:
                    measurement = await self.requester(cell)
                except TimeoutError:
                    measurement = RequestMeasurement.timeout()
                except Exception:
                    measurement = RequestMeasurement.client_failure()
                result.record(measurement)
                if result.request_count >= minimum_valid_requests and measurement_seconds == 0:
                    stopped.set()

        async with asyncio.TaskGroup() as group:
            for _ in range(cell.concurrency):
                group.create_task(worker())
        return result

    async def measure(
        self,
        cells: Sequence[LoadCell],
        *,
        warmup_seconds: float,
        measurement_seconds: float,
        minimum_valid_requests: int,
    ) -> dict[str, ReliabilityWindow]:
        results: dict[str, ReliabilityWindow] = {}

        async def run(cell: LoadCell) -> None:
            results[cell.load_cell_id] = await self.measure_cell(
                cell,
                window=None,
                warmup_seconds=warmup_seconds,
                measurement_seconds=measurement_seconds,
                minimum_valid_requests=minimum_valid_requests,
            )

        async with asyncio.TaskGroup() as group:
            for cell in cells:
                if cell.load_cell_id in results:
                    raise ValueError(f"duplicate load cell id: {cell.load_cell_id}")
                group.create_task(run(cell))
        return results


async def run(
    *,
    requester: Requester | None = None,
    control_plane_client: Any | None = None,
    publisher: Any | None = None,
    cells: Sequence[LoadCell] | None = None,
    settings: Any | None = None,
    warmup_seconds: float = 120.0,
    measurement_seconds: float = 600.0,
    minimum_valid_requests: int = 1,
) -> dict[str, ReliabilityWindow]:
    """Run a load plan through the Gateway and publish each trusted snapshot.

    Tests and embedding hosts may still inject all dependencies. The normal
    worker entrypoint obtains a published plan and publishes through the
    authenticated control-plane client configured in ``Settings``.
    """
    if requester is None and not isinstance(settings, Settings):
        raise RuntimeError(
            "loadgen dependencies must be injected or valid settings must be supplied"
        )
    managed_requester: _GatewayRequester | None = None
    managed_control_plane: ControlPlaneClient | None = None
    metrics = RadarMetrics()
    metrics_server: MetricsServer | None = None
    published_profile_id: str | None = None
    if requester is None:
        assert isinstance(settings, Settings)
        managed_requester = _GatewayRequester(settings, metrics=metrics)
        requester = managed_requester
    if control_plane_client is None:
        assert isinstance(settings, Settings)
        managed_control_plane = ControlPlaneClient(settings, metrics=metrics)
        await managed_control_plane.start()
        control_plane_client = managed_control_plane
    if cells is None:
        assert isinstance(settings, Settings)
        if settings.load_plan_id is None:
            raise RuntimeError("RADAR_LOAD_PLAN_ID is required for an unmanaged loadgen run")
        plan_payload = await control_plane_client.get_load_plan(settings.load_plan_id)
        if str(plan_payload.get("status", "")).lower() != "published":
            raise RuntimeError("loadgen can execute only a published load plan")
        document = plan_payload.get("canonical_plan", plan_payload)
        if isinstance(document, str):
            try:
                document = json.loads(document)
            except json.JSONDecodeError as exc:
                raise RuntimeError("published load plan is not valid JSON") from exc
        if not isinstance(document, Mapping):
            raise RuntimeError("published load plan document is invalid")
        raw_profile_id = document.get(
            "reliability_profile_id",
            document.get("route_profile_version", settings.route_profile_version),
        )
        if not isinstance(raw_profile_id, str) or not raw_profile_id.strip():
            raise RuntimeError("published load plan reliability profile id is required")
        published_profile_id = raw_profile_id.strip()
        cells = build_load_cells(settings.load_plan_id, document)
    if publisher is None:
        publisher = control_plane_client

    if isinstance(settings, Settings) and settings.metrics_enabled:
        metrics_server = MetricsServer(
            metrics, host=settings.metrics_host, port=settings.metrics_port
        )
        await metrics_server.start()

    started_at = datetime.now(UTC)
    published_snapshots: list[Mapping[str, Any]] = []
    try:
        generator = LoadGenerator(requester, metrics=metrics)
        results = await generator.measure(
            cells,
            warmup_seconds=warmup_seconds,
            measurement_seconds=measurement_seconds,
            minimum_valid_requests=minimum_valid_requests,
        )
        if isinstance(settings, Settings) and settings.load_plan_id and settings.load_run_id:
            image_digest = settings.loadgen_image_digest
            if not image_digest:
                raise RuntimeError(
                    "RADAR_LOADGEN_IMAGE_DIGEST is required for snapshot publication"
                )
            window_end = datetime.now(UTC)
            profile_id = published_profile_id or settings.route_profile_version
            window_start = min(
                started_at,
                window_end - timedelta(seconds=max(1, measurement_seconds)),
            )
            for cell in cells:
                submission = build_reliability_snapshot(
                    worker_image_digest=image_digest,
                    run_id=settings.load_run_id,
                    load_plan_id=settings.load_plan_id,
                    profile_id=profile_id,
                    query_version=settings.analysis_version,
                    slice_key=f"{cell.model_alias}:{cell.region}:c{cell.concurrency}",
                    window_start=window_start,
                    window_end=window_end,
                    window=results[cell.load_cell_id],
                )
                receipt = await publisher.publish_reliability_snapshot(submission)
                if isinstance(receipt, Mapping):
                    published_snapshots.append(
                        {
                            "submission": submission.model_dump(mode="json"),
                            "receipt": dict(receipt),
                        }
                    )
                else:
                    published_snapshots.append(
                        {
                            "submission": submission.model_dump(mode="json"),
                            "receipt": str(receipt),
                        }
                    )
            write_reliability_report(
                settings.reliability_report_dir,
                run_id=settings.load_run_id,
                load_plan_id=settings.load_plan_id,
                worker_image_digest=image_digest,
                profile_id=profile_id,
                query_version=settings.analysis_version,
                cells=cells,
                results=results,
                published_snapshots=published_snapshots,
                started_at=started_at,
                finished_at=window_end,
            )
        return results
    finally:
        if managed_requester is not None:
            await managed_requester.close()
        if managed_control_plane is not None:
            await managed_control_plane.close()
        if metrics_server is not None:
            await metrics_server.close()


def main(argv: list[str] | None = None) -> None:
    parser = argparse.ArgumentParser(description="Sub2API Radar reliability load generator")
    parser.add_argument("--version", action="version", version="0.1.0")
    parser.parse_args(argv)
    _credential("RADAR_LOADGEN_WORKER_TOKEN")
    _credential("RADAR_LOADGEN_EVALUATION_API_KEY")
    settings = get_settings()
    asyncio.run(run(settings=settings))


if __name__ == "__main__":
    main()
