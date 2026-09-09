from __future__ import annotations

from decimal import Decimal
from typing import Any

from ..models import ExecutionEvidence, FailureClass
from .base import GradeResult, evidence_hashes


def _canonical_call(call: dict[str, Any]) -> tuple[Any, ...]:
    arguments = call.get("arguments", {})
    if isinstance(arguments, str):
        arguments = arguments.strip()
    return call.get("name"), arguments


def tool_call_grade(
    expected: list[dict[str, Any]], evidence: ExecutionEvidence, *, unordered: bool = False
) -> GradeResult:
    actual = list(evidence.tool_calls)
    left = [_canonical_call(item) for item in expected]
    right = [_canonical_call(item) for item in actual]
    if unordered:
        left.sort(key=repr)
        right.sort(key=repr)
    passed = left == right
    return GradeResult(
        Decimal(1 if passed else 0),
        passed,
        None if passed else FailureClass.CAPABILITY,
        "" if passed else "tool_call_mismatch",
        "tool call contract passed" if passed else "tool call sequence or arguments differ",
        evidence_hashes(evidence),
    )
