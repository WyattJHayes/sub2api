from __future__ import annotations

from decimal import Decimal

from ..models import ExecutionEvidence, FailureClass
from .base import GradeResult, evidence_hashes


def protocol_grade(evidence: ExecutionEvidence) -> GradeResult:
    events = evidence.protocol_events
    done_count = sum(1 for event in events if event.get("type") == "done")
    if evidence.transport_status.startswith("4") or evidence.transport_status.startswith("5"):
        return GradeResult(
            Decimal(0),
            None,
            FailureClass.UPSTREAM,
            f"upstream_{evidence.transport_status}",
            "upstream returned an error",
            evidence_hashes(evidence),
        )
    if done_count > 1:
        return GradeResult(
            Decimal(0),
            None,
            FailureClass.PROTOCOL,
            "duplicate_terminal_event",
            "stream contained multiple terminal events",
            evidence_hashes(evidence),
        )
    if evidence.error_code:
        return GradeResult(
            Decimal(0),
            None,
            FailureClass.PROTOCOL,
            evidence.error_code,
            "gateway reported a protocol error",
            evidence_hashes(evidence),
        )
    return GradeResult(
        Decimal(1), True, None, "", "protocol invariants passed", evidence_hashes(evidence)
    )
