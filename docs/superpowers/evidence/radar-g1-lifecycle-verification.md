# Radar G1 Lifecycle Verification

## Scope

This record covers migration 197, the controlled Worker cutover, lease fencing, failure-first Run reconciliation, exact P0 readiness, and ScoreRef based current-head reads.

Recorded at: 2026-07-28 UTC

Implementation commit before this evidence record: `7e1f050b3`

Migration 197 SHA-256: `f58f6606f0f51063d63d66391f5005a11f1bf05e58f64df841610e2ebf0d5af1`

Writer protocol used by the repository: version `1` in the current process identity. Staging Compose exposes the cutover target as `RADAR_LIFECYCLE_PROTOCOL_VERSION=2`; the backend promotion must update the server-side writer identity before `minimum_protocol_version` is raised.

## Automated Results

- `go test ./...` passed in `backend`.
- `go test -tags integration ./internal/integration -run TestRadarWriterCutover197 -count=1` completed with the repository integration harness skip because Docker is unavailable in this environment.
- `bash -n backend/scripts/radar_migration_197_cutover.sh backend/scripts/radar_migration_197_acceptance.sh` passed.
- `git diff --check` passed before the Task 7 and Task 8 commits.
- The six Reconciler decision tests cover failure-first ordering, superseded failure, exact P0 readiness, paused readiness, current Aggregate coverage, and terminal retry idempotency.
- Worker tests from Task 6 remain green: 43 passed, with Control Plane tests 6 passed.

## Cutover Evidence

The cutover script enforces the following sequence: `open/audit`, `draining`, `closed`, `enforce`. Close checks active Runner, Grader, and Statistics leases, active lease counters in writer sessions, old protocol sessions, and granted advisory locks. A failed check exits with status 1 and leaves the database in `draining`.

The E2E test uses `RADAR_TEST_DATABASE_URL` when provided. It keeps an active old protocol session during drain, verifies that the old writer is rejected under `closed/enforce`, verifies that the current protocol writer is accepted, and returns the database to `open/audit`.

## Known Limitations

- No live PostgreSQL or Docker execution evidence is available on this workstation. Run the integration test and acceptance script against staging before changing the production cutover row.
- The existing gateway service still owns the server-side writer identity. The Compose protocol variable is deployment metadata until that identity is promoted to protocol 2.
- Route Evidence terminalization outbox consumption and Evidence Revision Pipeline verification belong to the following plan.
- The historical `evaluation_scores.is_current` column remains for compatibility. Production reads now join `evaluation_score_heads` using `(score_id, score_created_at)`, and no writer updates the immutable Score row.

## Safety Checks

No bearer token, prompt, completion, canonical evidence payload, or secret appears in this record. Any staging incident keeps the cutover at `closed` until the rollback image and the acceptance test are both verified.
