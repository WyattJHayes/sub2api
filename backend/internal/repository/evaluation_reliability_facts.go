package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

const radarReliabilityFactsSchemaVersion = "radar-reliability-facts-v1"

// GetReliabilityFacts returns the immutable references consumed by the
// external acceptance verifier. Every query runs in one repeatable-read
// transaction so a head cannot move between the returned rows.
func (r *radarGovernanceRepository) GetReliabilityFacts(
	ctx context.Context, runID, policyID uuid.UUID, profileID string,
) (*service.RadarReliabilityFacts, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	if runID == uuid.Nil || policyID == uuid.Nil || strings.TrimSpace(profileID) == "" {
		return nil, errors.New("run, policy, and reliability profile are required")
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin reliability facts read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var tenantID int64
	if err := tx.QueryRowContext(ctx, `SELECT tenant_id FROM evaluation_runs WHERE id=$1`, runID).Scan(&tenantID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("load reliability facts run: %w", err)
	}
	if scopedTenant, scoped := radarTenant(ctx); scoped && scopedTenant != tenantID {
		return nil, service.ErrRadarForbidden
	}

	facts := &service.RadarReliabilityFacts{
		SchemaVersion:          radarReliabilityFactsSchemaVersion,
		RunID:                  runID,
		Snapshots:              make([]service.RadarReliabilityFactSnapshot, 0),
		ArtifactManifestHashes: make([]string, 0),
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT p.id, p.policy_hash
		FROM evaluation_gate_policies p
		WHERE p.id=$1 AND p.tenant_id=$2 AND p.retired_at IS NULL
		  AND EXISTS (
			SELECT 1 FROM evaluation_gate_decisions d
			WHERE d.run_id=$3 AND d.policy_id=p.id AND d.tenant_id=$2
		  )`, policyID, tenantID, runID).
		Scan(&facts.PolicyID, &facts.PolicyHash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("load reliability facts policy: %w", err)
	}

	if err := tx.QueryRowContext(ctx, `
		SELECT rs.id, rs.subject_hash
		FROM evaluation_release_subjects rs
		JOIN LATERAL (
			SELECT event_type, effective_at, expires_at
			FROM evaluation_release_subject_events
			WHERE release_subject_id=rs.id
			  AND effective_at <= transaction_timestamp()
			ORDER BY sequence DESC
			LIMIT 1
		) event ON TRUE
		WHERE rs.run_id=$1 AND rs.tenant_id=$2
		  AND event.event_type='activated'
		  AND event.expires_at > transaction_timestamp()
		ORDER BY created_at DESC, id DESC
		LIMIT 1`, runID, tenantID).Scan(&facts.ReleaseSubjectID, &facts.ReleaseSubjectHash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("load reliability facts release subject: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT s.id, s.snapshot_hash, s.run_id, s.load_plan_id,
		       s.reliability_profile_id, s.slice_key, s.window_start, s.window_end,
		       s.query_version, s.source_hash, s.source_watermark, s.fresh_until,
		       s.metrics
		FROM evaluation_reliability_heads h
		JOIN evaluation_reliability_snapshots s
		  ON s.id=h.snapshot_id AND s.run_id=h.run_id
		WHERE h.run_id=$1 AND h.reliability_profile_id=$2 AND s.tenant_id=$3
		ORDER BY s.slice_key`, runID, strings.TrimSpace(profileID), tenantID)
	if err != nil {
		return nil, fmt.Errorf("load reliability facts snapshots: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var snapshot service.RadarReliabilityFactSnapshot
		var loadPlanID uuid.NullUUID
		if err := rows.Scan(
			&snapshot.ID, &snapshot.SnapshotHash, &snapshot.RunID, &loadPlanID,
			&snapshot.ProfileID, &snapshot.SliceKey, &snapshot.WindowStart, &snapshot.WindowEnd,
			&snapshot.QueryVersion, &snapshot.SourceHash, &snapshot.SourceWatermark,
			&snapshot.FreshUntil, &snapshot.Metrics,
		); err != nil {
			return nil, fmt.Errorf("scan reliability facts snapshot: %w", err)
		}
		if !loadPlanID.Valid {
			return nil, errors.New("reliability snapshot has no load plan")
		}
		snapshot.LoadPlanID = loadPlanID.UUID
		if facts.LoadPlanID == uuid.Nil {
			facts.LoadPlanID = snapshot.LoadPlanID
		} else if facts.LoadPlanID != snapshot.LoadPlanID {
			return nil, errors.New("reliability snapshots reference multiple load plans")
		}
		facts.ProfileID = snapshot.ProfileID
		facts.Snapshots = append(facts.Snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reliability facts snapshots: %w", err)
	}
	if len(facts.Snapshots) == 0 || facts.LoadPlanID == uuid.Nil {
		return nil, errors.New("reliability facts have no current snapshots")
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT load_plan_sha256
		FROM evaluation_load_plans
		WHERE id=$1 AND tenant_id=$2 AND status='published'`, facts.LoadPlanID, tenantID).
		Scan(&facts.LoadPlanSHA256); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("load reliability facts load plan: %w", err)
	}

	var recovery service.RadarRecoveryFact
	if err := tx.QueryRowContext(ctx, `
		SELECT e.id, e.evidence_hash, e.run_id, e.experiment_id,
		       e.source_watermark, e.recovery_generation
		FROM evaluation_recovery_evidence e
		WHERE e.run_id=$1 AND e.tenant_id=$2 AND e.status='verified'
		ORDER BY e.created_at DESC, e.id DESC
		LIMIT 1`, runID, tenantID).Scan(
		&recovery.EvidenceID, &recovery.EvidenceHash, &recovery.RunID, &recovery.ExperimentID,
		&recovery.SourceWatermark, &recovery.RecoveryGeneration,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrRecoveryEvidenceInvalid
		}
		return nil, fmt.Errorf("load reliability facts recovery: %w", err)
	}
	facts.Recovery = &recovery

	artifactRows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT s.artifact_manifest_hash
		FROM evaluation_scores s
		JOIN evaluation_assignments a ON a.id=s.source_assignment_id
		JOIN evaluation_samples sample ON sample.id=a.sample_id AND sample.run_id=$1
		JOIN evaluation_runs score_run ON score_run.id=sample.run_id AND score_run.tenant_id=$2
		WHERE s.artifact_manifest_hash IS NOT NULL
		  AND s.artifact_manifest_hash ~ '^[0-9a-f]{64}$'
		ORDER BY s.artifact_manifest_hash`, runID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("load reliability facts artifact manifests: %w", err)
	}
	for artifactRows.Next() {
		var hash string
		if err := artifactRows.Scan(&hash); err != nil {
			artifactRows.Close()
			return nil, fmt.Errorf("scan reliability facts artifact manifest: %w", err)
		}
		facts.ArtifactManifestHashes = append(facts.ArtifactManifestHashes, strings.TrimSpace(hash))
	}
	if err := artifactRows.Err(); err != nil {
		artifactRows.Close()
		return nil, fmt.Errorf("iterate reliability facts artifact manifests: %w", err)
	}
	if err := artifactRows.Close(); err != nil {
		return nil, fmt.Errorf("close reliability facts artifact manifests: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit reliability facts read: %w", err)
	}
	return facts, nil
}

var _ service.RadarReliabilityFactsRepository = (*radarGovernanceRepository)(nil)
