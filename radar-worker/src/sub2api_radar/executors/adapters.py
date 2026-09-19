from __future__ import annotations

import asyncio
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from ..models import AssignmentLease


@dataclass(frozen=True)
class AdapterSpec:
    name: str
    version: str
    image_digest: str
    argv: tuple[str, ...]


class AdapterExecutor:
    def __init__(self, adapters: dict[str, AdapterSpec], *, timeout_seconds: float = 300) -> None:
        self.adapters = adapters
        self.timeout_seconds = timeout_seconds

    async def execute(self, lease: AssignmentLease, workdir: Path) -> dict[str, Any]:
        spec = lease.case.execution_spec
        name = str(spec.get("adapter", ""))
        adapter = self.adapters.get(name)
        if adapter is None:
            raise ValueError(f"adapter {name!r} is not configured")
        if (
            str(spec.get("adapter_version", "")) != adapter.version
            or str(spec.get("image_digest", "")) != adapter.image_digest
        ):
            raise ValueError("adapter version or image digest does not match the case")
        process = await asyncio.create_subprocess_exec(
            *adapter.argv,
            cwd=workdir,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )
        try:
            stdout, stderr = await asyncio.wait_for(process.communicate(), self.timeout_seconds)
        except TimeoutError as exc:
            process.kill()
            await process.wait()
            raise TimeoutError("adapter execution timed out") from exc
        return {
            "exit_code": process.returncode,
            "stdout": stdout[-1_000_000:].decode(errors="replace"),
            "stderr": stderr[-1_000_000:].decode(errors="replace"),
            "adapter": adapter.name,
            "version": adapter.version,
        }
