from __future__ import annotations

import asyncio
from dataclasses import dataclass
from decimal import Decimal
from pathlib import Path

from ..models import FailureClass
from .base import GradeResult


@dataclass(frozen=True)
class SandboxLimits:
    image_digest: str
    cpus: float = 1.0
    memory: str = "512m"
    pids: int = 128
    timeout_seconds: float = 30.0


class CodingSandbox:
    def __init__(self, limits: SandboxLimits) -> None:
        if "@sha256:" not in limits.image_digest:
            raise ValueError("coding image must be pinned by digest")
        self.limits = limits

    async def verify(self, assignment_dir: Path, command: list[str]) -> GradeResult:
        args = [
            "docker",
            "run",
            "--rm",
            "--network",
            "none",
            "--read-only",
            "--cpus",
            str(self.limits.cpus),
            "--memory",
            self.limits.memory,
            "--pids-limit",
            str(self.limits.pids),
            "--user",
            "65532:65532",
            "--cap-drop",
            "ALL",
            "--security-opt",
            "no-new-privileges",
            "--tmpfs",
            "/tmp:rw,noexec,nosuid,size=64m",
            "-v",
            f"{assignment_dir.resolve()}:/assignment:ro",
            self.limits.image_digest,
            *command,
        ]
        try:
            process = await asyncio.create_subprocess_exec(
                *args, stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.PIPE
            )
            stdout, stderr = await asyncio.wait_for(
                process.communicate(), timeout=self.limits.timeout_seconds
            )
        except TimeoutError:
            process.kill()
            await process.wait()
            return GradeResult(
                Decimal(0),
                None,
                FailureClass.INFRASTRUCTURE,
                "coding_timeout",
                "coding verifier timed out",
            )
        if process.returncode == 0:
            return GradeResult(Decimal(1), True, None, "", stdout[-4000:].decode(errors="replace"))
        return GradeResult(
            Decimal(0),
            False,
            FailureClass.CAPABILITY,
            "tests_failed",
            stderr[-4000:].decode(errors="replace"),
        )
