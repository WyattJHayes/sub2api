from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from .models import RecoveryEvidence, RecoveryObjectives, RecoveryObservation, RecoveryStatus
    from .verifier import RecoveryVerifier

__all__ = [
    "RecoveryEvidence",
    "RecoveryObjectives",
    "RecoveryObservation",
    "RecoveryStatus",
    "RecoveryVerifier",
]


def __getattr__(name: str) -> object:
    if name == "RecoveryVerifier":
        from .verifier import RecoveryVerifier

        return RecoveryVerifier
    if name in {"RecoveryEvidence", "RecoveryObjectives", "RecoveryObservation", "RecoveryStatus"}:
        from . import models

        return getattr(models, name)
    raise AttributeError(name)
