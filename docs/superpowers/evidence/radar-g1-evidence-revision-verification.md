# Radar G1 Evidence Revision Verification

## Verification Scope

This record covers migration 199 evidence integrity, migrations 200 and 201 compatibility, writer protocol 2 cutover, initial evaluation propagation, and completed Run regrade convergence. The source base before Task 9 was `bd3dac0383630c056a275d1c194e045bb361699b`. The deployment artifact must record its final immutable commit and image digests at execution time.

## Frozen Identifiers

| Item | Value |
| --- | --- |
| Target writer protocol | `2` |
| Migration 199 checksum | `34f7508f1d42bfe96f068e125f0253bdadcf186f1bd0d61aba4f3bda0c281b5b` |
| Migration 200 checksum | `efd09c42917274834aca83c1b4a7690a0eae9fba3b3a51d3e3835fb1deec196d` |
| Migration 201 checksum | `cd530cd6483b0ad59b935ae760a1b9ede2adc2a4964e79f07e9d2056bc874f7a` |
| Golden Envelope SHA256 | `d35bbe7efad5ca5055bc729447c3c009e43a2306d6fea02f1ebb88080f7ba492` |

The HMAC regression is executed by the unit suite. This record intentionally omits key material and signed payload bytes.

## Automated Proof

`TestRadarRevisionPipelineE2E` starts PostgreSQL 18, applies the repository migration set, and runs the real cutover script. It proves these conditions:

1. Seven controlled writer categories block close while live.
2. External database transactions and advisory locks independently block close.
3. Resource release permits `closed/audit`.
4. A modified migration copy fails checksum validation and leaves the database closed.
5. Migrations 199, 200, and 201 validate together before the minimum protocol advances to 2.
6. Reopen fails without a live protocol 2 identity.
7. Registration followed by reopen produces `open/enforce:2`.
8. A real Repository mutation succeeds with the protocol 2 process identity.
9. Four paired initial Scores produce two cell Snapshots, one global Snapshot, and the initial Gate Decision Head.
10. A completed Run creates one Revision Batch with four frozen grading requirements.
11. Four regrade Scores advance immutable Score Heads, then cell, global, and Gate propagation produce a new current Decision.
12. The Revision Batch reaches `completed`, and grading, analysis, and outbox leases return to zero.

## Adjacent Recovery Proof

The focused repository suite covers dead-letter replay, Batch fence, assignment replacement, recursive cause closure, stale event rejection, and recovery generation. The Task 8 implementation commit is `bd3dac0383630c056a275d1c194e045bb361699b`.

Required release verification commands:

```bash
cd backend
go test ./internal/service ./internal/repository ./internal/handler/internal ./internal/handler/admin ./internal/integration -count=1
go test ./internal/repository ./internal/integration -run 'Test.*(Finalize|RevisionBatch|Outbox|Aggregate|RevisionPipeline)' -count=20
go vet ./...

cd ../radar-worker
uv run ruff check .
uv run mypy src
uv run pytest -q
```

## Local Verification Result

Verification on 2026-07-28 produced these results:

- Planned backend scope passed. Service completed in 99.353 seconds, Repository in 4.610 seconds, admin handler in 1.285 seconds, and the real Integration test in 30.369 seconds.
- The repeated Repository and Integration command passed with `count=20`. The complete Integration loop took 474.760 seconds.
- A fresh real E2E rerun passed in 40.161 seconds after the provider portability check was added.
- `go test ./... -count=1` passed. The Integration package safely skipped the container test when no Testcontainers provider was configured.
- Unit-tagged Config and Service tests passed. Service completed in 148.391 seconds.
- `go vet ./...`, Bash syntax validation, and Git whitespace validation exited with status zero.
- Worker Ruff passed, Mypy reported no issues in 30 source files, and Pytest passed 7 tests.

The release operator attaches command exit codes, durations, failure count zero, deployment commit, and image digests to the staging change record. This file contains no request content, model output, token value, signing secret, or real account and channel reference.

## Task 9 Execution Evidence

Verification was rerun on 2026-08-01 from the Radar release worktree.

- `go test ./... -count=1` passed with exit code 0.
- `go vet ./...` passed with exit code 0.
- `go build -buildvcs=false -o /tmp/radar-server-20260801 ./cmd/server` passed with exit code 0. The default VCS stamping path raised a host `bus error`, so the build identity is supplied by the release image arguments.
- `uv run ruff check .` passed, `uv run mypy src` reported no issues in 43 source files, and `uv run pytest -q` passed 151 tests.
- Remote focused repository integration tests covering Gate, Revision, Outbox, Aggregate, Grading, and migration contracts passed except for one unrelated existing SQL failure in `TestEvaluationGradingRepository_FailedAssignmentTerminatesRun`; migration SQL idempotency checks that read files directly require the complete repository source tree in the test image.
- Remote `TestRadarOutboxConsumerMultiCellToGateAndRunCompletion`, `TestRadarOutboxConsumerSingleCellCompatibilityAndReplay`, `TestRadarOutboxConsumerRevisionBatchEpochFencing`, `TestRadarOutboxConsumerFullModeProjectsGateAtomicallyAndIdempotently`, `TestRadarRevisionPipelineE2E`, `TestRadarTrustedGateE2E`, and `TestRadarTrustedGateCutoverRejectsExpiredWriter` all passed.
- The Revision E2E exposed a nullable `rule_ids` write path. The repository now normalizes a nil rule list to a PostgreSQL empty array, and the manual E2E decision fixture supplies the `pass` rule explicitly.
- A first staging candidate was rejected by the immutable migration check because the database held the known historical checksum `734a495...` for migration 203 while the current file computes `d2a557...`. The runner now accepts this exact migration-name and hash pair through its existing compatibility mechanism and continues to reject unknown hashes. The candidate was rolled back before health verification; staging returned to healthy on the preserved `sha256:304abe...` image.

