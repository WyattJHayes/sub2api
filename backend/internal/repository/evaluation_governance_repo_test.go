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

func TestValidateGateReliabilityWatermarkUsesTransactionTimestampForFreshness(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mock.ExpectBegin()
	tx, err := db.Begin()
	require.NoError(t, err)

	runID := uuid.New()
	policyID := uuid.New()
	snapshotID := uuid.New()
	headEventID := uuid.New()
	policyHash := strings.Repeat("a", 64)
	policy := json.RawMessage(`{"observation_days":14,"critical_domain_delta_pp":3,"aggregate_delta_pp":2,"confidence_level":0.95,"require_ci_exclude_zero":true,"reliability":{"required_slices":[{"profile_id":"profile-v1","slice_key":"region:global"}],"allowed_query_versions":["reliability-query-v1"],"max_p99_latency_ms":1000,"max_error_rate":"0.01","max_cost_per_success":"1"}}`)
	snapshotHash := strings.Repeat("b", 64)
	sourceHash := strings.Repeat("c", 64)
	createdAt := time.Now().UTC().Add(-time.Hour)
	freshUntil := time.Now().UTC().Add(time.Hour)
	databaseNow := freshUntil.Add(time.Hour)
	watermark, err := json.Marshal(radarGateReliabilityWatermark{
		Version: "radar-gate-reliability-watermark-v1", RunID: runID, PolicyID: policyID,
		PolicyHash: policyHash, ObservedAt: time.Now().UTC(),
		SnapshotRefs: []service.RadarGateReliabilitySnapshotRef{{
			SnapshotID: snapshotID, HeadEventID: headEventID, ProfileID: "profile-v1", SliceKey: "region:global",
			SnapshotHash: snapshotHash, SourceHash: sourceHash, CreatedAt: createdAt,
		}},
	})
	require.NoError(t, err)
	mock.ExpectQuery(`(?s)SELECT policy_hash.*FROM evaluation_gate_policies`).
		WithArgs(policyID).
		WillReturnRows(sqlmock.NewRows([]string{"policy_hash", "policy"}).AddRow(policyHash, policy))
	mock.ExpectQuery(`SELECT transaction_timestamp\(\)`).
		WillReturnRows(sqlmock.NewRows([]string{"transaction_timestamp"}).AddRow(databaseNow))
	mock.ExpectQuery(`(?s)SELECT h.snapshot_id, h.head_event_id, h.snapshot_hash.*FROM evaluation_reliability_heads`).
		WithArgs(runID, "profile-v1", "region:global").
		WillReturnRows(sqlmock.NewRows([]string{
			"snapshot_id", "head_event_id", "snapshot_hash", "source_hash", "created_at", "fresh_until",
		}).AddRow(snapshotID, headEventID, snapshotHash, sourceHash, createdAt, freshUntil))
	mock.ExpectRollback()

	err = validateGateReliabilityWatermark(context.Background(), tx, runID, policyID, watermark)
	require.ErrorIs(t, err, service.ErrGovernanceHeadConflict)
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRecordGateDecisionRejectsWhenPolicyHeadChangedAfterEvidenceLoad(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	runID, policyID := uuid.New(), uuid.New()
	policyHash := strings.Repeat("a", 64)
	policy := json.RawMessage(`{"observation_days":14,"critical_domain_delta_pp":3,"aggregate_delta_pp":2,"confidence_level":0.95,"require_ci_exclude_zero":true,"reliability":{"required_slices":[{"profile_id":"profile-v1","slice_key":"region:global"}],"allowed_query_versions":["reliability-query-v1"],"max_p99_latency_ms":1000,"max_error_rate":"0.01","max_cost_per_success":"1"}}`)
	snapshotID, headEventID := uuid.New(), uuid.New()
	createdAt := time.Now().UTC().Add(-time.Minute)
	freshUntil := time.Now().UTC().Add(time.Hour)
	sourceHash, snapshotHash := strings.Repeat("b", 64), strings.Repeat("c", 64)
	watermark, err := json.Marshal(radarGateReliabilityWatermark{
		Version: "radar-gate-reliability-watermark-v1", RunID: runID, PolicyID: policyID,
		PolicyHash: policyHash, ObservedAt: time.Now().UTC(), SnapshotRefs: []service.RadarGateReliabilitySnapshotRef{{
			SnapshotID: snapshotID, HeadEventID: headEventID, ProfileID: "profile-v1", SliceKey: "region:global",
			SnapshotHash: snapshotHash, SourceHash: sourceHash, CreatedAt: createdAt,
		}},
	})
	require.NoError(t, err)

	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery(`(?s)SELECT policy_hash.*FROM evaluation_gate_policies`).
		WithArgs(policyID).WillReturnRows(sqlmock.NewRows([]string{"policy_hash", "policy"}).AddRow(policyHash, policy))
	mock.ExpectQuery(`SELECT transaction_timestamp\(\)`).
		WillReturnRows(sqlmock.NewRows([]string{"transaction_timestamp"}).AddRow(time.Now().UTC()))
	mock.ExpectQuery(`(?s)SELECT h\.snapshot_id, h\.head_event_id, h\.snapshot_hash.*FROM evaluation_reliability_heads`).
		WithArgs(runID, "profile-v1", "region:global").WillReturnRows(sqlmock.NewRows([]string{
		"snapshot_id", "head_event_id", "snapshot_hash", "source_hash", "created_at", "fresh_until",
	}).AddRow(snapshotID, headEventID, snapshotHash, sourceHash, createdAt, freshUntil))

	subjectID := uuid.New()
	releaseSubjectHash := strings.Repeat("d", 64)
	mock.ExpectQuery(`(?s)SELECT rs\.id,rs\.subject_hash,rs\.canonical_subject.*FROM evaluation_release_subjects`).
		WithArgs(runID).WillReturnRows(sqlmock.NewRows([]string{"id", "subject_hash", "canonical_subject", "tenant_id"}).AddRow(
		subjectID, releaseSubjectHash, []byte(`{"deployment_environment":"production","scope_type":"global","scope_id":"global"}`), int64(41),
	))
	mock.ExpectExec("SELECT pg_advisory_xact_lock").WithArgs("release-subject:" + subjectID.String()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`(?s)SELECT rs\.subject_hash,rs\.canonical_subject.*WHERE rs\.id=\$1`).
		WithArgs(subjectID).WillReturnRows(sqlmock.NewRows([]string{"subject_hash", "canonical_subject"}).AddRow(
		releaseSubjectHash, []byte(`{"deployment_environment":"production","scope_type":"global","scope_id":"global"}`),
	))
	mock.ExpectExec("SELECT pg_advisory_xact_lock").WithArgs("policy:41:production:global:global").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT policy_id FROM evaluation_gate_policy_heads`).
		WithArgs(int64(41), "production", "global", "global").WillReturnRows(sqlmock.NewRows([]string{"policy_id"}).AddRow(uuid.New()))
	mock.ExpectRollback()

	_, err = (&radarGovernanceRepository{db: db}).RecordGateDecision(context.Background(), service.RadarGateDecisionInput{
		RunID: runID, PolicyID: policyID, Status: service.RadarGatePassed, Evidence: json.RawMessage(`{"version":"radar-gate-evidence-v1"}`),
		EvidenceHash: strings.Repeat("e", 64), ReleaseSubjectHash: releaseSubjectHash, SourceWatermark: watermark,
	})
	require.ErrorIs(t, err, service.ErrGovernanceHeadConflict)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRegisterRadarWorkerIsIdentityIdempotent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
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
	defer func() { _ = db.Close() }()
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
	defer func() { _ = db.Close() }()
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
	defer func() { _ = db.Close() }()
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
	defer func() { _ = db.Close() }()
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
	defer func() { _ = db.Close() }()
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
	defer func() { _ = db.Close() }()
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
	defer func() { _ = db.Close() }()
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
	defer func() { _ = db.Close() }()
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
	defer func() { _ = db.Close() }()
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
	defer func() { _ = db.Close() }()
	mock.ExpectQuery(`SELECT role FROM evaluation_role_bindings.*scope = '\{\}'::jsonb`).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("test_operator"))

	repo := &radarGovernanceRepository{db: db}
	require.NoError(t, repo.Require(context.Background(), 7, service.PermissionRunControl))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRadarPermissionsAreTenantScoped(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mock.ExpectQuery(`SELECT role FROM evaluation_role_bindings.*tenant_id = \$2`).
		WithArgs(int64(7), int64(41)).
		WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("quality_admin"))

	permissions, err := (&radarGovernanceRepository{db: db}).ListPermissions(service.WithRadarTenant(context.Background(), 41), 7)
	require.NoError(t, err)
	require.Contains(t, permissions, service.PermissionPolicyApprove)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRadarRoleBindingsListUsesTenantScope(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	bindingID := uuid.New()
	createdAt := time.Date(2026, 7, 31, 2, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT id, actor_id, role, scope, enabled, created_by, created_at, disabled_at FROM evaluation_role_bindings WHERE tenant_id = \$1`).
		WithArgs(int64(41)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "actor_id", "role", "scope", "enabled", "created_by", "created_at", "disabled_at"}).
			AddRow(bindingID, int64(77), "quality_admin", []byte(`{}`), true, int64(41), createdAt, nil))

	bindings, err := (&radarGovernanceRepository{db: db}).ListRoleBindings(service.WithRadarTenant(context.Background(), 41), nil)
	require.NoError(t, err)
	require.Len(t, bindings, 1)
	require.Equal(t, int64(77), bindings[0].ActorID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestProposeBaselinePersistsTenantScope(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	runID := uuid.New()
	baselineID := uuid.New()
	now := time.Now().UTC()
	manifestHash := strings.Repeat("a", 64)
	evidenceHash := strings.Repeat("b", 64)
	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery(`INSERT INTO evaluation_baselines.*VALUES \(\$1,\$2,\$3,\$4,\$5,\$6,\$7,\$8,\$9\)`).
		WithArgs(sqlmock.AnyArg(), "deepseek", runID, manifestHash, evidenceHash, "route-v1", 3, int64(41), int64(41)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "model_route", "run_id", "dataset_manifest_sha256", "evidence_hash", "route_profile_version",
			"policy_version", "status", "proposed_by", "proposed_at", "activated_at", "retired_at",
		}).AddRow(baselineID, "deepseek", runID, manifestHash, evidenceHash, "route-v1", 3, "proposed", int64(41), now, nil, nil))
	mock.ExpectExec("INSERT INTO evaluation_baseline_events").
		WithArgs(sqlmock.AnyArg(), baselineID, evidenceHash, int64(41)).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	baseline, err := (&radarGovernanceRepository{db: db}).ProposeBaseline(
		service.WithRadarTenant(context.Background(), 41),
		service.RadarBaselineInput{
			ModelRoute: "deepseek", RunID: runID, DatasetManifestSHA256: manifestHash,
			EvidenceHash: evidenceHash, RouteProfileVersion: "route-v1", PolicyVersion: 3, ProposedBy: 41,
		},
	)
	require.NoError(t, err)
	require.Equal(t, baselineID, baseline.ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetBaselineUsesResourceTenantScope(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	baselineID := uuid.New()
	createdAt := time.Date(2026, 7, 31, 3, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`(?s)FROM evaluation_baselines b WHERE b\.id=\$1 AND b\.tenant_id=\$2`).
		WithArgs(baselineID, int64(41)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "model_route", "run_id", "dataset_manifest_sha256", "evidence_hash", "route_profile_version",
			"policy_version", "status", "proposed_by", "proposed_at", "activated_at", "retired_at",
		}).AddRow(
			baselineID, "deepseek", uuid.New(), strings.Repeat("a", 64), strings.Repeat("b", 64), "route-v1",
			1, "proposed", int64(7), createdAt, nil, nil,
		))

	baseline, err := (&radarGovernanceRepository{db: db}).GetBaseline(
		service.WithRadarTenant(context.Background(), 41), baselineID,
	)
	require.NoError(t, err)
	require.Equal(t, baselineID, baseline.ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestApproveBaselineRejectsResourceFromAnotherTenant(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	baselineID := uuid.New()
	now := time.Now().UTC()
	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery(`SELECT tenant_id FROM evaluation_baselines WHERE id=\$1`).
		WithArgs(baselineID).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id"}).AddRow(int64(42)))
	mock.ExpectRollback()

	_, err = (&radarGovernanceRepository{db: db}).ApproveBaseline(
		service.WithRadarTenant(context.Background(), 41),
		service.RadarBaselineApprovalInput{
			BaselineID: baselineID, ApproverID: 8, Role: service.RoleQualityAdmin,
			EvidenceHash: strings.Repeat("a", 64), EffectiveAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour),
		},
	)
	require.ErrorIs(t, err, service.ErrRadarForbidden)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestActivateBaselineRejectsResourceFromAnotherTenant(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	baselineID := uuid.New()
	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery(`SELECT tenant_id FROM evaluation_baselines WHERE id=\$1`).
		WithArgs(baselineID).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id"}).AddRow(int64(42)))
	mock.ExpectRollback()

	_, err = (&radarGovernanceRepository{db: db}).ActivateBaselineHead(
		service.WithRadarTenant(context.Background(), 41),
		service.RadarBaselineActivationInput{
			BaselineID: baselineID,
			Scope:      service.RadarGovernanceScope{Environment: "production", ScopeType: "global", ScopeID: service.GlobalReleaseScopeID},
			ActorID:    8,
		},
	)
	require.ErrorIs(t, err, service.ErrRadarForbidden)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestActivateGatePolicyRejectsResourceFromAnotherTenant(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	policyID := uuid.New()
	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery(`SELECT tenant_id FROM evaluation_gate_policies WHERE id=\$1`).
		WithArgs(policyID).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id"}).AddRow(int64(42)))
	mock.ExpectRollback()

	_, err = (&radarGovernanceRepository{db: db}).ActivateGatePolicy(
		service.WithRadarTenant(context.Background(), 41),
		service.RadarGatePolicyActivationInput{
			PolicyID: policyID,
			Scope:    service.RadarGovernanceScope{Environment: "production", ScopeType: "global", ScopeID: service.GlobalReleaseScopeID},
			ActorID:  8,
		},
	)
	require.ErrorIs(t, err, service.ErrRadarForbidden)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestActivateReleaseSubjectRejectsResourceFromAnotherTenant(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	subjectID := uuid.New()
	now := time.Now().UTC()
	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery(`SELECT tenant_id FROM evaluation_release_subjects WHERE id=\$1`).
		WithArgs(subjectID).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id"}).AddRow(int64(42)))
	mock.ExpectRollback()

	_, err = (&radarGovernanceRepository{db: db}).ActivateReleaseSubject(
		service.WithRadarTenant(context.Background(), 41),
		service.ReleaseSubjectActivationInput{
			ReleaseSubjectID: subjectID, ActorID: 8, EffectiveAt: now, ExpiresAt: now.Add(time.Hour),
		},
	)
	require.ErrorIs(t, err, service.ErrRadarForbidden)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRecordGateDecisionRejectsPolicyFromAnotherTenant(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	runID, policyID := uuid.New(), uuid.New()
	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery(`SELECT tenant_id FROM evaluation_runs WHERE id=\$1`).
		WithArgs(runID).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id"}).AddRow(int64(41)))
	mock.ExpectQuery(`SELECT tenant_id FROM evaluation_gate_policies WHERE id=\$1`).
		WithArgs(policyID).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id"}).AddRow(int64(42)))
	mock.ExpectRollback()

	_, err = (&radarGovernanceRepository{db: db}).RecordGateDecision(
		service.WithRadarTenant(context.Background(), 41),
		service.RadarGateDecisionInput{
			RunID: runID, PolicyID: policyID, Status: service.RadarGatePassed,
			Evidence: json.RawMessage(`{}`), EvidenceHash: strings.Repeat("a", 64),
		},
	)
	require.ErrorIs(t, err, service.ErrRadarForbidden)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRadarPermissionsAllowAdminBootstrapOnlyWhenNoBindingsExist(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
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

func TestRadarPermissionsAllowTenantScopedAdminBootstrap(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT role FROM evaluation_role_bindings.*tenant_id = \$2`).
		WithArgs(int64(1), int64(41)).
		WillReturnRows(sqlmock.NewRows([]string{"role"}))
	mock.ExpectQuery(`SELECT EXISTS.*tenant_id = \$2`).
		WithArgs(int64(1), int64(41)).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	permissions, err := (&radarGovernanceRepository{db: db}).ListPermissions(
		service.WithRadarTenant(context.Background(), 41), 1,
	)
	require.NoError(t, err)
	require.Contains(t, permissions, service.PermissionRoleManage)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCreateRadarRoleBindingRejectsUnsupportedScope(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
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

func TestCreateRadarRoleBindingRejectsTargetOutsideTenant(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = (&radarGovernanceRepository{db: db}).CreateRoleBinding(
		service.WithRadarTenant(context.Background(), 41),
		service.RadarRoleBindingInput{ActorID: 99, Role: service.RoleViewer, CreatedBy: 41},
	)
	require.ErrorIs(t, err, service.ErrRadarForbidden)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCreateRadarRoleBindingAllowsTargetWithAuthenticatedActor(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	createdAt := time.Date(2026, 7, 31, 3, 0, 0, 0, time.UTC)
	bindingID := uuid.New()
	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery(`INSERT INTO evaluation_role_bindings`).
		WithArgs(sqlmock.AnyArg(), int64(99), service.RoleViewer, "{}", int64(77), int64(41)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "actor_id", "role", "scope", "enabled", "created_by", "created_at", "disabled_at",
		}).AddRow(bindingID, int64(99), service.RoleViewer, []byte(`{}`), true, int64(77), createdAt, nil))
	mock.ExpectCommit()

	ctx := service.WithRadarActor(service.WithRadarTenant(context.Background(), 41), 77)
	binding, err := (&radarGovernanceRepository{db: db}).CreateRoleBinding(ctx, service.RadarRoleBindingInput{
		ActorID: 99, Role: service.RoleViewer, CreatedBy: 77,
	})
	require.NoError(t, err)
	require.Equal(t, bindingID, binding.ID)
	require.Equal(t, int64(99), binding.ActorID)
	require.Equal(t, service.RoleViewer, binding.Role)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDisableRadarRoleBindingUsesTenantScopeForTargetActor(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	bindingID := uuid.New()
	expectRadarWorkerWriter(t, mock)
	mock.ExpectExec(`UPDATE evaluation_role_bindings.*tenant_id = \$2`).
		WithArgs(bindingID, int64(41)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	ctx := service.WithRadarActor(service.WithRadarTenant(context.Background(), 41), 77)
	err = (&radarGovernanceRepository{db: db}).DisableRoleBinding(ctx, bindingID, 77)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetReleaseSubjectUsesCurrentEffectiveEventAndTenantScope(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	subjectID := uuid.New()
	runID := uuid.New()
	createdAt := time.Date(2026, 7, 31, 1, 0, 0, 0, time.UTC)
	effectiveAt := createdAt.Add(-time.Hour)
	expiresAt := createdAt.Add(time.Hour)
	mock.ExpectQuery(`(?s)SELECT rs\.id,rs\.run_id,rs\.subject_hash,rs\.canonical_subject,rs\.created_at.*effective_at <= transaction_timestamp\(\).*WHERE rs\.id=\$1.*AND rs\.tenant_id=\$2`).
		WithArgs(subjectID, int64(41)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "run_id", "subject_hash", "canonical_subject", "created_at", "active", "effective_at", "expires_at",
		}).AddRow(subjectID, runID, strings.Repeat("a", 64), []byte(`{}`), createdAt, true, effectiveAt, expiresAt))

	record, err := (&radarGovernanceRepository{db: db}).GetReleaseSubject(
		service.WithRadarTenant(context.Background(), 41), subjectID,
	)
	require.NoError(t, err)
	require.Equal(t, subjectID, record.ID)
	require.Equal(t, runID, record.RunID)
	require.True(t, record.Active)
	require.NotNil(t, record.EffectiveAt)
	require.Equal(t, effectiveAt, *record.EffectiveAt)
	require.NotNil(t, record.ExpiresAt)
	require.Equal(t, expiresAt, *record.ExpiresAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestActivatePolicyAdvancesScopedHeadWithEvent(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	repo := &radarGovernanceRepository{db: db}
	policyID := uuid.New()
	eventTime := time.Now().UTC()
	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery("SELECT tenant_id FROM evaluation_gate_policies WHERE id=\\$1").WithArgs(policyID).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id"}).AddRow(int64(7)))
	mock.ExpectExec("SELECT pg_advisory_xact_lock").WithArgs("policy:7:production:global:global").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT p.policy_hash.*evaluation_gate_policy_approvals").WithArgs(policyID).
		WillReturnRows(sqlmock.NewRows([]string{"policy_hash", "eligible"}).AddRow(strings.Repeat("a", 64), true))
	mock.ExpectQuery("SELECT policy_id,event_id,updated_at FROM evaluation_gate_policy_heads").
		WithArgs(int64(7), "production", "global", "global").WillReturnError(sql.ErrNoRows)
	mock.ExpectExec("INSERT INTO evaluation_gate_policy_events").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT advance_evaluation_gate_policy_head").WillReturnRows(sqlmock.NewRows([]string{"advanced"}).AddRow(true))
	mock.ExpectExec("INSERT INTO evaluation_gate_reevaluation_outbox").WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectQuery("SELECT updated_at FROM evaluation_gate_policy_heads").
		WithArgs(int64(7), "production", "global", "global").WillReturnRows(sqlmock.NewRows([]string{"updated_at"}).AddRow(eventTime))
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

func TestActivatePolicyRejectsWrongExpectedIDWhenRequestedPolicyIsAlreadyHead(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	repo := &radarGovernanceRepository{db: db}
	policyID := uuid.New()
	wrongExpectedID := uuid.New()
	eventID := uuid.New()
	updatedAt := time.Now().UTC()
	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery("SELECT tenant_id FROM evaluation_gate_policies WHERE id=\\$1").WithArgs(policyID).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id"}).AddRow(int64(7)))
	mock.ExpectExec("SELECT pg_advisory_xact_lock").WithArgs("policy:7:production:global:global").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT p.policy_hash.*evaluation_gate_policy_approvals").WithArgs(policyID).
		WillReturnRows(sqlmock.NewRows([]string{"policy_hash", "eligible"}).AddRow(strings.Repeat("a", 64), true))
	mock.ExpectQuery("SELECT policy_id,event_id,updated_at FROM evaluation_gate_policy_heads").
		WithArgs(int64(7), "production", "global", "global").WillReturnRows(sqlmock.NewRows([]string{"policy_id", "event_id", "updated_at"}).AddRow(policyID, eventID, updatedAt))
	mock.ExpectRollback()

	_, err = repo.ActivateGatePolicy(context.Background(), service.RadarGatePolicyActivationInput{
		PolicyID: policyID,
		Scope:    service.RadarGovernanceScope{Environment: "production", ScopeType: "global", ScopeID: "global"},
		ActorID:  7, ExpectedPolicyID: &wrongExpectedID,
	})
	require.ErrorIs(t, err, service.ErrGovernanceHeadConflict)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestActivateBaselineAdvancesRouteEnvironmentScopeHead(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
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
	defer func() { _ = db.Close() }()
	repo := &radarGovernanceRepository{db: db}
	policyID := uuid.New()
	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery("SELECT tenant_id FROM evaluation_gate_policies WHERE id=\\$1").WithArgs(policyID).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id"}).AddRow(int64(7)))
	mock.ExpectExec("SELECT pg_advisory_xact_lock").WithArgs("policy:7:production:global:global").WillReturnResult(sqlmock.NewResult(0, 1))
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
	defer func() { _ = db.Close() }()
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
	defer func() { _ = db.Close() }()
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
	defer func() { _ = db.Close() }()
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
	defer func() { _ = db.Close() }()
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
	defer func() { _ = db.Close() }()
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
	defer func() { _ = db.Close() }()
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
