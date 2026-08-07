from __future__ import annotations

import argparse
import asyncio
import logging
import math
import os
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Protocol
from uuid import UUID

import httpx

from ..config import Settings, get_settings
from ..control_plane import ControlPlaneClient
from .models import (
    ExperimentEventType,
    ExperimentStatus,
    FaultExperiment,
    FaultKind,
    JSONValue,
    StateEvent,
    TargetKind,
)

log = logging.getLogger(__name__)


class ExperimentRejected(ValueError):
    """Raised before any adapter side effect when an experiment is unsafe."""


class FaultExecutionError(RuntimeError):
    """Raised after an adapter failure has triggered an immediate rollback attempt."""


def _credential(name: str) -> str:
    value = os.environ.get(name, "").strip()
    if len(value) < 32 or value.lower().startswith(("change-me", "placeholder", "test-")):
        raise SystemExit(f"credential {name} is required and must be a dedicated secret")
    return value


class FaultAdapter(Protocol):
    def inject(self, experiment: FaultExperiment) -> Mapping[str, JSONValue]: ...

    def rollback(self, experiment: FaultExperiment) -> Mapping[str, JSONValue]: ...


class ControlPlaneFaultAdapter:
    """Execute a pre-approved fault through the authenticated control plane."""

    def __init__(self, settings: Settings) -> None:
        self._settings = settings
        self._client = httpx.Client(
            timeout=httpx.Timeout(
                settings.request_timeout_seconds, connect=settings.connect_timeout_seconds
            )
        )

    def close(self) -> None:
        self._client.close()

    def inject(self, experiment: FaultExperiment) -> Mapping[str, JSONValue]:
        return self._action(experiment, "inject")

    def rollback(self, experiment: FaultExperiment) -> Mapping[str, JSONValue]:
        return self._action(experiment, "rollback")

    def publish_event(self, event: StateEvent) -> None:
        response = self._client.post(
            self._settings.control_plane_url.rstrip("/")
            + f"/internal/radar/v1/fault-experiments/{event.experiment_id}/events",
            headers=self._headers(event.event_hash),
            json=event.model_dump(mode="json"),
        )
        response.raise_for_status()

    def _action(self, experiment: FaultExperiment, action: str) -> Mapping[str, JSONValue]:
        response = self._client.post(
            self._settings.control_plane_url.rstrip("/")
            + f"/internal/radar/v1/fault-experiments/{experiment.experiment_id}/actions",
            headers=self._headers(f"{experiment.experiment_id}:{action}"),
            json={
                "action": action,
                "fault_kind": experiment.fault_kind.value,
                "target_kind": experiment.target_kind.value,
                "target_ref": experiment.target_ref,
            },
        )
        response.raise_for_status()
        payload = response.json()
        if not isinstance(payload, Mapping):
            raise RuntimeError("control plane returned an invalid fault adapter receipt")
        data = payload.get("data", payload)
        if not isinstance(data, Mapping):
            raise RuntimeError("control plane returned an invalid fault adapter receipt")
        return dict(data)

    def _headers(self, idempotency_key: str) -> dict[str, str]:
        return {
            "Authorization": f"Bearer {self._settings.worker_token.get_secret_value()}",
            "Accept": "application/json",
            "Content-Type": "application/json",
            "Idempotency-Key": idempotency_key,
            "User-Agent": "sub2api-radar-chaos/0.1",
        }


@dataclass(frozen=True)
class _ActiveFault:
    adapter: FaultAdapter
    experiment: FaultExperiment


