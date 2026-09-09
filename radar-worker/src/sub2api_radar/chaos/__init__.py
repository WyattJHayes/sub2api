from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from .controller import (
        ChaosController,
        ExperimentRejected,
        FaultAdapter,
        FaultExecutionError,
        GuardrailPolicy,
    )
    from .models import (
        ExperimentEventType,
        ExperimentStatus,
        FaultExperiment,
        FaultKind,
        StateEvent,
        TargetKind,
    )

__all__ = [
    "ChaosController",
    "ExperimentEventType",
    "ExperimentRejected",
    "ExperimentStatus",
    "FaultAdapter",
    "FaultExecutionError",
    "FaultExperiment",
    "FaultKind",
    "GuardrailPolicy",
    "StateEvent",
    "TargetKind",
]


def __getattr__(name: str) -> object:
    if name in {
        "ChaosController",
        "ExperimentRejected",
        "FaultAdapter",
        "FaultExecutionError",
        "GuardrailPolicy",
    }:
        from . import controller

        return getattr(controller, name)
    if name in {
        "ExperimentEventType",
        "ExperimentStatus",
        "FaultExperiment",
        "FaultKind",
        "StateEvent",
        "TargetKind",
    }:
        from . import models

        return getattr(models, name)
    raise AttributeError(name)
