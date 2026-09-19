from __future__ import annotations

import asyncio
import hashlib
import json
import math
import random
import time
from collections.abc import Mapping
from typing import Any
from uuid import UUID

import httpx

from . import __version__
from .config import Settings
from .loadgen.publisher import ReliabilitySnapshotSubmission
from .models import (
    AggregateSubmission,
    AnalysisLease,
    ArtifactConfirmation,
    ArtifactDownload,
    ArtifactPresignRequest,
    ArtifactReceipt,
    ArtifactUpload,
    AssignmentLease,
    EvidenceReceipt,
    ExecutionEvidence,
    GradingLease,
    ScoreReceipt,
    ScoreSubmission,
)
from .observability import RadarMetrics, current_traceparent, traceparent_for


class ControlPlaneError(RuntimeError):
    def __init__(self, status_code: int, message: str, payload: Any = None) -> None:
        super().__init__(message)
        self.status_code = status_code
        self.payload = payload


class LeaseFencedError(ControlPlaneError):
    pass


class ControlPlaneClient:
    def __init__(
        self,
        settings: Settings,
        client: httpx.AsyncClient | None = None,
        *,
        metrics: RadarMetrics | None = None,
    ) -> None:
        self.settings = settings
        self._client = client
        self._owns_client = client is None
        self.metrics = metrics or RadarMetrics()

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
            "User-Agent": f"sub2api-radar-worker/{__version__}",
            "traceparent": current_traceparent() or traceparent_for(self.settings.worker_id),
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
        started = time.monotonic()
        try:
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
        finally:
            self.metrics.observe_latency(
                "control_plane", (time.monotonic() - started) * 1000
            )

    def _decode(self, response: httpx.Response) -> Mapping[str, Any]:
        if response.status_code == 409:
            raise LeaseFencedError(response.status_code, "lease fenced", self._payload(response))
        if response.is_error:
            raise ControlPlaneError(
                response.status_code,
                f"control plane returned {response.status_code}",
                self._payload(response),
            )
        payload = response.json() if response.content.strip() else {}
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
        data = self._data(payload)
        if isinstance(data, Mapping):
            queue_lag = data.get("queue_lag_seconds")
            if (
                isinstance(queue_lag, int | float)
                and not isinstance(queue_lag, bool)
                and math.isfinite(float(queue_lag))
                and queue_lag >= 0
            ):
                self.metrics.observe_queue_lag(float(queue_lag))
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
                "failure_code": failure_code,
                "lease_epoch": lease_epoch,
            },
            key=self._key(assignment_id, "fail"),
        )

    async def presign_artifact(
        self, assignment_id: UUID, token: str, request: ArtifactPresignRequest, lease_epoch: int = 0
    ) -> ArtifactUpload:
        payload = await self._request(
            "POST",
            f"/internal/radar/v1/leases/{assignment_id}/artifacts/presign",
            json_body={
                "lease_token": token,
                "lease_epoch": lease_epoch,
                **request.model_dump(mode="json"),
            },
            key=self._key(assignment_id, f"presign:{request.sha256}"),
        )
        return ArtifactUpload.model_validate(self._data(payload))

    async def upload_artifact(self, upload: ArtifactUpload, content: bytes) -> None:
        if len(content) != upload.bytes or hashlib.sha256(content).hexdigest() != upload.sha256:
            raise ValueError("artifact upload content does not match its signed identity")
        headers = {
            name: value
            for name, value in upload.upload_headers.items()
            if name.lower() not in {"authorization", "cookie", "proxy-authorization"}
        }
        await self.start()
        assert self._client is not None
        response = await self._client.put(upload.upload_url, content=content, headers=headers)
        if response.status_code == 412:
            return
        if response.is_error:
            raise ControlPlaneError(
                response.status_code,
                f"artifact object store returned {response.status_code}",
                response.text,
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

    async def presign_grading_artifact(
        self,
        lease_id: UUID,
        token: str,
        artifact_id: UUID,
        lease_epoch: int,
    ) -> ArtifactDownload:
        payload = await self._request(
            "POST",
            f"/internal/radar/v1/grading-leases/{lease_id}/artifacts/{artifact_id}/read",
            json_body={"lease_token": token, "lease_epoch": lease_epoch},
            key=self._key(lease_id, f"read-artifact:{artifact_id}"),
        )
        return ArtifactDownload.model_validate(self._data(payload))

    async def download_artifact(self, download: ArtifactDownload) -> bytes:
        await self.start()
        assert self._client is not None
        response = await self._client.get(download.download_url)
        if response.is_error:
            raise ControlPlaneError(
                response.status_code,
                f"artifact object store returned {response.status_code}",
                response.text,
            )
        content = response.content
        if len(content) != download.bytes or hashlib.sha256(content).hexdigest() != download.sha256:
            raise ValueError("downloaded artifact does not match its trusted identity")
        return content

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
    ) -> ScoreReceipt:
        payload = await self._request(
            "POST",
            f"/internal/radar/v1/grading-leases/{lease_id}/complete",
            json_body={
                "lease_token": token,
                "lease_epoch": lease_epoch,
                **submission.model_dump(mode="json"),
            },
            key=self._key(lease_id, "score"),
        )
        return ScoreReceipt.model_validate(self._data(payload))

    async def fail_grading(
        self, lease_id: UUID, token: str, failure_code: str, lease_epoch: int = 0
    ) -> None:
        await self._request(
            "POST",
            f"/internal/radar/v1/grading-leases/{lease_id}/fail",
            json_body={
                "lease_token": token,
                "failure_code": failure_code,
                "lease_epoch": lease_epoch,
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
        submission_payload = submission.model_dump(mode="json", by_alias=True)
        if submission.quality_report is None:
            submission_payload.pop("quality_report", None)
        return await self._request(
            "POST",
            f"/internal/radar/v1/analysis-jobs/{lease_id}/complete",
            json_body={
                "lease_token": token,
                "lease_epoch": lease_epoch,
                **submission_payload,
            },
            key=self._key(lease_id, "aggregate"),
        )

    async def fail_analysis(
        self, lease_id: UUID, token: str, failure_code: str, lease_epoch: int = 0
    ) -> None:
        await self._request(
            "POST",
            f"/internal/radar/v1/analysis-jobs/{lease_id}/fail",
            json_body={
                "lease_token": token,
                "failure_code": failure_code,
                "lease_epoch": lease_epoch,
            },
            key=self._key(lease_id, "analysis-fail"),
        )

    async def publish_reliability_snapshot(
        self, submission: ReliabilitySnapshotSubmission
    ) -> Mapping[str, Any]:
        return await self._request(
            "POST",
            "/internal/radar/v1/reliability-snapshots",
            json_body=submission.model_dump(mode="json"),
            key=hashlib.sha256(
                f"{submission.run_id}:{submission.load_plan_id}:{submission.source_watermark}".encode()
            ).hexdigest(),
        )

    async def get_load_plan(self, load_plan_id: UUID) -> Mapping[str, Any]:
        payload = await self._request(
            "GET", f"/internal/radar/v1/load-plans/{load_plan_id}"
        )
        data = self._data(payload)
        if not isinstance(data, Mapping):
            raise ControlPlaneError(200, "control plane returned invalid load plan", data)
        return data

    async def get_fault_experiment(self, experiment_id: UUID) -> Mapping[str, Any]:
        payload = await self._request(
            "GET", f"/internal/radar/v1/fault-experiments/{experiment_id}"
        )
        data = self._data(payload)
        if not isinstance(data, Mapping):
            raise ControlPlaneError(200, "control plane returned invalid fault experiment", data)
        return data

    async def record_fault_event(
        self, experiment_id: UUID, event: Mapping[str, Any]
    ) -> Mapping[str, Any]:
        key = hashlib.sha256(
            f"{experiment_id}:{event.get('event_type', '')}:{event.get('event_hash', '')}".encode()
        ).hexdigest()
        return await self._request(
            "POST",
            f"/internal/radar/v1/fault-experiments/{experiment_id}/events",
            json_body=dict(event),
            key=key,
        )

    async def get_recovery_observation(self, evidence_id: UUID) -> Mapping[str, Any]:
        payload = await self._request(
            "GET", f"/internal/radar/v1/recovery-evidence/{evidence_id}/observation"
        )
        data = self._data(payload)
        if not isinstance(data, Mapping):
            raise ControlPlaneError(
                200, "control plane returned invalid recovery observation", data
            )
        return data

    async def publish_recovery_evidence(
        self, evidence_id: UUID, evidence: Mapping[str, Any]
    ) -> Mapping[str, Any]:
        encoded = json.dumps(dict(evidence), sort_keys=True, separators=(",", ":"))
        return await self._request(
            "POST",
            f"/internal/radar/v1/recovery-evidence/{evidence_id}",
            json_body=dict(evidence),
            key=hashlib.sha256(f"{evidence_id}:{encoded}".encode()).hexdigest(),
        )
