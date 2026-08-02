from datetime import UTC, datetime
from uuid import uuid4

import httpx
import pytest
import respx
from pydantic import SecretStr

from sub2api_radar.config import Settings
from sub2api_radar.control_plane import ControlPlaneClient, LeaseFencedError
from sub2api_radar.models import (
    ArtifactConfirmation,
    ArtifactPresignRequest,
    AssignmentLease,
    CaseSpec,
    ExecutionEvidence,
)
from sub2api_radar.observability import RadarMetrics, trace_scope, traceparent_for


def settings() -> Settings:
    return Settings(
        control_plane_url="https://radar.example.test",
        worker_token=SecretStr("t" * 40),
        worker_id="runner-1",
        region="test",
        route_profile_version="v1",
    )


def lease() -> AssignmentLease:
    return AssignmentLease(
        id=uuid4(),
        sample_id=uuid4(),
        run_id=uuid4(),
        case=CaseSpec(
            case_id=uuid4(),
            case_key="case-1",
            capability_domain="reasoning",
            priority="P1",
            weight=1,
            grader_id="exact",
            grader_version="v1",
            content_sha256="a" * 64,
            confidentiality="public",
        ),
        model_route="model-a",
        attempt=1,
        lease_token="lease-token-123456",
        lease_expires_at=datetime.now(UTC),
        lease_epoch=7,
        worker_image_digest="runner@sha256:" + "e" * 64,
        work_origin="initial",
        gateway_evaluation_token="gateway-token",
        route_trace_id="trace-1",
    )


@pytest.mark.asyncio
@respx.mock
async def test_claim_and_evidence_use_worker_auth_and_stable_idempotency() -> None:
    item = lease()
    route = respx.post("https://radar.example.test/internal/radar/v1/leases:claim").mock(
        return_value=httpx.Response(200, json={"data": {"lease": item.model_dump(mode="json")}})
    )
    async with ControlPlaneClient(settings()) as client:
        claimed = await client.claim_assignment(["reasoning"])
    assert claimed == item
    assert route.calls[0].request.headers["Authorization"] == "Bearer " + "t" * 40


@pytest.mark.asyncio
@respx.mock
async def test_control_plane_forwards_traceparent_and_records_latency() -> None:
    item = lease()
    route = respx.post("https://radar.example.test/internal/radar/v1/leases:claim").mock(
        return_value=httpx.Response(200, json={"data": {"lease": item.model_dump(mode="json")}})
    )
    metrics = RadarMetrics()
    traceparent = traceparent_for("assignment-trace")

    async with ControlPlaneClient(settings(), metrics=metrics) as client:
        with trace_scope(traceparent):
            await client.claim_assignment(["reasoning"])

    assert route.calls[0].request.headers["traceparent"] == traceparent
    assert 'stage="control_plane"' in metrics.render()


@pytest.mark.asyncio
@respx.mock
async def test_claim_records_queue_lag_from_control_plane_envelope() -> None:
    item = lease()
    route = respx.post("https://radar.example.test/internal/radar/v1/leases:claim").mock(
        return_value=httpx.Response(
            200,
            json={
                "data": {
                    "queue_lag_seconds": 4.5,
                    "lease": item.model_dump(mode="json"),
                }
            },
        )
    )
    metrics = RadarMetrics()
    async with ControlPlaneClient(settings(), metrics=metrics) as client:
        claimed = await client.claim_assignment(["reasoning"])

    assert claimed == item
    assert "radar_queue_lag_seconds 4.5" in metrics.render()
    assert route.called


@pytest.mark.asyncio
@respx.mock
async def test_claim_accepts_control_plane_direct_lease_envelope() -> None:
    item = lease()
    respx.post("https://radar.example.test/internal/radar/v1/leases:claim").mock(
        return_value=httpx.Response(200, json={"data": item.model_dump(mode="json")})
    )

    async with ControlPlaneClient(settings()) as client:
        claimed = await client.claim_assignment(["reasoning"])

    assert claimed == item


