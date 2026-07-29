package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// EvaluationWriterIdentity identifies the process that owns a Radar write
// transaction. The value is registered in the database for cutover auditing.
type EvaluationWriterIdentity struct {
	InstanceID      string
	Kind            string
	ProtocolVersion int64
}

var ErrRadarCutoverActive = service.ErrRadarCutoverActive

func defaultEvaluationWriterIdentity(kind string) EvaluationWriterIdentity {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "unknown"
	}
	return EvaluationWriterIdentity{
		InstanceID:      fmt.Sprintf("sub2api:%s:%d:%s", host, os.Getpid(), kind),
		Kind:            kind,
		ProtocolVersion: 1,
	}
}

type evaluationCutoverState struct {
	writeMode       string
	guardMode       string
	minimumProtocol int64
}

// WithEvaluationWriterTx is the single transaction entry point for Radar
// writers. It registers the writer session, sets transaction-local identity,
// evaluates the current cutover state, and commits the callback atomically.
func WithEvaluationWriterTx(
	ctx context.Context,
	db *sql.DB,
	identity EvaluationWriterIdentity,
	fn func(*sql.Tx) error,
) error {
	if db == nil {
		return errors.New("nil evaluation writer database")
	}
	if fn == nil {
		return errors.New("nil evaluation writer callback")
	}
	identity.InstanceID = strings.TrimSpace(identity.InstanceID)
	identity.Kind = strings.TrimSpace(identity.Kind)
	if identity.InstanceID == "" || identity.Kind == "" || len(identity.InstanceID) > 200 || len(identity.Kind) > 32 || identity.ProtocolVersion < 0 {
		return errors.New("invalid evaluation writer identity")
	}

	tx, err := beginEvaluationWriterTx(ctx, db, identity)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return mapRadarCutoverError(err)
	}
	if err := tx.Commit(); err != nil {
		return mapRadarCutoverError(fmt.Errorf("commit evaluation writer transaction: %w", err))
	}
	return nil
}

func beginEvaluationWriterTx(ctx context.Context, db *sql.DB, identity EvaluationWriterIdentity) (*sql.Tx, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin evaluation writer transaction: %w", err)
	}
	rollback := func(err error) (*sql.Tx, error) {
		_ = tx.Rollback()
		return nil, mapRadarCutoverError(err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_writer_sessions (
			id, instance_id, writer_kind, protocol_version,
			last_transaction_at, heartbeat_expires_at, updated_at
		) VALUES ($1, $2, $3, $4, NOW(), NOW() + INTERVAL '60 seconds', NOW())
		ON CONFLICT (instance_id) DO UPDATE SET
			writer_kind = EXCLUDED.writer_kind,
			protocol_version = EXCLUDED.protocol_version,
			last_transaction_at = EXCLUDED.last_transaction_at,
			heartbeat_expires_at = EXCLUDED.heartbeat_expires_at,
			updated_at = EXCLUDED.updated_at`,
		uuid.New(), identity.InstanceID, identity.Kind, identity.ProtocolVersion); err != nil {
		return rollback(fmt.Errorf("register evaluation writer session: %w", err))
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.evaluation_writer_protocol', $1, true)`, fmt.Sprintf("%d", identity.ProtocolVersion)); err != nil {
		return rollback(fmt.Errorf("set evaluation writer protocol: %w", err))
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.evaluation_writer_instance_id', $1, true)`, identity.InstanceID); err != nil {
		return rollback(fmt.Errorf("set evaluation writer instance: %w", err))
	}

	var state evaluationCutoverState
	if err := tx.QueryRowContext(ctx, `
		SELECT write_mode, guard_mode, minimum_protocol_version
		FROM evaluation_schema_cutovers
		WHERE id = 1
		FOR UPDATE`).Scan(&state.writeMode, &state.guardMode, &state.minimumProtocol); err != nil {
		return rollback(fmt.Errorf("load evaluation writer cutover: %w", err))
	}

	if err := validateEvaluationWriterCutover(identity, state); err != nil {
		return rollback(err)
	}
	if state.guardMode == "audit" && identity.ProtocolVersion < state.minimumProtocol {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO evaluation_writer_audit_events (
				instance_id, writer_kind, protocol_version, event_type
			) VALUES ($1, $2, $3, $4)`, identity.InstanceID, identity.Kind,
			identity.ProtocolVersion, "old_protocol"); err != nil {
			return rollback(fmt.Errorf("record evaluation writer audit: %w", err))
		}
	}
	return tx, nil
}

func validateEvaluationWriterCutover(identity EvaluationWriterIdentity, state evaluationCutoverState) error {
	if state.writeMode == "draining" && identity.Kind != "migration" {
		return ErrRadarCutoverActive
	}
	if state.writeMode == "closed" && identity.Kind != "migration" {
		return ErrRadarCutoverActive
	}
	if state.guardMode == "enforce" && identity.ProtocolVersion < state.minimumProtocol && identity.Kind != "migration" {
		return ErrRadarCutoverActive
	}
	return nil
}

func mapRadarCutoverError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrRadarCutoverActive) {
		return err
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && strings.Contains(strings.ToLower(pqErr.Message), "radar_cutover_active") {
		return fmt.Errorf("%w: %s", ErrRadarCutoverActive, pqErr.Message)
	}
	if strings.Contains(strings.ToLower(err.Error()), "radar_cutover_active") {
		return fmt.Errorf("%w: %s", ErrRadarCutoverActive, err)
	}
	return err
}

// HeartbeatEvaluationWriter refreshes a registered session without opening a
// business transaction. It is used by long-running workers during cutover.
func HeartbeatEvaluationWriter(ctx context.Context, db *sql.DB, identity EvaluationWriterIdentity, ttl time.Duration) error {
	if db == nil {
		return errors.New("nil evaluation writer database")
	}
	if ttl <= 0 {
		return errors.New("evaluation writer heartbeat ttl must be positive")
	}
	result, err := db.ExecContext(ctx, `
		UPDATE evaluation_writer_sessions
		SET heartbeat_expires_at = NOW() + $3::interval, updated_at = NOW()
		WHERE instance_id = $1 AND protocol_version = $2`, identity.InstanceID,
		identity.ProtocolVersion, postgresInterval(ttl))
	if err != nil {
		return fmt.Errorf("heartbeat evaluation writer: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("read evaluation writer heartbeat result: %w", err)
	} else if affected == 0 {
		return errors.New("evaluation writer session is unavailable")
	}
	return nil
}
