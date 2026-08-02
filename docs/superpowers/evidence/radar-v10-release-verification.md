# Radar v10 Release Verification

## Verification Identity

* Release branch: `codex/radar-v10-release`
* Initial Git commit: `fd7a1304233fbd7454e0d2e43ca536a8c716b35d`
* Verification start: `2026-08-02T14:13:58Z`
* Staging host: `101.43.35.235`

## Old Baseline Failure

Command:

```bash
cd backend
go test ./...
```

Result: exit code 1.

The `internal/integration` package failed to compile because the committed baseline contains `radar_outbox_consumer_e2e_test.go` while the service and repository packages lack the implementation used by that test. Representative missing symbols were:

```text
service.EvaluationOutboxConsumerModeCore
service.EvaluationOutboxConsumerModeFull
service.EvaluationOutboxConsumerRuntime
service.NewEvaluationOutboxDispatcher
EvaluationOutboxRepository.EnsureConsumerWorker
```

All other packages that completed before the final exit passed, including `internal/service`, `internal/repository`, `internal/server`, and `migrations`. This result proves the old Git commit cannot serve as the v10 release source by itself.

## Running Staging Identity Before Reconstruction

* Control-plane image ID: `sha256:5de118fe8e0be180bb27e270a4a7f686b757083737853173ab828d459bdcb015`
* Server binary SHA256: `bfe1c72ef8f9d5f8a514f9a3df55d161af80522a0a7888dab1f031dc09d00a24`
* Pricing file SHA256: `139de8a906ce61dc3f086ed394cd01b6c2110341054d7576dce4c4775f358569`
* Applied database migrations: 255
* Terminalization outbox: 0 pending, 18 processed

Further sections are appended only after their commands complete and their outputs have been reviewed.

## Source Reconstruction

The candidate snapshot was imported without deletion using the exclusion contract in the release plan. The final terminalization runtime and test were then overlaid from the recorded outbox E2E source.

Verified overlay hashes:

```text
d5c21629edbd52b0d3fb7b7ce3f28ed7bbf59e15299af95a16f1b1f5c8c54781  evaluation_terminalization_runtime.go
6ad2d34e4141f84909f2a501119bf5821015e64022491f9e85adfb7853ab19f6  evaluation_terminalization_runtime_test.go
```

Verified source properties:

* SQL migration count: 255
* Required compatibility migration: `190_add_users_email_alias_dedup_index_notx.sql` present
* Pricing file SHA256: `139de8a906ce61dc3f086ed394cd01b6c2110341054d7576dce4c4775f358569`
* Files larger than 20 MiB outside `.git`: zero
* Imported `.env` files: zero
* AppleDouble files: zero
* `*.pre-tenant-hotfix` files: zero
* Imported compiled control-plane binaries: zero
* `git diff --check`: exit code 0

A checksum-based `rsync` comparison against the candidate snapshot reported exactly two content differences, the two intentional terminalization overlay files.

The secret-pattern scan found only existing test fixtures and the synthetic staging harness API-key fixture. It found no environment files, credential bundles, PEM files, or private key files in the imported release source.

## Candidate Test Consistency Repair

The first reconstructed full Go test compiled the production server and most packages, then exposed two stale test-side contracts in the remote snapshot.

* `wire_gen_test.go` omitted the terminalization and outbox runtime parameters already required by `provideCleanup`.
* `radar_revision_pipeline_e2e_test.go` lacked the domains-aware fixture already consumed by `radar_outbox_consumer_e2e_test.go`.

The corresponding preserved release-worktree tests were compared against the current production interfaces and copied as a test-only consistency overlay. Their hashes are:

```text
a1f022bdbfe39da1e3540b18d7116b167d4f9999286d67642cfa24c3fd493e3e  wire_gen_test.go
72db544467e3d9868c39631d6d8d9e766e6685b2539e785fde22edb8277a2427  radar_revision_pipeline_e2e_test.go
```

These files are excluded from the server build graph and therefore cannot change the reproduced release binary.

## Worker Verification

The first `uv run` invocation had only runtime dependencies and fell through to an unrelated Miniconda pytest executable. After `uv sync --extra dev`, all Worker checks used the worktree virtual environment.

* pytest: 131 passed
* Ruff: all checks passed
* mypy: no issues in 45 source files

## Pure v10 Source Verification

After the test-consistency overlay, the following commands exited zero:

```text
go test ./...
go build -buildvcs=false ./cmd/server
uv run pytest
uv run ruff check .
uv run mypy
```

The Go suite included successful compilation of `internal/integration`, the full service and repository packages, all server packages, and all 255 migration resources.

## Linux AMD64 Reproduction

