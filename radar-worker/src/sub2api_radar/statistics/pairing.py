from __future__ import annotations

from collections.abc import Iterable
from uuid import UUID

from ..models import PairedScore


def pair_scores(scores: Iterable[PairedScore]) -> tuple[PairedScore, ...]:
    """Keep one deterministic current pair per case, route, and sample index."""
    keyed: dict[tuple[UUID, str, int], PairedScore] = {}
    for score in scores:
        key = (score.case_id, score.model_route, score.sample_index)
        keyed.setdefault(key, score)
    return tuple(
        keyed[key] for key in sorted(keyed, key=lambda item: (str(item[0]), item[1], item[2]))
    )
