from __future__ import annotations

import argparse
import asyncio
import hashlib
import logging
import os
import platform
import socket
import time
from typing import Protocol, cast
from uuid import UUID

import httpx
import rfc8785

from .config import Settings, get_settings
from .control_plane import ControlPlaneClient, ControlPlaneError, LeaseFencedError
from .models import (
    ArtifactConfirmation,
    ArtifactPresignRequest,
    AssignmentLease,
    EvidenceReceipt,
    ExecutionEvidence,
)
from .observability import MetricsServer, RadarMetrics, trace_scope
from .state import AtomicStateStore, LocalState, StateRecord

log = logging.getLogger(__name__)


class Executor(Protocol):
    async def execute(self, lease: AssignmentLease) -> ExecutionEvidence: ...


class HeartbeatLost(RuntimeError):
    pass


def canonical_evidence_bytes(evidence: ExecutionEvidence) -> bytes:
    return rfc8785.dumps(evidence.model_dump(mode="json"))


class Runner:
    def __init__(
        self,
        settings: Settings,
        client: ControlPlaneClient,
        executor: Executor,
        *,
        capabilities: list[str],
        state_store: AtomicStateStore | None = None,
        slots: int = 1,
        metrics: RadarMetrics | None = None,
    ) -> None:
        self.settings = settings
        self.client = client
        self.executor = executor
        self.capabilities = tuple(capabilities)
        self.state_store = state_store or AtomicStateStore(settings.state_dir)
        self.slots = max(1, slots)
        self.metrics = metrics or RadarMetrics()
        self._stop = asyncio.Event()
        self._control_plane_retry_count = 0

    def stop(self) -> None:
        self._stop.set()

    async def run_forever(self, stop: asyncio.Event | None = None) -> None:
        stop_event = stop or self._stop
        while not stop_event.is_set():
            try:
                await self.recover_pending(stop_event)
                if stop_event.is_set():
                    break
                lease = await self.client.claim_assignment(list(self.capabilities))
                self._control_plane_retry_count = 0
            except httpx.RequestError as exc:
                await self._wait_for_control_plane_retry(stop_event, exc)
                continue
            except ControlPlaneError as exc:
                if exc.status_code not in {408, 429} and exc.status_code < 500:
                    raise
                await self._wait_for_control_plane_retry(stop_event, exc)
                continue
            if lease is None:
                try:
                    await self.client.wait_assignment()
                except Exception:
                    pass
                try:
                    await asyncio.wait_for(
                        stop_event.wait(), timeout=self.settings.poll_interval_seconds
                    )
                except TimeoutError:
                    pass
                continue
            await self.execute_lease(lease)

    async def _wait_for_control_plane_retry(
        self, stop_event: asyncio.Event, error: BaseException | None = None
    ) -> None:
        self._control_plane_retry_count += 1
        delay = min(30.0, float(2 ** min(self._control_plane_retry_count - 1, 5)))
        log.warning(
            "control plane unavailable; retrying in %.1fs (attempt %d): %s",
            delay,
            self._control_plane_retry_count,
            error,
        )
        try:
            await asyncio.wait_for(stop_event.wait(), timeout=delay)
        except TimeoutError:
            pass

    async def recover_pending(self, stop: asyncio.Event) -> None:
        for record in self.state_store.list_records():
            if stop.is_set():
                return
            if record.state is not LocalState.EVIDENCE_READY or not record.evidence:
                continue
            if not record.lease_token:
                log.error("recovery record %s has no lease token", record.assignment_id)
                continue
            try:
                evidence = ExecutionEvidence.model_validate(record.evidence)
                receipt = await self._publish_evidence(
                    record.assignment_id, record.lease_token, evidence, record.lease_epoch
                )
                self.state_store.save(
                    StateRecord(
                        record.assignment_id,
                        LocalState.EVIDENCE_ACCEPTED,
                        record.idempotency_key,
                        receipt.model_dump(mode="json"),
                        record.lease_token,
                    )
                )
                self.state_store.delete(record.assignment_id)
            except LeaseFencedError:
                self.state_store.save(
                    StateRecord(
                        record.assignment_id,
                        LocalState.TERMINAL,
                        record.idempotency_key,
                        record.evidence,
                        record.lease_token,
                        record.lease_epoch,
                    )
                )
                log.warning("recovery lease %s was fenced", record.assignment_id)
            except Exception:
                log.exception("failed to recover evidence for %s", record.assignment_id)

    async def execute_lease(self, lease: AssignmentLease) -> None:
        with trace_scope(lease.route_trace_id):
            await self._execute_lease(lease)

    async def _execute_lease(self, lease: AssignmentLease) -> None:
        lease_started = time.monotonic()
        key = hashlib.sha256(f"{lease.id}:evidence".encode()).hexdigest()
        self.state_store.save(
            StateRecord(
                lease.id,
                LocalState.CLAIMED,
                key,
                lease_token=lease.lease_token,
                lease_epoch=lease.lease_epoch,
            )
        )
        heartbeat_failed = asyncio.Event()
        heartbeat_task = asyncio.create_task(
            self._heartbeat(lease, heartbeat_failed, lease_started)
        )
        execution_task = asyncio.create_task(self.executor.execute(lease))
        try:
            self.state_store.save(
                StateRecord(
                    lease.id,
                    LocalState.EXECUTING,
                    key,
                    lease_token=lease.lease_token,
                    lease_epoch=lease.lease_epoch,
                )
            )
            evidence = await self._race_execution(execution_task, heartbeat_failed)
            self.state_store.save(
                StateRecord(
                    lease.id,
                    LocalState.EVIDENCE_READY,
                    key,
                    evidence.model_dump(mode="json"),
                    lease.lease_token,
                    lease.lease_epoch,
                )
            )
            receipt = await self._publish_evidence(
                lease.id, lease.lease_token, evidence, lease.lease_epoch
            )
            self.state_store.save(
                StateRecord(
                    lease.id,
                    LocalState.EVIDENCE_ACCEPTED,
                    key,
                    receipt.model_dump(mode="json"),
                    lease.lease_token,
                    lease.lease_epoch,
                )
            )
            # Evidence upload advances the assignment to evidence_uploaded.
            # The grader owns the next transition and completes the assignment
            # after submitting the score, so the runner must not close it here.
            heartbeat_task.cancel()
            await asyncio.gather(heartbeat_task, return_exceptions=True)
            self.state_store.delete(lease.id)
        except LeaseFencedError:
            execution_task.cancel()
            await asyncio.gather(execution_task, return_exceptions=True)
            self.state_store.save(
                StateRecord(
                    lease.id,
                    LocalState.TERMINAL,
                    key,
                    evidence.model_dump(mode="json"),
                    lease.lease_token,
                    lease.lease_epoch,
                )
            )
            log.warning("lease %s was fenced; local evidence retained", lease.id)
        except HeartbeatLost:
            execution_task.cancel()
            await asyncio.gather(execution_task, return_exceptions=True)
            try:
                await self.client.fail_assignment(
                    lease.id, lease.lease_token, "heartbeat_lost", lease.lease_epoch
                )
            except Exception:
                log.exception("failed to release heartbeat-lost lease %s", lease.id)
        except asyncio.CancelledError:
            execution_task.cancel()
            await asyncio.gather(execution_task, return_exceptions=True)
            raise
        except Exception:
            execution_task.cancel()
            await asyncio.gather(execution_task, return_exceptions=True)
            try:
                await self.client.fail_assignment(
                    lease.id, lease.lease_token, "runner_error", lease.lease_epoch
                )
            except Exception:
                log.exception("failed to report runner error for %s", lease.id)
        finally:
            heartbeat_task.cancel()
            await asyncio.gather(heartbeat_task, return_exceptions=True)
            self.metrics.observe_lease_age(time.monotonic() - lease_started)

    async def _race_execution(
        self, task: asyncio.Task[ExecutionEvidence], failed: asyncio.Event
    ) -> ExecutionEvidence:
        failure_waiter = asyncio.create_task(failed.wait())
        done, pending = await asyncio.wait(
            {task, failure_waiter}, return_when=asyncio.FIRST_COMPLETED
        )
        for pending_task in pending:
            pending_task.cancel()
        if failure_waiter in done and failed.is_set():
            task.cancel()
            await asyncio.gather(task, return_exceptions=True)
            raise HeartbeatLost("heartbeat failed during execution")
        return task.result()

    async def _publish_evidence(
        self,
        assignment_id: UUID,
        lease_token: str,
        evidence: ExecutionEvidence,
        lease_epoch: int,
    ) -> EvidenceReceipt:
        payload = canonical_evidence_bytes(evidence)
        digest = hashlib.sha256(payload).hexdigest()
        upload = await self.client.presign_artifact(
            assignment_id,
            lease_token,
            ArtifactPresignRequest(
                mime_type="application/json",
                bytes=len(payload),
                sha256=digest,
            ),
            lease_epoch,
        )
        await self.client.upload_artifact(upload, payload)
        artifact = await self.client.confirm_artifact(
            assignment_id,
            lease_token,
            ArtifactConfirmation(
                artifact_id=upload.artifact_id,
                object_key=upload.object_key,
                sha256=upload.sha256,
                bytes=upload.bytes,
            ),
            lease_epoch,
        )
        if artifact.scan_status != "clean" or artifact.confirmed_at is None:
            raise RuntimeError("evidence artifact was not confirmed clean")
        return await self.client.submit_evidence(
            assignment_id, lease_token, evidence, lease_epoch
        )

    async def _heartbeat(
        self, lease: AssignmentLease, failed: asyncio.Event, lease_started: float
    ) -> None:
        interval = min(max(1, self.settings.lease_ttl_seconds // 3), 30)
        while True:
            await asyncio.sleep(interval)
            self.metrics.observe_worker_heartbeat(
                "runner", time.monotonic() - lease_started
            )
            try:
                await self.client.heartbeat(lease.id, lease.lease_token, lease.lease_epoch)
                self.metrics.observe_worker_heartbeat("runner", 0.0)
            except LeaseFencedError:
                failed.set()
                return
            except Exception:
                failed.set()
                return


def environment_fingerprint() -> str:
    value = "|".join(
        (
            socket.gethostname(),
            platform.platform(),
            platform.python_version(),
            os.getenv("HOSTNAME", ""),
        )
    )
    return hashlib.sha256(value.encode()).hexdigest()


def build_executor(settings: Settings) -> Executor:
    mode = (settings.executor or os.getenv("RADAR_EXECUTOR") or "").strip().lower()
    if not mode:
        raise SystemExit(
            "RADAR_EXECUTOR is required; set it to openai, anthropic, gemini, sse, or adapter"
        )
    if mode in {"openai", "http", "anthropic", "gemini", "sse"}:
        from .executors.anthropic import AnthropicExecutor
        from .executors.gemini import GeminiExecutor
        from .executors.openai import OpenAIExecutor

        client = httpx.AsyncClient(
            base_url=settings.executor_url or settings.control_plane_url,
            timeout=httpx.Timeout(settings.executor_timeout_seconds),
        )
        if mode == "anthropic":
            return cast(Executor, AnthropicExecutor(client))
        if mode == "gemini":
            return cast(Executor, GeminiExecutor(client))
        return cast(Executor, OpenAIExecutor(client))
    raise SystemExit(f"unsupported RADAR_EXECUTOR={mode!r}")


async def run(settings: Settings) -> None:
    executor = build_executor(settings)
    metrics = RadarMetrics()
    metrics_server = (
        MetricsServer(metrics, host=settings.metrics_host, port=settings.metrics_port)
        if settings.metrics_enabled
        else None
    )
    if metrics_server is not None:
        await metrics_server.start()
    try:
        async with ControlPlaneClient(settings, metrics=metrics) as client:
            await Runner(
                settings,
                client,
                executor,
                capabilities=list(settings.capabilities),
                slots=1,
                metrics=metrics,
            ).run_forever()
    finally:
        if metrics_server is not None:
            await metrics_server.close()


def runner_main(argv: list[str] | None = None) -> None:
    parser = argparse.ArgumentParser(description="Sub2API Radar runner")
    parser.add_argument("--version", action="version", version="0.1.0")
    parser.parse_args(argv)
    logging.basicConfig(level=logging.INFO)
    settings = get_settings()
    asyncio.run(run(settings))


def main() -> None:
    runner_main()
