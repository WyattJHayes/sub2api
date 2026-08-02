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
