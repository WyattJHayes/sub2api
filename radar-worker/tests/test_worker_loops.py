from __future__ import annotations

import asyncio
import hashlib
from datetime import UTC, datetime, timedelta
from decimal import Decimal
from types import SimpleNamespace
from uuid import uuid4

import httpx
import pytest
import rfc8785
from pydantic import SecretStr

from sub2api_radar.config import Settings
from sub2api_radar.control_plane import LeaseFencedError
from sub2api_radar.grader import GraderWorker
from sub2api_radar.models import (
    AnalysisLease,
    ArtifactReceipt,
    AssignmentLease,
    CaseSpec,
    ExecutionEvidence,
    GradingLease,
    QualityAnalysisContext,
    QualityAnalysisDimensionInput,
    QualityDimension,
    QualityPolicy,
    QualityProbeEventClass,
    QualitySourceCandidate,
)
from sub2api_radar.runner import Runner, build_executor, runner_main
from sub2api_radar.state import AtomicStateStore, LocalState, StateRecord
from sub2api_radar.statistics.service import StatisticsWorker


def settings(mode: str = "runner") -> Settings:
    return Settings(
        control_plane_url="https://radar.example.test",
        worker_token=SecretStr("t" * 40),
        worker_id=f"{mode}-1",
        mode=mode,
        region="test",
        route_profile_version="v1",
    )


def quality_context(run_id):
    now = datetime.now(UTC)
    return QualityAnalysisContext(
        run_id=run_id,
        model_alias="model-a",
        policy_version="quality-v1",
        policy=QualityPolicy(
            minimum_coverage=Decimal("0.8"),
            minimum_confidence=Decimal("0.7"),
            minimum_margin=Decimal("0.15"),
            minimum_samples_per_dimension=3,
            observe_delta_pp=Decimal("5"),
            suspected_delta_pp=Decimal("10"),
            high_risk_delta_pp=Decimal("20"),
            freshness_hours=24,
        ),
        dimensions=tuple(
            QualityAnalysisDimensionInput(
                key=key,
                baseline_score=Decimal("0.80"),
                candidate_score=Decimal("0.70"),
                sample_count=3,
                reference_baseline_delta_pp=Decimal("-10"),
                probe_event_class=QualityProbeEventClass.RESPONSE_SHAPE,
                probe_spec_hash="a" * 64,
                observation_hash="b" * 64,
                observed_at=now,
            )
            for key in QualityDimension
        ),
        source_candidates=(
            QualitySourceCandidate(
                display_name="GPT-4.1", confidence=Decimal("0.90"), sample_count=3,
                baseline_score=Decimal("0.80"), candidate_score=Decimal("0.70"),
                probe_event_class=QualityProbeEventClass.FINGERPRINT,
                probe_spec_hash="c" * 64, observation_hash="d" * 64, observed_at=now,
            ),
            QualitySourceCandidate(
                display_name="Claude-3.7-Sonnet", confidence=Decimal("0.70"), sample_count=3,
                baseline_score=Decimal("0.80"), candidate_score=Decimal("0.70"),
                probe_event_class=QualityProbeEventClass.FINGERPRINT,
                probe_spec_hash="e" * 64, observation_hash="f" * 64, observed_at=now,
            ),
        ),
    )


