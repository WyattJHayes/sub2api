from __future__ import annotations

import hashlib
import hmac
import json
import os
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, cast
from urllib.parse import urlsplit

MAX_REQUEST_BYTES = 1024 * 1024
MODEL_OUTPUTS = {
    "radar-synthetic-baseline": "Paris",
    "radar-synthetic-candidate": "Lyon",
}


class SyntheticUpstreamServer(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, address: tuple[str, int], api_key: str) -> None:
        self.api_key = api_key
        super().__init__(address, SyntheticUpstreamHandler)


class SyntheticUpstreamHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    @property
    def synthetic_server(self) -> SyntheticUpstreamServer:
        return cast(SyntheticUpstreamServer, self.server)

    def log_message(self, format: str, *args: object) -> None:
        return

    def do_GET(self) -> None:
        if urlsplit(self.path).path == "/health":
            self._send(HTTPStatus.OK, {"status": "ok"})
            return
        self._send(HTTPStatus.NOT_FOUND, {"error": {"code": "not_found"}})

    def do_POST(self) -> None:
        if urlsplit(self.path).path != "/v1/chat/completions":
            self._send(HTTPStatus.NOT_FOUND, {"error": {"code": "not_found"}})
            return
        if not self._authorized():
            self._send(HTTPStatus.UNAUTHORIZED, {"error": {"code": "unauthorized"}})
            return
        payload = self._read_payload()
        if payload is None:
            return
        model = payload.get("model")
        if not isinstance(model, str) or model not in MODEL_OUTPUTS:
            self._send(
                HTTPStatus.BAD_REQUEST,
                {"error": {"code": "unsupported_model"}},
            )
            return

        messages = payload.get("messages", [])
        request_digest = hashlib.sha256(
            json.dumps(messages, ensure_ascii=False, separators=(",", ":")).encode()
        ).hexdigest()
        response = {
            "id": "chatcmpl-radar-" + request_digest[:16],
            "object": "chat.completion",
            "created": 0,
            "model": model,
            "choices": [
                {
                    "index": 0,
                    "message": {"role": "assistant", "content": MODEL_OUTPUTS[model]},
                    "finish_reason": "stop",
                }
            ],
            "usage": {
                "prompt_tokens": 8,
                "completion_tokens": 1,
                "total_tokens": 9,
            },
            "system_fingerprint": "radar-synthetic-v1",
        }
        self._send(
            HTTPStatus.OK,
            response,
            extra_headers={"X-Request-ID": "radar-syn-" + request_digest[:24]},
        )

    def _authorized(self) -> bool:
        expected = f"Bearer {self.synthetic_server.api_key}"
        return hmac.compare_digest(self.headers.get("Authorization", ""), expected)

    def _read_payload(self) -> dict[str, Any] | None:
        try:
            content_length = int(self.headers.get("Content-Length", ""))
        except ValueError:
            content_length = 0
        if content_length <= 0:
            self._send(HTTPStatus.BAD_REQUEST, {"error": {"code": "invalid_json"}})
            return None
        if content_length > MAX_REQUEST_BYTES:
            self._send(
                HTTPStatus.REQUEST_ENTITY_TOO_LARGE,
                {"error": {"code": "request_too_large"}},
            )
            return None
        try:
            payload = json.loads(self.rfile.read(content_length))
        except (UnicodeDecodeError, json.JSONDecodeError):
            self._send(HTTPStatus.BAD_REQUEST, {"error": {"code": "invalid_json"}})
            return None
        if not isinstance(payload, dict):
            self._send(HTTPStatus.BAD_REQUEST, {"error": {"code": "invalid_json"}})
            return None
        return payload

    def _send(
        self,
        status: HTTPStatus,
        payload: dict[str, Any],
        *,
        extra_headers: dict[str, str] | None = None,
    ) -> None:
        body = json.dumps(payload, separators=(",", ":")).encode()
        self.send_response(status.value)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        for name, value in (extra_headers or {}).items():
            self.send_header(name, value)
        self.end_headers()
        self.wfile.write(body)


def create_server(host: str, port: int, api_key: str) -> SyntheticUpstreamServer:
    if not api_key:
        raise ValueError("synthetic upstream API key is required")
    return SyntheticUpstreamServer((host, port), api_key)


def main() -> None:
    host = os.getenv("RADAR_SYNTHETIC_HOST", "0.0.0.0")
    port = int(os.getenv("RADAR_SYNTHETIC_PORT", "8090"))
    api_key = os.getenv("RADAR_SYNTHETIC_API_KEY", "")
    server = create_server(host, port, api_key)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
