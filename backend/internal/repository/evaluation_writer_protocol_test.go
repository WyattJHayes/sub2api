package repository

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func TestEvaluationWriterAuditAllowsAndRecordsOldProtocol(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO evaluation_writer_sessions`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`set_config\('app.evaluation_writer_protocol'`).WithArgs("1").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`set_config\('app.evaluation_writer_instance_id'`).WithArgs("legacy-runner").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`SELECT write_mode, guard_mode, minimum_protocol_version FROM evaluation_schema_cutovers`).
		WillReturnRows(sqlmock.NewRows([]string{"write_mode", "guard_mode", "minimum_protocol_version"}).AddRow("open", "audit", int64(2)))
	mock.ExpectExec(`INSERT INTO evaluation_writer_audit_events`).WithArgs("legacy-runner", "runner", int64(1), "old_protocol").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`SELECT 1`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	called := false
	err = WithEvaluationWriterTx(context.Background(), db, EvaluationWriterIdentity{
		InstanceID: "legacy-runner", Kind: "runner", ProtocolVersion: 1,
	}, func(tx *sql.Tx) error {
		called = true
		_, err := tx.ExecContext(context.Background(), "SELECT 1")
		return err
	})
	if err != nil {
		t.Fatalf("WithEvaluationWriterTx() error = %v", err)
	}
	if !called {
		t.Fatal("writer callback was not called")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEvaluationWriterEnforceRejectsOldProtocol(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO evaluation_writer_sessions`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`set_config\('app.evaluation_writer_protocol'`).WithArgs("1").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`set_config\('app.evaluation_writer_instance_id'`).WithArgs("old-runner").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`SELECT write_mode, guard_mode, minimum_protocol_version FROM evaluation_schema_cutovers`).
		WillReturnRows(sqlmock.NewRows([]string{"write_mode", "guard_mode", "minimum_protocol_version"}).AddRow("open", "enforce", int64(2)))
	mock.ExpectRollback()

	called := false
	err = WithEvaluationWriterTx(context.Background(), db, EvaluationWriterIdentity{
		InstanceID: "old-runner", Kind: "runner", ProtocolVersion: 1,
	}, func(*sql.Tx) error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrRadarCutoverActive) {
		t.Fatalf("error = %v, want ErrRadarCutoverActive", err)
	}
	if called {
		t.Fatal("writer callback was called after protocol rejection")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEvaluationWriterDrainingRejectsNewWrite(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO evaluation_writer_sessions`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`set_config\('app.evaluation_writer_protocol'`).WithArgs("2").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`set_config\('app.evaluation_writer_instance_id'`).WithArgs("new-runner").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`SELECT write_mode, guard_mode, minimum_protocol_version FROM evaluation_schema_cutovers`).
		WillReturnRows(sqlmock.NewRows([]string{"write_mode", "guard_mode", "minimum_protocol_version"}).AddRow("draining", "enforce", int64(2)))
	mock.ExpectRollback()

	err = WithEvaluationWriterTx(context.Background(), db, EvaluationWriterIdentity{
		InstanceID: "new-runner", Kind: "runner", ProtocolVersion: 2,
	}, func(*sql.Tx) error { return nil })
	if !errors.Is(err, ErrRadarCutoverActive) {
		t.Fatalf("error = %v, want ErrRadarCutoverActive", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEvaluationWriterClosedAllowsMigrationOwnerOnly(t *testing.T) {
	t.Run("migration owner", func(t *testing.T) {
		db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		mock.ExpectBegin()
		mock.ExpectExec(`INSERT INTO evaluation_writer_sessions`).WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectExec(`set_config\('app.evaluation_writer_protocol'`).WithArgs("3").WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectExec(`set_config\('app.evaluation_writer_instance_id'`).WithArgs("migration-owner").WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectQuery(`SELECT write_mode, guard_mode, minimum_protocol_version FROM evaluation_schema_cutovers`).
			WillReturnRows(sqlmock.NewRows([]string{"write_mode", "guard_mode", "minimum_protocol_version"}).AddRow("closed", "enforce", int64(3)))
		mock.ExpectExec(`SELECT 1`).WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()

		err = WithEvaluationWriterTx(context.Background(), db, EvaluationWriterIdentity{
			InstanceID: "migration-owner", Kind: "migration", ProtocolVersion: 3,
		}, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(context.Background(), "SELECT 1")
			return err
		})
		if err != nil {
			t.Fatalf("migration owner error = %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("business writer", func(t *testing.T) {
		db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		mock.ExpectBegin()
		mock.ExpectExec(`INSERT INTO evaluation_writer_sessions`).WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectExec(`set_config\('app.evaluation_writer_protocol'`).WithArgs("3").WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectExec(`set_config\('app.evaluation_writer_instance_id'`).WithArgs("runner").WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectQuery(`SELECT write_mode, guard_mode, minimum_protocol_version FROM evaluation_schema_cutovers`).
			WillReturnRows(sqlmock.NewRows([]string{"write_mode", "guard_mode", "minimum_protocol_version"}).AddRow("closed", "enforce", int64(3)))
		mock.ExpectRollback()

		err = WithEvaluationWriterTx(context.Background(), db, EvaluationWriterIdentity{
			InstanceID: "runner", Kind: "runner", ProtocolVersion: 3,
		}, func(*sql.Tx) error { return nil })
		if !errors.Is(err, ErrRadarCutoverActive) {
			t.Fatalf("business writer error = %v, want ErrRadarCutoverActive", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}