def analysis_lease_with_quality_context() -> AnalysisLease:
    run_id = uuid4()
    return AnalysisLease(
        id=uuid4(),
        run_id=run_id,
        capability_domain="reasoning",
        model_route="route-a",
        window="daily",
        analysis_version="v1",
        window_start=datetime.now(UTC),
        lease_token="analysis-lease-token-123456",
        lease_expires_at=datetime.now(UTC) + timedelta(minutes=1),
        lease_epoch=7,
        score_ids=(uuid4(),),
        quality_context=quality_context(run_id),
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
        self.failures = []

    async def claim_analysis(self, capabilities):
        return self.lease

    async def complete_analysis(self, lease_id, token, submission, lease_epoch=0):
        self.completed.append(submission)
        self.completed_epochs.append(lease_epoch)
        return {}

    async def fail_analysis(self, lease_id, token, failure_code, lease_epoch=0):
        self.failures.append((lease_id, token, failure_code, lease_epoch))


class FencedStatisticsClientStub(StatisticsClientStub):
    async def complete_analysis(self, lease_id, token, submission, lease_epoch=0):
        await super().complete_analysis(lease_id, token, submission, lease_epoch)
        raise LeaseFencedError(409, "lease fenced")


class RunnerClientStub:
    def __init__(self) -> None:
        self.evidence_submissions = []
        self.complete_calls = []
        self.fail_calls = []
        self.artifact_calls = []
        self.artifact_requests = []
        self.artifact_payloads = []

    async def presign_artifact(self, assignment_id, token, request, lease_epoch=0):
        self.artifact_calls.append("presign")
        self.artifact_requests.append(request)
        return SimpleNamespace(
            artifact_id=uuid4(),
            object_key=f"evaluation-artifacts/run/sample/{assignment_id}",
            upload_url="https://objects.example.test/upload",
            upload_headers={"Content-Type": "application/json"},
            sha256=request.sha256,
            bytes=request.bytes,
            mime_type=request.mime_type,
            expires_at=datetime.now(UTC) + timedelta(minutes=5),
        )

    async def upload_artifact(self, upload, payload):
        self.artifact_calls.append("upload")
        self.artifact_payloads.append(payload)

    async def confirm_artifact(self, assignment_id, token, confirmation, lease_epoch=0):
        self.artifact_calls.append("confirm")
        return ArtifactReceipt(
            id=confirmation.artifact_id,
            object_key=confirmation.object_key,
            sha256=confirmation.sha256,
            bytes=confirmation.bytes,
            mime_type="application/json",
            scan_status="clean",
            scanner="clamav",
            scanned_at=datetime.now(UTC),
            confirmed_at=datetime.now(UTC),
        )

    async def submit_evidence(self, assignment_id, token, item, lease_epoch=0):
        self.artifact_calls.append("submit")
        self.evidence_submissions.append((assignment_id, token, item, lease_epoch))
        return type("Receipt", (), {"model_dump": lambda self, mode="json": {"accepted": True}})()

    async def complete_assignment(self, assignment_id, token):
        self.complete_calls.append((assignment_id, token))

    async def fail_assignment(self, assignment_id, token, failure_code, lease_epoch=0):
        self.fail_calls.append((assignment_id, token, failure_code))

    async def heartbeat(self, assignment_id, token, lease_epoch=0):
        return ""


class FencedRunnerClientStub(RunnerClientStub):
    async def submit_evidence(self, assignment_id, token, item, lease_epoch=0):
        self.evidence_submissions.append((assignment_id, token, item, lease_epoch))
        raise LeaseFencedError(409, "lease fenced")


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
async def test_grader_loads_bound_evidence_from_controlled_artifact() -> None:
    assignment_id = uuid4()
    sample_id = uuid4()
    item = evidence(sample_id, assignment_id)
    payload = rfc8785.dumps(item.model_dump(mode="json"))
    digest = hashlib.sha256(payload).hexdigest()
    artifact_id = uuid4()
    lease = GradingLease(
        id=uuid4(),
        assignment_id=assignment_id,
        sample_id=sample_id,
        run_id=uuid4(),
        case=case(),
        route_trace_id="trace",
        evidence_manifest=item.model_dump(mode="json"),
        evidence=(
            ArtifactReceipt(
                id=artifact_id,
                object_key="evaluation-artifacts/run/sample/evidence.json",
                sha256=digest,
                bytes=len(payload),
                mime_type="application/json",
                scan_status="clean",
                scanner="clamav",
                scanned_at=datetime.now(UTC),
                confirmed_at=datetime.now(UTC),
            ),
        ),
        lease_token="lease-token-123456",
        lease_expires_at=datetime.now(UTC) + timedelta(minutes=1),
        lease_epoch=7,
    )

    class ControlledArtifactGraderClient(GraderClientStub):
        def __init__(self, grading_lease):
            super().__init__(grading_lease)
            self.artifact_calls = []

        async def presign_grading_artifact(
            self, lease_id, token, requested_artifact_id, lease_epoch=0
        ):
            self.artifact_calls.append(("presign", requested_artifact_id, lease_epoch))
            return SimpleNamespace(
                artifact_id=requested_artifact_id,
                object_key="evaluation-artifacts/run/sample/evidence.json",
                download_url="https://objects.example.test/read",
                sha256=digest,
                bytes=len(payload),
                mime_type="application/json",
                expires_at=datetime.now(UTC) + timedelta(minutes=5),
            )

        async def download_artifact(self, download):
            self.artifact_calls.append(("download", download.artifact_id, 7))
            return payload

    client = ControlledArtifactGraderClient(lease)
    worker = GraderWorker(settings("grader"), client, capabilities=["exact"])

    await worker.process_once()

    assert client.artifact_calls == [
        ("presign", artifact_id, 7),
        ("download", artifact_id, 7),
    ]
    assert len(client.submissions) == 1
    assert client.submissions[0].evidence_hashes == (digest,)


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
async def test_statistics_submits_one_quality_report_with_the_existing_lease_epoch() -> None:
    lease = analysis_lease_with_quality_context()
    client = StatisticsClientStub(lease)
    worker = StatisticsWorker(
        settings("statistics"),
        client,
        analyzer=lambda _lease: {"candidate_score": "0.9", "baseline_score": "0.9"},
    )

    assert await worker.process_once()
    assert len(client.completed) == 1
    assert client.completed_epochs == [lease.lease_epoch]
    assert client.completed[0].quality_report is not None
    assert client.completed[0].quality_report.run_id == lease.run_id
    assert len(client.completed[0].quality_report.probe_observations) == len(QualityDimension)


@pytest.mark.asyncio
async def test_statistics_does_not_retry_a_fenced_quality_completion() -> None:
    lease = analysis_lease_with_quality_context()
    client = FencedStatisticsClientStub(lease)
    worker = StatisticsWorker(
        settings("statistics"),
        client,
        analyzer=lambda _lease: {"candidate_score": "0.9", "baseline_score": "0.9"},
    )

    assert await worker.process_once()
    assert len(client.completed) == 1
    assert client.completed[0].quality_report is not None
    assert client.failures == []


@pytest.mark.asyncio
async def test_statistics_reports_analyzer_failure_to_control_plane() -> None:
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
    )
    client = StatisticsClientStub(lease)

    def analyzer(_lease):
        raise ValueError("malformed analyzer output")

    worker = StatisticsWorker(settings("statistics"), client, analyzer=analyzer)
    assert await worker.process_once()
    assert client.failures == [(lease.id, lease.lease_token, "statistics_worker_error", 7)]


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
    assert client.artifact_calls == ["presign", "upload", "confirm", "submit"]
    assert len(client.artifact_payloads) == 1
    assert (
        hashlib.sha256(client.artifact_payloads[0]).hexdigest()
        == client.artifact_requests[0].sha256
    )
    assert len(client.artifact_payloads[0]) == client.artifact_requests[0].bytes
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


