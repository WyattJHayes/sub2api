from __future__ import annotations

import asyncio
from datetime import UTC, datetime, timedelta
from decimal import Decimal
from uuid import uuid4

import pytest
from pydantic import SecretStr

from sub2api_radar.config import Settings
from sub2api_radar.grader import GraderWorker
from sub2api_radar.models import (
    AnalysisLease,
    ArtifactReceipt,
    AssignmentLease,
    CaseSpec,
    ExecutionEvidence,
    GradingLease,
)
from sub2api_radar.runner import Runner, build_executor, runner_main
from sub2api_radar.statistics.service import StatisticsWorker
from sub2api_radar.state import AtomicStateStore, LocalState, StateRecord


def settings(mode: str = "runner") -> Settings:
    return Settings(
        control_plane_url="https://radar.example.test",
        worker_token=SecretStr("t" * 40),
        worker_id=f"{mode}-1",
        mode=mode,
        region="test",
        route_profile_version="v1",
    )


def case(grader_id: str = "exact") -> CaseSpec:
    return CaseSpec(
        case_id=uuid4(),
        case_key="case-1",
        capability_domain="reasoning",
        priority="P1",
        weight=Decimal("1"),
        prompt_spec="answer",
        expected_spec="answer",
        grader_id=grader_id,
        grader_version="v1",
        content_sha256="a" * 64,
        confidentiality="public",
    )


def evidence(sample_id, assignment_id) -> ExecutionEvidence:
    now = datetime.now(UTC)
    return ExecutionEvidence(
        assignment_id=assignment_id,
        sample_id=sample_id,
        case_content_sha256="a" * 64,
        execution_image_digest="worker@sha256:" + "b" * 64,
        request_sha256="c" * 64,
        response_sha256="d" * 64,
        route_trace_id="trace",
        started_at=now,
        finished_at=now,
        latency_ms=1,
        transport_status="200",
        final_output="answer",
        environment_fingerprint="env",
    )


class GraderClientStub:
    def __init__(self, lease: GradingLease):
        self.lease = lease
        self.heartbeats = 0
        self.submissions = []
        self.heartbeat_epochs = []
        self.submission_epochs = []

    async def claim_grading(self, capabilities):
        return self.lease

    async def heartbeat_grading(self, lease_id, token, lease_epoch=0):
        self.heartbeats += 1
        self.heartbeat_epochs.append(lease_epoch)
        return ""

    async def submit_score(self, lease_id, token, submission, lease_epoch=0):
        self.submissions.append(submission)
        self.submission_epochs.append(lease_epoch)
        return {}

    async def fail_grading(self, lease_id, token, failure_code, lease_epoch=0):
        raise AssertionError(failure_code)


class StatisticsClientStub:
    def __init__(self, lease: AnalysisLease):
        self.lease = lease
        self.completed = []
        self.completed_epochs = []

    async def claim_analysis(self, capabilities):
        return self.lease

    async def complete_analysis(self, lease_id, token, submission, lease_epoch=0):
        self.completed.append(submission)
        self.completed_epochs.append(lease_epoch)
        return {}


class RunnerClientStub:
    def __init__(self) -> None:
        self.evidence_submissions = []
        self.complete_calls = []
        self.fail_calls = []

    async def submit_evidence(self, assignment_id, token, item, lease_epoch=0):
        self.evidence_submissions.append((assignment_id, token, item, lease_epoch))
        return type("Receipt", (), {"model_dump": lambda self, mode="json": {"accepted": True}})()

    async def complete_assignment(self, assignment_id, token):
        self.complete_calls.append((assignment_id, token))

    async def fail_assignment(self, assignment_id, token, failure_code, lease_epoch=0):
        self.fail_calls.append((assignment_id, token, failure_code))

    async def heartbeat(self, assignment_id, token, lease_epoch=0):
        return ""


class RunnerExecutorStub:
    def __init__(self, item):
        self.item = item

    async def execute(self, lease):
        return self.item


