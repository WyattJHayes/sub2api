from __future__ import annotations

from dataclasses import dataclass
from decimal import Decimal

from ..models import ExecutionEvidence, FailureClass


@dataclass(frozen=True)
class GradeResult:
    score: Decimal
    passed: bool | None
    failure_class: FailureClass | None
    failure_code: str
    explanation: str
    evidence_hashes: tuple[str, ...] = ()

    @classmethod
    def capability(
        cls, passed: bool, explanation: str, *, evidence_hashes: tuple[str, ...] = ()
    ) -> GradeResult:
        return cls(
            Decimal(1 if passed else 0),
            passed,
            None if passed else FailureClass.CAPABILITY,
            "" if passed else "answer_mismatch",
            explanation,
            evidence_hashes,
        )


def evidence_hashes(evidence: ExecutionEvidence) -> tuple[str, ...]:
    return tuple(sorted({artifact.sha256 for artifact in evidence.artifacts}))


def infrastructure_result(
    code: str, explanation: str, evidence: ExecutionEvidence | None = None
) -> GradeResult:
    return GradeResult(
        Decimal(0),
        None,
        FailureClass.INFRASTRUCTURE,
        code,
        explanation,
        evidence_hashes(evidence) if evidence else (),
    )


def protocol_result(
    code: str, explanation: str, evidence: ExecutionEvidence | None = None
) -> GradeResult:
    return GradeResult(
        Decimal(0),
        None,
        FailureClass.PROTOCOL,
        code,
        explanation,
        evidence_hashes(evidence) if evidence else (),
    )
