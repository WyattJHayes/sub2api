from __future__ import annotations

from collections.abc import Iterable
from decimal import Decimal


def ewma(
    values: Iterable[Decimal], *, lam: Decimal = Decimal("0.2"), initial: Decimal | None = None
) -> Decimal | None:
    current = initial
    for value in values:
        current = value if current is None else lam * value + (Decimal("1") - lam) * current
    return current
