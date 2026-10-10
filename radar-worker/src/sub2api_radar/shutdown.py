from __future__ import annotations

import asyncio
import signal
from collections.abc import Callable, Iterator
from contextlib import contextmanager


@contextmanager
def stop_on_signals(stop: Callable[[], None]) -> Iterator[None]:
    """Request a drain without cancelling the current lease's work."""
    loop = asyncio.get_running_loop()
    previous = {}
    try:
        for stop_signal in (signal.SIGTERM, signal.SIGINT):
            handler = signal.getsignal(stop_signal)
            loop.add_signal_handler(stop_signal, stop)
            previous[stop_signal] = handler
        yield
    finally:
        for stop_signal, handler in previous.items():
            loop.remove_signal_handler(stop_signal)
            signal.signal(stop_signal, handler)
