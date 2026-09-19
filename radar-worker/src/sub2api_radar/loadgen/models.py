from __future__ import annotations

from dataclasses import dataclass, field
from decimal import Decimal
from typing import Literal

from pydantic import Field

from ..models import StrictModel
from .histogram import FixedHistogram


class LoadCell(StrictModel):
    load_cell_id: str = Field(min_length=1, max_length=160)
    model_alias: str = Field(min_length=1, max_length=200)
    region: str = Field(min_length=1, max_length=100)
    concurrency: int = Field(gt=0, le=100000)
    input_tokens: int = Field(gt=0)
    output_tokens: int = Field(gt=0)
    streaming: bool = False


@dataclass(frozen=True)
class RequestMeasurement:
    outcome: Literal["success", "error", "timeout", "client_failure"]
    latency_ms: int = 0
    ttft_ms: int | None = None
    cost: Decimal = Decimal("0")
    retry_count: int = 0
    protocol_error: bool = False
    billing_idempotency_failure: bool = False

    def __post_init__(self) -> None:
        if self.latency_ms < 0 or (self.ttft_ms is not None and self.ttft_ms < 0):
            raise ValueError("latency values must be non-negative")
        if self.cost < 0 or self.retry_count < 0:
            raise ValueError("cost and retry count must be non-negative")

    @classmethod
    def success(
        cls,
        *,
        ttft_ms: int | None = None,
        latency_ms: int,
        cost: Decimal = Decimal("0"),
        retry_count: int = 0,
        protocol_error: bool = False,
        billing_idempotency_failure: bool = False,
    ) -> RequestMeasurement:
        return cls(
            "success",
            latency_ms,
            ttft_ms,
            cost,
            retry_count,
            protocol_error,
            billing_idempotency_failure,
        )

    @classmethod
    def error(cls, *, latency_ms: int = 0, retry_count: int = 0) -> RequestMeasurement:
        return cls("error", latency_ms=latency_ms, retry_count=retry_count)

    @classmethod
    def timeout(cls, *, latency_ms: int = 0, retry_count: int = 0) -> RequestMeasurement:
        return cls("timeout", latency_ms=latency_ms, retry_count=retry_count)

    @classmethod
    def client_failure(cls) -> RequestMeasurement:
        return cls("client_failure")


@dataclass
class ReliabilityWindow:
    request_count: int = 0
    success_count: int = 0
    error_count: int = 0
    timeout_count: int = 0
    client_failure_count: int = 0
    retry_count: int = 0
    protocol_error_count: int = 0
    billing_idempotency_failures: int = 0
    cost_amount: Decimal = Decimal("0")
    ttft_histogram: FixedHistogram = field(default_factory=FixedHistogram)
    latency_histogram: FixedHistogram = field(default_factory=FixedHistogram)

    @property
    def terminal_count(self) -> int:
        return (
            self.success_count
            + self.error_count
            + self.timeout_count
            + self.client_failure_count
        )

    def record(self, measurement: RequestMeasurement) -> None:
        self.request_count += 1
        self.retry_count += measurement.retry_count
        self.cost_amount += measurement.cost
        if measurement.protocol_error:
            self.protocol_error_count += 1
        if measurement.billing_idempotency_failure:
            self.billing_idempotency_failures += 1
        if measurement.outcome == "success":
            self.success_count += 1
            self.latency_histogram.observe(measurement.latency_ms)
            if measurement.ttft_ms is not None:
                self.ttft_histogram.observe(measurement.ttft_ms)
        elif measurement.outcome == "error":
            self.error_count += 1
        elif measurement.outcome == "timeout":
            self.timeout_count += 1
        elif measurement.outcome == "client_failure":
            self.client_failure_count += 1
        else:
            raise ValueError(f"unknown request outcome: {measurement.outcome}")

    def record_success(
        self, *, ttft_ms: int | None, latency_ms: int, cost: Decimal = Decimal("0")
    ) -> None:
        self.record(RequestMeasurement.success(ttft_ms=ttft_ms, latency_ms=latency_ms, cost=cost))

    def record_timeout(self) -> None:
        self.record(RequestMeasurement.timeout())

    def record_client_failure(self) -> None:
        self.record(RequestMeasurement.client_failure())
