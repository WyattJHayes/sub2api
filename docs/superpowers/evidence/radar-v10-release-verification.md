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
