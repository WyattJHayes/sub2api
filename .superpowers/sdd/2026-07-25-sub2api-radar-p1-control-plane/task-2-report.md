# Task 2 Control Plane Repair Report

## Scope

This repair restores the historical radar control plane migration and moves the
evaluation sample execution identity fields into a new ordered migration. The
repository now retains numeric JSON lexemes while freezing a model configuration
for a lease.

## Migration work

`191_add_radar_control_plane.sql` is byte identical to the Task 1 migration at
`ed7701c86`. A mechanical comparison against the recovered historical copy and
the Git object both returned zero.

`192_add_evaluation_sample_execution_identity.sql` adds `model_config` and
`model_config_sha256`, backfills existing rows with `{}` and its SHA256, applies
defaults and non-null constraints, validates lower-case 64-character SHA256
values, and installs the immutable execution identity trigger. Status updates
remain allowed. Changes to route, frozen config, hash, sample position,
priority, cost, or parent identity raise a database exception.

No migration checksum compatibility exception was added.

## Repository work

`evaluationMatrixEntries` now decodes stored JSON with `UseNumber`. Values over
2^53 and high precision decimal literals survive the decode and marshal cycle.
The canonical JSON bytes are persisted as `model_config`; the SHA256 is computed
from the same bytes and returned in the lease.

## Test coverage

The test commit precedes the production commit and contains:

* `TestEvaluationMatrixEntriesPreservesNumericLexemes`, a pure unit regression
  test for exact frozen JSON and SHA256.
* `TestEvaluationRepository_FreezesLosslessNumericModelConfiguration`, an
  integration scenario exercising sample creation and lease return.
* `TestEvaluationSampleExecutionIdentityMigrationUpgradesOriginal191`, a real
  PostgreSQL upgrade scenario that applies 191 in an isolated schema, inserts a
  complete old sample, applies 192, checks the backfill, permits a status update,
  and rejects a route mutation.

## Verification evidence

RED was executed with the old `json.Unmarshal` implementation:

```text
config = {"route":"route-a","seed":9007199254740992,"temperature":0.12345678901234568}
want   {"route":"route-a","seed":9007199254740993,"temperature":0.12345678901234567890123456789}
FAIL TestEvaluationMatrixEntriesPreservesNumericLexemes
```

GREEN was executed after restoring `UseNumber`:

```text
PASS TestEvaluationMatrixEntriesPreservesNumericLexemes
ok github.com/Wei-Shaw/sub2api/internal/repository
```

The focused integration command compiled and completed. Its harness reported:

```text
docker is not available; skipping integration tests (start Docker to enable)
ok github.com/Wei-Shaw/sub2api/internal/repository
```

Compilation was also verified with and without the integration build tag using
`go test -run '^$' ./internal/repository`; both commands exited successfully.

## Remaining environment constraints

The local Docker daemon is unavailable and no PostgreSQL executable is
installed, so the real PostgreSQL upgrade test could not execute in this
environment. The unrestricted repository test command also reaches existing
tests that need local listeners; sandbox policy rejects their port binding.
Remote CI could not be triggered because pushing was blocked by the environment
policy. These constraints leave the PostgreSQL scenario compiled and reviewed,
with execution deferred to an environment that provides PostgreSQL or Docker.

## Recovery note

The requested worktree is backed by APFS/iCloud placeholder files and its Git
metadata is currently unavailable. Work and commits were performed in the
recovery clone at `/private/tmp/task2-radar-recovery`; the scoped changed files
are copied back mechanically after final verification.
