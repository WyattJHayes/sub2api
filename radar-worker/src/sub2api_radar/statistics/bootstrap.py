from __future__ import annotations

from collections.abc import Sequence
from decimal import Decimal

import numpy as np

from ..models import PairedScore


def weighted_delta(pairs: Sequence[PairedScore]) -> Decimal:
    if not pairs:
        return Decimal("0")
    weights = [float(item.weight) for item in pairs]
    deltas = [float(item.candidate_score - item.baseline_score) for item in pairs]
    return Decimal(str(float(np.average(deltas, weights=weights))))


def bootstrap_delta(
    pairs: Sequence[PairedScore], *, seed: int, draws: int = 10_000
) -> tuple[Decimal, Decimal]:
    if not pairs:
        return Decimal("0"), Decimal("0")
    generator = np.random.Generator(np.random.PCG64(seed))
    weights = np.asarray([float(item.weight) for item in pairs], dtype=float)
    deltas = np.asarray(
        [float(item.candidate_score - item.baseline_score) for item in pairs], dtype=float
    )
    indices = generator.integers(0, len(pairs), size=(draws, len(pairs)))
    sampled_weights = weights[indices]
    sampled_deltas = deltas[indices]
    estimates = np.sum(sampled_weights * sampled_deltas, axis=1) / np.sum(sampled_weights, axis=1)
    low, high = np.percentile(estimates, [2.5, 97.5])
    return Decimal(str(float(low))), Decimal(str(float(high)))
