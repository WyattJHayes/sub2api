package repository

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestEvaluationOutboxRepositoryRejectsNilParameters(t *testing.T) {
	repo := &evaluationOutboxRepository{}
	ctx := context.Background()

	_, err := repo.Enqueue(ctx, service.EnqueueEvaluationOutboxInput{})
	require.ErrorIs(t, err, service.ErrEvaluationOutboxInvalid)

	_, err = repo.Claim(ctx, uuid.Nil, nil, 1, time.Minute)
	require.ErrorIs(t, err, service.ErrEvaluationOutboxInvalid)

	err = repo.Heartbeat(ctx, uuid.Nil, "", 0, time.Minute)
	require.ErrorIs(t, err, service.ErrEvaluationOutboxInvalid)

	err = repo.Complete(ctx, uuid.Nil, "", 0)
	require.ErrorIs(t, err, service.ErrEvaluationOutboxInvalid)

	err = repo.DeadLetter(ctx, uuid.Nil, "", 0, "handler_failed")
	require.ErrorIs(t, err, service.ErrEvaluationOutboxInvalid)

	_, err = repo.ReplayDeadLetter(ctx, uuid.Nil)
	require.ErrorIs(t, err, service.ErrEvaluationOutboxInvalid)
}

func TestEvaluationOutboxConsumerHeartbeatUsesWriterProtocol(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	identity := defaultEvaluationWriterIdentity("worker")
	expectWriterSetup(mock, identity)
	workerID := uuid.New()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE evaluation_workers")).
		WithArgs(workerID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err = (&evaluationOutboxRepository{db: db}).TouchConsumerWorkerHeartbeat(context.Background(), workerID)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestEvaluationOutboxEnsureConsumerWorkerUsesTenantScopedConflictKey(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	identity := defaultEvaluationWriterIdentity("api")
	expectWriterSetup(mock, identity)
	workerName := "radar-control-plane-outbox"
	tokenHash := hashString("evaluation-outbox-consumer-token\x00" + workerName)
	workerID := uuid.New()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).
		WithArgs("evaluation-outbox-consumer:" + workerName).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("ON CONFLICT (tenant_id, name) DO UPDATE SET")).
		WithArgs(sqlmock.AnyArg(), workerName, tokenHash, tokenHash[:12]).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(workerID))
	mock.ExpectCommit()

	got, err := (&evaluationOutboxRepository{db: db}).EnsureConsumerWorker(context.Background(), workerName)
	require.NoError(t, err)
	require.Equal(t, workerID, got)
	require.NoError(t, mock.ExpectationsWereMet())
}
