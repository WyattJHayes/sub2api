from __future__ import annotations

import argparse
import asyncio
import hashlib
import logging
import os
import platform
import socket
from typing import Protocol, cast

import httpx

from .config import Settings, get_settings
from .control_plane import ControlPlaneClient, LeaseFencedError
from .models import AssignmentLease, ExecutionEvidence
from .state import AtomicStateStore, LocalState, StateRecord

log = logging.getLogger(__name__)


class Executor(Protocol):
    async def execute(self, lease: AssignmentLease) -> ExecutionEvidence: ...


class HeartbeatLost(RuntimeError):
    pass


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
    ) -> None:
        self.settings = settings
        self.client = client
        self.executor = executor
        self.capabilities = tuple(capabilities)
        self.state_store = state_store or AtomicStateStore(settings.state_dir)
        self.slots = max(1, slots)
        self._stop = asyncio.Event()

    def stop(self) -> None:
        self._stop.set()

    async def run_forever(self, stop: asyncio.Event | None = None) -> None:
        stop_event = stop or self._stop
        while not stop_event.is_set():
            await self.recover_pending(stop_event)
            if stop_event.is_set():
                break
            lease = await self.client.claim_assignment(list(self.capabilities))
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
                receipt = await self.client.submit_evidence(
                    record.assignment_id, record.lease_token, evidence, record.lease_epoch
                )
                self.state_store.save(
                    StateRecord(
                        record.assignment_id,
                        LocalState.EVIDENCE_ACCEPTED,
                        record.idempotency_key,
                        receipt.model_dump(mode="json"),
                        record.lease_token,
                        record.lease_epoch,
                    )
                )
                self.state_store.delete(record.assignment_id)
            except LeaseFencedError:
                log.warning("recovery lease %s was fenced", record.assignment_id)
            except Exception:
                log.exception("failed to recover evidence for %s", record.assignment_id)

    async def execute_lease(self, lease: AssignmentLease) -> None:
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
        heartbeat_task = asyncio.create_task(self._heartbeat(lease, heartbeat_failed))
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
            receipt = await self.client.submit_evidence(
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

    async def _heartbeat(self, lease: AssignmentLease, failed: asyncio.Event) -> None:
        interval = min(max(1, self.settings.lease_ttl_seconds // 3), 30)
        while True:
            await asyncio.sleep(interval)
            try:
                await self.client.heartbeat(lease.id, lease.lease_token, lease.lease_epoch)
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
    async with ControlPlaneClient(settings) as client:
        await Runner(
            settings,
            client,
            executor,
            capabilities=list(settings.capabilities),
            slots=1,
        ).run_forever()


def runner_main(argv: list[str] | None = None) -> None:
    parser = argparse.ArgumentParser(description="Sub2API Radar runner")
    parser.add_argument("--version", action="version", version="0.1.0")
    parser.parse_args(argv)
    logging.basicConfig(level=logging.INFO)
    settings = get_settings()
    asyncio.run(run(settings))


def main() -> None:
    runner_main()