@dataclass(frozen=True)
class GuardrailPolicy:
    max_customer_error_rate_delta: float = 0.005
    max_customer_p99_ratio: float = 1.20
    min_control_plane_availability: float = 0.999

    def __post_init__(self) -> None:
        thresholds = (
            self.max_customer_error_rate_delta,
            self.max_customer_p99_ratio,
            self.min_control_plane_availability,
        )
        if not all(math.isfinite(value) for value in thresholds):
            raise ValueError("guardrail thresholds must be finite")
        if not 0 <= self.max_customer_error_rate_delta <= 1:
            raise ValueError("guardrail error-rate threshold must be between zero and one")
        if self.max_customer_p99_ratio < 1:
            raise ValueError("guardrail P99 ratio must be at least one")
        if not 0 <= self.min_control_plane_availability <= 1:
            raise ValueError("guardrail availability threshold must be between zero and one")

    @classmethod
    def default(cls) -> GuardrailPolicy:
        return cls()

    def stop_reasons(self, signal: Mapping[str, object]) -> tuple[str, ...]:
        reasons: list[str] = []
        self._append_above(
            reasons,
            signal,
            "customer_error_rate_delta",
            self.max_customer_error_rate_delta,
        )
        self._append_above(
            reasons,
            signal,
            "customer_p99_ratio",
            self.max_customer_p99_ratio,
        )
        self._append_below(
            reasons,
            signal,
            "control_plane_availability",
            self.min_control_plane_availability,
        )
        for key in (
            "data_hash_consistent",
            "alert_delivery_ok",
            "budget_ledger_consistent",
        ):
            if key in signal and signal[key] is not True:
                reasons.append(key)
        return tuple(reasons)

    def must_stop(self, signal: Mapping[str, object]) -> bool:
        return bool(self.stop_reasons(signal))

    @staticmethod
    def _append_above(
        reasons: list[str], signal: Mapping[str, object], key: str, limit: float
    ) -> None:
        if key not in signal:
            return
        value = signal[key]
        if (
            isinstance(value, bool)
            or not isinstance(value, int | float)
            or not math.isfinite(value)
            or value > limit
        ):
            reasons.append(key)

    @staticmethod
    def _append_below(
        reasons: list[str], signal: Mapping[str, object], key: str, limit: float
    ) -> None:
        if key not in signal:
            return
        value = signal[key]
        if (
            isinstance(value, bool)
            or not isinstance(value, int | float)
            or not math.isfinite(value)
            or value < limit
        ):
            reasons.append(key)


_ALLOWED_PAIRS = frozenset(
    {
        (FaultKind.WORKER_KILL, TargetKind.WORKER),
        (FaultKind.WORKER_NETWORK_ISOLATION, TargetKind.WORKER),
        (FaultKind.UPSTREAM_LATENCY, TargetKind.UPSTREAM),
        (FaultKind.REDIS_PARTITION, TargetKind.REDIS),
        (FaultKind.ARTIFACT_STORE_OUTAGE, TargetKind.ARTIFACT_STORE),
    }
)

_PRODUCTION_PAIRS = frozenset(
    {
        (FaultKind.WORKER_KILL, TargetKind.WORKER),
        (FaultKind.WORKER_NETWORK_ISOLATION, TargetKind.WORKER),
    }
)

_ALLOWED_ENVIRONMENTS = frozenset({"staging", "preproduction", "production"})
_RESERVED_TERMINAL_EVENT_KEYS = frozenset(
    {"adapter_receipt", "adapter_receipt_valid", "from_status", "to_status"}
)


