from __future__ import annotations

from collections.abc import Iterable
from dataclasses import dataclass


class SSEProtocolError(ValueError):
    """Raised when an upstream streaming response violates the SSE contract."""


@dataclass(frozen=True, slots=True)
class SSEEvent:
    event: str
    data: str
    event_id: str | None = None


class SSEDecoder:
    """Incrementally decode UTF-8 server-sent events without buffering a stream."""

    def __init__(self, *, max_event_bytes: int = 1_048_576) -> None:
        if max_event_bytes <= 0:
            raise ValueError("max_event_bytes must be positive")
        self._max_event_bytes = max_event_bytes
        self._buffer = b""
        self._event = "message"
        self._event_id: str | None = None
        self._data: list[str] = []
        self._event_bytes = 0

    def feed(self, chunk: bytes) -> list[SSEEvent]:
        if not isinstance(chunk, bytes):
            raise TypeError("SSE chunks must be bytes")
        self._buffer += chunk
        events: list[SSEEvent] = []
        while True:
            separator = self._buffer.find(b"\n")
            if separator < 0:
                break
            raw_line = self._buffer[:separator]
            self._buffer = self._buffer[separator + 1 :]
            if raw_line.endswith(b"\r"):
                raw_line = raw_line[:-1]
            events.extend(self._line(raw_line))
        return events

    def finish(self) -> list[SSEEvent]:
        if self._buffer:
            raise SSEProtocolError("incomplete SSE line at end of stream")
        if self._data:
            raise SSEProtocolError("unterminated SSE event")
        return []

    def _line(self, raw_line: bytes) -> list[SSEEvent]:
        if len(raw_line) > self._max_event_bytes:
            raise SSEProtocolError("SSE event exceeds limit")
        if not raw_line:
            return self._dispatch()
        if raw_line.startswith(b":"):
            return []
        if b":" not in raw_line:
            raise SSEProtocolError("malformed SSE field")
        field_bytes, value_bytes = raw_line.split(b":", 1)
        try:
            field = field_bytes.decode("utf-8")
            value = value_bytes[1:] if value_bytes.startswith(b" ") else value_bytes
            decoded = value.decode("utf-8")
        except UnicodeDecodeError as exc:
            raise SSEProtocolError("SSE field is not valid UTF-8") from exc
        self._event_bytes += len(raw_line)
        if self._event_bytes > self._max_event_bytes:
            raise SSEProtocolError("SSE event exceeds limit")
        if field == "event":
            if not decoded:
                raise SSEProtocolError("SSE event name is empty")
            self._event = decoded
        elif field == "data":
            self._data.append(decoded)
        elif field == "id":
            self._event_id = decoded
        elif field == "retry":
            if decoded and not decoded.isdecimal():
                raise SSEProtocolError("SSE retry field is invalid")
        else:
            raise SSEProtocolError(f"unknown SSE field: {field}")
        return []

    def _dispatch(self) -> list[SSEEvent]:
        if not self._data:
            self._event = "message"
            self._event_id = None
            self._event_bytes = 0
            return []
        event = SSEEvent(self._event, "\n".join(self._data), self._event_id)
        self._event = "message"
        self._event_id = None
        self._data = []
        self._event_bytes = 0
        return [event]


def parse_sse_payload(payload: bytes | bytearray | memoryview) -> list[SSEEvent]:
    decoder = SSEDecoder()
    events = decoder.feed(bytes(payload))
    events.extend(decoder.finish())
    return events


def parse_sse_lines(lines: Iterable[bytes]) -> list[SSEEvent]:
    decoder = SSEDecoder()
    events: list[SSEEvent] = []
    for line in lines:
        events.extend(decoder.feed(line))
    events.extend(decoder.finish())
    return events