@pytest.mark.asyncio
@respx.mock
async def test_claim_accepts_full_go_assignment_contract() -> None:
    item = lease()
    payload = item.model_dump(mode="json")
    payload.update(
        {
            "model_config_sha256": "b" * 64,
            "dataset_version_id": str(uuid4()),
            "dataset_key": "reasoning-smoke",
            "dataset_version": "2026-07-27",
            "dataset_manifest_sha256": "c" * 64,
        }
    )
    respx.post("https://radar.example.test/internal/radar/v1/leases:claim").mock(
        return_value=httpx.Response(200, json={"data": payload})
    )

    async with ControlPlaneClient(settings()) as client:
        claimed = await client.claim_assignment(["reasoning"])

    assert claimed is not None
    assert claimed.model_config_sha256 == "b" * 64
    assert claimed.dataset_key == "reasoning-smoke"
    assert claimed.dataset_manifest_sha256 == "c" * 64


@pytest.mark.asyncio
@respx.mock
async def test_submit_evidence_sends_sample_identity_at_handler_boundary() -> None:
    item = lease()
    evidence = {
        "assignment_id": str(item.id),
        "sample_id": str(item.sample_id),
        "case_content_sha256": "a" * 64,
        "execution_image_digest": "worker@sha256:" + "0" * 64,
        "request_sha256": "b" * 64,
        "response_sha256": "c" * 64,
        "route_trace_id": item.route_trace_id,
        "started_at": datetime.now(UTC).isoformat(),
        "finished_at": datetime.now(UTC).isoformat(),
        "latency_ms": 1,
        "transport_status": "200",
        "environment_fingerprint": "test",
    }
    route = respx.post(
        f"https://radar.example.test/internal/radar/v1/leases/{item.id}/evidence"
    ).mock(
        return_value=httpx.Response(
            200,
            json={
                "data": {
                    "assignment_id": str(item.id),
                    "evidence_manifest_sha256": "d" * 64,
                    "accepted_at": datetime.now(UTC).isoformat(),
                }
            },
        )
    )

    async with ControlPlaneClient(settings()) as client:
        await client.submit_evidence(
            item.id,
            item.lease_token,
            ExecutionEvidence.model_validate(evidence),
            item.lease_epoch,
        )

    body = __import__("json").loads(route.calls[0].request.content)
    assert body["sample_id"] == str(item.sample_id)
    assert body["lease_epoch"] == item.lease_epoch


@pytest.mark.asyncio
@respx.mock
async def test_409_is_fencing_error_and_not_retried() -> None:
    item = lease()
    route = respx.post(
        f"https://radar.example.test/internal/radar/v1/leases/{item.id}/complete"
    ).mock(return_value=httpx.Response(409, json={"code": "LEASE_FENCED"}))
    async with ControlPlaneClient(settings()) as client:
        with pytest.raises(LeaseFencedError):
            await client.complete_assignment(item.id, item.lease_token, item.lease_epoch)
    assert route.call_count == 1


@pytest.mark.asyncio
@respx.mock
async def test_mutating_assignment_calls_forward_lease_epoch() -> None:
    item = lease()
    route = respx.post(
        f"https://radar.example.test/internal/radar/v1/leases/{item.id}/complete"
    ).mock(return_value=httpx.Response(200, json={"data": {"status": "completed"}}))
    async with ControlPlaneClient(settings()) as client:
        await client.complete_assignment(item.id, item.lease_token, item.lease_epoch)
    body = __import__("json").loads(route.calls[0].request.content)
    assert body["lease_epoch"] == item.lease_epoch


@pytest.mark.asyncio
@respx.mock
async def test_heartbeat_forwards_lease_epoch() -> None:
    item = lease()
    route = respx.post(
        f"https://radar.example.test/internal/radar/v1/leases/{item.id}/heartbeat"
    ).mock(return_value=httpx.Response(200, json={"data": {"lease_expires_at": "next"}}))

    async with ControlPlaneClient(settings()) as client:
        await client.heartbeat(item.id, item.lease_token, item.lease_epoch)

    body = __import__("json").loads(route.calls[0].request.content)
    assert body["lease_epoch"] == item.lease_epoch


@pytest.mark.asyncio
@respx.mock
async def test_fail_forwards_lease_epoch() -> None:
    item = lease()
    route = respx.post(
        f"https://radar.example.test/internal/radar/v1/leases/{item.id}/fail"
    ).mock(return_value=httpx.Response(200, json={"data": {"status": "failed"}}))

    async with ControlPlaneClient(settings()) as client:
        await client.fail_assignment(item.id, item.lease_token, "runner_error", item.lease_epoch)

    body = __import__("json").loads(route.calls[0].request.content)
    assert body["lease_epoch"] == item.lease_epoch


