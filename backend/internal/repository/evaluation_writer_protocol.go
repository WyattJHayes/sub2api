package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// EvaluationWriterIdentity identifies a process that is allowed to mutate the
// trusted evaluation tables during the schema cutover.
type EvaluationWriterIdentity struct {
	InstanceID      string
	Kind            string
	ProtocolVersion int64
}

var (
	// ErrRadarCutoverActive is returned when the database is draining or closed
	// and the caller is not an approved migration owner.
	ErrRadarCutoverActive = errors.New("radar_cutover_active")
	// ErrRadarWriterProtocol is returned when a writer has no accepted session or
	// uses a protocol older than the configured cutover minimum.
	ErrRadarWriterProtocol = errors.New("radar_writer_protocol")
)

const (
	defaultEvaluationWriterLease           = "5 minutes"
	currentEvaluationWriterProtocolVersion = int64(2)
)

var (
	processEvaluationWriterID = uuid.NewString()
	processEvaluationWriterMu sync.Mutex
)

func defaultEvaluationWriterIdentity(kind string) EvaluationWriterIdentity {
	processEvaluationWriterMu.Lock()
	defer processEvaluationWriterMu.Unlock()
	instanceID := processEvaluationWriterID
	if configured := strings.TrimSpace(os.Getenv("RADAR_WRITER_INSTANCE_ID")); configured != "" {
		if _, err := uuid.Parse(configured); err == nil {
			instanceID = configured
		}
	}
	return EvaluationWriterIdentity{InstanceID: instanceID, Kind: kind, ProtocolVersion: currentEvaluationWriterProtocolVersion}
}

// beginRadarWriterTx is the compatibility bridge for legacy repository methods
// that still own their commit and rollback logic. New code should prefer
// WithEvaluationWriterTx so guard errors are mapped at the transaction boundary.
func beginRadarWriterTx(ctx context.Context, db *sql.DB, kind string) (*sql.Tx, error) {
	identity := defaultEvaluationWriterIdentity(kind)
	if db == nil {
		return nil, errors.New("nil evaluation database")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, mapEvaluationWriterError(err)
	}
	if err := registerEvaluationWriterSession(ctx, tx, identity); err != nil {
		_ = tx.Rollback()
		return nil, mapEvaluationWriterError(err)
	}
	if err := setEvaluationWriterIdentity(ctx, tx, identity); err != nil {
		_ = tx.Rollback()
		return nil, mapEvaluationWriterError(err)
	}
	return tx, nil
}

func (r *evaluationRepository) WithEvaluationWriterTx(ctx context.Context, identity EvaluationWriterIdentity, fn func(*sql.Tx) error) error {
	return withEvaluationWriterTx(ctx, r.db, identity, fn)
}

func (r *evaluationGradingRepository) WithEvaluationWriterTx(ctx context.Context, identity EvaluationWriterIdentity, fn func(*sql.Tx) error) error {
	return withEvaluationWriterTx(ctx, r.db, identity, fn)
}

func (r *radarGovernanceRepository) WithEvaluationWriterTx(ctx context.Context, identity EvaluationWriterIdentity, fn func(*sql.Tx) error) error {
	return withEvaluationWriterTx(ctx, r.db, identity, fn)
}

// HeartbeatEvaluationWriterSession refreshes a writer lease without running a
// business mutation. It deliberately uses the same upsert as the transaction
// wrapper so a restarted process can recover its session idempotently.
func (r *evaluationRepository) HeartbeatEvaluationWriterSession(ctx context.Context, identity EvaluationWriterIdentity) error {
	return heartbeatEvaluationWriterSession(ctx, r.db, identity)
}

func (r *evaluationGradingRepository) HeartbeatEvaluationWriterSession(ctx context.Context, identity EvaluationWriterIdentity) error {
	return heartbeatEvaluationWriterSession(ctx, r.db, identity)
}

func (r *radarGovernanceRepository) HeartbeatEvaluationWriterSession(ctx context.Context, identity EvaluationWriterIdentity) error {
	return heartbeatEvaluationWriterSession(ctx, r.db, identity)
}

