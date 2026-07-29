from __future__ import annotations

import json
import os
from dataclasses import dataclass
from enum import StrEnum
from pathlib import Path
from tempfile import NamedTemporaryFile
from typing import Any
from uuid import UUID


class LocalState(StrEnum):
    CLAIMED = "claimed"
    EXECUTING = "executing"
    EVIDENCE_READY = "evidence_ready"
    EVIDENCE_ACCEPTED = "evidence_accepted"
    TERMINAL = "terminal"


@dataclass(frozen=True)
class StateRecord:
    assignment_id: UUID
    state: LocalState
    idempotency_key: str
    evidence: dict[str, Any] | None = None
    lease_token: str | None = None
    lease_epoch: int = 0

    def to_json(self) -> dict[str, Any]:
        return {
            "assignment_id": str(self.assignment_id),
            "state": self.state.value,
            "idempotency_key": self.idempotency_key,
            "evidence": self.evidence,
            "lease_token": self.lease_token,
            "lease_epoch": self.lease_epoch,
        }

    @classmethod
    def from_json(cls, payload: dict[str, Any]) -> StateRecord:
        return cls(
            assignment_id=UUID(str(payload["assignment_id"])),
            state=LocalState(str(payload["state"])),
            idempotency_key=str(payload["idempotency_key"]),
            evidence=payload.get("evidence"),
            lease_token=payload.get("lease_token"),
            lease_epoch=int(payload.get("lease_epoch", 0)),
        )


class AtomicStateStore:
    def __init__(self, root: str | Path) -> None:
        self.root = Path(root)
        self.root.mkdir(parents=True, exist_ok=True)

    def path_for(self, assignment_id: UUID) -> Path:
        return self.root / f"{assignment_id}.json"

    def save(self, record: StateRecord) -> None:
        destination = self.path_for(record.assignment_id)
        with NamedTemporaryFile("w", encoding="utf-8", dir=self.root, delete=False) as handle:
            json.dump(record.to_json(), handle, separators=(",", ":"), sort_keys=True)
            handle.flush()
            os.fsync(handle.fileno())
            temporary = Path(handle.name)
        os.replace(temporary, destination)

    def load(self, assignment_id: UUID) -> StateRecord | None:
        path = self.path_for(assignment_id)
        try:
            payload = json.loads(path.read_text(encoding="utf-8"))
        except FileNotFoundError:
            return None
        if not isinstance(payload, dict):
            raise ValueError("state file must contain an object")
        return StateRecord.from_json(payload)

    def list_records(self) -> list[StateRecord]:
        records: list[StateRecord] = []
        for path in sorted(self.root.glob("*.json")):
            payload = json.loads(path.read_text(encoding="utf-8"))
            if isinstance(payload, dict):
                records.append(StateRecord.from_json(payload))
        return records

    def delete(self, assignment_id: UUID) -> None:
        self.path_for(assignment_id).unlink(missing_ok=True)
