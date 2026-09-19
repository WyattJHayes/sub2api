from __future__ import annotations

import hashlib
import json
from datetime import UTC, datetime, timedelta
from decimal import Decimal
from typing import Any
from uuid import UUID

from pydantic import Field

from ..models import StrictModel
from .models import ReliabilityWindow


class ReliabilitySnapshotSubmission(StrictModel):
    worker_image_digest: str = Field(pattern=r"^(?:[^\s]+@)?sha256:[0-9a-f]{64}$")
    run_id: UUID
    load_plan_id: UUID
    profile_id: str = Field(min_length=1, max_length=100)
    window_start: datetime
    window_end: datetime
    source_watermark: str = Field(pattern=r"^[0-9a-f]{64}$")
    source_manifest: dict[str, Any]
    query_version: str = Field(min_length=1, max_length=100)
    slice_key: str = Field(min_length=1, max_length=200)
    request_count: int = Field(ge=0)
    success_count: int = Field(ge=0)
    error_count: int = Field(ge=0)
    timeout_count: int = Field(ge=0)
    retry_count: int = Field(ge=0)
    protocol_error_count: int = Field(ge=0)
    billing_idempotency_failures: int = Field(ge=0)
    ttft_histogram_hash: str = Field(pattern=r"^[0-9a-f]{64}$")
    latency_histogram_hash: str = Field(pattern=r"^[0-9a-f]{64}$")
    ttft_histogram: dict[str, Any]
    latency_histogram: dict[str, Any]
    p99_latency_ms: int = Field(ge=0)
    error_rate: Decimal = Field(ge=0, le=1)
    cost_amount: Decimal = Field(ge=0)
    fresh_until: datetime


def build_reliability_snapshot(
    *,
    worker_image_digest: str,
    run_id: UUID,
    load_plan_id: UUID,
    profile_id: str | None = None,
    source_watermark: str | None = None,
    query_version: str,
    slice_key: str,
    window_start: datetime,
    window_end: datetime,
    window: ReliabilityWindow,
    fresh_until: datetime | None = None,
) -> ReliabilitySnapshotSubmission:
    if window.request_count != window.terminal_count:
        raise ValueError("reliability window has incomplete terminal denominator")
    if window.request_count < 1:
        raise ValueError("reliability window must contain requests")
    if window_start.tzinfo != UTC or window_end.tzinfo != UTC or window_start >= window_end:
        raise ValueError("reliability window must be a positive UTC interval")
    resolved_profile_id = (profile_id or query_version).strip()
    if not resolved_profile_id:
        raise ValueError("reliability profile id is required")
    freshness = fresh_until or (window_end + timedelta(minutes=5))
    if freshness <= window_end:
        raise ValueError("reliability freshness must extend beyond the window")
    error_count = window.error_count + window.client_failure_count
    failed_count = error_count + window.timeout_count
    error_rate = Decimal(failed_count) / Decimal(window.request_count)
    p99_latency_ms = window.latency_histogram.percentile(0.99)
    ttft_hash = window.ttft_histogram.sha256()
    latency_hash = window.latency_histogram.sha256()
    source_manifest: dict[str, Any] = {
        "billing_idempotency_failures": window.billing_idempotency_failures,
        "cost_amount": str(window.cost_amount),
        "error_count": error_count,
        "error_rate": str(error_rate),
        "fresh_until": freshness.isoformat().replace("+00:00", "Z"),
        "latency_histogram_hash": latency_hash,
        "load_plan_id": str(load_plan_id),
        "profile_id": resolved_profile_id,
        "p99_latency_ms": p99_latency_ms,
        "protocol_error_count": window.protocol_error_count,
        "query_version": query_version,
        "request_count": window.request_count,
        "retry_count": window.retry_count,
        "run_id": str(run_id),
        "slice_key": slice_key,
        "success_count": window.success_count,
        "timeout_count": window.timeout_count,
        "ttft_histogram_hash": ttft_hash,
        "version": "radar-reliability-source-v1",
        "window_end": window_end.isoformat().replace("+00:00", "Z"),
        "window_start": window_start.isoformat().replace("+00:00", "Z"),
        "worker_image_digest": worker_image_digest,
    }
    source_bytes = json.dumps(
        source_manifest, ensure_ascii=False, sort_keys=True, separators=(",", ":")
    ).encode()
    computed_watermark = hashlib.sha256(source_bytes).hexdigest()
    if source_watermark is not None and source_watermark != computed_watermark:
        raise ValueError("source watermark does not match the canonical source manifest")
    return ReliabilitySnapshotSubmission(
        worker_image_digest=worker_image_digest,
        run_id=run_id,
        load_plan_id=load_plan_id,
        profile_id=resolved_profile_id,
        window_start=window_start,
        window_end=window_end,
        source_watermark=computed_watermark,
        source_manifest=source_manifest,
        query_version=query_version,
        slice_key=slice_key,
        request_count=window.request_count,
        success_count=window.success_count,
        error_count=error_count,
        timeout_count=window.timeout_count,
        retry_count=window.retry_count,
        protocol_error_count=window.protocol_error_count,
        billing_idempotency_failures=window.billing_idempotency_failures,
        ttft_histogram_hash=ttft_hash,
        latency_histogram_hash=latency_hash,
        ttft_histogram=window.ttft_histogram.canonical_object(),
        latency_histogram=window.latency_histogram.canonical_object(),
        p99_latency_ms=p99_latency_ms,
        error_rate=error_rate,
        cost_amount=window.cost_amount,
        fresh_until=freshness,
    )


class ReliabilityPublisher:
    def __init__(self, client: Any) -> None:
        self.client = client

    async def publish(self, submission: ReliabilitySnapshotSubmission) -> Any:
        return await self.client.publish_reliability_snapshot(submission)
