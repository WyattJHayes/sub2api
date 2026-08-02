# Radar Staging Reliability Harness

The reliability services live in a separate Compose overlay so the default staging stack does not interpolate or receive load, chaos, or recovery credentials.

Render the load generator and recovery verifier configuration without starting containers:

```bash
docker compose \
  -f deploy/docker-compose.radar-staging.yml \
  -f deploy/docker-compose.radar-reliability.yml \
  --profile reliability config --quiet
```

Render the chaos controller and recovery verifier configuration:

```bash
docker compose \
  -f deploy/docker-compose.radar-staging.yml \
  -f deploy/docker-compose.radar-reliability.yml \
  --profile chaos config --quiet
```

## Required Environment

Both profiles require `RADAR_RELIABILITY_UID` and `RADAR_RELIABILITY_GID`. Use the non-root UID and GID baked into the staging Worker image.

The `reliability` profile also requires:

- `RADAR_LOADGEN_WORKER_TOKEN`
- `RADAR_LOADGEN_EVALUATION_API_KEY`
- `RADAR_LOAD_PLAN_ID`
- `RADAR_LOAD_RUN_ID`
- `RADAR_LOADGEN_IMAGE_DIGEST`
- `RADAR_RECOVERY_VERIFIER_TOKEN`
- `RADAR_RECOVERY_EVIDENCE_ID`
- `RADAR_RELIABILITY_EVIDENCE_DIR`
- `RADAR_RELIABILITY_REPORT_DIR`

The `chaos` profile also requires:

- `RADAR_CHAOS_CONTROLLER_TOKEN`
- `RADAR_CHAOS_AUTO_ROLLBACK_SECONDS`
- `RADAR_FAULT_EXPERIMENT_ID`
- `RADAR_CHAOS_TARGET_WORKER_ID`
- `RADAR_RECOVERY_VERIFIER_TOKEN`
- `RADAR_RECOVERY_EVIDENCE_ID`
- `RADAR_RELIABILITY_EVIDENCE_DIR`
- `RADAR_RELIABILITY_REPORT_DIR`

The tokens and evaluation API key must belong only to the staging Radar tenant. The experiment ID must already be approved and restricted to the named Worker. The controller receives no Docker socket, host namespace, Linux capability, or writable host mount. The automatic rollback delay is bounded to 3600 seconds by Worker configuration and is shortened when the experiment abort deadline is nearer.

## Live Staging E2E

The live script is an opt-in, pre-bound drill. The control plane currently exposes private execution reads for fault and recovery records, while creation and approval remain an administrative workflow. Prepare these records first and bind all of them to the same `RUN_ID`:

- a published `RADAR_LIVE_LOAD_PLAN_ID`
- a non-terminal `RADAR_LIVE_RUN_ID`
- a pre-approved, unexpired `RADAR_LIVE_POLICY_ID` whose policy head is scoped to this run
- an active `RADAR_LIVE_RELEASE_SUBJECT_ID` bound to the same run, environment, and scope
- an approved `RADAR_LIVE_FAULT_EXPERIMENT_ID`
- a pending `RADAR_LIVE_RECOVERY_EVIDENCE_ID`
- the exact approved `RADAR_LIVE_CHAOS_TARGET_WORKER_ID`

The complete script also requires `RADAR_LIVE_ENV_FILE`. It must be a regular file with mode `0600` and contain the Compose release, database, object-store, ClamAV, and staging service settings. The script does not source this file, so administrator credentials, evaluation key, and Worker tokens must be exported in the invoking environment. This keeps the file format declarative and avoids executing shell code from a credentials file.

Live mode is fail-closed. `RADAR_LIVE_E2E=1` is required before any Docker or network operation. Every administrator credential, evaluation key, and Worker token must be a dedicated value of at least 32 characters. Values containing synthetic, placeholder, demo, fake, test, or example markers, whitespace, or a repeated single character are rejected. The staging contract harness exercises these failures with a Docker stub, so it does not contact staging or start containers.

Example invocation:

```bash
export RADAR_LIVE_E2E=1
export RADAR_LIVE_ENV_FILE=/absolute/path/radar-staging.env
export RADAR_LIVE_ADMIN_API_KEY="${STAGING_ADMIN_API_KEY:?load this value from the staging secret manager}"
export RADAR_LIVE_EVALUATION_API_KEY="${STAGING_EVALUATION_API_KEY:?load this value from the staging secret manager}"
export RADAR_RUNNER_WORKER_TOKEN="${STAGING_RUNNER_TOKEN:?load this value from the staging secret manager}"
export RADAR_GRADER_WORKER_TOKEN="${STAGING_GRADER_TOKEN:?load this value from the staging secret manager}"
export RADAR_STATISTICS_WORKER_TOKEN="${STAGING_STATISTICS_TOKEN:?load this value from the staging secret manager}"
export RADAR_LOADGEN_WORKER_TOKEN="${STAGING_LOADGEN_TOKEN:?load this value from the staging secret manager}"
export RADAR_CHAOS_CONTROLLER_TOKEN="${STAGING_CHAOS_TOKEN:?load this value from the staging secret manager}"
export RADAR_RECOVERY_VERIFIER_TOKEN="${STAGING_RECOVERY_TOKEN:?load this value from the staging secret manager}"
export RADAR_LIVE_LOAD_PLAN_ID='00000000-0000-4000-8000-000000000010'
export RADAR_LIVE_RUN_ID='00000000-0000-4000-8000-000000000011'
export RADAR_LIVE_POLICY_ID='00000000-0000-4000-8000-000000000015'
export RADAR_LIVE_RELEASE_SUBJECT_ID='00000000-0000-4000-8000-000000000016'
export RADAR_LIVE_FAULT_EXPERIMENT_ID='00000000-0000-4000-8000-000000000012'
export RADAR_LIVE_RECOVERY_EVIDENCE_ID='00000000-0000-4000-8000-000000000014'
export RADAR_LIVE_CHAOS_TARGET_WORKER_ID='00000000-0000-4000-8000-000000000013'
export RADAR_LIVE_CHAOS_HOLD_SECONDS=15
deploy/tests/radar-live-e2e.sh
```

