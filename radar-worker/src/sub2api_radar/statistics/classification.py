from __future__ import annotations

from collections import Counter
from collections.abc import Iterable

from ..models import FailureClass


def count_failures(failures: Iterable[FailureClass | None]) -> dict[FailureClass, int]:
    counts = Counter(item for item in failures if item is not None)
    return {key: counts[key] for key in FailureClass if counts[key]}
