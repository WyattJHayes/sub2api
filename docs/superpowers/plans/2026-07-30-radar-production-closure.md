# Radar Production Closure Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the production release gaps in the Radar reliability platform in the agreed priority order, with trusted artifact evidence, tenant isolation, live staging verification, fact-bound acceptance evidence, accurate load measurement, and production observability.

**Architecture:** Keep the existing lease and fencing protocol. Introduce a service-level artifact object store with an S3-compatible implementation and an explicit scan state machine; the database remains the source of artifact metadata while object storage is the source of bytes. Carry tenant identity through authenticated control-plane calls and enforce it at every Radar repository boundary. Keep the current contract harness as the fast path and add an opt-in live E2E mode that exercises the real stack.

**Tech Stack:** Go, PostgreSQL, AWS SDK v2 S3-compatible APIs, Wire, Python 3.11+, httpx, pytest, Docker Compose, Prometheus/OpenTelemetry-compatible instrumentation.

## Global Constraints

* Preserve all existing user changes in the linked worktree.
* Keep lease epoch and fencing checks on every worker mutation.
* Do not mark an artifact clean without an object-store metadata match and a successful scan result.
* Do not trust acceptance JSON values until they are bound to immutable backend identifiers and hashes.
* Use ASCII for newly authored source unless the existing file requires another character set.
* Every behavior change starts with a failing test and ends with focused plus full verification.

---

### Task 1: Artifact object-store contract and S3 implementation

**Files:**
- Create: `backend/internal/service/evaluation_artifact_store.go`
- Create: `backend/internal/repository/evaluation_artifact_store_s3.go`
- Create: `backend/internal/repository/evaluation_artifact_store_s3_test.go`
- Modify: `backend/internal/service/evaluation_grading.go`
- Modify: `backend/internal/config/config.go`
- Modify: `backend/internal/repository/wire.go`
- Modify: `backend/cmd/server/wire_gen.go`

**Interfaces:**
- `service.EvaluationArtifactObjectStore` exposes `PresignPut`, `Head`, `PresignGet`, and `Delete`.
- `service.ArtifactObjectMetadata` carries key, byte count, MIME type, SHA256, and ETag.
- `repository.S3EvaluationArtifactObjectStore` implements the contract using `newS3Client` and a dedicated `RadarArtifactStorageConfig`.

- [x] Write failing tests for presigned upload construction, HEAD metadata normalization, checksum mismatch, and delete/presign-get behavior using an HTTP S3-compatible test server.
- [x] Run `go test ./internal/repository -run ArtifactObjectStore` and confirm the tests fail because the contract and implementation are absent.
- [x] Add the service contract and configuration validation. Require bucket, access key, secret key, and a bounded presign expiry when Radar is enabled.
- [x] Implement S3 PUT presigning with content type, content length, and SHA256 metadata headers; implement HEAD and GET presigning with the same object key.
- [x] Run the focused tests and confirm they pass.

### Task 2: Artifact confirmation state machine and cleanup

**Files:**
- Modify: `backend/internal/repository/evaluation_grading_repo.go`
- Modify: `backend/internal/service/evaluation_grading.go`
- Create: `backend/internal/repository/evaluation_artifact_lifecycle_test.go`
- Modify: `backend/migrations/200_add_radar_reliability_and_dr.sql`
- Create: `backend/internal/service/evaluation_artifact_cleanup.go`
- Create: `backend/internal/service/evaluation_artifact_cleanup_test.go`

**Interfaces:**
- `ConfirmArtifact` calls object-store `Head`, compares byte count, MIME type, and SHA256, and leaves the row in `pending` until an injected scanner returns `clean`.
- `ArtifactScanner` returns `clean`, `rejected`, or `failed` with a stable reason.
- Cleanup deletes only expired objects and marks database rows as deleted after successful object deletion.

- [x] Add failing repository tests proving missing objects, wrong metadata, scanner rejection, scanner failure, and idempotent clean confirmation are rejected or retained in the correct state.
- [x] Run the focused tests and confirm the old direct `clean` behavior is exposed.
- [x] Add the object-store and scanner calls outside the database transaction, then use a short fenced transaction to persist the verified state and scan result.
- [x] Add scan reason, scanned timestamp, and deletion timestamp columns with migration constraints and indexes.
- [x] Add cleanup service tests for expired clean, pending, rejected, and missing-object rows.
- [x] Run repository, service, and migration tests.

### Task 3: Tenant/workspace scope propagation

