# Radar v10 Release Closure Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close all six Radar v10 release risks and produce a reproducible, observable, capacity-gated production release with tested rollback.

**Architecture:** Reconstruct the exact running v10 source in an isolated Git worktree from the recorded candidate snapshot and final overlay, then prove binary provenance before changing behavior. Apply the two runtime fixes with TDD, add a fixture-driven host release gate, and use immutable image digests plus database backups for staging and production operations.

**Tech Stack:** Go 1.26.5, Python 3 standard library, PostgreSQL 18, Docker Compose, Linux AMD64 Docker BuildKit, shell release tooling.

## Global Constraints

* Preserve all user changes in the main worktree and `codex/radar-release` worktree.
* Keep the three protected control-plane images and all staging data until rollback verification completes.
* Import no credentials, `.env` files, caches, compiled binaries, AppleDouble files, or temporary hotfix backups into Git.
* Begin every behavior change with a failing test and record the expected failure.
* Build the pure v10 baseline with `VERSION=radar-terminalization-pricing-security-v10-20260802`, `COMMIT=radar-candidate-heartbeat-v8-plus-terminalization`, and `DATE=2026-08-02T12:36:00Z`.
* Require root filesystem use at or below 85 percent and free capacity at or above 10 GiB for promotion.
* Require zero restart-count increase for every observed Radar container.
* Use immutable image digests for production promotion and rollback.

---

### Task 1: Reconstruct and prove the pure v10 source baseline

**Files:**
* Import source files from `/opt/sub2api-builds/radar-candidate-heartbeat-20260802` into the isolated worktree.
* Overlay: `backend/internal/service/evaluation_terminalization_runtime.go`
* Overlay: `backend/internal/service/evaluation_terminalization_runtime_test.go`
* Test consistency overlay: `backend/cmd/server/wire_gen_test.go`
* Test consistency overlay: `backend/internal/integration/radar_revision_pipeline_e2e_test.go`
* Create: `docs/superpowers/evidence/radar-v10-release-verification.md`

**Interfaces:**
* Consumes the recorded remote source snapshot, final terminalization overlay, and fixed build arguments.
* Produces a committed Git source baseline with 255 SQL migrations and a reproducible Linux binary.

- [x] **Step 1: Record the failed old-baseline test**

Run:

```bash
cd backend
go test ./...
```

Expected result: the integration package fails to compile because the old baseline lacks `EvaluationOutboxConsumerRuntime`, its modes, and repository methods.

- [x] **Step 2: Import the candidate source snapshot**

Use `rsync` without deletion and with this exclusion set:

```text
.git/
.superpowers/
.env
.env.*
node_modules/
.venv/
__pycache__/
dist/
bin/
._*
radar-control-plane*
backend/docker.tmp
*.pre-tenant-hotfix
frontend/*.tsbuildinfo
frontend/vite.config.js
frontend/vite.config.d.ts
```

- [x] **Step 3: Overlay the final terminalization files**

Copy the two files from `/opt/sub2api-builds/radar-outbox-e2e-20260801/backend/internal/service/` and verify these SHA256 values:

```text
d5c21629edbd52b0d3fb7b7ce3f28ed7bbf59e15299af95a16f1b1f5c8c54781  evaluation_terminalization_runtime.go
6ad2d34e4141f84909f2a501119bf5821015e64022491f9e85adfb7853ab19f6  evaluation_terminalization_runtime_test.go
```

- [x] **Step 4: Audit the imported tree**

Run checks proving 255 migrations, no excluded files, no files larger than 20 MiB, and no secret-bearing environment files. Inspect every untracked file and run `git diff --check`.

The candidate snapshot contains two stale test files. Replace them with the preserved release-worktree versions and verify these SHA256 values:

```text
a1f022bdbfe39da1e3540b18d7116b167d4f9999286d67642cfa24c3fd493e3e  wire_gen_test.go
72db544467e3d9868c39631d6d8d9e766e6685b2539e785fde22edb8277a2427  radar_revision_pipeline_e2e_test.go
```

- [x] **Step 5: Run source verification**

Run:

```bash
cd backend
go test ./...
go build -buildvcs=false ./cmd/server
cd ../radar-worker
uv run pytest
uv run ruff check .
uv run mypy
```

All commands must exit zero.

- [x] **Step 6: Reproduce the original binary on Linux AMD64**

Build with the recorded Dockerfile, toolchain, build arguments, and pricing file. Extract `/app/sub2api` and require SHA256 `bfe1c72ef8f9d5f8a514f9a3df55d161af80522a0a7888dab1f031dc09d00a24`.