class ChaosController:
    def __init__(
        self,
        *,
        adapters: Mapping[FaultKind | str, FaultAdapter],
        service_identity: str,
        clock: Callable[[], datetime] | None = None,
        guardrails: GuardrailPolicy | None = None,
    ) -> None:
        if not service_identity.strip():
            raise ValueError("service identity is required")
        self._adapters = {FaultKind(kind): adapter for kind, adapter in adapters.items()}
        self._service_identity = service_identity
        self._clock = clock or (lambda: datetime.now(UTC))
        self._guardrails = guardrails or GuardrailPolicy.default()
        self._active: dict[UUID, _ActiveFault] = {}

    def start(self, experiment: FaultExperiment, *, actor_id: int | None = None) -> StateEvent:
        now = self._clock()
        adapter, deadline = self._validate_start(experiment, now)
        self._active[experiment.experiment_id] = _ActiveFault(adapter, experiment)
        try:
            receipt = dict(adapter.inject(experiment))
        except Exception as exc:
            self._rollback_failed_start(experiment, adapter)
            raise FaultExecutionError("fault adapter injection failed and was rolled back") from exc

        completed_at = self._clock()
        try:
            if (
                completed_at.tzinfo is None
                or completed_at.utcoffset() is None
                or completed_at.utcoffset() != UTC.utcoffset(completed_at)
            ):
                raise ExperimentRejected("controller clock must use UTC")
            if deadline <= completed_at:
                raise ExperimentRejected("abort deadline expired during fault injection")
            return self._event(
                experiment,
                ExperimentEventType.STARTED,
                actor_id=actor_id,
                cause_event="approved-experiment",
                created_at=completed_at,
                payload={
                    "adapter_receipt": receipt,
                    "fault_kind": experiment.fault_kind.value,
                    "from_status": experiment.status.value,
                    "target_kind": experiment.target_kind.value,
                    "target_ref": experiment.target_ref,
                    "to_status": ExperimentStatus.RUNNING.value,
                },
            )
        except ExperimentRejected:
            self._rollback_failed_start(experiment, adapter)
            raise
        except Exception as exc:
            self._rollback_failed_start(experiment, adapter)
            raise FaultExecutionError(
                "fault adapter result could not produce an auditable event and was rolled back"
            ) from exc

    def enforce_guardrails(
        self,
        experiment: FaultExperiment,
        signal: Mapping[str, object],
        *,
        actor_id: int | None = None,
    ) -> StateEvent | None:
        reasons = self._guardrails.stop_reasons(signal)
        if not reasons:
            return None
        return self.abort(
            experiment,
            actor_id=actor_id,
            cause_event="guardrail-stop",
            details={
                "guardrail_reasons": list(reasons),
                "signal": self._json_mapping(signal),
            },
        )

    def abort(
        self,
        experiment: FaultExperiment,
        *,
        actor_id: int | None = None,
        cause_event: str = "operator-stop",
        details: Mapping[str, JSONValue] | None = None,
    ) -> StateEvent:
        return self._rollback(
            experiment,
            event_type=ExperimentEventType.ABORTED,
            terminal_status=ExperimentStatus.ABORTED,
            actor_id=actor_id,
            cause_event=cause_event,
            details=details,
        )

    def complete(
        self,
        experiment: FaultExperiment,
        *,
        actor_id: int | None = None,
    ) -> StateEvent:
        return self._rollback(
            experiment,
            event_type=ExperimentEventType.COMPLETED,
            terminal_status=ExperimentStatus.COMPLETED,
            actor_id=actor_id,
            cause_event="planned-stop",
            details=None,
        )

    def _rollback(
        self,
        experiment: FaultExperiment,
        *,
        event_type: ExperimentEventType,
        terminal_status: ExperimentStatus,
        actor_id: int | None,
        cause_event: str,
        details: Mapping[str, JSONValue] | None,
    ) -> StateEvent:
        active = self._active.get(experiment.experiment_id)
        if active is None:
            adapter = self._validate_external_rollback(experiment)
        else:
            if self._execution_identity(active.experiment) != self._execution_identity(experiment):
                raise ExperimentRejected("active fault execution identity cannot be changed")
            adapter = active.adapter
        if details and _RESERVED_TERMINAL_EVENT_KEYS.intersection(details):
            raise ExperimentRejected("terminal event details contain reserved keys")
        event_time = self._clock()
        preview_payload: dict[str, JSONValue] = {
            "adapter_receipt": {},
            "adapter_receipt_valid": True,
            "from_status": ExperimentStatus.RUNNING.value,
            "to_status": terminal_status.value,
        }
        if details:
            preview_payload.update(details)
        try:
            self._event(
                experiment,
                event_type,
                actor_id=actor_id,
                cause_event=cause_event,
                created_at=event_time,
                payload=preview_payload,
            )
        except (TypeError, ValueError) as exc:
            raise ExperimentRejected("terminal event metadata is invalid") from exc

        raw_receipt = adapter.rollback(experiment)
        receipt, receipt_valid = self._safe_adapter_receipt(raw_receipt)
        payload = dict(preview_payload)
        payload["adapter_receipt"] = receipt
        payload["adapter_receipt_valid"] = receipt_valid
        event = self._event(
            experiment,
            event_type,
            actor_id=actor_id,
            cause_event=cause_event,
            created_at=event_time,
            payload=payload,
        )
        self._active.pop(experiment.experiment_id, None)
        return event

    def _validate_start(
        self, experiment: FaultExperiment, now: datetime
    ) -> tuple[FaultAdapter, datetime]:
        if now.tzinfo is None or now.utcoffset() is None or now.utcoffset() != UTC.utcoffset(now):
            raise ExperimentRejected("controller clock must use UTC")
        if experiment.status is not ExperimentStatus.APPROVED:
            raise ExperimentRejected("experiment must be in approved status")
        if experiment.approved_by is None:
            raise ExperimentRejected("experiment must identify its approver")
        deadline = experiment.abort_deadline
        if deadline is None:
            raise ExperimentRejected("experiment requires an abort deadline")
        if (
            deadline.tzinfo is None
            or deadline.utcoffset() is None
            or deadline.utcoffset() != UTC.utcoffset(deadline)
        ):
            raise ExperimentRejected("abort deadline must use UTC")
        if deadline <= now:
            raise ExperimentRejected("abort deadline has expired")
        environment = experiment.environment.strip().lower()
        if environment not in _ALLOWED_ENVIRONMENTS:
            raise ExperimentRejected("fault experiment environment is not allowed")
        pair = (experiment.fault_kind, experiment.target_kind)
        if pair not in _ALLOWED_PAIRS:
            raise ExperimentRejected("fault kind and target kind are not an allowed pair")
        if (
            environment == "production"
            and pair not in _PRODUCTION_PAIRS
        ):
            raise ExperimentRejected("fault kind and target are not allowed in production")
        if not experiment.target_ref.strip():
            raise ExperimentRejected("fault target reference is required")
        adapter = self._adapters.get(experiment.fault_kind)
        if adapter is None:
            raise ExperimentRejected("fault kind has no injected adapter")
        if experiment.experiment_id in self._active:
            raise ExperimentRejected("experiment already has an active fault")
        return adapter, deadline

    def _validate_external_rollback(self, experiment: FaultExperiment) -> FaultAdapter:
        if experiment.status is not ExperimentStatus.RUNNING:
            raise ExperimentRejected("experiment has no active injected fault to rollback")
        if experiment.approved_by is None:
            raise ExperimentRejected("running experiment must identify its approver")
        environment = experiment.environment.strip().lower()
        if environment not in _ALLOWED_ENVIRONMENTS:
            raise ExperimentRejected("fault experiment environment is not allowed")
        pair = (experiment.fault_kind, experiment.target_kind)
        if pair not in _ALLOWED_PAIRS:
            raise ExperimentRejected("fault kind and target kind are not an allowed pair")
        if (
            environment == "production"
            and pair not in _PRODUCTION_PAIRS
        ):
            raise ExperimentRejected("fault kind and target are not allowed in production")
        if not experiment.target_ref.strip():
            raise ExperimentRejected("fault target reference is required")
        adapter = self._adapters.get(experiment.fault_kind)
        if adapter is None:
            raise ExperimentRejected("fault kind has no injected adapter")
        return adapter

    @staticmethod
    def _execution_identity(experiment: FaultExperiment) -> tuple[object, ...]:
        return (
            experiment.experiment_id,
            experiment.run_id,
            experiment.load_plan_id,
            experiment.environment.strip().lower(),
            experiment.fault_kind,
            experiment.target_kind,
            experiment.target_ref,
            experiment.approved_by,
            experiment.abort_deadline,
        )

    def _rollback_failed_start(
        self, experiment: FaultExperiment, adapter: FaultAdapter
    ) -> None:
        try:
            adapter.rollback(experiment)
        except Exception as exc:
            raise FaultExecutionError(
                "fault start failed and adapter rollback also failed"
            ) from exc
        del self._active[experiment.experiment_id]

    @classmethod
    def _safe_adapter_receipt(
        cls, value: object
    ) -> tuple[dict[str, JSONValue], bool]:
        try:
            converted = cls._strict_json_value(value)
        except ValueError:
            return {}, False
        if not isinstance(converted, dict):
            return {}, False
        return converted, True

    @classmethod
    def _strict_json_value(cls, value: object) -> JSONValue:
        if value is None or isinstance(value, str | bool | int):
            return value
        if isinstance(value, float):
            if not math.isfinite(value):
                raise ValueError("non-finite JSON number")
            return value
        if isinstance(value, list):
            return [cls._strict_json_value(item) for item in value]
        if isinstance(value, Mapping):
            converted: dict[str, JSONValue] = {}
            for key, item in value.items():
                if not isinstance(key, str):
                    raise ValueError("JSON object keys must be strings")
                converted[key] = cls._strict_json_value(item)
            return converted
        raise ValueError("value is not canonical JSON")

    def _event(
        self,
        experiment: FaultExperiment,
        event_type: ExperimentEventType,
        *,
        actor_id: int | None,
        cause_event: str,
        created_at: datetime,
        payload: dict[str, JSONValue],
    ) -> StateEvent:
        return StateEvent.build(
            experiment_id=experiment.experiment_id,
            run_id=experiment.run_id,
            event_type=event_type,
            actor_id=actor_id,
            service_identity=self._service_identity,
            cause_event=cause_event,
            created_at=created_at,
            payload=payload,
        )

    @staticmethod
    def _json_mapping(signal: Mapping[str, object]) -> dict[str, JSONValue]:
        converted: dict[str, JSONValue] = {}
        for key, value in signal.items():
            if isinstance(value, float) and not math.isfinite(value):
                converted[key] = str(value)
            elif value is None or isinstance(value, str | int | float | bool):
                converted[key] = value
            else:
                converted[key] = str(value)
        return converted


