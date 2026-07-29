from __future__ import annotations

import argparse
import asyncio
import hashlib
import inspect
import json
import logging
from collections.abc import Callable, Sequence
from pathlib import Path
from typing import Any

from pydantic import ValidationError

from .config import Settings, get_settings
from .control_plane import ControlPlaneClient, LeaseFencedError
from .graders.base import GradeResult
from .graders.coding import CodingSandbox, SandboxLimits
from .graders.exact import exact_grade, json_grade
from .graders.protocol import protocol_grade
from .graders.safety import safety_grade
from .graders.tool_call import tool_call_grade
from .models import ExecutionEvidence, GradingLease, ScoreSubmission

log = logging.getLogger(__name__)


EvidenceLoader = Callable[[GradingLease], ExecutionEvidence | Any]


def _validate_evidence_identity(lease: GradingLease, evidence: ExecutionEvidence) -> None:
    if lease.assignment_id is None:
        raise RuntimeError("grading lease assignment_id is missing")
    if evidence.assignment_id != lease.assignment_id:
        raise RuntimeError("evidence assignment_id mismatch")
    if evidence.sample_id != lease.sample_id:
        raise RuntimeError("evidence sample_id mismatch")
    if lease.case is None:
        raise RuntimeError("grading lease case is missing")
    if evidence.case_content_sha256 != lease.case.content_sha256:
        raise RuntimeError("evidence content_sha256 mismatch")
    if lease.route_trace_id is not None and evidence.route_trace_id != lease.route_trace_id:
        raise RuntimeError("evidence route_trace_id mismatch")


def _parse_evidence_manifest(lease: GradingLease, payload: Any) -> ExecutionEvidence:
    try:
        evidence = ExecutionEvidence.model_validate(payload)
    except ValidationError as exc:
        raise RuntimeError("grader evidence manifest is invalid") from exc
    _validate_evidence_identity(lease, evidence)
    return evidence


def _artifact_relative_path(object_key: str) -> Path:
    value = object_key.strip()
    if value.startswith("staging://"):
        value = value[len("staging://") :]
    if not value or "\x00" in value:
        raise RuntimeError("artifact object key is invalid")
    path = Path(value)
    if path.is_absolute():
        raise RuntimeError("artifact object key must be relative")
    return path


def _read_artifact_manifest(lease: GradingLease, artifact_root: str | Path) -> ExecutionEvidence:
    receipts = [
        receipt
        for receipt in lease.evidence
        if receipt.mime_type.split(";", 1)[0].strip().lower() == "application/json"
    ]
    if len(receipts) != 1:
        raise RuntimeError("exactly one JSON evidence artifact is required")
    receipt = receipts[0]
    if receipt.scan_status != "clean":
        raise RuntimeError("evidence artifact scan_status must be clean")
    root = Path(artifact_root).expanduser().resolve()
    relative = _artifact_relative_path(receipt.object_key)
    path = (root / relative).resolve()
    try:
        path.relative_to(root)
    except ValueError as exc:
        raise RuntimeError("artifact object key escapes artifact root") from exc
    if not path.is_file():
        raise RuntimeError("evidence artifact is missing")
    actual_size = path.stat().st_size
    if actual_size != receipt.bytes:
        raise RuntimeError("evidence artifact bytes mismatch")
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    if digest.hexdigest() != receipt.sha256:
        raise RuntimeError("evidence artifact sha256 mismatch")
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise RuntimeError("evidence artifact is not valid JSON") from exc
    return _parse_evidence_manifest(lease, payload)


def load_evidence(
    lease: GradingLease, *, artifact_root: str | Path | None = None
) -> ExecutionEvidence:
    """Restore runner evidence from the lease or its controlled artifact.

    Inline manifests are authoritative. The artifact fallback only reads a
    local ``staging://`` object beneath the configured artifact root and
    verifies its scan state, size, digest, JSON schema, and evaluation identity.
    """
    if lease.evidence_manifest is not None:
        return _parse_evidence_manifest(lease, lease.evidence_manifest)
    if artifact_root is None:
        raise RuntimeError(
            "grader evidence manifest is missing and RADAR_ARTIFACT_ROOT is not configured"
        )
    return _read_artifact_manifest(lease, artifact_root)


