from __future__ import annotations

from enum import StrEnum
from functools import lru_cache
from typing import Annotated
from urllib.parse import urlparse

from pydantic import Field, SecretStr, ValidationInfo, field_validator
from pydantic_settings import BaseSettings, NoDecode, SettingsConfigDict


class WorkerMode(StrEnum):
    RUNNER = "runner"
    GRADER = "grader"
    STATISTICS = "statistics"


class Settings(BaseSettings):
    """Environment-only configuration with fail-closed validation."""

    model_config = SettingsConfigDict(
        env_prefix="RADAR_",
        extra="forbid",
        case_sensitive=False,
        frozen=True,
    )

    allow_insecure_http: bool = False
    control_plane_url: str = Field(min_length=1)
    worker_token: SecretStr = Field(min_length=32)
    worker_id: str = Field(min_length=1)
    mode: WorkerMode = WorkerMode.RUNNER
    request_timeout_seconds: float = Field(default=15.0, gt=0, le=120)
    connect_timeout_seconds: float = Field(default=5.0, gt=0, le=30)
    lease_ttl_seconds: int = Field(default=90, ge=15, le=900)
    heartbeat_interval_seconds: int | None = Field(default=None, ge=1, le=300)
    poll_interval_seconds: float = Field(default=5.0, gt=0, le=300)
    state_dir: str = Field(default="/var/lib/sub2api-radar")
    artifact_root: str = Field(default="/var/lib/sub2api-radar/artifacts", min_length=1)
    max_artifact_bytes: int = Field(default=50 * 1024 * 1024, gt=0, le=1024 * 1024 * 1024)
    region: str = Field(min_length=1)
    route_profile_version: str = Field(min_length=1)
    internal_domains: Annotated[tuple[str, ...], NoDecode] = ()
    executor: str | None = None
    executor_url: str | None = None
    executor_timeout_seconds: float = Field(default=60.0, gt=0, le=600)
    capabilities: Annotated[tuple[str, ...], NoDecode] = ()
    grader_ids: Annotated[tuple[str, ...], NoDecode] = ()
    analysis_capabilities: Annotated[tuple[str, ...], NoDecode] = ()
    analysis_version: str = "v1"

    @field_validator("control_plane_url")
    @classmethod
    def validate_control_plane_url(cls, value: str, info: ValidationInfo) -> str:
        parsed = urlparse(value)
        if parsed.scheme == "https":
            return value.rstrip("/")
        if parsed.scheme == "http" and (
            parsed.hostname in {"localhost", "127.0.0.1", "::1"}
            or bool(info.data.get("allow_insecure_http", False))
        ):
            return value.rstrip("/")
        raise ValueError(
            "control_plane_url must use HTTPS unless insecure HTTP is explicitly enabled"
        )

    @field_validator("heartbeat_interval_seconds")
    @classmethod
    def default_heartbeat_interval(cls, value: int | None, info: ValidationInfo) -> int:
        if value is not None:
            return value
        # Pydantic exposes validated data through ValidationInfo at runtime.
        lease_ttl = int(info.data.get("lease_ttl_seconds", 90))
        return int(min(max(1, lease_ttl // 3), 30))

    @field_validator("internal_domains", mode="before")
    @classmethod
    def split_internal_domains(cls, value: object) -> tuple[str, ...]:
        if value is None or value == "":
            return ()
        if isinstance(value, str):
            return tuple(part.strip().lower() for part in value.split(",") if part.strip())
        if isinstance(value, list | tuple | set):
            return tuple(str(part).strip().lower() for part in value if str(part).strip())
        return (str(value).strip().lower(),)

    @field_validator("capabilities", "grader_ids", "analysis_capabilities", mode="before")
    @classmethod
    def split_capabilities(cls, value: object) -> tuple[str, ...]:
        if value is None or value == "":
            return ()
        if isinstance(value, str):
            return tuple(part.strip() for part in value.split(",") if part.strip())
        if isinstance(value, list | tuple | set):
            return tuple(str(part).strip() for part in value if str(part).strip())
        return (str(value).strip(),)


@lru_cache(maxsize=1)
def get_settings() -> Settings:
    return Settings()  # type: ignore[call-arg]