The live script never creates or self-approves a Gate Policy or Release Subject. Create the immutable policy through the administrative workflow, record both distinct `quality_admin` and `release_manager` approvals with a validity window covering the drill, and pass its UUID through `RADAR_LIVE_POLICY_ID`. Create and activate the Release Subject through the same workflow, then pass its UUID through `RADAR_LIVE_RELEASE_SUBJECT_ID`. The script submits an activation request for the `staging` and `run` scope; the control plane enforces the approval window and rejects a head replacement unless `RADAR_LIVE_EXPECTED_POLICY_ID` explicitly supplies the current head. Set `RADAR_LIVE_POLICY_SCOPE_TYPE` and `RADAR_LIVE_POLICY_SCOPE_ID` only when the pre-bound release subject uses a different canonical scope.

Use `RADAR_LIVE_DRY_RUN=1` with the same dedicated credentials to render the base Compose configuration without starting services. The deterministic contract checks can be run with:

```bash
deploy/tests/radar-staging-reliability-test.sh
```

Set `RADAR_LIVE_CLEANUP=1` when the run should tear down the reliability overlay on exit. Evidence is written below `RADAR_LIVE_EVIDENCE_DIR` or a private temporary directory.

## Acceptance Gate

After the load and recovery jobs write a combined evidence bundle, run:

```bash
deploy/radar/reliability-acceptance.py /absolute/path/to/reliability-acceptance.json
```

The live run requires the published load plan to expand to exactly 30 cells. It checks every measured cell for a complete terminal denominator and zero billing idempotency failures, then replays every published snapshot through the worker endpoint and requires the same immutable snapshot ID and hash. The gate fails closed when any slice exceeds its P99 SLO, terminal outcomes do not equal the complete request denominator, the reported error rate does not recompute, a billing idempotency failure exists, RPO or RTO exceeds its evidence-bound limit, the deterministic 30-pair recovery hash changes, or rollback evidence is incomplete. It reads one JSON file and does not call Docker or mutate staging state.

Rollback evidence includes the failing run and active lease IDs, prior image digests, binary SHA256 values, migration checksums, immutable score and aggregate hashes, artifact manifest hashes, unchanged budget ledger totals, and a new smoke run ID.

## Backend Fact Verification

The control plane exposes a tenant-scoped immutable fact read at:

```text
GET /api/v1/admin/radar/runs/:run_id/reliability-facts?policy_id=:policy_id&profile_id=:profile_id
```

The response is a repeatable-read projection of the run, load plan, policy hash, Release Subject hash, current reliability heads, verified recovery evidence, and artifact manifest hashes. It contains no prompt or completion payloads. Verify it before accepting an evidence bundle:

```bash
deploy/radar/verify-reliability-facts.py \
  --url "https://staging.example/api/v1/admin/radar/runs/${RADAR_LIVE_RUN_ID}/reliability-facts" \
  --run-id "$RADAR_LIVE_RUN_ID" \
  --policy-id "$RADAR_LIVE_POLICY_ID" \
  --profile-id "$RADAR_LIVE_PROFILE_ID" \
  --bearer-token "$RADAR_LIVE_ADMIN_API_KEY" \
  --tenant-id "$RADAR_LIVE_TENANT_ID" \
  --acceptance /absolute/path/to/reliability-acceptance.json
```

The verifier recomputes every returned snapshot hash from its immutable fields, checks tenant and identity bindings, and compares the fact manifest in the acceptance bundle. A missing current head, recovery record, or artifact manifest fails closed.

## Observability

Enable the Worker metrics endpoint with `RADAR_METRICS_ENABLED=true`, then set `RADAR_METRICS_HOST` and `RADAR_METRICS_PORT` for the container network. The endpoint serves Prometheus text at `/metrics`. It exposes fixed-boundary latency and TTFT histograms, Gateway outcomes, queue lag, lease age, analysis lag, recovery duration, cost, billing idempotency failures, worker heartbeat age, GPU utilization, and W3C trace propagation on control-plane and Gateway calls.

Load the alert rules from `deploy/radar/prometheus-rules.yml` and import `deploy/radar/grafana-dashboard.json` into Grafana. The operational procedures for secret rotation, digest pinning, migration cutover, backup and PITR, failover, artifact retention, and rollback are in `deploy/radar/production-runbook.md`.
