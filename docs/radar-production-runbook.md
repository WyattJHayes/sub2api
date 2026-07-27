# Radar Production Runbook

This runbook covers the controlled deployment of the Sub2API Model Quality Radar. The staging compose file is suitable for acceptance testing. Production requires the object-store, network, identity, governance, and recovery controls described here.

## Readiness gates

Before enabling production workers, verify all of the following:

- PostgreSQL migrations are applied and the current and next two score and aggregate partitions exist.
- A dedicated evaluation tenant, exclusive group, API key, quota, concurrency limit, and rate limit are provisioned.
- The evaluation key was enabled through Radar governance and its `evaluation_key_events` record names the authenticated actor.
- `RADAR_CONTEXT_SIGNING_KEY` and `RADAR_EVIDENCE_HASH_KEY` are distinct, randomly generated, and stored in the secret manager.
- Runner, Grader, and Statistics have distinct tokens, worker IDs, capabilities, and least-privilege database access.
- Artifact storage uses S3 or MinIO presigned upload, server-side HEAD and SHA256 verification, quarantine, asynchronous malware scanning, and retention policies.
- Worker routes are reachable only on the worker network. Production uses mTLS or an equivalent workload identity in addition to bearer tokens.
- The first 14 complete days are record-only for statistical quality thresholds. P0, route identity, reliability, and evidence preconditions remain active; statistical enforcement starts only after `enforcement_starts_at` is approved.
- Alert delivery, on-call ownership, backup restore, and regional failover tests have current evidence.

## Deployment sequence

1. Apply the migration bundle during a maintenance window. Verify partition creation and migration checksums.
2. Provision the evaluation identity and enable the exact key through the Radar governance API.
3. Configure the control plane with the signing keys, route profile version, region, and maximum context TTL.
4. Register workers idempotently with `worker_kind`, capabilities, image digest, region, and token hash. Never insert worker rows manually during a rollout.
5. Deploy the control plane with a rolling strategy. Confirm `/health`, migration status, and route profile version.
6. Deploy one Runner, one Grader, and one Statistics worker. Confirm heartbeats, lease claims, evidence uploads, score submissions, and aggregate snapshots before scaling.
7. Scale workers within the evaluation quota. Do not allow Radar traffic to consume customer quotas or production concurrency.

## Staging acceptance sequence

Use the staging Compose stack to prove the complete lifecycle before promotion:

1. Confirm the control plane, PostgreSQL, Redis, synthetic upstream, Runner, Grader, and Statistics containers are healthy.
2. Confirm migrations `195_bind_evaluation_gateway_api_key.sql` and `196_add_evaluation_key_events.sql` are present in `schema_migrations` with their expected checksums.
3. Create the first global `platform_admin` Radar role binding, then create `quality_admin`, `test_operator`, and `release_manager` bindings for the same staging administrator. Verify bootstrap access has closed. `platform_admin` is intentionally not a super-role.
4. Provision a bounded API key and enable it through `POST /api/v1/admin/radar/evaluation-keys/:id/enable`.
5. Set `RADAR_SYNTHETIC_UPSTREAM_API_KEY` to a dedicated staging-only value and register the `radar-synthetic-upstream:8090` OpenAI-compatible account. The Compose service is attached only to `control_plane` and publishes no host port.
6. Create and publish a synthetic dataset whose execution path is a same-origin relative gateway path.
7. Create a plan with distinct baseline and candidate route configurations, then start a manual run.
8. Observe assignment claim, gateway route evidence, evidence submission, grading, aggregation, gate decision, and alert lifecycle.
9. Verify the Runs, Datasets, Models, Gates, Alerts, and Workers views at desktop and mobile widths.
10. Save the run ID, route trace IDs, model configuration hashes, dataset manifest hash, score IDs, aggregate ID, gate ID, alert ID, image digests, and migration checksums as acceptance evidence.

