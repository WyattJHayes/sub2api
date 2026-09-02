package repository

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func writerTestIdentity(kind string, protocol int64) EvaluationWriterIdentity {
	return EvaluationWriterIdentity{
		InstanceID:      uuid.NewString(),
		Kind:            kind,
		ProtocolVersion: protocol,
	}
}

func expectWriterSetup(mock sqlmock.Sqlmock, identity EvaluationWriterIdentity) {
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO evaluation_writer_sessions").WithArgs(
		identity.InstanceID, identity.Kind, identity.ProtocolVersion,
	).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("SELECT set_config\\('app.evaluation_writer_instance_id'").WithArgs(identity.InstanceID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("SELECT set_config\\('app.evaluation_writer_protocol'").WithArgs(strconv.FormatInt(identity.ProtocolVersion, 10)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("SELECT set_config\\('app.evaluation_writer_kind'").WithArgs(identity.Kind).WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestEvaluationWriterAuditAllowsAndRecordsOldProtocol(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	identity := writerTestIdentity("worker", 1)
	expectWriterSetup(mock, identity)
	mock.ExpectExec("INSERT INTO evaluation_scores").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	repo := &evaluationRepository{db: db}
	err = repo.WithEvaluationWriterTx(context.Background(), identity, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(), "INSERT INTO evaluation_scores (id) VALUES ($1)", uuid.New())
		return err
	})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestEvaluationWriterEnforceRejectsOldProtocol(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	identity := writerTestIdentity("worker", 1)
	expectWriterSetup(mock, identity)
	mock.ExpectExec("INSERT INTO evaluation_scores").WillReturnError(&pq.Error{Message: "unknown_writer_session"})
	mock.ExpectRollback()

	err = (&evaluationRepository{db: db}).WithEvaluationWriterTx(context.Background(), identity, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(), "INSERT INTO evaluation_scores (id) VALUES ($1)", uuid.New())
		return err
	})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrRadarWriterProtocol)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestEvaluationWriterDrainingRejectsNewWrite(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	identity := writerTestIdentity("worker", 2)
	expectWriterSetup(mock, identity)
	mock.ExpectExec("INSERT INTO evaluation_scores").WillReturnError(&pq.Error{Message: "radar_cutover_active"})
	mock.ExpectRollback()

	err = (&evaluationRepository{db: db}).WithEvaluationWriterTx(context.Background(), identity, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(), "INSERT INTO evaluation_scores (id) VALUES ($1)", uuid.New())
		return err
	})
	require.ErrorIs(t, err, ErrRadarCutoverActive)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestEvaluationWriterClosedAllowsMigrationOwnerOnly(t *testing.T) {
	tests := []struct {
		name       string
		kind       string
		wantErr    error
		callbackOK bool
	}{
		{name: "business writer", kind: "worker", wantErr: ErrRadarCutoverActive},
		{name: "migration owner", kind: "migration", callbackOK: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer func() { _ = db.Close() }()

			identity := writerTestIdentity(test.kind, 2)
			expectWriterSetup(mock, identity)
			if test.callbackOK {
				mock.ExpectExec("INSERT INTO evaluation_schema_cutovers").WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectCommit()
			} else {
				mock.ExpectExec("INSERT INTO evaluation_schema_cutovers").WillReturnError(&pq.Error{Message: "radar_cutover_active"})
				mock.ExpectRollback()
			}

			err = (&evaluationRepository{db: db}).WithEvaluationWriterTx(context.Background(), identity, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(context.Background(), "INSERT INTO evaluation_schema_cutovers (id) VALUES ($1)", 1)
				return err
			})
			if test.wantErr != nil {
				require.ErrorIs(t, err, test.wantErr)
			} else {
				require.NoError(t, err)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestEvaluationWriterProtocolMapsWrappedGuardErrors(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	identity := writerTestIdentity("worker", 2)
	expectWriterSetup(mock, identity)
	mock.ExpectExec("UPDATE evaluation_scores").WillReturnError(errors.New("pq: radar_cutover_active"))
	mock.ExpectRollback()

	err = (&evaluationRepository{db: db}).WithEvaluationWriterTx(context.Background(), identity, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(), "UPDATE evaluation_scores SET score = $1", 1)
		return err
	})
	require.ErrorIs(t, err, ErrRadarCutoverActive)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestEvaluationAssignmentRenewalUsesWriterProtocol(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	identity := defaultEvaluationWriterIdentity("worker")
	expectWriterSetup(mock, identity)
	assignmentID := uuid.New()
	expiresAt := time.Now().UTC().Add(time.Minute)
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE evaluation_assignments")).
		WithArgs(assignmentID, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"lease_expires_at"}).AddRow(expiresAt))
	mock.ExpectCommit()

	got, err := (&evaluationRepository{db: db}).RenewLease(context.Background(), assignmentID, "lease-token", time.Minute)
	require.NoError(t, err)
	require.Equal(t, expiresAt, got)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDefaultEvaluationWriterIdentityUsesConfiguredInstanceID(t *testing.T) {
	configured := uuid.NewString()
	t.Setenv("RADAR_WRITER_INSTANCE_ID", configured)

	identity := defaultEvaluationWriterIdentity("api")

	require.Equal(t, configured, identity.InstanceID)
	require.Equal(t, currentEvaluationWriterProtocolVersion, identity.ProtocolVersion)
}

func TestEvaluationGradingHeartbeatUsesWriterProtocol(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	identity := defaultEvaluationWriterIdentity("worker")
	expectWriterSetup(mock, identity)
	leaseID := uuid.New()
	expiresAt := time.Now().UTC().Add(time.Minute)
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE evaluation_grading_jobs")).
		WithArgs(leaseID, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"lease_expires_at"}).AddRow(expiresAt))
	mock.ExpectCommit()

	got, err := (&evaluationGradingRepository{db: db}).HeartbeatGradingLease(context.Background(), leaseID, "lease-token", time.Minute)
	require.NoError(t, err)
	require.Equal(t, expiresAt, got)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestEvaluationWorkerHeartbeatUsesWriterProtocol(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	identity := defaultEvaluationWriterIdentity("worker")
	expectWriterSetup(mock, identity)
	workerID := uuid.New()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE evaluation_workers SET last_heartbeat_at = NOW(), updated_at = NOW()")).
		WithArgs(workerID, "runner").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err = (&evaluationGradingRepository{db: db}).TouchWorkerHeartbeat(context.Background(), workerID, "runner")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