The first Docker reproduction attempt used the recorded Dockerfile unchanged. It stopped during `vue-tsc` because the current `node:24-alpine` process reached the Dockerfile's fixed 1536 MB V8 heap limit. The Go build had not started, so this attempt produced no candidate binary.

A temporary Dockerfile changed only the frontend `NODE_OPTIONS` value from 1536 MB to 4096 MB. The repository Dockerfile remained unchanged. The second build retained the exact source, lock file, base image references, Go toolchain, Go command, and fixed release arguments.

The second build completed with:

```text
platform=linux/amd64
image=sha256:d2ee31947458278d01ecbce50d0a85f4e4a86f95efa2f8840c7b0f354ac2a8c9
binary_sha256=bfe1c72ef8f9d5f8a514f9a3df55d161af80522a0a7888dab1f031dc09d00a24
version=radar-terminalization-pricing-security-v10-20260802
commit=radar-candidate-heartbeat-v8-plus-terminalization
built=2026-08-02T12:36:00Z
```

The binary SHA256 exactly matches the running staging v10 binary. This proves the reconstructed source and build identity produce the deployed server byte for byte. The temporary frontend heap adjustment changed build scheduling only and had no effect on the release bytes.

## Artifact Cleanup Log Severity

The regression tests add direct `slog.JSONHandler` coverage for the cleanup polling helper.

Red verification used a temporary detached worktree at commit `91c6d4694` with only the test diff applied. The command exited 1 as expected:

```text
go test ./internal/service -run '^TestLogArtifactCleanupResult' -count=1
internal/service/evaluation_artifact_cleanup_test.go:74:2: undefined: logArtifactCleanupResult
FAIL
```

Green verification in the release worktree:

```text
go test ./internal/service -run '^TestLogArtifactCleanupResult' -count=1
ok  	github.com/Wei-Shaw/sub2api/internal/service	1.894s

go test ./internal/service -run 'ArtifactCleanup' -count=1
ok  	github.com/Wei-Shaw/sub2api/internal/service	1.256s

go test ./internal/service ./internal/repository -count=1
ok  	github.com/Wei-Shaw/sub2api/internal/service	96.771s
ok  	github.com/Wei-Shaw/sub2api/internal/repository	3.824s
```

The implemented helper emits `DEBUG` for empty successful polls, `INFO` for selected successful work, and `ERROR` only when an error is returned. The structured fields retained are `component`, `selected`, `deleted`, `skipped`, and `failed`.

## Pricing Fallback Source Health

The regression tests cover a remote hash failure after a literal local pricing file has been loaded. The service keeps resolving the local model price, reports `Source=local`, records the remote refresh failure, and increments the fallback counter once.

Red verification:

```text
go test ./internal/service -run 'Pricing.*(Fallback|Source|Observability)' -count=1
internal/service/pricing_observability_test.go:13:68: undefined: PricingSourceSnapshot
internal/service/pricing_observability_test.go:18:2: undefined: logPricingSourceSnapshot
internal/service/pricing_observability_test.go:65:13: undefined: PricingSourceMetrics
internal/service/pricing_service_test.go:81:18: svc.SnapshotPricingSource undefined
FAIL
```

Green verification:

```text
go test ./internal/service -run 'Pricing.*(Fallback|Source|Observability)' -count=1
ok  	github.com/Wei-Shaw/sub2api/internal/service	1.610s

go test ./internal/service ./internal/repository -count=1
ok  	github.com/Wei-Shaw/sub2api/internal/service	97.898s
ok  	github.com/Wei-Shaw/sub2api/internal/repository	3.442s
```

The new bounded observability surface is:

```text
PricingSourceSnapshot
SnapshotPricingSource
logPricingSourceSnapshot
PricingSourceMetrics
```

`logPricingSourceSnapshot` emits `WARN` for fallback or failed refresh states and `ERROR` for an empty source. The published low-cardinality series are:

```text
radar_pricing_source_current{source="local|remote|embedded|empty|unknown"}
radar_pricing_source_age_seconds
radar_pricing_last_refresh_ok
radar_pricing_fallback_total
```

The repository has an Ops SQL collector, but no existing Prometheus registry or generic application-gauge exporter in the current source. For this release fix, the metrics are exposed as a stable in-process series producer with literal tests, ready for the deployment probe or a future exporter to scrape without adding new dependencies.

## Release Host Gate

The host gate script records container ID, start timestamp, restart count, running state, and health state, then verifies a fresh capture against that baseline with disk capacity thresholds.

Red verification:

```text
python3 -m unittest deploy/radar/test_release_host_gate.py
FileNotFoundError: release_host_gate.py
exit code 1
```

Green local verification:

```text
python3 -m unittest deploy/radar/test_release_host_gate.py
.......
Ran 7 tests in 0.001s
OK

python3 -m unittest deploy/radar/test_release_host_gate.py deploy/radar/test_reliability_evidence.py
..........
Ran 10 tests in 0.009s
OK

python3 -m py_compile deploy/radar/release_host_gate.py deploy/radar/test_release_host_gate.py
exit code 0

git diff --check
exit code 0
```