def _controlled_hold_window(
    experiment: FaultExperiment, requested_seconds: float
) -> tuple[float, str]:
    if not math.isfinite(requested_seconds) or requested_seconds < 0:
        raise ExperimentRejected("automatic rollback delay must be a finite non-negative number")
    deadline = experiment.abort_deadline
    if deadline is None:
        raise ExperimentRejected("automatic rollback requires an abort deadline")
    now = datetime.now(UTC)
    remaining = max(0.0, (deadline - now).total_seconds())
    effective = min(requested_seconds, remaining)
    cause_event = (
        "auto-rollback-timeout"
        if effective >= requested_seconds
        else "abort-deadline-guard"
    )
    return effective, cause_event


async def _execute_controlled_experiment(
    *,
    controller: ChaosController,
    experiment: FaultExperiment,
    actor_id: int | None,
    auto_rollback_seconds: float | None,
    publish_event: Callable[[StateEvent], None] | None = None,
) -> StateEvent:
    """Inject one approved fault and optionally roll it back after a bounded hold."""
    started = controller.start(experiment, actor_id=actor_id)
    if publish_event is not None:
        try:
            publish_event(started)
        except Exception as exc:
            try:
                aborted = controller.abort(
                    experiment,
                    actor_id=actor_id,
                    cause_event="control-plane-event-publish-failed",
                    details={"publish_error": str(exc)},
                )
                try:
                    publish_event(aborted)
                except Exception:
                    log.exception("failed to publish chaos rollback event", exc_info=True)
            except Exception as rollback_exc:
                raise FaultExecutionError(
                    "started event publication failed and rollback could not be audited"
                ) from rollback_exc
            raise FaultExecutionError(
                "started event publication failed and fault was rolled back"
            ) from exc

    if auto_rollback_seconds is None:
        return started

    effective_seconds, cause_event = _controlled_hold_window(
        experiment, auto_rollback_seconds
    )
    try:
        if effective_seconds > 0:
            await asyncio.sleep(effective_seconds)
    except asyncio.CancelledError:
        aborted = controller.abort(
            experiment,
            actor_id=actor_id,
            cause_event="controller-cancelled",
            details={
                "requested_hold_seconds": auto_rollback_seconds,
                "effective_hold_seconds": effective_seconds,
            },
        )
        if publish_event is not None:
            try:
                publish_event(aborted)
            except Exception:
                log.exception("failed to publish cancellation rollback event", exc_info=True)
        raise

    aborted = controller.abort(
        experiment,
        actor_id=actor_id,
        cause_event=cause_event,
        details={
            "requested_hold_seconds": auto_rollback_seconds,
            "effective_hold_seconds": effective_seconds,
        },
    )
    if publish_event is not None:
        try:
            publish_event(aborted)
        except Exception as exc:
            raise FaultExecutionError(
                "automatic rollback completed but its terminal event could not be audited"
            ) from exc
    return aborted


