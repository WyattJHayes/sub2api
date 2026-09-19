from __future__ import annotations

import random
from collections.abc import Callable, Sequence
from decimal import Decimal

from ..models import FailureClass
from .base import GradeResult


def blinded_pair(answer_a: str, answer_b: str, seed: int) -> tuple[str, str, int]:
    rng = random.Random(seed)
    if rng.randrange(2) == 0:
        return answer_a, answer_b, seed
    return answer_b, answer_a, seed


def judge_grade(
    answer_a: str,
    answer_b: str,
    judges: Sequence[Callable[[str, str], bool]],
    *,
    seed: int,
    agreement_ratio: float = 2 / 3,
) -> GradeResult:
    if not judges:
        return GradeResult(
            Decimal(0), None, FailureClass.JUDGE, "no_judge", "no judge configured", ()
        )
    first, second, _ = blinded_pair(answer_a, answer_b, seed)
    votes = [bool(judge(first, second)) for judge in judges]
    agreement = max(sum(votes), len(votes) - sum(votes)) / len(votes)
    if agreement < agreement_ratio:
        return GradeResult(
            Decimal(0),
            None,
            FailureClass.JUDGE,
            "judge_disagreement",
            "judge agreement is below policy threshold",
            (),
        )
    passed = sum(votes) > len(votes) / 2
    return GradeResult(
        Decimal(1 if passed else 0),
        passed,
        None if passed else FailureClass.CAPABILITY,
        "" if passed else "judge_rejected",
        "blinded judge consensus reached",
    )
