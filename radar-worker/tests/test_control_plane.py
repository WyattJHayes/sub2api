from datetime import UTC, datetime
from uuid import uuid4

import httpx
import pytest
import respx
from pydantic import SecretStr

from sub2api_radar.config import Settings
from sub2api_radar.control_plane import ControlPlaneClient, LeaseFencedError
from sub2api_radar.models import AssignmentLease, CaseSpec, ExecutionEvidence


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