func withEvaluationWriterTx(ctx context.Context, db *sql.DB, identity EvaluationWriterIdentity, fn func(*sql.Tx) error) (err error) {
	if db == nil {
		return errors.New("nil evaluation database")
	}
	if fn == nil {
		return errors.New("evaluation writer transaction callback is required")
	}
	if err := validateEvaluationWriterIdentity(identity); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return mapEvaluationWriterError(fmt.Errorf("begin evaluation writer transaction: %w", err))
	}
	defer func() {
		if rollbackErr := tx.Rollback(); err == nil && rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = rollbackErr
		}
	}()

	if err = registerEvaluationWriterSession(ctx, tx, identity); err != nil {
		return mapEvaluationWriterError(err)
	}
	if err = setEvaluationWriterIdentity(ctx, tx, identity); err != nil {
		return mapEvaluationWriterError(err)
	}
	if err = fn(tx); err != nil {
		return mapEvaluationWriterError(err)
	}
	if err = tx.Commit(); err != nil {
		return mapEvaluationWriterError(fmt.Errorf("commit evaluation writer transaction: %w", err))
	}
	return nil
}

func heartbeatEvaluationWriterSession(ctx context.Context, db *sql.DB, identity EvaluationWriterIdentity) error {
	if db == nil {
		return errors.New("nil evaluation database")
	}
	if err := validateEvaluationWriterIdentity(identity); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return mapEvaluationWriterError(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := registerEvaluationWriterSession(ctx, tx, identity); err != nil {
		return mapEvaluationWriterError(err)
	}
	if err := tx.Commit(); err != nil {
		return mapEvaluationWriterError(err)
	}
	return nil
}

func validateEvaluationWriterIdentity(identity EvaluationWriterIdentity) error {
	if _, err := uuid.Parse(strings.TrimSpace(identity.InstanceID)); err != nil {
		return fmt.Errorf("invalid evaluation writer instance id: %w", err)
	}
	if strings.TrimSpace(identity.Kind) == "" || len(identity.Kind) > 32 {
		return errors.New("evaluation writer kind is required")
	}
	if identity.ProtocolVersion <= 0 {
		return errors.New("evaluation writer protocol version must be positive")
	}
	return nil
}

func registerEvaluationWriterSession(ctx context.Context, tx *sql.Tx, identity EvaluationWriterIdentity) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_writer_sessions (
			instance_id, writer_kind, protocol_version, heartbeat_expires_at,
			last_transaction_at, updated_at
		) VALUES ($1::uuid, $2, $3, NOW() + INTERVAL '`+defaultEvaluationWriterLease+`', NOW(), NOW())
		ON CONFLICT (instance_id) DO UPDATE SET
			writer_kind = EXCLUDED.writer_kind,
			protocol_version = EXCLUDED.protocol_version,
			heartbeat_expires_at = EXCLUDED.heartbeat_expires_at,
			last_transaction_at = EXCLUDED.last_transaction_at,
			updated_at = EXCLUDED.updated_at`,
		strings.TrimSpace(identity.InstanceID), identity.Kind, identity.ProtocolVersion)
	if err != nil {
		return fmt.Errorf("register evaluation writer session: %w", err)
	}
	return nil
}

func setEvaluationWriterIdentity(ctx context.Context, tx *sql.Tx, identity EvaluationWriterIdentity) error {
	settings := []struct {
		name  string
		value string
	}{
		{name: "app.evaluation_writer_instance_id", value: strings.TrimSpace(identity.InstanceID)},
		{name: "app.evaluation_writer_protocol", value: strconv.FormatInt(identity.ProtocolVersion, 10)},
		{name: "app.evaluation_writer_kind", value: strings.TrimSpace(identity.Kind)},
	}
	for _, setting := range settings {
		if _, err := tx.ExecContext(ctx, "SELECT set_config('"+setting.name+"', $1, true)", setting.value); err != nil {
			return fmt.Errorf("set evaluation writer identity %s: %w", setting.name, err)
		}
	}
	return nil
}

func mapEvaluationWriterError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrRadarCutoverActive) || errors.Is(err, ErrRadarWriterProtocol) {
		return err
	}
	message := strings.ToLower(err.Error())
	if pqErr := (*pq.Error)(nil); errors.As(err, &pqErr) && pqErr != nil {
		message += " " + strings.ToLower(pqErr.Message)
	}
	switch {
	case strings.Contains(message, "radar_cutover_active"):
		return fmt.Errorf("%w: %v", ErrRadarCutoverActive, err)
	case strings.Contains(message, "unknown_writer_session"),
		strings.Contains(message, "missing_writer_identity"),
		strings.Contains(message, "writer_protocol"):
		return fmt.Errorf("%w: %v", ErrRadarWriterProtocol, err)
	default:
		return err
	}
}
