"""Process fixture: real CLI/worker loops with controlled external operations."""
from __future__ import annotations

import asyncio
import hashlib
import json
import sys
from datetime import UTC, datetime, timedelta
from pathlib import Path
from types import SimpleNamespace
from uuid import uuid4

import rfc8785
from test_worker_loops import (
    RunnerClientStub,
    analysis_lease_with_quality_context,
    case,
    evidence,
    settings,
)

import sub2api_radar.config as config_module
import sub2api_radar.grader as grader_module
import sub2api_radar.runner as runner_module
import sub2api_radar.statistics.service as statistics_module
from sub2api_radar.models import ArtifactReceipt, AssignmentLease, GradingLease

mode, phase, directory = sys.argv[1:]
root = Path(directory)
sample_id, assignment_id = uuid4(), uuid4()
item = evidence(sample_id, assignment_id)
payload = rfc8785.dumps(item.model_dump(mode="json"))
artifact = ArtifactReceipt(
    id=uuid4(), object_key="evaluation-artifacts/probe/evidence.json",
    sha256=hashlib.sha256(payload).hexdigest(), bytes=len(payload),
    mime_type="application/json", scan_status="clean", scanner="clamav",
    scanned_at=datetime.now(UTC), confirmed_at=datetime.now(UTC),
)
worker_settings = settings(mode).model_copy(update={"state_dir": str(root / "state")})


async def hold() -> None:
    (root / "ready").touch()
    while not (root / "release").exists():
        await asyncio.sleep(0.01)


class Client(RunnerClientStub):
    def __init__(self) -> None:
        super().__init__()
        self.claims = 0
        self.results: list[object] = []

    async def __aenter__(self):
        return self

    async def __aexit__(self, *args):
        (root / "closed").touch()

    async def claim(self, lease):
        self.claims += 1
        if self.claims != 1:
            raise RuntimeError("new lease claimed after shutdown")
        if phase == "idle":
            if mode != "runner":
                (root / "ready").touch()
            return None
        if phase == "claiming":
            await hold()
        return lease

    async def claim_assignment(self, capabilities):
        return await self.claim(AssignmentLease(
            id=assignment_id, sample_id=sample_id, run_id=uuid4(), case=case(),
            model_route="route-a", attempt=1, lease_token="lease-token-123456",
            lease_expires_at=datetime.now(UTC) + timedelta(minutes=1), lease_epoch=7,
            gateway_evaluation_token="gateway-token", route_trace_id="trace",
        ))

    async def wait_assignment(self):
        (root / "ready").touch()
        # Long polling must be interrupted promptly when no lease is held.
        await asyncio.sleep(60)

    async def claim_grading(self, capabilities):
        return await self.claim(GradingLease(
            id=uuid4(), sample_id=sample_id, assignment_id=assignment_id,
            run_id=uuid4(), case=case(), route_trace_id="trace",
            evidence_manifest=item.model_dump(mode="json"),
            evidence=(artifact,),
            lease_token="lease-token-123456",
            lease_expires_at=datetime.now(UTC) + timedelta(minutes=1), lease_epoch=7,
        ))

    async def presign_grading_artifact(self, lease_id, token, artifact_id, lease_epoch=0):
        assert artifact_id == artifact.id and lease_epoch == 7
        return SimpleNamespace()

    async def download_artifact(self, download):
        return payload

    async def heartbeat_grading(self, lease_id, token, lease_epoch=0):
        assert lease_epoch == 7
        return ""

    async def submit_score(self, lease_id, token, submission, lease_epoch=0):
        assert lease_epoch == 7
        if phase == "active":
            await hold()
        self.results.append({"passed": submission.passed, "score": str(submission.score)})
        return {}

    async def fail_grading(self, *args):
        raise RuntimeError("grading failed during graceful drain")

    async def claim_analysis(self, capabilities):
        return await self.claim(analysis_lease_with_quality_context())

    async def complete_analysis(self, lease_id, token, submission, lease_epoch=0):
        assert lease_epoch == 7
        if phase == "active":
            await hold()
        self.results.append({"quality_report": submission.quality_report is not None})
        return {}

    async def fail_analysis(self, *args):
        raise RuntimeError("analysis failed during graceful drain")


class Executor:
    async def execute(self, lease):
        if phase == "active":
            await hold()
        return item


client = Client()
config_module.get_settings = lambda: worker_settings
module = {"runner": runner_module, "grader": grader_module, "statistics": statistics_module}[mode]
module.ControlPlaneClient = lambda *args, **kwargs: client
if mode == "runner":
    runner_module.get_settings = lambda: worker_settings
    runner_module.build_executor = lambda _settings: Executor()
    runner_module.runner_main([])
elif mode == "grader":
    grader_module.get_settings = lambda: worker_settings
    grader_module.main([])
else:
    statistics_module.main([])
(root / "result.json").write_text(json.dumps({
    "claims": client.claims, "results": client.results,
    "evidence_count": len(client.evidence_submissions),
    "failure_count": len(client.fail_calls),
    "state_records": len(list((root / "state").glob("*.json"))),
}), encoding="utf-8")