**Files:**
- Modify: `backend/internal/service/evaluation_governance.go`
- Modify: `backend/internal/service/evaluation_rbac.go`
- Modify: `backend/internal/handler/admin/evaluation_governance_handler.go`
- Modify: `backend/internal/repository/evaluation_governance_repo.go`
- Modify: Radar reliability repositories under `backend/internal/repository/`
- Modify: `backend/migrations/200_add_radar_reliability_and_dr.sql`
- Create: cross-tenant repository and handler tests beside existing Radar tests

- [ ] Add failing tests showing a tenant A actor cannot read, mutate, lease, or decide against tenant B resources.
- [ ] Run those tests and confirm the current global-scope behavior fails them.
- [ ] Define one tenant identifier in the authenticated request context and add it to role bindings, plans, runs, workers, snapshots, evidence, and gate decisions.
- [ ] Require the tenant predicate in every Radar query and add database constraints or RLS where practical.
- [ ] Run all Radar repository, handler, and integration tests.

### Task 4: Live staging E2E

**Files:**
- Modify: `deploy/tests/radar-staging-reliability-test.sh`
- Create: `deploy/tests/radar-live-e2e.sh`
- Modify: `deploy/radar/README.md`
- Modify: reliability Compose overlays and Worker entrypoints under `deploy/` and `radar-worker/deploy/`

- [ ] Add a failing dry-run test that requires an explicit `RADAR_LIVE_E2E=1` and refuses synthetic credentials in live mode.
- [ ] Start the stack with health checks, run a published 30-pair plan through the real Gateway, collect immutable evidence, inject an approved fault, verify recovery, and persist a gate decision.
- [ ] Add billing idempotency replay and duplicate-evidence assertions.
- [ ] Keep contract-only mode fast and deterministic when live mode is unset.
- [ ] Run contract mode and a documented live mode against staging.

### Task 5: Fact-bound acceptance evidence

**Files:**
- Modify: `deploy/radar/reliability-acceptance.py`
- Create: `deploy/radar/reliability-evidence.schema.json`
- Modify: `deploy/tests/radar-staging-reliability-test.sh`
- Modify: backend admin/control-plane APIs used to fetch immutable snapshots and recovery evidence

- [ ] Add failing validator tests for mismatched run ID, load plan, profile, snapshot hash, recovery watermark, and artifact manifest.
- [ ] Add a signed evidence manifest containing those references and a backend verifier command that fetches and recomputes them.
- [ ] Make the acceptance script fail closed when a reference cannot be fetched or recomputed.
- [ ] Run positive and tampered evidence fixtures.

### Task 6: Accurate loadgen streaming measurements

**Files:**
- Modify: `radar-worker/src/sub2api_radar/loadgen/runner.py`
- Create or modify: `radar-worker/src/sub2api_radar/loadgen/sse.py`
- Modify: `radar-worker/tests/test_loadgen_runner.py`
- Create: `radar-worker/tests/test_loadgen_sse.py`

- [ ] Add failing tests for SSE first-token timing, terminal event parsing, malformed SSE, retry count, and required request headers.
- [ ] Replace buffered JSON parsing for streaming cells with `httpx.AsyncClient.stream` and parse the first valid token timestamp.
- [ ] Add tokenizer-backed prompt buckets with a deterministic fallback that reports approximation explicitly.
- [ ] Implement bounded retry measurement without double-counting billing or request totals.
- [ ] Run Worker tests, Ruff, and mypy.

### Task 7: Radar observability and release runbook

**Files:**
- Modify: Radar backend and Worker modules to emit trace and metric fields
- Create: `deploy/radar/prometheus-rules.yml`
- Create: `deploy/radar/grafana-dashboard.json`
- Modify: `deploy/radar/README.md`
- Create: `deploy/radar/production-runbook.md`

- [ ] Add failing metric tests for request, queue, lease, analysis, recovery, cost, and worker heartbeat series.
- [ ] Add trace propagation from Gateway through control plane and Worker.
- [ ] Add P99/TTFT, error budget, queue lag, stale lease, analysis lag, GPU, and billing idempotency alerts.
- [ ] Document secret rotation, image digest pinning, migration cutover, backup/PITR, failover, artifact retention, and rollback steps.
- [ ] Run metric assertions and a staging alert smoke test.

### Task 8: Full verification and release audit

**Files:**
- No new source files unless verification exposes a defect.

- [ ] Run Go unit and integration tests, Worker tests/lint/type checks, migration checks, contract harness, and live staging E2E.
- [ ] Review the diff for tenant predicates, artifact state transitions, secret handling, and generated Wire consistency.
- [ ] Confirm the worktree is intentionally staged for review and record remaining non-blocking risks.
