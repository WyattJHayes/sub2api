package repository

import (
	"context"
	"database/sql"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func TestValidRunReasonUsesFiniteReasonCodes(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		valid  bool
	}{
		{name: "operator request", reason: "operator_request", valid: true},
		{name: "digits", reason: "incident_2026", valid: true},
		{name: "empty", reason: "", valid: false},
		{name: "free text", reason: "operator requested a pause", valid: false},
		{name: "uppercase", reason: "Operator_Request", valid: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validRunReason(tt.reason); got != tt.valid {
				t.Fatalf("validRunReason(%q) = %v, want %v", tt.reason, got, tt.valid)
			}
		})
	}
}

func TestLoadRunControlEventRestoresIdempotentResponseMetadata(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	runID := uuid.New()
	eventID := uuid.New()
	key := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	payload := `{"request_hash":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","affected_work_count":3,"replacement_ids":["00000000-0000-0000-0000-000000000001"],"previous_epoch":4}`
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, run_id, event_type, payload, from_status, to_status, control_epoch FROM evaluation_run_events WHERE idempotency_key = $1 FOR UPDATE")).
		WithArgs(key).
		WillReturnRows(sqlmock.NewRows([]string{"id", "run_id", "event_type", "payload", "from_status", "to_status", "control_epoch"}).
			AddRow(eventID, runID, "run_fenced", payload, "running", "running", int64(5)))
	event, err := loadRunControlEvent(context.Background(), tx, key)
	if err != nil {
		t.Fatal(err)
	}
	if event == nil || event.ID != eventID || event.RunID != runID {
		t.Fatalf("unexpected event: %#v", event)
	}
	if event.RequestHash == "" || event.Affected != 3 || event.ControlEpoch != 5 || event.PreviousEpoch != 4 {
		t.Fatalf("event metadata was not restored: %#v", event)
	}
	if len(event.ReplacementIDs) != 1 || event.ReplacementIDs[0].String() != "00000000-0000-0000-0000-000000000001" {
		t.Fatalf("replacement ids were not restored: %#v", event.ReplacementIDs)
	}
	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil && err != sql.ErrTxDone {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