def grade_lease(lease: GradingLease, evidence: ExecutionEvidence) -> GradeResult:
    case = lease.case
    if case is None:
        raise ValueError("grading lease did not include case specification")
    grader_id = lease.grader_id or case.grader_id
    expected = case.expected_spec
    actual = evidence.final_output or ""
    if grader_id in {"exact", "exact_text"}:
        return exact_grade(expected, actual, evidence)
    if grader_id in {"json", "exact_json"}:
        return json_grade(expected if isinstance(expected, str | dict) else {}, actual, evidence)
    if grader_id in {"protocol", "protocol_v1"}:
        return protocol_grade(evidence)
    if grader_id in {"safety", "safety_v1"}:
        return safety_grade(actual, evidence)
    if grader_id in {"tool_call", "tool_call_v1"}:
        expected_calls = expected if isinstance(expected, list) else []
        return tool_call_grade(expected_calls, evidence)
    if grader_id.startswith("coding"):
        spec = case.execution_spec
        CodingSandbox(
            SandboxLimits(
                image_digest=str(spec.get("image_digest", "")),
                cpus=float(spec.get("cpus", 1.0)),
                memory=str(spec.get("memory", "512m")),
                pids=int(spec.get("pids", 128)),
                timeout_seconds=float(spec.get("timeout_seconds", 30.0)),
            )
        )
        raise RuntimeError("coding grader requires an isolated async sandbox adapter")
    raise ValueError(f"unsupported grader {grader_id!r}")


class GraderWorker:
    def __init__(
        self,
        settings: Settings,
        client: ControlPlaneClient,
        *,
        capabilities: Sequence[str] = (),
        evidence_loader: EvidenceLoader | None = None,
    ) -> None:
        self.settings = settings
        self.client = client
        self.capabilities = tuple(capabilities) or settings.grader_ids or settings.capabilities
        self.evidence_loader: EvidenceLoader = evidence_loader or (
            lambda lease: load_evidence(lease, artifact_root=settings.artifact_root)
        )
        self._stop = asyncio.Event()

    def stop(self) -> None:
        self._stop.set()

    async def process_once(self) -> bool:
        lease = await self.client.claim_grading(list(self.capabilities))
        if lease is None:
            return False
        try:
            await self.client.heartbeat_grading(lease.id, lease.lease_token, lease.lease_epoch)
            heartbeat = asyncio.create_task(self._heartbeat(lease))
            try:
                evidence = self.evidence_loader(lease)
                if inspect.isawaitable(evidence):
                    evidence = await evidence
                if not isinstance(evidence, ExecutionEvidence):
                    evidence = ExecutionEvidence.model_validate(evidence)
                result = grade_lease(lease, evidence)
                submission = ScoreSubmission(
                    sample_id=lease.sample_id,
                    grader_id=lease.grader_id or (lease.case.grader_id if lease.case else ""),
                    grader_version=lease.grader_version
                    or (lease.case.grader_version if lease.case else "v1"),
                    score=result.score,
                    passed=result.passed,
                    failure_class=result.failure_class,
                    failure_code=result.failure_code,
                    explanation=result.explanation,
                    evidence_hashes=result.evidence_hashes,
                )
                await self.client.submit_score(
                    lease.id, lease.lease_token, submission, lease.lease_epoch
                )
            finally:
                heartbeat.cancel()
                await asyncio.gather(heartbeat, return_exceptions=True)
            return True
        except LeaseFencedError:
            log.warning("grading lease %s fenced", lease.id)
            return True
        except Exception:
            log.exception("grading lease %s failed", lease.id)
            try:
                await self.client.fail_grading(
                    lease.id, lease.lease_token, "grader_error", lease.lease_epoch
                )
            except Exception:
                log.exception("failed to report grader error for %s", lease.id)
            return True

    async def run_forever(self, stop: asyncio.Event | None = None) -> None:
        stop_event = stop or self._stop
        while not stop_event.is_set():
            claimed = await self.process_once()
            if not claimed:
                try:
                    await asyncio.wait_for(stop_event.wait(), self.settings.poll_interval_seconds)
                except TimeoutError:
                    pass

    async def _heartbeat(self, lease: GradingLease) -> None:
        interval = self.settings.heartbeat_interval_seconds or min(
            max(1, self.settings.lease_ttl_seconds // 3), 30
        )
        while True:
            await asyncio.sleep(interval)
            await self.client.heartbeat_grading(lease.id, lease.lease_token, lease.lease_epoch)


async def run(settings: Settings) -> None:
    async with ControlPlaneClient(settings) as client:
        await GraderWorker(settings, client).run_forever()


def main(argv: list[str] | None = None) -> None:
    parser = argparse.ArgumentParser(description="Sub2API Radar grader")
    parser.add_argument("--version", action="version", version="0.1.0")
    parser.parse_args(argv)
    settings = get_settings()
    logging.basicConfig(level=logging.INFO)
    asyncio.run(run(settings))
