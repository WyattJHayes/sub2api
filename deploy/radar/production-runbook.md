# Radar Production Runbook

This runbook covers the controlled Worker fleet, reliability load generation, artifact evidence, Gate Policy activation, and rollback. The release owner keeps the evidence bundle, image digests, migration checksums, and approval records together for every production change.

## Operating Baseline

Each Worker exposes `/metrics` on the configured `RADAR_METRICS_HOST` and `RADAR_METRICS_PORT` when `RADAR_METRICS_ENABLED=true`. Prometheus loads `deploy/radar/prometheus-rules.yml`. Grafana imports `deploy/radar/grafana-dashboard.json`.

Required labels and identities are bounded to tenant, region, model, route profile, run, and worker kind. Request payloads, API keys, artifact bytes, and prompts never enter metric labels or trace attributes.

Before a release, confirm:

1. Every image is pinned by an immutable digest and the digest appears in the release manifest.
2. The database migration checksum matches the reviewed migration file.
3. The object store bucket, scanner, and retention policy are reachable from the staging Worker network.
4. The load plan is published, expands to the expected 30 cells, and has a fact-bound run ID.
5. The Gate Policy has distinct `quality_admin` and `release_manager` approvals with a validity window covering the run.
6. The release subject, fault experiment, recovery evidence, and Worker identities share the same tenant and run scope.

## Host Release Gate

Capture a restart baseline before changing a staging or production image:

```bash
python3 deploy/radar/release_host_gate.py capture \
  --container sub2api-radar-staging-sub2api-staging-1 \
  --container sub2api-radar-staging-radar-runner-1 \
  --container sub2api-radar-staging-radar-grader-1 \
  --container sub2api-radar-staging-radar-statistics-1 \
  --output /tmp/radar-host-gate-before.json
```

After the observation window, verify the fresh container state and host capacity:

```bash
python3 deploy/radar/release_host_gate.py verify \
  --baseline /tmp/radar-host-gate-before.json \
  --container sub2api-radar-staging-sub2api-staging-1 \
  --container sub2api-radar-staging-radar-runner-1 \
  --container sub2api-radar-staging-radar-grader-1 \
  --container sub2api-radar-staging-radar-statistics-1 \
  --disk-path / \
  --max-used-percent 85 \
  --min-free-gib 10 \
  --output /tmp/radar-host-gate-after.json
```

The verifier exits `0` only when every captured container keeps the same container ID, the same start timestamp, no restart-count increase, `running=true`, `health=healthy`, root filesystem use is at or below 85 percent, and free space is at or above 10 GiB. Exit `1` means a release gate failed and the JSON result contains individual checks. Exit `2` means the gate could not inspect the host or parse its inputs. Do not clean images, volumes, or backups from this command path.

## Sub2API Production Target Preflight

Before mutating an existing `/opt/sub2api` production directory, collect a read-only target snapshot and attach it to the release evidence:

```bash
python3 deploy/radar/production_target_preflight.py capture \
  --target-dir /opt/sub2api \
  --project sub2api \
  --output /tmp/radar-production-target-snapshot.json

python3 deploy/radar/production_target_preflight.py evaluate \
  --snapshot /tmp/radar-production-target-snapshot.json \
  --project sub2api \
  --app-service sub2api \
  --app-port 8080 \
  --output /tmp/radar-production-target-preflight.json
```

The tool exits `0` only when the production Compose project is running, the target application container is present, running, and healthy, `.env` is `0600`, host port `8080` is listening, and the DGC network exposes a Sub2API alias. Exit `1` means the JSON result contains blockers and required operator authorizations. Exit `2` means the tool could not capture or parse the target.

Use these read-only commands as a manual cross-check when the tool reports a blocker:

```bash
docker compose ls
cd /opt/sub2api
docker compose ps --all --format json
docker compose config --images
stat -c "%a %U:%G %s %n" .env data/config.yaml
sha256sum docker-compose.yml docker-compose.override.yml .env data/config.yaml
ss -ltnp | awk 'NR==1 || /:8080|:18080|:80 |:443 /'
docker network inspect dramagenai-cloud_dgc-net --format '{{range .Containers}}{{.Name}} {{.IPv4Address}}{{println}}{{end}}'
```

If `/opt/sub2api` has no active Compose containers, stop the promotion path and obtain explicit operator approval for each item below:

1. `/opt/sub2api` is the intended production target.
2. The stack may be started from the existing local `postgres_data`, `redis_data`, and `data` directories.
3. `.env` may be tightened to `0600`.
4. A fresh logical database backup may be created and checksummed.
5. The accepted candidate may be promoted by immutable digest after the active image digest and configuration hashes are captured.
6. Rollback may be exercised to the recorded digest, followed by restoring the accepted candidate and recording post-rollback evidence.

Treat the first `docker compose up` for an inactive production target as a production exposure event because it can bind host port `8080` and join the shared DGC network. Capture health, listeners, network aliases, image digests, config hashes, and backup checksums before continuing to the image promotion step.

## Secret Rotation

Rotate one identity at a time so the lease protocol continues to have a valid caller.

1. Create a new dedicated Worker token in the tenant secret manager.
2. Deploy the new token to the runner, grader, statistics, loadgen, chaos, and recovery services that use the identity.
3. Verify successful authenticated heartbeats and a harmless `GET /metrics` scrape.
4. Revoke the old token after the old process count reaches zero.
5. Rotate the evaluation API key separately from Worker tokens. Re-run the billing idempotency smoke test after rotation.
6. Record the secret version IDs in the release evidence. Keep secret values out of logs, Compose files, and evidence JSON.

