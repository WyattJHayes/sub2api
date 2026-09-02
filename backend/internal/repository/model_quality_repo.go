package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

type modelQualityRepository struct {
	db *sql.DB
}

func NewModelQualityRepository(db *sql.DB) service.QualityReportReader {
	return &modelQualityRepository{db: db}
}

func (r *modelQualityRepository) ListPublicQualitySummaries(ctx context.Context) ([]service.PublicQualitySummary, error) {
	tenantID, err := service.RequireRadarTenant(ctx)
	if err != nil {
		return nil, err
	}
	if r == nil || r.db == nil {
		return nil, errors.New("nil model quality repository")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT ON (report.model_alias)
			report.model_alias, report.overall_conclusion, report.adulteration_risk,
			report.degradation_risk, report.generated_at, report.fresh_until
		FROM quality_reports report
		WHERE report.tenant_id=$1
		ORDER BY report.model_alias, report.aggregate_revision DESC, report.generated_at DESC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list public quality summaries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	summaries := make([]service.PublicQualitySummary, 0)
	for rows.Next() {
		var summary service.PublicQualitySummary
		if err := rows.Scan(
			&summary.ModelAlias, &summary.OverallConclusion, &summary.AdulterationRisk,
			&summary.DegradationRisk, &summary.CheckedAt, &summary.FreshUntil,
		); err != nil {
			return nil, fmt.Errorf("scan public quality summary: %w", err)
		}
		summaries = append(summaries, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate public quality summaries: %w", err)
	}
	return summaries, nil
}

func (r *modelQualityRepository) GetPublicQualityReport(ctx context.Context, modelAlias string) (*service.PublicQualityReport, error) {
	tenantID, err := service.RequireRadarTenant(ctx)
	if err != nil {
		return nil, err
	}
	if r == nil || r.db == nil {
		return nil, errors.New("nil model quality repository")
	}
	modelAlias = strings.TrimSpace(modelAlias)
	if modelAlias == "" {
		return nil, service.ErrQualityReportNotFound
	}
	var reportID uuid.UUID
	var report service.PublicQualityReport
	var state service.QualitySourceState
	var displayName sql.NullString
	var confidence, coverage sql.NullFloat64
	var alternatesJSON []byte
	var sourceEvidence service.QualityEvidenceCode
	err = r.db.QueryRowContext(ctx, `
		SELECT report.id, report.model_alias, report.overall_conclusion,
			report.adulteration_risk, report.degradation_risk, report.generated_at,
			report.fresh_until, attribution.state, attribution.display_name,
			attribution.confidence, attribution.coverage, attribution.alternate_candidates,
			attribution.evidence_code
		FROM quality_reports report
		JOIN quality_source_attributions attribution ON attribution.report_id=report.id
			AND attribution.tenant_id=report.tenant_id
		WHERE report.tenant_id=$1 AND report.model_alias=$2
		ORDER BY report.aggregate_revision DESC, report.generated_at DESC
		LIMIT 1`, tenantID, modelAlias).Scan(
		&reportID, &report.ModelAlias, &report.OverallConclusion, &report.AdulterationRisk,
		&report.DegradationRisk, &report.GeneratedAt, &report.FreshUntil, &state,
		&displayName, &confidence, &coverage, &alternatesJSON, &sourceEvidence,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrQualityReportNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load public quality report: %w", err)
	}
	report.SourceAttribution.State = state
	report.SourceAttribution.EvidenceCode = sourceEvidence
	if displayName.Valid {
		report.SourceAttribution.DisplayName = displayName.String
	}
	if confidence.Valid {
		value := confidence.Float64
		report.SourceAttribution.Confidence = &value
	}
	if coverage.Valid {
		report.SourceAttribution.Coverage = coverage.Float64
	}
	if len(alternatesJSON) > 0 && string(alternatesJSON) != "null" {
		if err := json.Unmarshal(alternatesJSON, &report.SourceAttribution.AlternateCandidates); err != nil {
			return nil, fmt.Errorf("decode public quality source alternatives: %w", err)
		}
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT dimension_key, score, status, sample_count, confidence,
			stable_baseline_delta_pp, reference_baseline_delta_pp, checked_at, evidence_code
		FROM quality_dimension_results
		WHERE tenant_id=$1 AND report_id=$2
		ORDER BY dimension_key`, tenantID, reportID)
	if err != nil {
		return nil, fmt.Errorf("list public quality dimensions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var result service.QualityDimensionResult
		var stable, reference sql.NullFloat64
		if err := rows.Scan(
			&result.Key, &result.Score, &result.Status, &result.SampleCount, &result.Confidence,
			&stable, &reference, &result.CheckedAt, &result.EvidenceCode,
		); err != nil {
			return nil, fmt.Errorf("scan public quality dimension: %w", err)
		}
		if stable.Valid {
			value := stable.Float64
			result.StableBaselineDeltaPP = &value
		}
		if reference.Valid {
			value := reference.Float64
			result.ReferenceBaselineDeltaPP = &value
		}
		report.Dimensions = append(report.Dimensions, result)
		report.Evidence = append(report.Evidence, service.PublicQualityEvidence{
			DimensionKey: result.Key, Code: result.EvidenceCode,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate public quality dimensions: %w", err)
	}
	report.Evidence = append(report.Evidence, service.PublicQualityEvidence{Code: sourceEvidence})
	return &report, nil
}

// insertQualityReportTx accepts only the digest-safe worker publication and
// keeps all quality rows in the aggregate-completion transaction.
func insertQualityReportTx(ctx context.Context, tx *sql.Tx, tenantID int64, runID uuid.UUID, modelAlias string, aggregateRevision int64, report service.QualityReportPublication) error {
	if tx == nil || tenantID <= 0 || aggregateRevision < 0 {
		return service.ErrInvalidQualityPolicy
	}
	if err := report.Validate(); err != nil {
		return err
	}
	if report.RunID != runID {
		return service.ErrAggregateRunMismatch
	}
	if report.ModelAlias != strings.TrimPrefix(strings.TrimPrefix(modelAlias, "candidate:"), "baseline:") {
		return service.ErrInvalidQualityReportModelAlias
	}

	var rawPolicy []byte
	if err := tx.QueryRowContext(ctx, `
		SELECT policy FROM quality_policy_versions
		WHERE tenant_id=$1 AND version=$2`, tenantID, report.PolicyVersion).Scan(&rawPolicy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return service.ErrInvalidQualityPolicy
		}
		return fmt.Errorf("load quality policy: %w", err)
	}
	var storedPolicy service.QualityPolicy
	if err := json.Unmarshal(rawPolicy, &storedPolicy); err != nil || storedPolicy.Validate() != nil {
		return service.ErrInvalidQualityPolicy
	}
	if err := service.ValidateQualityReportAgainstPolicy(report, storedPolicy); err != nil {
		return err
	}

	reportID := uuid.New()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO quality_reports (
			id, tenant_id, run_id, model_alias, overall_conclusion, adulteration_risk,
			degradation_risk, policy_version, generated_at, fresh_until, aggregate_revision
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		reportID, tenantID, report.RunID, report.ModelAlias, report.OverallConclusion,
		report.AdulterationRisk, report.DegradationRisk, report.PolicyVersion,
		report.GeneratedAt, report.FreshUntil, aggregateRevision); err != nil {
		if gradingUniqueViolation(err) {
			return service.ErrQualityReportAlreadyPublished
		}
		return fmt.Errorf("insert quality report: %w", err)
	}

	for _, result := range report.Dimensions {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO quality_dimension_results (
				id, tenant_id, report_id, dimension_key, status, score, sample_count,
				confidence, stable_baseline_delta_pp, reference_baseline_delta_pp, checked_at, evidence_code
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			uuid.New(), tenantID, reportID, result.Key, result.Status, result.Score,
			result.SampleCount, result.Confidence, result.StableBaselineDeltaPP,
			result.ReferenceBaselineDeltaPP, result.CheckedAt, result.EvidenceCode); err != nil {
			return fmt.Errorf("insert quality dimension result: %w", err)
		}
	}

	alternates := []byte("[]")
	if len(report.SourceAttribution.AlternateCandidates) > 0 {
		var err error
		alternates, err = json.Marshal(report.SourceAttribution.AlternateCandidates)
		if err != nil {
			return fmt.Errorf("encode quality source alternatives: %w", err)
		}
	}
	var coverage any
	if report.SourceAttribution.State == service.QualitySourceInferred {
		coverage = report.SourceAttribution.Coverage
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO quality_source_attributions (
			id, tenant_id, report_id, state, display_name, confidence, coverage,
			alternate_candidates, fingerprint_version, evidence_code
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9,$10)`,
		uuid.New(), tenantID, reportID, report.SourceAttribution.State,
		nullQualitySourceDisplayName(report.SourceAttribution), report.SourceAttribution.Confidence,
		coverage, alternates, report.PolicyVersion, report.SourceAttribution.EvidenceCode); err != nil {
		return fmt.Errorf("insert quality source attribution: %w", err)
	}

	for _, observation := range report.ProbeObservations {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO quality_probe_observations (
				id, tenant_id, report_id, dimension_result_id, probe_spec_hash,
				observation_hash, event_class, event_digest, observed_at
			) VALUES ($1,$2,$3,NULL,$4,$5,$6,$7,$8)`,
			uuid.New(), tenantID, reportID, observation.ProbeSpecHash,
			observation.ObservationHash, observation.EventClass, observation.EventDigest,
			observation.ObservedAt); err != nil {
			return fmt.Errorf("insert quality probe observation: %w", err)
		}
	}
	return nil
}

func nullQualitySourceDisplayName(attribution service.QualitySourceAttribution) any {
	if attribution.DisplayName == "" {
		return nil
	}
	return attribution.DisplayName
}