The synthetic upstream accepts only `radar-synthetic-baseline` and `radar-synthetic-candidate`. For the same prompt it returns `Paris` for the baseline and `Lyon` for the candidate. An exact grader expecting `Paris` therefore produces a deterministic paired quality regression without calling an external model provider.

Use three `reasoning` cases with distinct `case_key` values and `sample_count: 10`. The three cases may share this deterministic body:

```json
{
  "prompt_spec": {
    "messages": [{"role": "user", "content": "What is the capital of France?"}]
  },
  "expected_spec": "Paris",
  "execution_spec": {"url": "/v1/chat/completions"},
  "grader_id": "exact",
  "grader_version": "v1",
  "confidentiality": "synthetic"
}
```

Use this paired matrix:

```json
[
  {
    "route": "radar-synthetic-quality",
    "baseline": {"route": "radar-synthetic-baseline", "temperature": 0},
    "candidate": {"route": "radar-synthetic-candidate", "temperature": 0}
  }
]
```

The run must create 60 samples organized as 30 baseline and candidate pairs. Acceptance requires 30 valid pairs, a baseline mean of `1`, a candidate mean of `0`, and a paired delta of `-100` percentage points. Any missing pair, route mismatch, invalid evidence, grading failure, or smaller absolute delta is a failed lifecycle acceptance even when all containers remain healthy.

During the first 14 complete days, record-only applies only to statistical quality thresholds. Missing evidence returns `insufficient_evidence`. A new P0 protocol or security failure, route identity mismatch, or reliability SLO breach returns `blocked` and stops promotion or traffic expansion immediately. At the end of the observation period, a `quality_admin` and a `release_manager` who are different users must approve the coverage report, false-positive review, alert delivery evidence, and rollback drill before a new enforcement policy is created.

The migration runner hashes SQL after trimming leading and trailing whitespace. The expected staging values are:

| Migration | Normalized SHA256 |
| --- | --- |
| `193_add_radar_grading_statistics.sql` | `82bef2a9a391adfc2e2dc748587c35e5ced4128574102a59526bdd0687074f59` |
| `194_add_radar_governance.sql` | `a3ff2a00364b10eae7fda2320a556757b356a7bad42a754ce619bbb509dd4603` |
| `195_bind_evaluation_gateway_api_key.sql` | `2f136254beebfbc9470b61798f0bd66ed6100bb67a728914b9c4237b26055e22` |
| `196_add_evaluation_key_events.sql` | `60aadda2e147b677fc6a75e45317dccc496ff0854d2df188289dbb2f5d22eaf4` |

## Staging deployment safety

The current Compose file is an acceptance harness. Keep the existing named volumes and use `docker compose up -d --build`; never run `docker compose down -v` against an environment whose evidence must survive. Preserve the root-owned `0600` environment file and generate a dedicated `RADAR_SYNTHETIC_UPSTREAM_API_KEY` without printing its value.

Before every rollout, render the configuration with the exact environment file, verify the host architecture matches the control-plane binary, and record the pre-rollout binary digest. After rollout, record all seven container image IDs and the new control-plane digest.

The staging base-image entrypoint drops PID 1 to `sub2api` after preparing `/app/data`; verify that runtime identity after every image change. Promotion to production additionally requires an explicit non-root image contract, a read-only control-plane root filesystem, authenticated Redis, separate database, cache, upstream, and worker trust networks, workload identity or mTLS, dependency-aware health probes, external secret delivery, and production artifact storage. The staging `kill -0 1` worker probes and `staging://` artifact adapter provide process-level acceptance evidence only.

## Production promotion security matrix

Every row is a blocking production gate. Evidence is attached to the release record and refreshed at the stated frequency.