- [x] **Step 7: Commit the pure v10 baseline**

```bash
git add --all
git commit -m "release(radar): reconstruct v10 source baseline"
```

### Task 2: Correct artifact cleanup log severity

**Files:**
* Modify: `backend/internal/service/evaluation_artifact_cleanup_test.go`
* Modify: `backend/internal/service/evaluation_artifact_cleanup.go`

**Interfaces:**
* Consumes `ArtifactCleanupResult` and the cleanup error.
* Produces `logArtifactCleanupResult(*slog.Logger, ArtifactCleanupResult, error)` with deterministic severity and structured fields.

- [x] **Step 1: Write the failing tests**

Add tests that install a `slog.JSONHandler` over a buffer and call the real logging helper. Assert literal outcomes:

```go
require.Equal(t, "DEBUG", record.Level)
require.Equal(t, "radar_artifact_cleanup_poll", record.Message)
require.EqualValues(t, 0, record.Fields["failed"])
```

Add separate tests for selected successful work at `INFO` and a returned error at `ERROR`.

- [x] **Step 2: Verify red**

```bash
go test ./internal/service -run 'TestLogArtifactCleanupResult' -count=1
```

Expected result: compilation fails because `logArtifactCleanupResult` does not exist.

- [x] **Step 3: Implement the minimal helper**

The helper emits `DEBUG` for an empty successful poll, `INFO` for selected successful work, and `ERROR` only when `err` is non-nil. `runOnce` delegates to the helper.

- [x] **Step 4: Verify green and regression scope**

```bash
go test ./internal/service -run 'ArtifactCleanup' -count=1
go test ./internal/service ./internal/repository -count=1
```

- [x] **Step 5: Commit**

```bash
git add backend/internal/service/evaluation_artifact_cleanup.go backend/internal/service/evaluation_artifact_cleanup_test.go
git commit -m "fix(radar): correct artifact cleanup log severity"
```

### Task 3: Expose pricing fallback source and health

**Files:**
* Modify: `backend/internal/service/pricing_service_test.go`
* Modify: `backend/internal/service/pricing_service.go`
* Create: `backend/internal/service/pricing_observability.go`
* Create: `backend/internal/service/pricing_observability_test.go`
* Modify the existing ops metrics collector file that owns application gauges and counters.

**Interfaces:**
* Produces `PricingSourceSnapshot` with source, hashes, timestamps, last refresh result, and fallback count.
* Produces bounded metrics for current source, source age, last refresh health, and fallback total.

- [x] **Step 1: Write failing fallback behavior tests**

Inject a remote hash fetch failure while a literal local price fixture is loaded. Assert the request still resolves pricing, `Source` remains `local` or `embedded`, `LastRefreshOK` is false, and `FallbackTotal` increases by one.

- [x] **Step 2: Write failing log severity test**

Capture the structured record and require `WARN`, `pricing_source`, `source_age_seconds`, and `fallback_total`. Add an empty-source case that requires `ERROR`.

- [x] **Step 3: Verify red**

```bash
go test ./internal/service -run 'Pricing.*(Fallback|Source|Observability)' -count=1
```

- [x] **Step 4: Implement the snapshot and logging**

Keep mutable state under the existing service lock. Use a monotonic atomic counter for fallback events. Avoid metric labels derived from models, URLs, or tenant data.

- [x] **Step 5: Wire and verify metrics**

Add literal metric assertions for the four published series and run:

```bash
go test ./internal/service ./internal/repository -count=1
```

- [x] **Step 6: Commit**

```bash
git add backend/internal/service backend/internal/repository
git commit -m "feat(radar): expose pricing fallback health"
```

### Task 4: Add restart and disk release gates

**Files:**
* Create: `deploy/radar/release_host_gate.py`
* Create: `deploy/radar/test_release_host_gate.py`
* Modify: `deploy/radar/production-runbook.md`

**Interfaces:**
* `capture` writes a JSON state containing container ID, start time, restart count, running state, and health state.
* `verify` compares a fresh capture with the saved state and checks disk thresholds.
* Both commands accept repeated `--container` values and use only the Python standard library plus the Docker CLI.

- [x] **Step 1: Write failing pure gate tests**

Use literal snapshots to prove unchanged historical restart counts pass, any count increase fails, a moved start timestamp fails, unhealthy or absent containers fail, disk use above 85 percent fails, and free space below 10 GiB fails.

- [x] **Step 2: Verify red**

```bash
python3 -m unittest deploy/radar/test_release_host_gate.py
```

Expected result: import fails because `release_host_gate.py` is absent.

- [x] **Step 3: Implement capture and verify**