The full repository integration binary also exercised unrelated legacy User and Subscription suites in parallel. Those suites failed on fixed-email collisions and shared-list fixture contamination. They remain separate from the Radar release gate and are recorded as a follow-up isolation issue.

## Task 10 Staging Consumer Closure Evidence

The outbox consumer release was rebuilt and observed on 2026-08-01 UTC. This section supersedes the earlier candidate-only staging note above for the current staging snapshot.

| Item | Observed value |
| --- | --- |
| Authorized embed artifact | `/tmp/radar-server-embed-20260801-v2`, SHA256 `69ff4ce13b7bce63d21983ec7e1fd5b34b1ad0c4dd9f767447fee24ef66d647a`, 116441250 bytes |
| Staging release commit | `f59afc0c` |
| Control-plane image | `sha256:96494731ee78dc9b7db1146c47258fc81597e46d73360c118324c0c91974f2d4` |
| Worker image | `sha256:6c09038135b5ef570a35a7f7751b83202b427283527ca45abd5f9079719a1430` |
| Preserved rollback image | `sha256:304abe975baa509485a6a865aaa115d31d531c3750f04067d401e561180c9865` |
| Effective consumer mode | `full` inside the running control-plane container |
| Cutover state | `open/enforce`, minimum writer protocol `2` |

Remote control-plane health probes at `15:01:39Z`, `15:01:59Z`, `15:02:19Z`, and `15:02:39Z` all returned `{"status":"ok"}`. The container stayed `healthy` with restart count `0` throughout the 60-second observation window.

Run `2719e76a-f573-4c89-bc6c-2c07d1ad8d68` reached `completed`. Its durable propagation counts were:

- `route_evidence_sealed`: 60 completed
- `cell_recompute`: 60 completed
- `gate_reevaluation`: 2 completed, with the newest event completing on attempt 3
- `analysis_jobs`: one `cell/reasoning/v1` job completed; no global job was expected for this single-cell run
- `aggregate_heads`: 1; `gate_decisions`: 1; `decision_heads`: 1; `release_projections`: 1
- Gate decision `insufficient_evidence` produced a blocked release projection and one open P0 alert with cause `insufficient_evidence`

The persistent worker `radar-control-plane-outbox` exists once, with worker kind `statistics`, capability `outbox_consumer`, maximum concurrency `4`, and active claim mode. Idempotency checks returned zero duplicate outbox dedup keys and zero duplicate Gate decision lineages; the decision-head-per-lineage and release-projection-per-subject maxima were both `1`. Active outbox, analysis, and assignment leases were all `0`, and supported pending or leased outbox rows were `0`.

The global outbox table still contains 177 historical `dead_letter` rows, created from 2026-07-29 through 2026-08-01 13:52:22Z. They do not belong to the acceptance Run and require a separate replay or retention decision before a global dead-letter count can be treated as zero.

Follow-up inspection on 2026-08-01 found that all 177 rows belong to failed initial Run `118da730-7829-4013-b71c-a1ecd0c5f14f`. They are `cell_recompute` events from `score_head_event` with `attempt=1` and the single error code `aggregate_dependency_timeout`; the Run is already `failed`, all 60 route evidence events are completed, and the Run has no active outbox leases. The rows predate the accepted consumer release and have no cause-closure records, so automatic replay would create new work against an already failed historical Run without a durable aggregate dependency contract. They are retained as immutable audit evidence. A future replay, if required by incident review, must be an explicit run-scoped operation with a replacement revision and idempotency review. No historical rows were deleted or replayed during this deployment.

Local release verification also passed the targeted service, repository, config, and server tests, the four outbox consumer E2E cases, `TestRadarRevisionPipelineE2E`, the complete package test command, `go build -buildvcs=false ./cmd/server`, and `git diff --check`. The default VCS-stamped build was retried with `-buildvcs=false` after the host Git metadata probe returned a bus error; this did not affect compilation.

The follow-up verification also passed the focused outbox repository integration suite, outbox dispatcher and runtime unit suite, runtime cancellation and fencing cases, server and route tests, Radar worker Ruff and Mypy checks, 151 Radar worker Pytest cases, both staging Compose config checks with required variables, and the Linux embed artifact SHA/version smoke test. The local race command remains unavailable in this macOS Go toolchain because it fails before test execution with `runtime/race: package testmain: cannot find package`; this is an environment limitation rather than a test failure in the runtime.

Post-upload staging revalidation returned `{"status":"ok"}` at `15:56:22Z`, `15:56:42Z`, `15:57:02Z`, and `15:57:22Z`. The running control-plane container remained healthy with restart count `0`, and `/app/sub2api` inside that container hashed to `69ff4ce13b7bce63d21983ec7e1fd5b34b1ad0c4dd9f767447fee24ef66d647a`, matching the authorized artifact byte for byte. The artifact `--version` smoke test reported Radar build commit `fcf9d32aa`.

This evidence excludes request bodies, model outputs, credentials, signing material, and real account or channel identifiers.
