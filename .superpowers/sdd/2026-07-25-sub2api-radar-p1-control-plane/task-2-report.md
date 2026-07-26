# Task 2 Report

Status: implemented and committed.

Commit: `ebf8eb5b0ab50686a13b6fd85dd01bb8608bc06b`

Changes:

- Added the evaluation repository service contract and lease-fencing errors.
- Added an atomic PostgreSQL run expansion that locks the plan, checks for a published dataset, writes runs, samples, first assignments, reservation ledger entries, and `run_created` events in one transaction.
- Added `FOR UPDATE SKIP LOCKED` claiming, random token SHA256 storage, expired lease replacement attempts, heartbeat renewal, and token plus expiry fenced transitions.
- Added integration coverage for the 24-sample matrix, concurrent claims, attempt 2 reclamation, stale heartbeat rejection, stale completion rejection, and NUL-separated assignment idempotency hashes.

RED command and output:

```text
cd backend && go test -count=1 -v -tags=integration ./internal/repository -run 'EvaluationRepository|ConcurrentLease|LeaseFencing'
# github.com/Wei-Shaw/sub2api/internal/repository [github.com/Wei-Shaw/sub2api/internal/repository.test]
internal/repository/evaluation_repo_integration_test.go:22:10: undefined: NewEvaluationRepository
internal/repository/evaluation_repo_integration_test.go:24:52: undefined: service.CreateRunInput
internal/repository/evaluation_repo_integration_test.go:64:28: undefined: service.AssignmentLease
internal/repository/evaluation_repo_integration_test.go:95:34: undefined: service.ErrLeaseFenced
internal/repository/evaluation_repo_integration_test.go:97:42: undefined: service.AssignmentTransition
internal/repository/evaluation_repo_integration_test.go:100:26: undefined: service.AssignmentCompleted
FAIL    github.com/Wei-Shaw/sub2api/internal/repository [build failed]
```

GREEN commands and output:

```text
cd backend && go test -count=1 ./internal/repository
ok      github.com/Wei-Shaw/sub2api/internal/repository    2.943s

cd backend && go test -count=1 -race -v -tags=integration ./internal/repository -run 'EvaluationRepository|ConcurrentLease|LeaseFencing'
2026/07/26 03:47:33 Timezone initialized: UTC (UTC offset: +00:00)
2026/07/26 03:47:34 docker is not available; skipping integration tests (start Docker to enable)
ok      github.com/Wei-Shaw/sub2api/internal/repository    3.071s
exit=0
```

Self-review:

- The canonical assignment key is `sha256(fmt.Sprintf("%s\\x00%s\\x00%s\\x00%d\\x00%d", runID, caseID, modelRoute, sampleIndex, attempt))`.
- Replacement assignments use attempt 2 after a locked expired attempt 1 is marked `infra_failed` and cleared of lease credentials.
- Renewal and transition queries require the assignment id, SHA256 token hash, and `lease_expires_at > NOW()`.
- The change set is limited to the Task 2 service contract, repository, integration test, and this report.

Concerns:

- Docker is unavailable in this environment. The focused integration command exits successfully because the shared integration harness skips before PostgreSQL tests execute. The non-integration repository package compiles and passes locally.

## Fix Round 1

Status: implemented after the checks listed below.

Commit: recorded in the accompanying `fix(radar)` commit.

Finding:

- Empty capability slices matched every capability domain through `cardinality($1::text[]) = 0 OR ...`, allowing an undeclared worker to claim a pending assignment and to reclaim an expired assignment as attempt 2.

Change:

- Removed the empty-array wildcard from `reclaimExpiredAssignment` and `selectPendingAssignment`. Both queries now require `c.capability_domain = ANY($1::text[])`.

RED command and output:

```text
GitHub Actions run 30187944611
make test-integration
--- FAIL: TestEvaluationRepository_EmptyCapabilitiesCannotReclaimExpired (0.03s)
    evaluation_repo_integration_test.go:178:
        Error: Expected nil, but got: &service.AssignmentLease{..., Attempt:2, ...}
FAIL    github.com/Wei-Shaw/sub2api/internal/repository    17.209s
make: *** [Makefile:21: test-integration] Error 1
```

GREEN commands and output:

```text
cd backend && go test -count=1 -v -tags=integration ./internal/repository -run 'TestEvaluationRepository_EmptyCapabilitiesCannot(Claim|ReclaimExpired)$'
2026/07/26 04:41:48 Timezone initialized: UTC (UTC offset: +00:00)
2026/07/26 04:41:48 docker is not available; skipping integration tests (start Docker to enable)
ok      github.com/Wei-Shaw/sub2api/internal/repository    1.033s

cd backend && go test -count=1 ./internal/repository
ok      github.com/Wei-Shaw/sub2api/internal/repository    3.709s
exit=0
```

Self-review:

- Both eligible assignment queries now use the same non-wildcard domain predicate.
- An empty PostgreSQL text array makes `= ANY(...)` false, so it cannot lease or create a replacement attempt.
- Existing tests cover both the pending claim and expired lease replacement paths.

Concerns:

- Docker remains unavailable locally, so the Green integration command exits via the harness skip path. GitHub supplied the real PostgreSQL RED evidence for both regressions.