@pytest.mark.asyncio
@respx.mock
async def test_confirm_artifact_accepts_scanner_audit_fields() -> None:
    item = lease()
    artifact_id = uuid4()
    scanned_at = datetime.now(UTC)
    route = respx.post(
        f"https://radar.example.test/internal/radar/v1/leases/{item.id}/artifacts/confirm"
    ).mock(
        return_value=httpx.Response(
            200,
            json={
                "data": {
                    "id": str(artifact_id),
                    "object_key": "evaluation-artifacts/run/sample/evidence.json",
                    "sha256": "d" * 64,
                    "bytes": 42,
                    "mime_type": "application/json",
                    "scan_status": "clean",
                    "scan_reason": "stream: OK",
                    "scanner": "clamav",
                    "scanned_at": scanned_at.isoformat(),
                    "confirmed_at": scanned_at.isoformat(),
                }
            },
        )
    )

    async with ControlPlaneClient(settings()) as client:
        receipt = await client.confirm_artifact(
            item.id,
            item.lease_token,
            ArtifactConfirmation(
                artifact_id=artifact_id,
                object_key="evaluation-artifacts/run/sample/evidence.json",
                sha256="d" * 64,
                bytes=42,
            ),
            item.lease_epoch,
        )

    assert receipt.scanner == "clamav"
    assert receipt.scanned_at == scanned_at
    assert receipt.confirmed_at == scanned_at
    assert route.called


@pytest.mark.asyncio
@respx.mock
async def test_presign_and_upload_artifact_forwards_only_signed_object_headers() -> None:
    item = lease()
    artifact_id = uuid4()
    upload_url = "https://objects.example.test/radar/evidence.json?signature=trusted"
    presign_route = respx.post(
        f"https://radar.example.test/internal/radar/v1/leases/{item.id}/artifacts/presign"
    ).mock(
        return_value=httpx.Response(
            200,
            json={
                "data": {
                    "artifact_id": str(artifact_id),
                    "object_key": "evaluation-artifacts/run/sample/evidence.json",
                    "upload_url": upload_url,
                    "upload_headers": {
                        "Content-Type": "application/json",
                        "Content-Length": "2",
                        "X-Amz-Meta-Sha256": (
                            "4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945"
                        ),
                        "X-Amz-Checksum-Sha256": "T1PNoYwrqgwDVLtfmj7L5e0Sq02OEbqHPC8RFhICuUU=",
                        "If-None-Match": "*",
                    },
                    "sha256": "4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945",
                    "bytes": 2,
                    "mime_type": "application/json",
                    "expires_at": datetime.now(UTC).isoformat(),
                }
            },
        )
    )
    upload_route = respx.put(upload_url).mock(return_value=httpx.Response(200))

    async with ControlPlaneClient(settings()) as client:
        upload = await client.presign_artifact(
            item.id,
            item.lease_token,
            ArtifactPresignRequest(
                mime_type="application/json",
                bytes=2,
                sha256="4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945",
            ),
            item.lease_epoch,
        )
        await client.upload_artifact(upload, b"[]")

    request = upload_route.calls[0].request
    assert request.content == b"[]"
    assert "Authorization" not in request.headers
    assert request.headers["If-None-Match"] == "*"
    assert presign_route.called


@pytest.mark.asyncio
@respx.mock
async def test_artifact_upload_treats_precondition_failure_as_recoverable() -> None:
    item = lease()
    artifact_id = uuid4()
    upload_url = "https://objects.example.test/radar/evidence.json?signature=trusted"
    respx.post(
        f"https://radar.example.test/internal/radar/v1/leases/{item.id}/artifacts/presign"
    ).mock(
        return_value=httpx.Response(
            200,
            json={
                "data": {
                    "artifact_id": str(artifact_id),
                    "object_key": "evaluation-artifacts/run/sample/evidence.json",
                    "upload_url": upload_url,
                    "upload_headers": {"Content-Type": "application/json"},
                    "sha256": "4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945",
                    "bytes": 2,
                    "mime_type": "application/json",
                    "expires_at": datetime.now(UTC).isoformat(),
                }
            },
        )
    )
    respx.put(upload_url).mock(return_value=httpx.Response(412))

    async with ControlPlaneClient(settings()) as client:
        upload = await client.presign_artifact(
            item.id,
            item.lease_token,
            ArtifactPresignRequest(
                mime_type="application/json",
                bytes=2,
                sha256="4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945",
            ),
            item.lease_epoch,
        )
        await client.upload_artifact(upload, b"[]")