Return exit code 0 only when every invariant passes. Emit one JSON result with individual checks and measured values. Never perform cleanup from this command.

- [x] **Step 4: Verify locally and on staging**

Run unit tests, capture the four Radar process containers, wait through the chosen observation window, and verify the delta plus disk thresholds.

- [x] **Step 5: Commit**

```bash
git add deploy/radar/release_host_gate.py deploy/radar/test_release_host_gate.py deploy/radar/production-runbook.md
git commit -m "feat(radar): gate releases on restart delta and capacity"
```

### Task 5: Build, rehearse, and observe the fixed staging candidate

**Files:**
* Modify: `docs/superpowers/evidence/radar-v10-release-verification.md`
* No production source changes unless a failed gate produces a TDD bug-fix cycle.

**Interfaces:**
* Consumes the committed source and Tasks 2 through 4.
* Produces an immutable candidate digest, disposable migration proof, and staging observation evidence.

- [x] **Step 1: Run complete local verification**

Run all Go tests, Worker tests, Ruff, mypy, Python gate tests, `git diff --check`, and secret scans. Record exact exit codes.

- [x] **Step 2: Build the fixed Linux image**

Use a new immutable candidate tag and record image digest, binary SHA256, pricing file SHA256, build arguments, and source commit.

- [x] **Step 3: Rehearse migrations**

Restore the verified staging backup into a disposable PostgreSQL container. Apply all 255 migrations twice, proving checksum compatibility and idempotency. Drop only the disposable container and volume after evidence is saved.

- [x] **Step 4: Deploy to staging**

Capture restart baselines first, update the staging control-plane image by digest, and leave Worker and data services unchanged unless their committed source changed.

- [x] **Step 5: Observe and verify**

Require healthy containers, zero restart delta, zero HTTP 5xx, zero false cleanup errors, pricing fallback at `WARN`, current pricing source metrics, terminalization pending zero, and successful runner, grader, and statistics requests.

- [x] **Step 6: Commit evidence**

```bash
git add docs/superpowers/evidence/radar-v10-release-verification.md
git commit -m "test(radar): record v10 staging release gates"
```

Task 5 evidence is recorded in `docs/superpowers/evidence/radar-v10-release-verification.md` under Worker Verification, Pure v10 Source Verification, Linux AMD64 Reproduction, Fixed Candidate Staging Evidence, Disposable Migration Rehearsal, Staging Deployment, and Staging Observation Gate. The accepted staging control-plane image is `sub2api/radar-control-plane:staging` with image ID `sha256:5c0b50508ba200a20fc3637e7d052f17cac900703bffc7e5334302791ddebf37`.

### Task 6: Promote by digest and prove rollback

**Files:**
* Modify: `docs/superpowers/evidence/radar-v10-release-verification.md`
* Update the production deployment manifest only at its existing image reference.

**Interfaces:**
* Consumes an accepted staging digest and verified backup.
* Produces production health evidence, rollback evidence, and a final accepted digest.

- [ ] **Step 1: Audit promotion inputs**

Require clean Git status, accepted staging evidence, backup checksum, production image digest, configuration hashes, migration compatibility proof, and available rollback images.

Current status: staging evidence, disposable migration proof, production configuration hashes, a fail-closed production target preflight, and a fail-closed promotion input audit tool are recorded. The production promotion input audit cannot pass yet because `/opt/sub2api` has no running production Compose project, no active production application container, no current production logical backup, no verified active production image digest, and `.env` is still mode `644`.

- [ ] **Step 2: Create and verify the production backup**

Store the backup outside the deployment directory, compute SHA256, and run a read-only restore listing or disposable restore check.

- [ ] **Step 3: Promote the immutable candidate digest**

Change only the production control-plane image reference. Wait for health and run API, outbox, pricing, artifact cleanup, and billing smoke checks.

- [ ] **Step 4: Exercise rollback**

Restore the recorded previous digest, require health and smoke success, and verify migration compatibility. Record restart deltas and database state.

- [ ] **Step 5: Restore the accepted candidate**

Promote the accepted digest again, repeat health and smoke checks, and keep both rollback digests until the retention window closes.

- [ ] **Step 6: Complete the evidence and commit**

```bash
git add docs/superpowers/evidence/radar-v10-release-verification.md
git commit -m "release(radar): record production promotion and rollback"
```

## Self Review

Every one of the six risks maps to one task and an explicit evidence artifact. Behavioral changes have red and green commands. Source import and deployment steps use existing tests and operational probes because they move artifacts and state without defining new application behavior. No task permits automatic deletion of protected images or production data.