async def run(
    *,
    controller: ChaosController | None = None,
    experiment: FaultExperiment | None = None,
    actor_id: int | None = None,
    settings: Settings | None = None,
) -> StateEvent:
    """Execute one approved experiment and persist its state event.

    Embedding hosts may inject the controller and experiment for deterministic
    tests. The normal entrypoint loads both from the authenticated control
    plane so a local process cannot invent an experiment or bypass approval.
    """
    if (controller is None) != (experiment is None):
        raise RuntimeError("chaos controller and approved experiment must be supplied together")
    if controller is not None and experiment is not None:
        hold_seconds = (
            getattr(settings, "chaos_auto_rollback_seconds", None)
            if settings is not None
            else None
        )
        return await _execute_controlled_experiment(
            controller=controller,
            experiment=experiment,
            actor_id=actor_id,
            auto_rollback_seconds=hold_seconds,
        )

    try:
        settings = settings or get_settings()
    except Exception as exc:
        raise RuntimeError(
            "chaos controller and approved experiment must be injected or valid settings supplied"
        ) from exc
    if settings.fault_experiment_id is None:
        raise RuntimeError(
            "RADAR_FAULT_EXPERIMENT_ID is required for an unmanaged chaos run; "
            "inject an experiment for embedded execution"
        )

    async with ControlPlaneClient(settings) as control_plane:
        payload = await control_plane.get_fault_experiment(settings.fault_experiment_id)
    raw_experiment = payload.get("experiment", payload)
    if not isinstance(raw_experiment, Mapping):
        raise RuntimeError("control plane returned an invalid fault experiment")
    loaded_experiment = FaultExperiment.model_validate(raw_experiment)
    if loaded_experiment.status is not ExperimentStatus.APPROVED:
        raise ExperimentRejected("control plane fault experiment is not approved")

    adapter = ControlPlaneFaultAdapter(settings)
    managed_controller = ChaosController(
        adapters={loaded_experiment.fault_kind: adapter},
        service_identity=f"{settings.worker_id}@{settings.region}",
    )
    try:
        return await _execute_controlled_experiment(
            controller=managed_controller,
            experiment=loaded_experiment,
            actor_id=actor_id,
            auto_rollback_seconds=settings.chaos_auto_rollback_seconds,
            publish_event=adapter.publish_event,
        )
    finally:
        adapter.close()


def main(argv: list[str] | None = None) -> None:
    parser = argparse.ArgumentParser(description="Sub2API Radar controlled fault controller")
    parser.add_argument("--version", action="version", version="0.1.0")
    parser.parse_args(argv)
    token = _credential("RADAR_CHAOS_CONTROLLER_TOKEN")
    if not os.environ.get("RADAR_WORKER_TOKEN", "").strip():
        os.environ["RADAR_WORKER_TOKEN"] = token
    settings = get_settings()
    asyncio.run(run(settings=settings))


if __name__ == "__main__":
    main()
