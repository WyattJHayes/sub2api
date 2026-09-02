from __future__ import annotations

from collections.abc import Iterable
from decimal import Decimal


def cusum(
    values: Iterable[Decimal], *, drift: Decimal = Decimal("0.5"), threshold: Decimal = Decimal("5")
) -> tuple[Decimal, Decimal, bool]:
    positive = Decimal("0")
    negative = Decimal("0")
    for value in values:
        positive = max(Decimal("0"), positive + value - drift)
        negative = min(Decimal("0"), negative + value + drift)
    signal = positive >= threshold or abs(negative) >= threshold
    return positive, negative, signal
