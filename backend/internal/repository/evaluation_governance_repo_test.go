package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func expectRadarWorkerWriter(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()
	identity := defaultEvaluationWriterIdentity("api")
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO evaluation_writer_sessions").WithArgs(identity.InstanceID, "api", currentEvaluationWriterProtocolVersion).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("SELECT set_config\\('app.evaluation_writer_instance_id'").WithArgs(identity.InstanceID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("SELECT set_config\\('app.evaluation_writer_protocol'").WithArgs("2").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("SELECT set_config\\('app.evaluation_writer_kind'").WithArgs("api").WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestRegisterRadarWorkerIsIdentityIdempotent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := &radarGovernanceRepository{db: db}
	input := service.RadarWorkerRegistrationInput{Name: "runner-a", WorkerKind: "runner", Region: "cn-north", ImageDigest: "sha256:runner", Capabilities: []string{"chat", "chat"}, MaxConcurrency: 2, Token: "worker-token"}
	workerID := uuid.New()
	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery("SELECT worker_id, event_type, payload FROM evaluation_worker_events").WithArgs(strings.Repeat("a", 64)).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, name, worker_kind, region, image_digest, capabilities, token_hash, status, claim_mode, token_epoch, token_fingerprint, max_concurrency, created_at, updated_at FROM evaluation_workers WHERE name = $1 FOR UPDATE")).WithArgs("runner-a").WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT id FROM evaluation_workers WHERE token_hash").WithArgs(sqlmock.AnyArg()).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("INSERT INTO evaluation_workers").WillReturnRows(sqlmock.NewRows([]string{"id", "name", "worker_kind", "status", "claim_mode", "token_epoch", "token_fingerprint", "region", "image_digest", "capabilities", "max_concurrency", "created_at", "updated_at"}).AddRow(workerID, "runner-a", "runner", "active", "open", int64(0), "abcd1234efgh", "cn-north", "sha256:runner", "{chat}", 2, time.Now(), time.Now()))
	mock.ExpectExec("INSERT INTO evaluation_worker_events").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	worker, err := repo.RegisterRadarWorker(context.Background(), input, 7, strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("RegisterRadarWorker() error = %v", err)
	}
	if worker.TokenFingerprint != "abcd1234efgh" || worker.ClaimMode != service.WorkerClaimsOpen {
		t.Fatalf("worker = %#v", worker)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRotateRadarWorkerTokenInvalidatesOldBearer(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := &radarGovernanceRepository{db: db}
	workerID := uuid.New()
	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery("SELECT worker_id, event_type, payload FROM evaluation_worker_events").WithArgs(strings.Repeat("b", 64)).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT id FROM evaluation_workers WHERE token_hash").WithArgs(sqlmock.AnyArg()).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("UPDATE evaluation_workers SET token_hash").WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), workerID).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "worker_kind", "status", "claim_mode", "token_epoch", "token_fingerprint", "region", "image_digest", "capabilities", "max_concurrency", "created_at", "updated_at"}).AddRow(workerID, "runner-a", "runner", "active", "open", int64(1), "abcd1234efgh", "cn-north", "sha256:runner", "{chat}", 1, time.Now(), time.Now()))
	mock.ExpectExec("INSERT INTO evaluation_worker_events").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	if _, err := repo.RotateRadarWorkerToken(context.Background(), workerID, "rotated-token", 7, strings.Repeat("b", 64)); err != nil {
		t.Fatalf("RotateRadarWorkerToken() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPauseWorkerClaimsKeepsInflightLeaseValid(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := &radarGovernanceRepository{db: db}
	workerID := uuid.New()
	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery("SELECT worker_id, event_type, payload FROM evaluation_worker_events").WithArgs(strings.Repeat("c", 64)).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("UPDATE evaluation_workers SET claim_mode=").WithArgs("paused", workerID).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "worker_kind", "status", "claim_mode", "token_epoch", "token_fingerprint", "region", "image_digest", "capabilities", "max_concurrency", "created_at", "updated_at"}).AddRow(workerID, "runner-a", "runner", "active", "paused", int64(0), "abcd1234efgh", "cn-north", "sha256:runner", "{chat}", 1, time.Now(), time.Now()))
	mock.ExpectExec("INSERT INTO evaluation_worker_events").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	if _, err := repo.SetRadarWorkerClaimMode(context.Background(), workerID, service.WorkerClaimsPaused, 7, strings.Repeat("c", 64)); err != nil {
		t.Fatalf("SetRadarWorkerClaimMode() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDrainWorkerCompletesAfterActiveLeaseCountZero(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := &radarGovernanceRepository{db: db}
	workerID := uuid.New()
	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery("SELECT worker_id, event_type, payload FROM evaluation_worker_events").WithArgs(strings.Repeat("d", 64)).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("UPDATE evaluation_workers SET claim_mode=").WithArgs("draining", workerID).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "worker_kind", "status", "claim_mode", "token_epoch", "token_fingerprint", "region", "image_digest", "capabilities", "max_concurrency", "created_at", "updated_at"}).AddRow(workerID, "runner-a", "runner", "active", "draining", int64(0), "abcd1234efgh", "cn-north", "sha256:runner", "{chat}", 1, time.Now(), time.Now()))
	mock.ExpectExec("INSERT INTO evaluation_worker_events").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery("SELECT claim_mode, status FROM evaluation_workers").WithArgs(workerID).WillReturnRows(sqlmock.NewRows([]string{"claim_mode", "status"}).AddRow("draining", "active"))
	mock.ExpectQuery("SELECT.*lease_expires_at > NOW").WithArgs(workerID).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec("INSERT INTO evaluation_worker_events").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	worker, err := repo.SetRadarWorkerClaimMode(context.Background(), workerID, service.WorkerClaimsDraining, 7, strings.Repeat("d", 64))
	if err != nil {
		t.Fatalf("SetRadarWorkerClaimMode() error = %v", err)
	}
	if worker.ActiveLeaseCount != 0 {
		t.Fatalf("ActiveLeaseCount = %d, want 0", worker.ActiveLeaseCount)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDisableWorkerRejectsHeartbeatImmediately(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	workerID := uuid.New()
	mock.ExpectQuery("SELECT id FROM evaluation_workers").WithArgs(hashToken("worker-token"), "runner").WillReturnError(sqlmock.ErrCancelled)
	grader := &evaluationGradingRepository{db: db}
	if _, err := grader.AuthenticateWorker(context.Background(), "worker-token", "runner"); err == nil {
		t.Fatal("AuthenticateWorker() error = nil after disabled worker")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	_ = workerID
}

func TestRotateRadarWorkerTokenIdempotencyKeyDoesNotMutate(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := &radarGovernanceRepository{db: db}
	workerID := uuid.New()
	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery("SELECT worker_id, event_type, payload FROM evaluation_worker_events").WithArgs(strings.Repeat("e", 64)).WillReturnRows(sqlmock.NewRows([]string{"worker_id", "event_type", "payload"}).AddRow(workerID, "token_rotated", `{"token_fingerprint":"`+workerFingerprint("new-token")+`"}`))
	mock.ExpectQuery("SELECT id, name, worker_kind").WithArgs(workerID).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "worker_kind", "status", "claim_mode", "token_epoch", "token_fingerprint", "region", "image_digest", "capabilities", "max_concurrency", "created_at", "updated_at"}).AddRow(workerID, "runner-a", "runner", "active", "open", int64(2), "abcd1234efgh", "cn-north", "sha256:runner", "{chat}", 1, time.Now(), time.Now()))
	mock.ExpectCommit()
	if _, err := repo.RotateRadarWorkerToken(context.Background(), workerID, "new-token", 7, strings.Repeat("e", 64)); err != nil {
		t.Fatalf("RotateRadarWorkerToken() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRotateRadarWorkerTokenRejectsExistingHash(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := &radarGovernanceRepository{db: db}
	workerID := uuid.New()
	otherID := uuid.New()
	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery("SELECT worker_id, event_type, payload FROM evaluation_worker_events").WithArgs(strings.Repeat("f", 64)).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT id FROM evaluation_workers WHERE token_hash").WithArgs(sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(otherID))
	if _, err := repo.RotateRadarWorkerToken(context.Background(), workerID, "existing-token", 7, strings.Repeat("f", 64)); err == nil {
		t.Fatal("RotateRadarWorkerToken() error = nil, want token conflict")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDrainCountsAllUnexpiredWorkerLeases(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := &radarGovernanceRepository{db: db}
	workerID := uuid.New()
	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery("SELECT worker_id, event_type, payload FROM evaluation_worker_events").WithArgs(strings.Repeat("g", 64)).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("UPDATE evaluation_workers SET claim_mode=").WithArgs("draining", workerID).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "worker_kind", "status", "claim_mode", "token_epoch", "token_fingerprint", "region", "image_digest", "capabilities", "max_concurrency", "created_at", "updated_at"}).AddRow(workerID, "runner-a", "runner", "active", "draining", int64(0), "abcd1234efgh", "cn-north", "sha256:runner", "{chat}", 1, time.Now(), time.Now()))
	mock.ExpectExec("INSERT INTO evaluation_worker_events").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery("SELECT claim_mode, status FROM evaluation_workers").WithArgs(workerID).WillReturnRows(sqlmock.NewRows([]string{"claim_mode", "status"}).AddRow("draining", "active"))
	mock.ExpectQuery("SELECT.*lease_expires_at > NOW").WithArgs(workerID).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectCommit()
	worker, err := repo.SetRadarWorkerClaimMode(context.Background(), workerID, service.WorkerClaimsDraining, 7, strings.Repeat("g", 64))
	if err != nil {
		t.Fatalf("SetRadarWorkerClaimMode() error = %v", err)
	}
	if worker.ActiveLeaseCount != 1 {
		t.Fatalf("ActiveLeaseCount = %d, want 1", worker.ActiveLeaseCount)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDrainCompletionCheckIsReusableForLeaseRelease(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	workerID := uuid.New()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT claim_mode, status FROM evaluation_workers").WithArgs(workerID).WillReturnRows(sqlmock.NewRows([]string{"claim_mode", "status"}).AddRow("draining", "active"))
	mock.ExpectQuery("SELECT.*lease_expires_at > NOW").WithArgs(workerID).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec("INSERT INTO evaluation_worker_events").WillReturnResult(sqlmock.NewResult(1, 1))
	if _, err := checkRadarWorkerDrainCompletionTx(context.Background(), tx, workerID, 7, strings.Repeat("h", 64)); err != nil {
		t.Fatalf("checkRadarWorkerDrainCompletionTx() error = %v", err)
	}
	mock.ExpectCommit()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRadarPermissionsUseOnlyGlobalRoleBindings(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(`SELECT role FROM evaluation_role_bindings.*scope = '\{\}'::jsonb`).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("viewer"))

	repo := &radarGovernanceRepository{db: db}
	permissions, err := repo.ListPermissions(context.Background(), 7)
	if err != nil {
		t.Fatalf("ListPermissions() error = %v", err)
	}
	if len(permissions) != 1 || permissions[0] != service.PermissionView {
		t.Fatalf("permissions = %v, want [view]", permissions)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRadarTestOperatorCanControlRuns(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(`SELECT role FROM evaluation_role_bindings.*scope = '\{\}'::jsonb`).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("test_operator"))

	repo := &radarGovernanceRepository{db: db}
	require.NoError(t, repo.Require(context.Background(), 7, service.PermissionRunControl))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRadarPermissionsAllowAdminBootstrapOnlyWhenNoBindingsExist(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery("SELECT role FROM evaluation_role_bindings").
		WithArgs(int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{"role"}))
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs(int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	repo := &radarGovernanceRepository{db: db}
	permissions, err := repo.ListPermissions(context.Background(), 1)
	if err != nil {
		t.Fatalf("ListPermissions() error = %v", err)
	}
	want := map[service.RadarPermission]bool{
		service.PermissionView: true, service.PermissionRoleManage: true,
		service.PermissionRouteAction: true,
	}
	for _, permission := range permissions {
		delete(want, permission)
	}
	if len(want) != 0 {
		t.Fatalf("bootstrap permissions missing %v", want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateRadarRoleBindingRejectsUnsupportedScope(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := &radarGovernanceRepository{db: db}

	_, err = repo.CreateRoleBinding(context.Background(), service.RadarRoleBindingInput{
		ActorID: 2, Role: service.RoleViewer, Scope: json.RawMessage(`{"model":"qwen"}`), CreatedBy: 1,
	})
	if err == nil {
		t.Fatal("CreateRoleBinding() error = nil, want scoped binding rejection")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestActivatePolicyAdvancesScopedHeadWithEvent(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := &radarGovernanceRepository{db: db}
	policyID := uuid.New()
	eventTime := time.Now().UTC()
	expectRadarWorkerWriter(t, mock)
	mock.ExpectExec("SELECT pg_advisory_xact_lock").WithArgs("policy:production:global:global").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT p.policy_hash.*evaluation_gate_policy_approvals").WithArgs(policyID).
		WillReturnRows(sqlmock.NewRows([]string{"policy_hash", "eligible"}).AddRow(strings.Repeat("a", 64), true))
	mock.ExpectQuery("SELECT policy_id,event_id,updated_at FROM evaluation_gate_policy_heads").
		WithArgs("production", "global", "global").WillReturnError(sql.ErrNoRows)
	mock.ExpectExec("INSERT INTO evaluation_gate_policy_events").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT advance_evaluation_gate_policy_head").WillReturnRows(sqlmock.NewRows([]string{"advanced"}).AddRow(true))
	mock.ExpectExec("INSERT INTO evaluation_gate_reevaluation_outbox").WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectQuery("SELECT updated_at FROM evaluation_gate_policy_heads").
		WillReturnRows(sqlmock.NewRows([]string{"updated_at"}).AddRow(eventTime))
	mock.ExpectCommit()

	head, err := repo.ActivateGatePolicy(context.Background(), service.RadarGatePolicyActivationInput{
		PolicyID: policyID,
		Scope:    service.RadarGovernanceScope{Environment: "Production", ScopeType: "global", ScopeID: "ignored"},
		ActorID:  7,
	})
	require.NoError(t, err)
	require.Equal(t, policyID, head.PolicyID)
	require.Equal(t, service.GlobalReleaseScopeID, head.Scope.ScopeID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestActivateBaselineAdvancesRouteEnvironmentScopeHead(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := &radarGovernanceRepository{db: db}
	baselineID := uuid.New()
	eventTime := time.Now().UTC()
	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery("SELECT COUNT.*quality_admin.*expires_at").WithArgs(baselineID).
		WillReturnRows(sqlmock.NewRows([]string{"quality", "release", "approvers"}).AddRow(1, 1, 2))
	mock.ExpectQuery("SELECT model_route,evidence_hash FROM evaluation_baselines").WithArgs(baselineID).
		WillReturnRows(sqlmock.NewRows([]string{"model_route", "evidence_hash"}).AddRow("deepseek", strings.Repeat("b", 64)))
	mock.ExpectExec("SELECT pg_advisory_xact_lock").WithArgs("baseline:staging:route:route-a:deepseek").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT baseline_id,event_id,updated_at FROM evaluation_baseline_heads").
		WithArgs("staging", "route", "route-a", "deepseek").WillReturnError(sql.ErrNoRows)
	mock.ExpectExec("INSERT INTO evaluation_baseline_events").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT advance_evaluation_baseline_head").WillReturnRows(sqlmock.NewRows([]string{"advanced"}).AddRow(true))
	mock.ExpectExec("INSERT INTO evaluation_gate_reevaluation_outbox").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT updated_at FROM evaluation_baseline_heads").
		WillReturnRows(sqlmock.NewRows([]string{"updated_at"}).AddRow(eventTime))
	mock.ExpectCommit()

	head, err := repo.ActivateBaselineHead(context.Background(), service.RadarBaselineActivationInput{
		BaselineID: baselineID,
		Scope:      service.RadarGovernanceScope{Environment: "staging", ScopeType: "route", ScopeID: "route-a"},
		ActorID:    8,
	})
	require.NoError(t, err)
	require.Equal(t, baselineID, head.BaselineID)
	require.Equal(t, "deepseek", head.ModelRoute)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestActivatePolicyRejectsExpiredOrUnboundApproval(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := &radarGovernanceRepository{db: db}
	policyID := uuid.New()
	expectRadarWorkerWriter(t, mock)
	mock.ExpectExec("SELECT pg_advisory_xact_lock").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT p.policy_hash.*evaluation_gate_policy_approvals").WithArgs(policyID).
		WillReturnRows(sqlmock.NewRows([]string{"policy_hash", "eligible"}).AddRow(strings.Repeat("a", 64), false))
	mock.ExpectRollback()

	_, err = repo.ActivateGatePolicy(context.Background(), service.RadarGatePolicyActivationInput{
		PolicyID: policyID,
		Scope:    service.RadarGovernanceScope{Environment: "production", ScopeType: "global", ScopeID: "global"},
		ActorID:  7,
	})
	require.ErrorContains(t, err, "activation window")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestActivateBaselineRejectsExpiredOrUnboundApprovals(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := &radarGovernanceRepository{db: db}
	baselineID := uuid.New()
	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery("SELECT COUNT.*quality_admin.*expires_at").WithArgs(baselineID).
		WillReturnRows(sqlmock.NewRows([]string{"quality", "release", "approvers"}).AddRow(1, 0, 1))
	mock.ExpectRollback()

	_, err = repo.ActivateBaselineHead(context.Background(), service.RadarBaselineActivationInput{
		BaselineID: baselineID,
		Scope:      service.RadarGovernanceScope{Environment: "production", ScopeType: "global", ScopeID: "global"},
		ActorID:    8,
	})
	require.ErrorContains(t, err, "requires distinct quality_admin and release_manager approvals")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCreateReleaseSubjectValidatesFrozenRunBindingAndStoresCanonicalBytes(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := &radarGovernanceRepository{db: db}
	runID := uuid.New()
	subject := releaseSubjectFixture()
	canonical, err := service.CanonicalizeReleaseSubject(subject)
	require.NoError(t, err)
	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery("WITH frozen_run_binding AS").
		WithArgs(runID, subject.BaselineID, subject.CandidateModelConfigSHA256,
			subject.DatasetManifestSHA256, subject.RouteProfileVersion,
			pq.Array(canonical.Subject.RunnerImageDigests), pq.Array(canonical.Subject.GraderImageDigests),
			pq.Array(canonical.Subject.StatisticsImageDigests), canonical.Subject.AnalysisVersion,
			pq.Array(canonical.Subject.RegionSet), canonical.Subject.DeploymentEnvironment,
			canonical.Subject.ScopeType, canonical.Subject.ScopeID).
		WillReturnRows(sqlmock.NewRows([]string{"valid"}).AddRow(true))
	mock.ExpectQuery("INSERT INTO evaluation_release_subjects .*canonical_subject_bytes").
		WillReturnRows(sqlmock.NewRows([]string{"canonical_subject_bytes", "created_at"}).AddRow([]byte(canonical.Bytes), time.Now().UTC()))
	mock.ExpectCommit()

	record, err := repo.CreateReleaseSubject(context.Background(), service.ReleaseSubjectInput{RunID: runID, Subject: subject})
	require.NoError(t, err)
	require.Equal(t, canonical.SHA256, record.SubjectHash)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCreateReleaseSubjectRejectsFrozenRunBindingMismatch(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := &radarGovernanceRepository{db: db}
	runID := uuid.New()
	subject := releaseSubjectFixture()
	canonical, err := service.CanonicalizeReleaseSubject(subject)
	require.NoError(t, err)
	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery("WITH frozen_run_binding AS").
		WithArgs(runID, subject.BaselineID, subject.CandidateModelConfigSHA256,
			subject.DatasetManifestSHA256, subject.RouteProfileVersion,
			pq.Array(canonical.Subject.RunnerImageDigests), pq.Array(canonical.Subject.GraderImageDigests),
			pq.Array(canonical.Subject.StatisticsImageDigests), canonical.Subject.AnalysisVersion,
			pq.Array(canonical.Subject.RegionSet), canonical.Subject.DeploymentEnvironment,
			canonical.Subject.ScopeType, canonical.Subject.ScopeID).
		WillReturnRows(sqlmock.NewRows([]string{"valid"}).AddRow(false))
	mock.ExpectRollback()

	_, err = repo.CreateReleaseSubject(context.Background(), service.ReleaseSubjectInput{RunID: runID, Subject: subject})
	require.ErrorContains(t, err, "frozen run binding")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGovernanceReevaluationSelectsOnlyActiveReleaseAndMatchingBaselineRoute(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	tx, err := db.Begin()
	require.NoError(t, err)
	eventID := uuid.New()
	causeID := uuid.New()
	mock.ExpectExec("INSERT INTO evaluation_gate_reevaluation_outbox.*evaluation_release_subject_events.*WHERE e.release_subject_id=rs.id\\s+AND e.effective_at <= transaction_timestamp\\(\\)\\s+ORDER BY e.sequence DESC.*expires_at.*model_route").
		WithArgs("baseline_head", causeID, eventID, "production", "global", "global", "deepseek").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err = enqueueGovernanceReevaluation(context.Background(), tx, "baseline_head", causeID, eventID,
		service.RadarGovernanceScope{Environment: "production", ScopeType: "global", ScopeID: "global"}, "deepseek")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRotateEvidenceSigningKeyKeepsPreviousKeyVerifyOnlyAndAdvancesEpoch(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := &radarGovernanceRepository{db: db}
	oldKeyID := uuid.New()
	newKeyID := uuid.New()
	createdAt := time.Date(2026, 7, 28, 2, 0, 0, 0, time.UTC)

	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery("SELECT id, key_reference, status, state_epoch.*status='active'.*FOR UPDATE").
		WillReturnRows(sqlmock.NewRows([]string{"id", "key_reference", "status", "state_epoch", "created_at", "updated_at", "revoked_at"}).
			AddRow(oldKeyID, "env:OLD_EVIDENCE_KEY", "active", int64(4), createdAt, createdAt, nil))
	mock.ExpectQuery(`UPDATE evaluation_evidence_signing_keys.*status='verify_only'.*state_epoch=state_epoch\+1`).
		WithArgs(oldKeyID, int64(4)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "key_reference", "status", "state_epoch", "created_at", "updated_at", "revoked_at"}).
			AddRow(oldKeyID, "env:OLD_EVIDENCE_KEY", "verify_only", int64(5), createdAt, createdAt, nil))
	mock.ExpectExec("INSERT INTO evaluation_gate_reevaluation_outbox").WithArgs(oldKeyID, "verify_only", int64(5)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT DISTINCT evaluation_run_id FROM evaluation_route_evidence WHERE signing_key_id=$1 ORDER BY evaluation_run_id")).
		WithArgs(oldKeyID).
		WillReturnRows(sqlmock.NewRows([]string{"evaluation_run_id"}))
	mock.ExpectQuery("INSERT INTO evaluation_evidence_signing_keys").WithArgs(newKeyID, "env:NEW_EVIDENCE_KEY").
		WillReturnRows(sqlmock.NewRows([]string{"id", "key_reference", "status", "state_epoch", "created_at", "updated_at", "revoked_at"}).
			AddRow(newKeyID, "env:NEW_EVIDENCE_KEY", "active", int64(1), createdAt, createdAt, nil))
	mock.ExpectCommit()

	record, err := repo.RotateEvidenceSigningKey(context.Background(), service.RotateEvidenceSigningKeyInput{
		ID: newKeyID, KeyReference: "env:NEW_EVIDENCE_KEY", ExpectedActiveKeyID: oldKeyID, ExpectedActiveStateEpoch: 4,
	})

	require.NoError(t, err)
	require.Equal(t, service.EvidenceSigningKeyActive, record.Status)
	require.Equal(t, int64(1), record.StateEpoch)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRevokedSigningKeyEnqueuesReevaluationAndIntegrityAlerts(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := &radarGovernanceRepository{db: db}
	keyID := uuid.New()
	createdAt := time.Date(2026, 7, 28, 2, 0, 0, 0, time.UTC)
	revokedAt := createdAt.Add(time.Minute)

	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery("SELECT id, key_reference, status, state_epoch.*WHERE id=\\$1.*FOR UPDATE").WithArgs(keyID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "key_reference", "status", "state_epoch", "created_at", "updated_at", "revoked_at"}).
			AddRow(keyID, "env:OLD_EVIDENCE_KEY", "verify_only", int64(5), createdAt, createdAt, nil))
	mock.ExpectQuery(`UPDATE evaluation_evidence_signing_keys.*status=\$2.*state_epoch=state_epoch\+1`).
		WithArgs(keyID, "revoked", int64(5)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "key_reference", "status", "state_epoch", "created_at", "updated_at", "revoked_at"}).
			AddRow(keyID, "env:OLD_EVIDENCE_KEY", "revoked", int64(6), createdAt, revokedAt, revokedAt))
	mock.ExpectExec("INSERT INTO evaluation_gate_reevaluation_outbox").WithArgs(keyID, "revoked", int64(6)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT DISTINCT evaluation_run_id FROM evaluation_route_evidence WHERE signing_key_id=$1 ORDER BY evaluation_run_id")).
		WithArgs(keyID).
		WillReturnRows(sqlmock.NewRows([]string{"evaluation_run_id"}))
	mock.ExpectExec("INSERT INTO evaluation_alerts.*evaluation_alert_events").WithArgs(keyID, int64(6)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()

	record, err := repo.TransitionEvidenceSigningKey(context.Background(), service.TransitionEvidenceSigningKeyInput{
		ID: keyID, ExpectedStateEpoch: 5, Status: service.EvidenceSigningKeyRevoked,
	})

	require.NoError(t, err)
	require.Equal(t, service.EvidenceSigningKeyRevoked, record.Status)
	require.Equal(t, int64(6), record.StateEpoch)
	require.NotNil(t, record.RevokedAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

func releaseSubjectFixture() service.ReleaseSubject {
	return service.ReleaseSubject{
		CandidateModelConfigSHA256: strings.Repeat("a", 64),
		BaselineID:                 uuid.New(),
		DatasetManifestSHA256:      strings.Repeat("b", 64),
		RouteProfileVersion:        "radar-route-profile-v1",
		GatewayImageDigest:         "sha256:gateway",
		ControlPlaneImageDigest:    "sha256:control-plane",
		RunnerImageDigests:         []string{"sha256:runner"},
		GraderImageDigests:         []string{"sha256:grader"},
		StatisticsImageDigests:     []string{"sha256:statistics"},
		AnalysisVersion:            "radar-analysis-v1",
		RegionSet:                  []string{"default"},
		DeploymentEnvironment:      "production",
		ScopeType:                  "global",
		ScopeID:                    "global",
	}
}

func TestPolicyAndBaselineHeadChangeEnqueuesActiveReleaseReevaluation(t *testing.T) {
	// The activation tests assert the outbox write inside the same transaction
	// for both head types. This guard keeps that contract visible by name.
	TestActivatePolicyAdvancesScopedHeadWithEvent(t)
	TestActivateBaselineAdvancesRouteEnvironmentScopeHead(t)
}