| Control | Pass condition | Required evidence | Owner | Frequency |
| --- | --- | --- | --- | --- |
| Runtime identity | Control plane, synthetic service, and workers run with UID other than 0 | Runtime process identity and image security-context export | Platform | Every image |
| Read-only filesystem | Control-plane root filesystem is read-only and writable paths are explicit tmpfs or volumes | Deployment manifest plus write-denial probe | Platform | Every release |
| Redis authentication | Authenticated client succeeds; unauthenticated client receives `NOAUTH` | Both probe results with endpoint redacted | Platform security | Every release and credential rotation |
| Network isolation | Workers cannot connect directly to PostgreSQL or Redis; synthetic upstream cannot reach customer networks | Denied connection matrix from each workload identity | Network security | Every network-policy change and quarterly |
| Workload identity | Missing, expired, wrong-kind, and revoked worker identities are rejected | Negative mTLS or workload-token contract tests | Platform security | Every release |
| External secrets | No secret value appears in image layers, deployment manifests, process arguments, logs, or ordinary environment inspection | Secret scan report and delivery-provider audit event | Security | Every release |
| Dependency health | Readiness fails when required database or cache operations fail; liveness remains process-scoped | Fault-injection probe and recovery timestamps | SRE | Every release |
| Artifact storage | Presign scope, server-side HEAD, SHA256, quarantine, scan, retention, and reader authorization all pass | End-to-end artifact receipt and rejection cases | Data platform | Every release |
| Tenant isolation | Cross-tenant list, detail, mutation, artifact and audit requests are denied | API and browser E2E with two isolated tenants | Application security | Every release |
| Recovery | PostgreSQL PITR and object-manifest recovery meet RPO and RTO with no duplicate score | Signed restore drill report | SRE and database owner | Quarterly |

The current staging Compose stack does not satisfy the production rows for authenticated Redis, separated trust networks, external secrets, dependency readiness, production artifacts, tenant isolation, or PITR. A successful synthetic run cannot waive those rows.

## Artifact protocol

The worker requests a presigned upload for an expected MIME type, byte count, and SHA256. The object key is generated by the control plane and cannot be selected by the worker. The worker uploads directly to quarantine storage. The control plane verifies object metadata, recomputes SHA256, runs the malware scanner, and marks the artifact `clean` only after the scanner succeeds. Graders read only clean artifacts. A missing, rejected, expired, or mismatched artifact produces `invalid_evidence` and never a capability regression.

The `staging://` adapter is for local acceptance only. It must be disabled in production.

## Safe rollout and rollback

- Roll out a new worker image to one worker of each kind and wait for one full lease cycle.
- Compare claim latency, heartbeat failures, evidence rejection, grading failures, and analysis lease age with the previous image.
- Stop claiming new work before rollback. Let active leases expire or explicitly fence them, then deploy the previous image digest.
- Never delete score, evidence, or aggregate rows during rollback. These records are immutable audit evidence.
- If route identity or signing validation fails, disable Radar traffic at the dedicated evaluation key and preserve the incident evidence.

For the staging Compose stack, use this rollback order:

1. Record the failing run IDs, active lease IDs, current image IDs, binary SHA256, and UTC start time.
2. Stop only `radar-runner` first so no new inference assignment is claimed. Keep Grader and Statistics alive while completed evidence drains.
3. Query `evaluation_assignments`, `evaluation_grading_jobs`, and `evaluation_analysis_jobs` for active leases. Wait no longer than the configured lease TTL plus one heartbeat interval.
4. Stop the remaining workers. Any lease still active after the deadline is treated as expired and must be recovered through fencing after restart.
5. Restore the previous control-plane binary or image digest and previous worker image digest, then run `docker compose up -d --build`. Never use `down -v`.
6. Verify all service health, migration checksums, worker identity, old score hashes, aggregate hashes, budget ledger totals, and artifact references before restarting `radar-runner`.
7. Start a new synthetic smoke run. Reopening or mutating the failed run is prohibited.

Production promotion is blocked until the control plane exposes an audited drain and fence operation. Container stop is a staging fallback and does not provide a production workload-identity revocation contract.

## SLOs and alerts

Track separate SLOs for control-plane availability, claim latency, lease expiry, evidence acceptance, grading completion, analysis completion, evaluator inference P99, upstream error rate, retry rate, truncation rate, and artifact scan latency. A quality pass cannot suppress a reliability alert. A reliability pass cannot suppress a quality regression.

