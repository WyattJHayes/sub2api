from typing import TYPE_CHECKING

from .histogram import FixedHistogram
from .models import LoadCell, ReliabilityWindow, RequestMeasurement

if TYPE_CHECKING:
    from .runner import LoadGenerator


def __getattr__(name: str) -> object:
    if name == "LoadGenerator":
        from .runner import LoadGenerator

        return LoadGenerator
    raise AttributeError(name)

__all__ = ["FixedHistogram", "LoadCell", "LoadGenerator", "ReliabilityWindow", "RequestMeasurement"]
