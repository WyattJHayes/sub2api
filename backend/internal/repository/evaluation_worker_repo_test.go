package repository

import (
	"context"
	"database/sql"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestWorkerRegistrationValidationRejectsNonHexIdempotencyKey(t *testing.T) {
	err := validateWorkerRegistration(service.RadarWorkerRegistrationInput{
		ActorID: 1, Name: "runner", WorkerKind: "runner", Region: "us-east",
		ImageDigest: "sha256:runner", MaxConcurrency: 1, Token: "secret",
		IdempotencyKey: "short",
	})
	require.ErrorContains(t, err, "idempotency key")
}

func TestWorkerCapabilitiesNormalizeAndFingerprintRedactsToken(t *testing.T) {
	require.Equal(t, []string{"coding", "grader"}, normalizeWorkerCapabilities([]string{"grader", "coding", "coding"}))
	hash := hashToken("worker-secret")
	require.Len(t, workerTokenFingerprint(hash), workerTokenFingerprintLength)
	require.NotContains(t, workerTokenFingerprint(hash), "worker-secret")
}

func TestPauseWorkerClaimsUsesWriterTransactionAndRecordsEvent(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()

	workerID := uuid.New()
	key := serviceContractHash
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO evaluation_writer_sessions`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`set_config\('app.evaluation_writer_protocol'`).WithArgs("1").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`set_config\('app.evaluation_writer_instance_id'`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`SELECT write_mode, guard_mode, minimum_protocol_version`).
		WillReturnRows(sqlmock.NewRows([]string{"write_mode", "guard_mode", "minimum_protocol_version"}).AddRow("open", "audit", int64(0)))
	mock.ExpectQuery(`SELECT id, worker_id, event_type, payload FROM evaluation_worker_events`).
		WithArgs(key).WillReturnError(sql.ErrNoRows)
	workerRows := sqlmock.NewRows([]string{
		"id", "name", "worker_kind", "region", "image_digest", "status", "claim_mode",
		"capabilities", "max_concurrency", "last_heartbeat_at", "token_hash",
	}).AddRow(workerID, "runner-a", "runner", "us-east", "sha256:runner", "active", "open", "{coding}", 1, nil, serviceContractHash)
	mock.ExpectQuery(`SELECT id, name, worker_kind, region, image_digest, status, claim_mode`).
		WithArgs(workerID).WillReturnRows(workerRows)
	mock.ExpectExec(`UPDATE evaluation_workers`).WithArgs(workerID, "active", "paused").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO evaluation_worker_events`).WithArgs(sqlmock.AnyArg(), workerID, "claims_paused", key, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`SELECT id, name, worker_kind, region, image_digest, status, claim_mode`).
		WithArgs(workerID).WillReturnRows(sqlmock.NewRows([]string{
		"id", "name", "worker_kind", "region", "image_digest", "status", "claim_mode",
		"capabilities", "max_concurrency", "last_heartbeat_at", "token_hash",
	}).AddRow(workerID, "runner-a", "runner", "us-east", "sha256:runner", "active", "paused", "{coding}", 1, nil, serviceContractHash))
	mock.ExpectCommit()

	repo := &radarGovernanceRepository{db: db}
	result, err := repo.PauseWorkerClaims(context.Background(), service.RadarWorkerActionInput{
		WorkerID: workerID, ActorID: 7, Reason: "maintenance", IdempotencyKey: key,
	})
	require.NoError(t, err)
	require.Equal(t, "paused", result.ClaimMode)
	require.Equal(t, "open", result.PreviousClaimMode)
	require.Equal(t, "paused", result.Worker.ClaimMode)
	require.False(t, result.Idempotent)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPauseWorkerClaimsIdempotencyReplayReturnsOriginalWorker(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()

	workerID := uuid.New()
	key := serviceContractHash
	requestHash := workerRequestHash("claims_paused", map[string]any{"worker_id": workerID, "reason": "maintenance"})
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO evaluation_writer_sessions`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`set_config\('app.evaluation_writer_protocol'`).WithArgs("1").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`set_config\('app.evaluation_writer_instance_id'`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`SELECT write_mode, guard_mode, minimum_protocol_version`).
		WillReturnRows(sqlmock.NewRows([]string{"write_mode", "guard_mode", "minimum_protocol_version"}).AddRow("open", "audit", int64(0)))
	mock.ExpectQuery(`SELECT id, worker_id, event_type, payload FROM evaluation_worker_events`).
		WithArgs(key).WillReturnRows(sqlmock.NewRows([]string{"id", "worker_id", "event_type", "payload"}).
		AddRow(uuid.New(), workerID, "claims_paused", `{"request_hash":"`+requestHash+`","previous_claim_mode":"open","claim_mode":"paused"}`))
	mock.ExpectQuery(`SELECT id, name, worker_kind, region, image_digest, status, claim_mode`).
		WithArgs(workerID).WillReturnRows(sqlmock.NewRows([]string{
		"id", "name", "worker_kind", "region", "image_digest", "status", "claim_mode",
		"capabilities", "max_concurrency", "last_heartbeat_at", "token_hash",
	}).AddRow(workerID, "runner-a", "runner", "us-east", "sha256:runner", "active", "paused", "{coding}", 1, nil, serviceContractHash))
	mock.ExpectCommit()

	repo := &radarGovernanceRepository{db: db}
	result, err := repo.PauseWorkerClaims(context.Background(), service.RadarWorkerActionInput{
		WorkerID: workerID, ActorID: 7, Reason: "maintenance", IdempotencyKey: key,
	})
	require.NoError(t, err)
	require.True(t, result.Idempotent)
	require.Equal(t, "open", result.PreviousClaimMode)
	require.Equal(t, "paused", result.ClaimMode)
	require.NoError(t, mock.ExpectationsWereMet())
}