Recommended initial alerts:

- control-plane availability below 99.9% over 15 minutes
- P99 claim latency above 500 ms for 10 minutes
- lease expiry rate above 1% of claims
- evidence rejection above 2% of completed executions
- analysis job age above 15 minutes
- evaluator inference P99 above the route SLO for 10 minutes
- new P0 protocol, tool-call, billing, or route-identity failure
- paired capability delta at or below the approved threshold with a 95% interval excluding zero

## Disaster recovery

Back up PostgreSQL with point-in-time recovery and replicate the object store across failure domains. Restore the database and artifact manifest into an isolated environment, verify SHA256 references and score immutability, then replay only pending leases. Do not replay completed score submissions without their idempotency keys. Re-register workers with new tokens after a credential compromise.

Production uses continuous WAL archiving with an archive delay of at most 5 minutes, a daily base backup, 35-day PITR retention, and quarterly restore drills. Object storage enables versioning and cross-failure-domain replication with at least the same 35-day retention for evidence referenced by a retained run. Gate, baseline, role, policy, alert, audit, score, aggregate, budget, and manifest tables are included in every recovery set.

The database owner selects the recovery timestamp immediately before the first confirmed corrupting event. The incident commander approves that timestamp and the target isolated environment. The storage owner selects the corresponding object-store version watermark. Recovery proceeds in this order:

1. Create an isolated database and object-store namespace with all external delivery and customer routes disabled.
2. Restore the latest base backup and replay WAL through the approved timestamp. Record the final transaction timestamp and database system identifier.
3. Restore object versions through the approved watermark. Do not expose the namespace to Grader readers yet.
4. Apply no new migrations. Verify `schema_migrations` checksums against the release manifest that was active at the recovery timestamp.
5. Compare run, sample, assignment, score, score-head, aggregate, gate, budget-ledger and artifact-manifest counts with the backup catalog. Recompute a sample of stored SHA256 values and all artifacts referenced by active baselines.
6. Verify every completed score still has one immutable head, every aggregate references valid scores, every gate references an existing immutable policy, and no completed idempotency key appears more than once.
7. Mark leases that were active at the recovery timestamp as expired. Never enqueue completed samples, grading jobs or analysis jobs.
8. Rotate worker credentials, register new worker identities, enable Grader read access, then run the deterministic 30-pair acceptance.
9. Enable evaluation traffic only after data, security, quality and release owners sign the recovery report. Customer traffic remains outside the recovered Radar environment until the platform failover runbook independently approves it.
10. After the primary fault is understood, repeat consistency checks before controlled failback. Preserve the recovered environment and incident evidence until the post-incident review closes.

RPO is measured from the last durable database transaction and object version available at recovery. RTO starts when failover is declared and ends when the deterministic acceptance and required approvals complete. Missing object versions, checksum mismatch, duplicate immutable records, untraceable policy versions or expired backup evidence fail the drill.

## Evidence retention and access review

Default retention is 400 days for governance, gate, alert, audit, score and aggregate metadata; 30 days for public or synthetic raw artifacts; and 7 days for confidential replay artifacts. A tenant policy may shorten raw-artifact retention. Legal hold may extend selected incident evidence and must record scope, approver, reason and expiry.

Deletion jobs operate from manifest references, write an immutable deletion event, and verify the object is unavailable after deletion. Quarterly access review covers secret-manager readers, object-store readers, database administrators, release approvers and artifact-link issuers. Alert templates and exported reports run automated redaction cases for prompts, completions, hidden reasoning, API keys, raw account IDs, raw channel IDs and upstream response bodies.

## Security boundaries

Never expose raw prompts, completions, hidden reasoning, API keys, account IDs, channel IDs, or arbitrary upstream errors in dashboards or alerts. Route evidence uses stable HMAC references. Worker logs redact authorization headers and evaluation tokens. Rotate worker tokens independently and fence the old workers before enabling replacements.
