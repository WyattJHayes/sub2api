from __future__ import annotations

import hashlib
import json
from datetime import UTC, datetime
from enum import StrEnum
from typing import Any, cast
from uuid import UUID

from pydantic import AliasChoices, Field, computed_field, model_validator

from ..models import StrictModel

type JSONValue = str | int | float | bool | None | list[JSONValue] | dict[str, JSONValue]

_POSTGRES_BIGINT_MAX = 9_223_372_036_854_775_807
_EVENT_PAYLOAD_METADATA_KEYS = frozenset({"cause_event", "service_identity"})


class FaultKind(StrEnum):
    WORKER_KILL = "worker_kill"
    WORKER_NETWORK_ISOLATION = "worker_network_isolation"
    UPSTREAM_LATENCY = "upstream_latency"
    REDIS_PARTITION = "redis_partition"
    ARTIFACT_STORE_OUTAGE = "artifact_store_outage"


class TargetKind(StrEnum):
    WORKER = "worker"
    UPSTREAM = "upstream"
    REDIS = "redis"
    ARTIFACT_STORE = "artifact_store"


class ExperimentStatus(StrEnum):
    PROPOSED = "proposed"
    APPROVED = "approved"
    RUNNING = "running"
    ABORTED = "aborted"
    COMPLETED = "completed"
    FAILED = "failed"
    CANCELLED = "cancelled"


class ExperimentEventType(StrEnum):
    PROPOSED = "proposed"
    APPROVED = "approved"
    STARTED = "started"
    ABORTED = "aborted"
    COMPLETED = "completed"
    FAILED = "failed"
    CANCELLED = "cancelled"


class FaultExperiment(StrictModel):
    experiment_id: UUID = Field(
        validation_alias=AliasChoices("experiment_id", "id"),
        serialization_alias="experiment_id",
    )
    run_id: UUID
    load_plan_id: UUID | None = None
    environment: str = Field(min_length=1, max_length=40)
    fault_kind: FaultKind
    target_kind: TargetKind
    target_ref: str = Field(min_length=1, max_length=200)
    status: ExperimentStatus
    approved_by: int | None = Field(
        default=None,
        strict=True,
        gt=0,
        le=_POSTGRES_BIGINT_MAX,
    )
    abort_deadline: datetime | None = None


def canonical_utc(value: datetime) -> str:
    if value.tzinfo is None or value.utcoffset() is None:
        raise ValueError("canonical timestamps must use UTC")
    utc_value = value.astimezone(UTC)
    if value.utcoffset() != utc_value.utcoffset():
        raise ValueError("canonical timestamps must use UTC")
    base = utc_value.strftime("%Y-%m-%dT%H:%M:%S")
    if utc_value.microsecond == 0:
        return base + "Z"
    fraction = f"{utc_value.microsecond:06d}".rstrip("0")
    return f"{base}.{fraction}Z"


def canonical_json_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        allow_nan=False,
        separators=(",", ":"),
        sort_keys=True,
    ).encode("utf-8")


def _canonical_event_json(value: object) -> JSONValue:
    if value is None or isinstance(value, str | bool | int):
        return value
    if isinstance(value, float):
        if not value == value or value in (float("inf"), float("-inf")):
            raise ValueError("event payload numbers must be finite")
        return repr(value)
    if isinstance(value, list):
        return [_canonical_event_json(item) for item in value]
    if isinstance(value, dict):
        converted: dict[str, JSONValue] = {}
        for key, item in value.items():
            if not isinstance(key, str):
                raise ValueError("event payload keys must be strings")
            converted[key] = _canonical_event_json(item)
        return converted
    raise ValueError("event payload must contain canonical JSON values")


class StateEvent(StrictModel):
    experiment_id: UUID
    run_id: UUID
    event_type: ExperimentEventType
    actor_id: int | None = Field(
        default=None,
        strict=True,
        gt=0,
        le=_POSTGRES_BIGINT_MAX,
    )
    service_identity: str = Field(min_length=1, max_length=200)
    cause_event: str = Field(min_length=1, max_length=200)
    created_at: datetime
    canonical_payload_bytes: bytes = Field(min_length=2, exclude=True, repr=False)
    event_hash: str = Field(pattern=r"^[0-9a-f]{64}$")

    @computed_field(repr=False)  # type: ignore[prop-decorator]
    @property
    def payload(self) -> dict[str, JSONValue]:
        value = json.loads(self.canonical_payload_bytes)
        if not isinstance(value, dict):
            raise ValueError("event payload must be a JSON object")
        return cast(dict[str, JSONValue], value)

    @classmethod
    def build(
        cls,
        *,
        experiment_id: UUID,
        run_id: UUID,
        event_type: ExperimentEventType,
        actor_id: int | None,
        service_identity: str,
        cause_event: str,
        created_at: datetime,
        payload: dict[str, JSONValue] | None = None,
    ) -> StateEvent:
        normalized = _canonical_event_json(payload or {})
        if not isinstance(normalized, dict):
            raise ValueError("event payload must be a JSON object")
        if _EVENT_PAYLOAD_METADATA_KEYS.intersection(normalized):
            raise ValueError("event payload contains reserved metadata keys")
        normalized["cause_event"] = cause_event
        normalized["service_identity"] = service_identity
        canonical_payload = canonical_json_bytes(normalized)
        fields: dict[str, JSONValue] = {
            "actor_id": actor_id,
            "created_at": canonical_utc(created_at),
            "event_type": event_type.value,
            "experiment_id": str(experiment_id),
            "payload": cast(dict[str, JSONValue], json.loads(canonical_payload)),
            "run_id": str(run_id),
        }
        event_hash = hashlib.sha256(canonical_json_bytes(fields)).hexdigest()
        return cls(
            experiment_id=experiment_id,
            run_id=run_id,
            event_type=event_type,
            actor_id=actor_id,
            service_identity=service_identity,
            cause_event=cause_event,
            created_at=created_at,
            canonical_payload_bytes=canonical_payload,
            event_hash=event_hash,
        )

    def canonical_bytes(self) -> bytes:
        return canonical_json_bytes(
            {
                "actor_id": self.actor_id,
                "created_at": canonical_utc(self.created_at),
                "event_type": self.event_type.value,
                "experiment_id": str(self.experiment_id),
                "payload": self.payload,
                "run_id": str(self.run_id),
            }
        )

    @model_validator(mode="after")
    def event_hash_matches_canonical_bytes(self) -> StateEvent:
        if self.payload.get("cause_event") != self.cause_event:
            raise ValueError("event payload cause does not match event identity")
        if self.payload.get("service_identity") != self.service_identity:
            raise ValueError("event payload service does not match event identity")
        if canonical_json_bytes(self.payload) != self.canonical_payload_bytes:
            raise ValueError("event payload bytes are not canonical JSON")
        expected = hashlib.sha256(self.canonical_bytes()).hexdigest()
        if self.event_hash != expected:
            raise ValueError("event_hash does not match canonical event bytes")
        return self