## Image Digests and Migration Cutover

Build and scan the Worker image, then publish its digest to the release manifest. Deploy the digest to one canary Worker and run the deterministic 30-pair smoke set. Promote only after the snapshot hash, artifact manifest hash, and billing ledger totals match the expected facts.

Run migrations with the database backup lock held:

```bash
./backend/scripts/radar_migration_198_cutover.sh
./backend/scripts/radar_migration_199_cutover.sh
```

The migration operator verifies the resulting schema version, indexes, tenant constraints, and migration checksums. Keep the previous image available until the acceptance gate and recovery drill finish.

## Latency and TTFT

For `RadarGatewayP99LatencyHigh` or `RadarGatewayTTFTP99High`:

1. Inspect the dashboard by model, region, and route profile.
2. Compare Gateway latency with queue lag, lease age, GPU utilization, upstream status, and retry count.
3. Stop a rollout when the error budget alert is active or when the same route profile breaches the SLO in two consecutive windows.
4. Preserve the load report and snapshot IDs before changing capacity.
5. Roll back the route profile or image through the release controller. Repeat the deterministic smoke set.

## Gateway Errors

For `RadarGatewayErrorBudgetBurn`, classify failures into upstream, Gateway, timeout, protocol, and client cancellation. Check whether retries use the same `Idempotency-Key`. A retry with a new key requires immediate billing review. Keep the affected run open until the immutable snapshot and ledger facts are reconciled.

## Queue and Leases

For queue lag, stale leases, or stale heartbeats:

1. Confirm the control plane health endpoint and database connection pool.
2. Check worker process health, CPU, memory, and GPU saturation.
3. Inspect lease epoch transitions. A fenced mutation must stop and retain local evidence for recovery.
4. Increase capacity only after confirming the queue is tenant scoped and the load plan budget remains within its ledger.
5. Never delete a local evidence state record while its artifact upload or confirmation is pending.

## Analysis and Recovery

For analysis lag, inspect statistics leases, score reference counts, snapshot freshness, and the analysis query version. A missing score or snapshot reference fails closed and requires a new analysis window.

For `RadarRecoveryDurationHigh`:

1. Capture the failover generation, source watermark, database transaction watermark, and object version.
2. Verify worker re-registration and lease fencing before accepting new work.
3. Run the deterministic 30-pair recovery acceptance set.
4. Compare pre-disaster and recovered acceptance hashes.
5. Publish recovery evidence only when RPO, RTO, duplicate score count, backup freshness, and alert delivery checks are complete.

## Billing and Evidence

For `RadarBillingIdempotencyFailure`:

1. Freeze the affected run and retain its request IDs and idempotency keys.
2. Replay the same request key through the Gateway read path.
3. Compare the billing ledger total, snapshot total, and load report total.
4. Reject duplicate evidence. A second artifact with the same immutable manifest is not a replacement.
5. Escalate to the billing owner when the ledger cannot prove one charge per logical request.

Artifact confirmation requires object metadata match, SHA-256 match, and a clean scanner result. Retention cleanup may delete only expired objects and must record the deletion timestamp after object deletion succeeds.

## Capacity

For `RadarGPUUtilizationSaturated`, compare GPU utilization with queue lag, concurrency, batch size, and P99. Scale within the approved tenant budget. Keep the same Worker image digest and route profile while measuring the effect so the comparison remains valid.

## Failover and Backup/PITR

Backups use encrypted storage with a documented retention period and point-in-time recovery window. The on-call operator records the backup ID and restore timestamp in the recovery observation. Restore into an isolated environment first, run migrations forward, and verify tenant predicates, lease epochs, artifact references, and billing ledger constraints.

During a failover:

1. Stop new reliability runs.
2. Fence the old control plane generation.
3. Promote the database and object store according to the approved provider procedure.
4. Re-register Workers with new lease epochs.
5. Run the deterministic smoke set and recovery verifier.
6. Re-enable traffic only after the Gate Policy and release subject facts still match the recovered head.

## Artifact Retention

Keep immutable acceptance bundles, policy approvals, release manifests, score hashes, aggregate hashes, recovery evidence, and billing ledger receipts for the compliance retention period. Keep raw prompts and responses only in the separately governed encrypted store. Cleanup jobs use a tenant predicate and an explicit expiry cutoff. A cleanup failure leaves the database row pending for the next run.

## Rollback

Rollback is a controlled release action:

1. Stop the loadgen and chaos profiles.
2. Mark the affected run as halted and preserve active lease IDs.
3. Restore the previous Worker and control plane image digests.
4. Revert the route profile only through a new approved policy head.
5. Run the smoke set, then the full 30-cell acceptance plan if the incident involved reliability metrics.
6. Compare snapshot hashes, policy hash, release subject hash, recovery watermark, artifact manifest hash, and budget ledger totals.
7. Record a new smoke run ID and attach it to the rollback evidence.

The acceptance command must fail closed when any immutable reference, HMAC manifest, binary SHA-256, or recovery fact cannot be recomputed.

## Post-Incident Record

Attach the alert timeline, trace IDs, request IDs, lease epochs, image digests, migration checksums, policy approvals, evidence hashes, billing reconciliation, and the final decision. Record residual risks and the owner for each follow-up before closing the incident.