@pytest.mark.asyncio
async def test_grader_processes_one_lease_and_heartbeats() -> None:
    sample_id = uuid4()
    lease = GradingLease(
        id=uuid4(),
        sample_id=sample_id,
        run_id=uuid4(),
        case=case(),
        evidence=(
            ArtifactReceipt(
                id=uuid4(),
                object_key="evidence.json",
                sha256="d" * 64,
                bytes=1,
                mime_type="application/json",
                scan_status="clean",
            ),
        ),
        lease_token="lease-token-123456",
        lease_expires_at=datetime.now(UTC) + timedelta(minutes=1),
        lease_epoch=7,
    )
    client = GraderClientStub(lease)
    worker = GraderWorker(settings("grader"), client, capabilities=["exact"])
    worker.evidence_loader = lambda _lease: evidence(sample_id, uuid4())
    await worker.process_once()
    assert len(client.submissions) == 1
    assert client.submissions[0].score == Decimal("1")
    assert client.heartbeat_epochs == [7]
    assert client.submission_epochs == [7]


@pytest.mark.asyncio
async def test_statistics_processes_one_lease_with_plugin() -> None:
    lease = AnalysisLease(
        id=uuid4(),
        run_id=uuid4(),
        capability_domain="reasoning",
        model_route="route-a",
        window="daily",
        analysis_version="v1",
        window_start=datetime.now(UTC),
        lease_token="lease-token-123456",
        lease_expires_at=datetime.now(UTC) + timedelta(minutes=1),
        lease_epoch=7,
        score_ids=(uuid4(),),
    )
    client = StatisticsClientStub(lease)

    def analyzer(_lease):
        return {"candidate_score": "0.9", "baseline_score": "0.9"}

    worker = StatisticsWorker(
        settings("statistics"), client, capabilities=["reasoning"], analyzer=analyzer
    )
    await worker.process_once()
    assert len(client.completed) == 1
    assert client.completed[0].effective_pair_count == 0
    assert client.completed_epochs == [7]


@pytest.mark.asyncio
async def test_runner_stops_after_evidence_upload_and_leaves_completion_to_grader(tmp_path) -> None:
    assignment_id = uuid4()
    sample_id = uuid4()
    item = evidence(sample_id, assignment_id)
    lease = AssignmentLease(
        id=assignment_id,
        sample_id=sample_id,
        run_id=uuid4(),
        case=case(),
        model_route="route-a",
        attempt=1,
        lease_token="lease-token-123456",
        lease_expires_at=datetime.now(UTC) + timedelta(minutes=1),
        lease_epoch=7,
        gateway_evaluation_token="gateway-token",
        route_trace_id="trace",
    )
    client = RunnerClientStub()
    worker = Runner(
        settings("runner").model_copy(update={"state_dir": str(tmp_path)}),
        client,
        RunnerExecutorStub(item),
        capabilities=["reasoning"],
    )

    await worker.execute_lease(lease)

    assert len(client.evidence_submissions) == 1
    assert client.evidence_submissions[0][3] == 7
    assert client.complete_calls == []
    assert client.fail_calls == []


@pytest.mark.asyncio
async def test_runner_recovery_forwards_persisted_lease_epoch(tmp_path) -> None:
    assignment_id = uuid4()
    sample_id = uuid4()
    item = evidence(sample_id, assignment_id)
    store = AtomicStateStore(tmp_path)
    store.save(
        StateRecord(
            assignment_id,
            LocalState.EVIDENCE_READY,
            "idem",
            item.model_dump(mode="json"),
            "lease-token-123456",
            7,
        )
    )
    client = RunnerClientStub()
    worker = Runner(
        settings("runner").model_copy(update={"state_dir": str(tmp_path)}),
        client,
        RunnerExecutorStub(item),
        capabilities=["reasoning"],
    )

    await worker.recover_pending(asyncio.Event())

    assert client.evidence_submissions[0][3] == 7


def test_runner_executor_mode_fails_fast_without_executor(monkeypatch) -> None:
    monkeypatch.setenv("RADAR_EXECUTOR", "")
    with pytest.raises(SystemExit, match="RADAR_EXECUTOR"):
        build_executor(settings())


def test_runner_main_reports_missing_executor(monkeypatch) -> None:
    monkeypatch.setenv("RADAR_CONTROL_PLANE_URL", "https://radar.example.test")
    monkeypatch.setenv("RADAR_WORKER_TOKEN", "t" * 40)
    monkeypatch.setenv("RADAR_WORKER_ID", "runner-1")
    monkeypatch.setenv("RADAR_REGION", "test")
    monkeypatch.setenv("RADAR_ROUTE_PROFILE_VERSION", "v1")
    monkeypatch.delenv("RADAR_EXECUTOR", raising=False)
    with pytest.raises(SystemExit, match="RADAR_EXECUTOR"):
        runner_main([])
