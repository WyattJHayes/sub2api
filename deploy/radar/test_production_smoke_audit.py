from __future__ import annotations

import importlib.util
import sys
import unittest
from pathlib import Path
from typing import Any


RADAR_DIR = Path(__file__).resolve().parent
sys.path.insert(0, str(RADAR_DIR))


def load_script(name: str, filename: str) -> Any:
    spec = importlib.util.spec_from_file_location(name, RADAR_DIR / filename)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load {filename}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


smoke_audit = load_script("radar_production_smoke_audit", "production_smoke_audit.py")


SHA_A = "sha256:" + "a" * 64
HEX_A = "1" * 64


def smoke_document(**overrides: Any) -> dict[str, Any]:
    document: dict[str, Any] = {
        "schema_version": smoke_audit.INPUT_SCHEMA_VERSION,
        "accepted_candidate_digest": SHA_A,
        "active_image_digest": SHA_A,
        "app_health": "healthy",
        "health_http_status": 200,
        "api_smoke_ok": True,
        "api_success_count": 3,
        "api_error_count": 0,
        "p99_latency_ms": 480,
        "p99_slo_ms": 500,
        "terminalization_outbox_pending": 0,
        "evaluation_outbox_pending": 0,
        "pricing_source": "local",
        "pricing_resource_sha256": HEX_A,
        "pricing_fallback_failure_count": 0,
        "artifact_cleanup_error_count": 0,
        "billing_idempotency_failures": 0,
        "http_5xx_count": 0,
        "panic_count": 0,
        "control_plane_error_count": 0,
    }
    document.update(overrides)
    return document


class ProductionSmokeAuditTests(unittest.TestCase):
    def test_complete_smoke_evidence_passes(self) -> None:
        result = smoke_audit.audit_smoke(
            smoke_document(),
            min_api_success_count=1,
        )

        self.assertTrue(result["ok"], result)
        self.assertEqual([], result["blockers"])
        self.assertEqual(SHA_A, result["summary"]["active_image_digest"])
        self.assertEqual("local", result["summary"]["pricing_source"])

    def test_active_digest_and_health_must_match_release_candidate(self) -> None:
        result = smoke_audit.audit_smoke(
            smoke_document(
                active_image_digest="sha256:" + "b" * 64,
                app_health="unhealthy",
                health_http_status=503,
            ),
            min_api_success_count=1,
        )

        self.assertFalse(result["ok"], result)
        self.assertIn("active_image_digest_matches_candidate", result["blockers"])
        self.assertIn("app_health", result["blockers"])
        self.assertIn("health_http_status", result["blockers"])

    def test_api_smoke_latency_and_runtime_errors_are_blocking(self) -> None:
        result = smoke_audit.audit_smoke(
            smoke_document(
                api_smoke_ok=False,
                api_success_count=0,
                api_error_count=1,
                p99_latency_ms=501,
                http_5xx_count=1,
                panic_count=1,
                control_plane_error_count=1,
            ),
            min_api_success_count=1,
        )

        self.assertFalse(result["ok"], result)
        self.assertIn("api_smoke_ok", result["blockers"])
        self.assertIn("api_success_count", result["blockers"])
        self.assertIn("api_error_count", result["blockers"])
        self.assertIn("p99_latency_slo", result["blockers"])
        self.assertIn("http_5xx_count", result["blockers"])
        self.assertIn("panic_count", result["blockers"])
        self.assertIn("control_plane_error_count", result["blockers"])

    def test_outbox_pending_counts_must_be_zero(self) -> None:
        result = smoke_audit.audit_smoke(
            smoke_document(
                terminalization_outbox_pending=1,
                evaluation_outbox_pending=2,
            ),
            min_api_success_count=1,
        )

        self.assertFalse(result["ok"], result)
        self.assertIn("terminalization_outbox_pending", result["blockers"])
        self.assertIn("evaluation_outbox_pending", result["blockers"])

    def test_pricing_artifact_cleanup_and_billing_evidence_must_be_clean(self) -> None:
        result = smoke_audit.audit_smoke(
            smoke_document(
                pricing_source="unknown",
                pricing_resource_sha256="bad",
                pricing_fallback_failure_count=1,
                artifact_cleanup_error_count=1,
                billing_idempotency_failures=1,
            ),
            min_api_success_count=1,
        )

        self.assertFalse(result["ok"], result)
        self.assertIn("pricing_source", result["blockers"])
        self.assertIn("pricing_resource_sha256", result["blockers"])
        self.assertIn("pricing_fallback_failure_count", result["blockers"])
        self.assertIn("artifact_cleanup_error_count", result["blockers"])
        self.assertIn("billing_idempotency_failures", result["blockers"])


if __name__ == "__main__":
    unittest.main()