@pytest.mark.asyncio
async def test_runner_recovery_quarantines_fenced_evidence_without_retry(tmp_path) -> None:
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
    client = FencedRunnerClientStub()
    worker = Runner(
        settings("runner").model_copy(update={"state_dir": str(tmp_path)}),
        client,
        RunnerExecutorStub(item),
        capabilities=["reasoning"],
        state_store=store,
    )

    await worker.recover_pending(asyncio.Event())
    await worker.recover_pending(asyncio.Event())

    record = store.load(assignment_id)
    assert record is not None
    assert record.state is LocalState.TERMINAL
    assert record.evidence == item.model_dump(mode="json")
    assert len(client.evidence_submissions) == 1


@pytest.mark.asyncio
async def test_runner_execution_quarantines_fenced_evidence_without_recovery_retry(
    tmp_path,
) -> None:
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
    store = AtomicStateStore(tmp_path)
    client = FencedRunnerClientStub()
    worker = Runner(
        settings("runner").model_copy(update={"state_dir": str(tmp_path)}),
        client,
        RunnerExecutorStub(item),
        capabilities=["reasoning"],
        state_store=store,
    )

    await worker.execute_lease(lease)
    await worker.recover_pending(asyncio.Event())

    record = store.load(assignment_id)
    assert record is not None
    assert record.state is LocalState.TERMINAL
    assert record.evidence == item.model_dump(mode="json")
    assert len(client.evidence_submissions) == 1


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


@pytest.mark.asyncio
async def test_runner_retries_transient_control_plane_disconnect_without_exiting(tmp_path) -> None:
    class DisconnectedClient:
        def __init__(self) -> None:
            self.claims = 0

        async def claim_assignment(self, _capabilities):
            self.claims += 1
            raise httpx.ConnectError("control plane unavailable", request=httpx.Request("POST", "https://radar.example.test"))

        async def wait_assignment(self):
            raise AssertionError("claim failure should be handled before wait")

    client = DisconnectedClient()
    worker = Runner(
        settings("runner").model_copy(update={"state_dir": str(tmp_path)}),
        client,
        RunnerExecutorStub(evidence(uuid4(), uuid4())),
        capabilities=["reasoning"],
    )
    stop = asyncio.Event()

    async def stop_after_retry(*_args) -> None:
        stop.set()

    worker._wait_for_control_plane_retry = stop_after_retry
    await worker.run_forever(stop)

    assert client.claims == 1