@pytest.mark.asyncio
@respx.mock
async def test_grader_downloads_artifact_without_forwarding_worker_credentials() -> None:
    lease_id = uuid4()
    artifact_id = uuid4()
    download_url = "https://objects.example.test/radar/evidence.json?signature=read"
    presign_route = respx.post(
        f"https://radar.example.test/internal/radar/v1/grading-leases/{lease_id}/artifacts/{artifact_id}/read"
    ).mock(
        return_value=httpx.Response(
            200,
            json={
                "data": {
                    "artifact_id": str(artifact_id),
                    "object_key": "evaluation-artifacts/run/sample/evidence.json",
                    "download_url": download_url,
                    "sha256": "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a",
                    "bytes": 2,
                    "mime_type": "application/json",
                    "expires_at": datetime.now(UTC).isoformat(),
                }
            },
        )
    )
    download_route = respx.get(download_url).mock(return_value=httpx.Response(200, content=b"{}"))

    async with ControlPlaneClient(settings()) as client:
        download = await client.presign_grading_artifact(
            lease_id, "grading-lease-token", artifact_id, 9
        )
        content = await client.download_artifact(download)

    assert content == b"{}"
    assert "Authorization" not in download_route.calls[0].request.headers
    assert presign_route.called


@pytest.mark.asyncio
@respx.mock
async def test_control_plane_exposes_reliability_and_recovery_execution_api() -> None:
    load_plan_id = uuid4()
    experiment_id = uuid4()
    evidence_id = uuid4()
    load_plan_route = respx.get(
        f"https://radar.example.test/internal/radar/v1/load-plans/{load_plan_id}"
    ).mock(return_value=httpx.Response(200, json={"data": {"id": str(load_plan_id)}}))
    experiment_route = respx.get(
        f"https://radar.example.test/internal/radar/v1/fault-experiments/{experiment_id}"
    ).mock(return_value=httpx.Response(200, json={"data": {"id": str(experiment_id)}}))
    event_route = respx.post(
        f"https://radar.example.test/internal/radar/v1/fault-experiments/{experiment_id}/events"
    ).mock(return_value=httpx.Response(200, json={"data": {"accepted": True}}))
    observation_route = respx.get(
        f"https://radar.example.test/internal/radar/v1/recovery-evidence/{evidence_id}/observation"
    ).mock(return_value=httpx.Response(200, json={"data": {"run_id": str(uuid4())}}))
    publish_route = respx.post(
        f"https://radar.example.test/internal/radar/v1/recovery-evidence/{evidence_id}"
    ).mock(return_value=httpx.Response(200, json={"data": {"accepted": True}}))

    async with ControlPlaneClient(settings()) as client:
        assert (await client.get_load_plan(load_plan_id))["id"] == str(load_plan_id)
        assert (await client.get_fault_experiment(experiment_id))["id"] == str(experiment_id)
        await client.record_fault_event(experiment_id, {"event_type": "started"})
        assert (await client.get_recovery_observation(evidence_id))["run_id"]
        await client.publish_recovery_evidence(evidence_id, {"status": "verified"})

    assert load_plan_route.called
    assert experiment_route.called
    assert event_route.called
    assert observation_route.called
    assert publish_route.called


@pytest.mark.asyncio
@respx.mock
async def test_control_plane_persists_analysis_failure_with_lease_fencing_fields() -> None:
    lease_id = uuid4()
    route = respx.post(
        f"https://radar.example.test/internal/radar/v1/analysis-jobs/{lease_id}/fail"
    ).mock(return_value=httpx.Response(200, json={"data": {"status": "failed"}}))

    async with ControlPlaneClient(settings()) as client:
        await client.fail_analysis(lease_id, "lease-token-123456", "statistics_worker_error", 9)

    body = __import__("json").loads(route.calls[0].request.content)
    assert body == {
        "lease_token": "lease-token-123456",
        "failure_code": "statistics_worker_error",
        "lease_epoch": 9,
    }
