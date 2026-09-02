from __future__ import annotations

import bisect
import hashlib
import json
from dataclasses import dataclass, field

DEFAULT_BUCKETS_MS: tuple[int, ...] = (
    1,
    5,
    10,
    25,
    50,
    100,
    250,
    500,
    1000,
    2000,
    5000,
    10000,
    30000,
    60000,
    120000,
)


@dataclass
class FixedHistogram:
    bucket_bounds_ms: tuple[int, ...] = DEFAULT_BUCKETS_MS
    counts: list[int] = field(default_factory=lambda: [0] * (len(DEFAULT_BUCKETS_MS) + 1))
    sample_count: int = 0
    sum_ms: int = 0
    max_ms: int = 0

    def __post_init__(self) -> None:
        if len(self.counts) != len(self.bucket_bounds_ms) + 1:
            raise ValueError("histogram counts must include the overflow bucket")
        if any(value <= 0 for value in self.bucket_bounds_ms):
            raise ValueError("histogram bounds must be positive")
        if tuple(sorted(set(self.bucket_bounds_ms))) != self.bucket_bounds_ms:
            raise ValueError("histogram bounds must be strictly increasing")
        if self.sample_count < 0 or self.sum_ms < 0 or self.max_ms < 0:
            raise ValueError("histogram aggregates must be non-negative")
        if sum(self.counts) != self.sample_count:
            raise ValueError("histogram bucket count must match sample count")

    @classmethod
    def observe_many(cls, values: list[int] | tuple[int, ...]) -> FixedHistogram:
        histogram = cls()
        for value in values:
            histogram.observe(value)
        return histogram

    def observe(self, value_ms: int) -> None:
        if value_ms < 0:
            raise ValueError("histogram observations must be non-negative")
        index = bisect.bisect_left(self.bucket_bounds_ms, value_ms)
        self.counts[index] += 1
        self.sample_count += 1
        self.sum_ms += value_ms
        self.max_ms = max(self.max_ms, value_ms)

    def percentile(self, quantile: float) -> int:
        if not 0 < quantile <= 1:
            raise ValueError("quantile must be in (0, 1]")
        if self.sample_count == 0:
            return 0
        target = max(1, int((self.sample_count * quantile) + 0.999999))
        seen = 0
        for index, count in enumerate(self.counts):
            seen += count
            if seen >= target:
                if index < len(self.bucket_bounds_ms):
                    return self.bucket_bounds_ms[index]
                return self.bucket_bounds_ms[-1]
        return self.bucket_bounds_ms[-1]

    def canonical_object(self) -> dict[str, object]:
        return {
            "bucket_bounds_ms": list(self.bucket_bounds_ms),
            "counts": list(self.counts),
            "max_ms": self.max_ms,
            "sample_count": self.sample_count,
            "sum_ms": self.sum_ms,
        }

    def canonical_bytes(self) -> bytes:
        return json.dumps(
            self.canonical_object(), ensure_ascii=False, sort_keys=True, separators=(",", ":")
        ).encode()

    def sha256(self) -> str:
        return hashlib.sha256(self.canonical_bytes()).hexdigest()