Staging host verification:

```text
uploaded: deploy/radar/release_host_gate.py -> root@101.43.35.235:/tmp/radar-release-host-gate-20260802.py
python3 /tmp/radar-release-host-gate-20260802.py capture ... --output /tmp/radar-host-gate-before-20260802.json
exit code 0
python3 /tmp/radar-release-host-gate-20260802.py verify ... --disk-path / --max-used-percent 85 --min-free-gib 10 --output /tmp/radar-host-gate-after-20260802.json
exit code 0
```

Verified targets:

```text
sub2api-radar-staging-sub2api-staging-1
sub2api-radar-staging-radar-runner-1
sub2api-radar-staging-radar-grader-1
sub2api-radar-staging-radar-statistics-1
```

Result summary:

```text
ok=true
checks=22
failures=0
disk_used_percent=83.54
disk_free_bytes=12156874752
```

The non-Radar container `compose-celery-worker-1` is still unhealthy on the shared host. It was deliberately excluded from the Radar release gate target set and remains a shared-host operational risk, not a blocker for the four observed Radar containers.

## Fixed Candidate Staging Evidence

The fixed source candidate was built on the staging host from archive:

```text
source_archive=/tmp/radar-v10-fixed-bc6e476a95ab-20260802.tar.gz
source_archive_sha256=df6501e3d05c68614e321a7f142192c5540456b9f35dc561bba64f5b114fd7d1
unpacked_source=/tmp/radar-build-bc6e476a95ab-20260802-153830
source_commit=bc6e476a95ab
```

The first fixed image completed successfully but did not contain the runtime pricing fallback resource:

```text
image=sub2api/radar-control-plane:radar-v10-fixed-bc6e476a95ab-20260802
image_id=sha256:57869fd765deb30d468ab815aca8bdae66c278c81abde832b2ed57b2a1f07302
binary_sha256=8b7aa76f2bcbb57fd268777b5df07c557aa35d6dfe8f896f7d5f228f51bef571
pricing_resource=/app/resources/model-pricing/model_prices_and_context_window.json
pricing_resource_status=missing
```

The running pre-fix staging image and the source tree both contained the same pricing fallback file:

```text
pricing_resource_sha256=139de8a906ce61dc3f086ed394cd01b6c2110341054d7576dce4c4775f358569
```

A second candidate layer added only `backend/resources/model-pricing/` to `/app/resources/model-pricing/`. It did not change the server binary:

```text
image=sub2api/radar-control-plane:radar-v10-fixed-bc6e476a95ab-20260802-pricing
image_id=sha256:5c0b50508ba200a20fc3637e7d052f17cac900703bffc7e5334302791ddebf37
binary_sha256=8b7aa76f2bcbb57fd268777b5df07c557aa35d6dfe8f896f7d5f228f51bef571
pricing_resource_sha256=139de8a906ce61dc3f086ed394cd01b6c2110341054d7576dce4c4775f358569
```

The candidate labels record the release identity and fixed hashes:

```text
io.sub2api.radar.candidate=true
io.sub2api.radar.source.commit=bc6e476a95ab
io.sub2api.radar.binary.sha256=8b7aa76f2bcbb57fd268777b5df07c557aa35d6dfe8f896f7d5f228f51bef571
io.sub2api.radar.model-pricing.sha256=139de8a906ce61dc3f086ed394cd01b6c2110341054d7576dce4c4775f358569
org.opencontainers.image.version=radar-v10-fixed-20260802-pricing
```

## Disposable Migration Rehearsal

A fresh staging dump was created and retained on the host:

```text
backup_dir=/opt/sub2api-backups/radar-v10-fixed-staging-20260803-162311
backup_file=/opt/sub2api-backups/radar-v10-fixed-staging-20260803-162311/radar-staging.dump
backup_sha256=098541cd8f2e907e2d3ea4717ec40c3e39820679e6a1d3d05c6d5a782e0af21b
backup_size_bytes=26720147
schema_migrations=255
```

The final rehearsal restored that dump into a disposable PostgreSQL 18 container, started a disposable Redis container, and booted the final fixed candidate twice with `AUTO_SETUP=true` so that `repository.ApplyMigrations` executed on each boot. The disposable containers and network used only the `radar-v10-migration-*` namespace and were removed after completion.

Evidence log:

```text
/opt/sub2api-backups/radar-v10-fixed-staging-20260803-162311/migration-rehearsal-pricing.log
```

Result summary:

```text
restored_schema_migrations=255
radar-v10-migration-app1-20260803 healthy_after=6s
255|206_remove_gate_policy_head_tenant_default.sql
radar-v10-migration-app2-20260803 healthy_after=6s
255|206_remove_gate_policy_head_tenant_default.sql
final_schema_migrations=255
```

Pricing initialization in the disposable rehearsal succeeded with the fallback resource present:

```text
[Pricing] Downloaded 218 models successfully
[Pricing] Merged 1 fallback-only models
[Pricing] Service initialized with 218 models
```

## Staging Deployment

The control-plane staging tag was moved to the fixed pricing candidate, then only `sub2api-staging` was recreated with the recorded compose environment file:

```text
compose_file=/opt/sub2api-builds/radar-outbox-e2e-20260801/deploy/docker-compose.radar-staging.yml
compose_env_file=/opt/sub2api-builds/radar-release-20260801-f59afc0c/.env
command=docker compose ... up -d --no-deps --force-recreate sub2api-staging
```

Pre-rollout control-plane state:

```text
image=sub2api/radar-control-plane:candidate-terminalization-pricing-security-v10-20260802
image_id=sha256:5de118fe8e0be180bb27e270a4a7f686b757083737853173ab828d459bdcb015
binary_sha256=bfe1c72ef8f9d5f8a514f9a3df55d161af80522a0a7888dab1f031dc09d00a24
pricing_resource_sha256=139de8a906ce61dc3f086ed394cd01b6c2110341054d7576dce4c4775f358569
```

Post-rollout control-plane state:

```text
image=sub2api/radar-control-plane:staging
image_id=sha256:5c0b50508ba200a20fc3637e7d052f17cac900703bffc7e5334302791ddebf37
health=healthy
restart_count=0
binary_sha256=8b7aa76f2bcbb57fd268777b5df07c557aa35d6dfe8f896f7d5f228f51bef571
pricing_resource_sha256=139de8a906ce61dc3f086ed394cd01b6c2110341054d7576dce4c4775f358569
```

Runtime identity probe:

```text
PID   USER     COMMAND
1     sub2api  /app/sub2api
```

The control-plane recreate briefly made the internal service endpoint unavailable. The runner did not restart. The grader and statistics workers restarted and then recovered:

```text
sub2api-radar-staging-radar-runner-1 restart_count=0
sub2api-radar-staging-radar-grader-1 restart_count=128
sub2api-radar-staging-radar-statistics-1 restart_count=125
```

The post-deploy observation baseline intentionally starts after those recovered worker restarts:

```text
baseline=/opt/sub2api-backups/radar-v10-fixed-staging-20260803-162311/post-deploy-observation-baseline.json
```

## Staging Observation Gate

The first post-deploy gate verify failed only the host capacity checks:

```text
ok=false
checks=22
failures=2
disk_used_percent=98.47
disk_free_bytes=1130532864
```

Root cause investigation found build-only cache pressure:

```text
/tmp/radar-go-build-cache=3.0G
/tmp/radar-go-mod-cache=1007M
docker_build_cache=5.484G
```

The cleanup removed only build caches and temporary extracted image directories. It did not remove staging volumes, backups, running container data, rollback images, or the active candidate image.

After cleanup:

```text
df=/dev/vda2 69G 55G 12G 83%
docker_build_cache=0B
```

The second post-deploy gate verify passed:

```text
verify=/opt/sub2api-backups/radar-v10-fixed-staging-20260803-162311/post-deploy-observation-verify-after-cleanup.json
ok=true
checks=22
failures=0
disk_used_percent=83.24
disk_free_bytes=12377600000
```

Container state at the passing gate:

```text
sub2api-radar-staging-sub2api-staging-1 restarts=0 health=healthy
sub2api-radar-staging-radar-runner-1 restarts=0 health=healthy
sub2api-radar-staging-radar-grader-1 restarts=128 health=healthy
sub2api-radar-staging-radar-statistics-1 restarts=125 health=healthy
```

Application smoke:

```text
GET /health -> 200
terminalization_pending=0
terminalization_processed=18
evaluation_outbox_pending=0
evaluation_outbox_dead_letter=177
analysis_pending_total=3
```

The three pending analysis jobs were created on 2026-08-01 and remain historical staging work. They were not introduced by this deployment. The release-critical terminalization and evaluation outbox pending counts are both zero.

No control-plane `ERROR`, panic, HTTP 5xx, pricing fallback failure, or worker connection error appeared in logs after the post-deploy observation baseline timestamp.

## Production Promotion Status

Production promotion remains gated. The fixed candidate now has staging evidence for source, tests, binary hash, pricing fallback resource, disposable migration rehearsal, runtime identity, health, restart observation, and disk capacity. The production part of the release design still requires a production database backup, current production image and config hashes, immutable digest promotion, production smoke checks, rollback to the recorded digest, and post-rollback restoration evidence.
