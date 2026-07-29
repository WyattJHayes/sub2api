from __future__ import annotations

import asyncio
import hashlib
import json
import random
from collections.abc import Mapping
from typing import Any
from uuid import UUID

import httpx

from .config import Settings
from .models import (
    AggregateSubmission,
    AnalysisLease,
    ArtifactConfirmation,
    ArtifactPresignRequest,
    ArtifactReceipt,
    AssignmentLease,
    EvidenceReceipt,
    ExecutionEvidence,
    GradingLease,
    ScoreSubmission,
)


class ControlPlaneError(RuntimeError):
    def __init__(self, status_code: int, message: str, payload: Any = None) -> None:
        super().__init__(message)
        self.status_code = status_code
        self.payload = payload


class LeaseFencedError(ControlPlaneError):
    pass


class ControlPlaneClient:
    def __init__(self, settings: Settings, client: httpx.AsyncClient | None = None) -> None:
        self.settings = settings
        self._client = client
        self._owns_client = client is None

    async def __aenter__(self) -> ControlPlaneClient:
        await self.start()
        return self

    async def __aexit__(self, *_: object) -> None:
        await self.close()

    async def start(self) -> None:
        if self._client is None:
            timeout = httpx.Timeout(
                self.settings.request_timeout_seconds, connect=self.settings.connect_timeout_seconds
            )
            self._client = httpx.AsyncClient(timeout=timeout)

    async def close(self) -> None:
        if self._client is not None and self._owns_client:
            await self._client.aclose()
            self._client = None

    def _headers(self, idempotency_key: str | None = None) -> dict[str, str]:
        headers = {
            "Authorization": f"Bearer {self.settings.worker_token.get_secret_value()}",
            "Accept": "application/json",
            "User-Agent": "sub2api-radar-worker/0.1",
        }
        if idempotency_key:
            headers["Idempotency-Key"] = idempotency_key
        return headers

    def _key(self, resource: UUID, action: str) -> str:
        return hashlib.sha256(f"{resource}:{action}".encode()).hexdigest()

    async def _request(
        self, method: str, path: str, *, json_body: Any = None, key: str | None = None
    ) -> Mapping[str, Any]:
        await self.start()
        assert self._client is not None
        attempts = 0
        while True:
            try:
                response = await self._client.request(
                    method,
                    f"{self.settings.control_plane_url}{path}",
                    json=json_body,
                    headers=self._headers(key),
                )
            except (
                httpx.ConnectError,
                httpx.ConnectTimeout,
                httpx.ReadTimeout,
                httpx.NetworkError,
            ):
                if attempts >= 3:
                    raise
                await asyncio.sleep(min(2**attempts, 8) + random.random() / 10)
                attempts += 1
                continue
            if response.status_code in {408, 429} or response.status_code >= 500:
                if attempts >= 3:
                    return self._raise_response(response)
                retry_after = response.headers.get("Retry-After")
                delay = (
                    float(retry_after)
                    if retry_after and retry_after.isdigit()
                    else min(2**attempts, 8)
                )
                await asyncio.sleep(delay)
                attempts += 1
                continue
            return self._decode(response)

    def _decode(self, response: httpx.Response) -> Mapping[str, Any]:
        if response.status_code == 409:
            raise LeaseFencedError(response.status_code, "lease fenced", self._payload(response))
        if response.is_error:
            raise ControlPlaneError(
                response.status_code,
                f"control plane returned {response.status_code}",
                self._payload(response),
            )
        payload = response.json() if response.content else {}
        if not isinstance(payload, Mapping):
            raise ControlPlaneError(
                response.status_code, "control plane returned a non-object payload", payload
            )
        return payload

    def _raise_response(self, response: httpx.Response) -> Mapping[str, Any]:
        return self._decode(response)

    @staticmethod
    def _payload(response: httpx.Response) -> Any:
        try:
            return response.json()
        except json.JSONDecodeError:
            return response.text

    @staticmethod
    def _data(payload: Mapping[str, Any]) -> Any:
        return payload.get("data", payload)

    @classmethod
    def _lease(cls, payload: Mapping[str, Any]) -> Mapping[str, Any] | None:
        data = cls._data(payload)
        if data in (None, {}):
            return None
        if not isinstance(data, Mapping):
            raise ControlPlaneError(200, "control plane returned invalid lease data", data)
        lease = data.get("lease", data)
        if lease in (None, {}):
            return None
        if not isinstance(lease, Mapping):
            raise ControlPlaneError(200, "control plane returned invalid lease data", lease)
        return lease

    async def claim_assignment(self, capabilities: list[str]) -> AssignmentLease | None:
        payload = await self._request(
            "POST",
            "/internal/radar/v1/leases:claim",
            json_body={"worker_id": self.settings.worker_id, "capabilities": capabilities},
        )
        data = self._lease(payload)
        if data is None:
            return None
        return AssignmentLease.model_validate(data)

    async def wait_assignment(self) -> None:
        await self._request(
            "POST",
            "/internal/radar/v1/leases/wait",
            json_body={"worker_id": self.settings.worker_id},
        )

    async def heartbeat(self, assignment_id: UUID, token: str, lease_epoch: int = 0) -> str:
        payload = await self._request(
            "POST",
            f"/internal/radar/v1/leases/{assignment_id}/heartbeat",
            json_body={"lease_token": token, "lease_epoch": lease_epoch},
            key=self._key(assignment_id, "heartbeat"),
        )
        return str(self._data(payload).get("lease_expires_at", ""))

    async def submit_evidence(
        self, assignment_id: UUID, token: str, evidence: ExecutionEvidence, lease_epoch: int = 0
    ) -> EvidenceReceipt:
        payload = await self._request(
            "POST",
            f"/internal/radar/v1/leases/{assignment_id}/evidence",
            json_body={
                "lease_token": token,
                "lease_epoch": lease_epoch,
                "sample_id": str(evidence.sample_id),
                "evidence": evidence.model_dump(mode="json"),
            },
            key=self._key(assignment_id, "evidence"),
        )
        return EvidenceReceipt.model_validate(self._data(payload))

    async def complete_assignment(
        self, assignment_id: UUID, token: str, lease_epoch: int = 0
    ) -> None:
        await self._request(
            "POST",
            f"/internal/radar/v1/leases/{assignment_id}/complete",
            json_body={"lease_token": token, "lease_epoch": lease_epoch},
            key=self._key(assignment_id, "complete"),
        )

    async def fail_assignment(
        self, assignment_id: UUID, token: str, failure_code: str, lease_epoch: int = 0
    ) -> None:
        await self._request(
            "POST",
            f"/internal/radar/v1/leases/{assignment_id}/fail",
            json_body={
                "lease_token": token,
                "lease_epoch": lease_epoch,
                "failure_code": failure_code,
            },
            key=self._key(assignment_id, "fail"),
        )

    async def presign_artifact(
        self, assignment_id: UUID, token: str, request: ArtifactPresignRequest, lease_epoch: int = 0
    ) -> Mapping[str, Any]:
        return await self._request(
            "POST",
            f"/internal/radar/v1/leases/{assignment_id}/artifacts/presign",
            json_body={
                "lease_token": token,
                "lease_epoch": lease_epoch,
                **request.model_dump(mode="json"),
            },
            key=self._key(assignment_id, f"presign:{request.sha256}"),
        )

    async def confirm_artifact(
        self,
        assignment_id: UUID,
        token: str,
        confirmation: ArtifactConfirmation,
        lease_epoch: int = 0,
    ) -> ArtifactReceipt:
        payload = await self._request(
            "POST",
            f"/internal/radar/v1/leases/{assignment_id}/artifacts/confirm",
            json_body={
                "lease_token": token,
                "lease_epoch": lease_epoch,
                **confirmation.model_dump(mode="json"),
            },
            key=self._key(assignment_id, f"confirm:{confirmation.artifact_id}"),
        )
        return ArtifactReceipt.model_validate(self._data(payload))

    async def claim_grading(self, capabilities: list[str]) -> GradingLease | None:
        payload = await self._request(
            "POST",
            "/internal/radar/v1/grading-leases:claim",
            json_body={"worker_id": self.settings.worker_id, "capabilities": capabilities},
        )
        data = self._lease(payload)
        if data is None:
            return None
        return GradingLease.model_validate(data)

    async def heartbeat_grading(self, lease_id: UUID, token: str, lease_epoch: int = 0) -> str:
        payload = await self._request(
            "POST",
            f"/internal/radar/v1/grading-leases/{lease_id}/heartbeat",
            json_body={"lease_token": token, "lease_epoch": lease_epoch},
            key=self._key(lease_id, "grading-heartbeat"),
        )
        return str(self._data(payload).get("lease_expires_at", ""))

    async def submit_score(
        self, lease_id: UUID, token: str, submission: ScoreSubmission, lease_epoch: int = 0
    ) -> Mapping[str, Any]:
        return await self._request(
            "POST",
            f"/internal/radar/v1/grading-leases/{lease_id}/complete",
            json_body={
                "lease_token": token,
                "lease_epoch": lease_epoch,
                **submission.model_dump(mode="json"),
            },
            key=self._key(lease_id, "score"),
        )

    async def fail_grading(
        self, lease_id: UUID, token: str, failure_code: str, lease_epoch: int = 0
    ) -> None:
        await self._request(
            "POST",
            f"/internal/radar/v1/grading-leases/{lease_id}/fail",
            json_body={
                "lease_token": token,
                "lease_epoch": lease_epoch,
                "failure_code": failure_code,
            },
            key=self._key(lease_id, "grading-fail"),
        )

    async def claim_analysis(self, capabilities: list[str]) -> AnalysisLease | None:
        payload = await self._request(
            "POST",
            "/internal/radar/v1/analysis-jobs:claim",
            json_body={"worker_id": self.settings.worker_id, "capabilities": capabilities},
        )
        data = self._lease(payload)
        if data is None:
            return None
        return AnalysisLease.model_validate(data)

    async def complete_analysis(
        self, lease_id: UUID, token: str, submission: AggregateSubmission, lease_epoch: int = 0
    ) -> Mapping[str, Any]:
        return await self._request(
            "POST",
            f"/internal/radar/v1/analysis-jobs/{lease_id}/complete",
            json_body={
                "lease_token": token,
                "lease_epoch": lease_epoch,
                **submission.model_dump(mode="json"),
            },
            key=self._key(lease_id, "aggregate"),
        )
