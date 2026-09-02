from __future__ import annotations

import json
import re
import unicodedata
from decimal import Decimal, InvalidOperation
from typing import Any

from ..models import ExecutionEvidence
from .base import GradeResult, evidence_hashes


def normalize_text(value: str) -> str:
    value = unicodedata.normalize("NFKC", value).replace("\r\n", "\n")
    return re.sub(r"\s+", " ", value).strip()


def normalize_value(value: Any) -> Any:
    if isinstance(value, str):
        return normalize_text(value)
    if isinstance(value, list):
        return [normalize_value(item) for item in value]
    if isinstance(value, dict):
        return {str(key): normalize_value(item) for key, item in sorted(value.items())}
    return value


def _numeric_equal(left: Any, right: Any, tolerance: Decimal = Decimal("0")) -> bool:
    try:
        left_decimal = Decimal(str(left))
        right_decimal = Decimal(str(right))
    except (InvalidOperation, ValueError):
        return False
    return abs(left_decimal - right_decimal) <= tolerance


def exact_grade(
    expected: Any,
    actual: Any,
    evidence: ExecutionEvidence,
    *,
    numeric_tolerance: Decimal = Decimal("0"),
) -> GradeResult:
    normalized_expected = normalize_value(expected)
    normalized_actual = normalize_value(actual)
    matches = (
        _numeric_equal(normalized_expected, normalized_actual, numeric_tolerance)
        if isinstance(normalized_expected, int | float | Decimal)
        and isinstance(normalized_actual, int | float | Decimal)
        else normalized_expected == normalized_actual
    )
    return GradeResult(
        Decimal(1 if matches else 0),
        matches,
        None
        if matches
        else __import__("sub2api_radar.models", fromlist=["FailureClass"]).FailureClass.CAPABILITY,
        "" if matches else "answer_mismatch",
        "exact comparison passed" if matches else "normalized output differs from expected value",
        evidence_hashes(evidence),
    )


def json_grade(
    expected: str | dict[str, Any], actual: str, evidence: ExecutionEvidence
) -> GradeResult:
    try:
        expected_value = json.loads(expected) if isinstance(expected, str) else expected
        actual_value = json.loads(actual)
    except (TypeError, json.JSONDecodeError):
        return GradeResult(
            Decimal(0),
            False,
            __import__("sub2api_radar.models", fromlist=["FailureClass"]).FailureClass.CAPABILITY,
            "invalid_json_answer",
            "output is not valid JSON",
            evidence_hashes(evidence),
        )
    return exact_grade(expected_value, actual_value, evidence)
