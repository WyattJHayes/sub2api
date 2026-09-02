package repository

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestInsertQualityReportTxWritesOnlyValidatedPublicationData(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	report := validQualityReportForPersistence()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT policy FROM quality_policy_versions").
		WithArgs(int64(17), "quality-v1").
		WillReturnRows(sqlmock.NewRows([]string{"policy"}).AddRow([]byte(service.DefaultQualityPolicyJSON())))
	mock.ExpectExec("INSERT INTO quality_reports").WithArgs(
		sqlmock.AnyArg(), int64(17), report.RunID, report.ModelAlias,
		report.OverallConclusion, report.AdulterationRisk, report.DegradationRisk,
		report.PolicyVersion, report.GeneratedAt, report.FreshUntil, int64(7),
	).WillReturnResult(sqlmock.NewResult(1, 1))
	for range 8 {
		mock.ExpectExec("INSERT INTO quality_dimension_results").WillReturnResult(sqlmock.NewResult(1, 1))
	}
	mock.ExpectExec("INSERT INTO quality_source_attributions").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO quality_probe_observations").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	err = insertQualityReportTx(context.Background(), tx, 17, report.RunID, report.ModelAlias, 7, report)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestInsertQualityReportTxRejectsPolicyMismatchBeforeAnyWrite(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*service.QualityReportPublication)
	}{
		{
			name: "source attribution policy",
			mutate: func(report *service.QualityReportPublication) {
				report.SourceAttributionPolicy.MinimumMargin = 0.20
			},
		},
		{
			name: "derived dimension status",
			mutate: func(report *service.QualityReportPublication) {
				report.Dimensions[0].ReferenceBaselineDeltaPP = repositoryQualityFloatPointer(20)
			},
		},
		{
			name: "derived evidence code",
			mutate: func(report *service.QualityReportPublication) {
				report.Dimensions[1].EvidenceCode = service.QualityEvidenceWithinPolicyBounds
			},
		},
		{
			name: "policy freshness",
			mutate: func(report *service.QualityReportPublication) {
				report.FreshUntil = report.GeneratedAt.Add(23 * time.Hour)
			},
		},
		{
			name: "derived aggregate conclusion",
			mutate: func(report *service.QualityReportPublication) {
				report.OverallConclusion = service.QualityConclusionObserve
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			report := validQualityReportForPersistence()
			test.mutate(&report)
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT policy FROM quality_policy_versions").
				WithArgs(int64(17), "quality-v1").
				WillReturnRows(sqlmock.NewRows([]string{"policy"}).AddRow([]byte(service.DefaultQualityPolicyJSON())))
			mock.ExpectRollback()

			tx, err := db.BeginTx(context.Background(), nil)
			require.NoError(t, err)
			err = insertQualityReportTx(context.Background(), tx, 17, report.RunID, report.ModelAlias, 7, report)
			require.ErrorIs(t, err, service.ErrInvalidQualityPolicy)
			require.NoError(t, tx.Rollback())
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestModelQualityRepositoryListsLatestTenantSummaries(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	checkedAt := time.Date(2026, time.August, 11, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery("ORDER BY report.model_alias, report.aggregate_revision DESC, report.generated_at DESC").
		WithArgs(int64(17)).
		WillReturnRows(sqlmock.NewRows([]string{
			"model_alias", "overall_conclusion", "adulteration_risk", "degradation_risk", "generated_at", "fresh_until",
		}).AddRow("model-a", "suspected", "high_risk", "observe", checkedAt, checkedAt.Add(time.Hour)))

	reports, err := NewModelQualityRepository(db).ListPublicQualitySummaries(
		service.WithRadarTenant(context.Background(), 17),
	)

	require.NoError(t, err)
	require.Equal(t, []service.PublicQualitySummary{{
		ModelAlias: "model-a", OverallConclusion: service.QualityConclusionSuspected,
		AdulterationRisk: service.QualityConclusionHighRisk, DegradationRisk: service.QualityConclusionObserve,
		CheckedAt: checkedAt, FreshUntil: checkedAt.Add(time.Hour),
	}}, reports)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestModelQualityRepositoryReadsLatestRevisionTenantScopedPublicDetailWithoutProbeData(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	reportID := uuid.New()
	checkedAt := time.Date(2026, time.August, 11, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery("ORDER BY report.aggregate_revision DESC, report.generated_at DESC").
		WithArgs(int64(17), "model-a").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "model_alias", "overall_conclusion", "adulteration_risk", "degradation_risk",
			"generated_at", "fresh_until", "state", "display_name", "confidence", "coverage",
			"alternate_candidates", "evidence_code",
		}).AddRow(
			reportID, "model-a", "suspected", "high_risk", "observe", checkedAt, checkedAt.Add(time.Hour),
			"insufficient_evidence", nil, nil, nil, []byte("[]"), "source_insufficient_evidence",
		))
	mock.ExpectQuery("SELECT dimension_key, score, status").
		WithArgs(int64(17), reportID).
		WillReturnRows(sqlmock.NewRows([]string{
			"dimension_key", "score", "status", "sample_count", "confidence",
			"stable_baseline_delta_pp", "reference_baseline_delta_pp", "checked_at", "evidence_code",
		}).AddRow(
			"model_fingerprint", 0.7, "suspected", 3, 0.9, nil, -10.0, checkedAt, "fingerprint_mismatch",
		))

	report, err := NewModelQualityRepository(db).GetPublicQualityReport(
		service.WithRadarTenant(context.Background(), 17), "model-a",
	)

	require.NoError(t, err)
	require.Equal(t, "model-a", report.ModelAlias)
	require.Len(t, report.Dimensions, 1)
	require.Equal(t, service.QualitySourceInsufficientEvidence, report.SourceAttribution.State)
	require.Equal(t, []service.PublicQualityEvidence{
		{DimensionKey: service.QualityDimensionModelFingerprint, Code: service.QualityEvidenceFingerprintMismatch},
		{Code: service.QualityEvidenceSourceInsufficientEvidence},
	}, report.Evidence)
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "probe_spec_hash")
	require.NotContains(t, string(encoded), "observation_hash")
	require.NoError(t, mock.ExpectationsWereMet())
}

func validQualityReportForPersistence() service.QualityReportPublication {
	checkedAt := time.Date(2026, time.August, 11, 0, 0, 0, 0, time.UTC)
	dimensions := []service.QualityDimension{
		service.QualityDimensionKnowledgeFreshness,
		service.QualityDimensionModelFingerprint,
		service.QualityDimensionReasoningStability,
		service.QualityDimensionStructureCompliance,
		service.QualityDimensionParameterFidelity,
		service.QualityDimensionInstructionHierarchy,
		service.QualityDimensionProtocolSchema,
		service.QualityDimensionStreamCompleteness,
	}
	results := make([]service.QualityDimensionResult, 0, len(dimensions))
	for _, dimension := range dimensions {
		results = append(results, service.QualityDimensionResult{
			Key: dimension, Score: 0.8, Status: service.QualityConclusionNoSignificantAnomaly,
			SampleCount: 3, Confidence: 1, CheckedAt: checkedAt,
			EvidenceCode:             service.QualityEvidenceWithinPolicyBounds,
			ReferenceBaselineDeltaPP: repositoryQualityFloatPointer(0),
		})
	}
	results[1].EvidenceCode = service.QualityEvidenceFingerprintMatched
	return service.QualityReportPublication{
		RunID: uuid.New(), ModelAlias: "model-a", PolicyVersion: "quality-v1",
		OverallConclusion: service.QualityConclusionNoSignificantAnomaly,
		AdulterationRisk:  service.QualityConclusionNoSignificantAnomaly,
		DegradationRisk:   service.QualityConclusionNoSignificantAnomaly,
		GeneratedAt:       checkedAt, FreshUntil: checkedAt.Add(24 * time.Hour),
		Dimensions: results,
		SourceAttribution: service.QualitySourceAttribution{
			State:        service.QualitySourceInsufficientEvidence,
			EvidenceCode: service.QualityEvidenceSourceInsufficientEvidence,
		},
		SourceAttributionPolicy: service.QualitySourceAttributionPolicy{
			MinimumCoverage: 0.8, MinimumConfidence: 0.7, MinimumMargin: 0.15,
		},
		ProbeObservations: []service.QualityProbeObservation{{
			ProbeSpecHash:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			ObservationHash: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			EventClass:      service.QualityProbeEventClassResponseShape,
			EventDigest:     "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			ObservedAt:      checkedAt,
		}},
	}
}

func repositoryQualityFloatPointer(value float64) *float64 {
	return &value
}
