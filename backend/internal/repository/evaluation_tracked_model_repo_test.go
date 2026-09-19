package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestListModelHealthIncludesTenantTrackedModelWithoutAggregate(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	freshness := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery("WITH latest_aggregates.*evaluation_tracked_models.*tm.tenant_id").
		WithArgs(int64(17)).
		WillReturnRows(sqlmock.NewRows([]string{
			"model_alias", "model_route", "capability_domain", "baseline_score", "candidate_score",
			"delta_pp", "ci_low_pp", "ci_high_pp", "effective_pair_count",
			"aggregate_p99_ms", "aggregate_error_rate", "route_p99_ms", "route_error_rate", "window_start",
		}).AddRow("gpt-5.6-sol", "gpt-5.6-sol", "", nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, freshness))

	ctx := service.WithRadarTenant(context.Background(), 17)
	items, err := (&radarGovernanceRepository{db: db}).ListModelHealth(ctx)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, "gpt-5.6-sol", items[0].ModelRoute)
	require.Equal(t, "insufficient_evidence", items[0].HealthState)
	require.Nil(t, items[0].SampleCount)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestListModelHealthSeparatesTrackedAliasFromVisibleRoute(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	freshness := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery("WITH latest_aggregates.*evaluation_tracked_models.*tm.tenant_id").
		WithArgs(int64(17)).
		WillReturnRows(sqlmock.NewRows([]string{
			"model_alias", "model_route", "capability_domain", "baseline_score", "candidate_score",
			"delta_pp", "ci_low_pp", "ci_high_pp", "effective_pair_count",
			"aggregate_p99_ms", "aggregate_error_rate", "route_p99_ms", "route_error_rate", "window_start",
		}).
			AddRow("", "global", "coding", nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, freshness).
			AddRow("gpt-5.6-sol", "gpt-5.6-sol", "", nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, freshness))

	items, err := (&radarGovernanceRepository{db: db}).ListModelHealth(
		service.WithRadarTenant(context.Background(), 17),
	)
	require.NoError(t, err)
	require.Len(t, items, 2)
	require.Equal(t, "global", items[0].ModelRoute)
	require.Equal(t, "gpt-5.6-sol", items[1].ModelRoute)
	payload, err := json.Marshal(items)
	require.NoError(t, err)
	var decoded []map[string]any
	require.NoError(t, json.Unmarshal(payload, &decoded))
	require.NotContains(t, decoded[0], "model_alias")
	require.Equal(t, "gpt-5.6-sol", decoded[1]["model_alias"])
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRegisterTrackedModelKeepsOriginalTenantRecord(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	createdAt := time.Date(2026, 8, 10, 1, 0, 0, 0, time.UTC)
	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery("INSERT INTO evaluation_tracked_models").
		WithArgs(int64(17), "gpt-5.6-terra", int64(77)).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT model_alias, created_by, created_at.*evaluation_tracked_models").
		WithArgs(int64(17), "gpt-5.6-terra").
		WillReturnRows(sqlmock.NewRows([]string{"model_alias", "created_by", "created_at"}).
			AddRow("gpt-5.6-terra", int64(9), createdAt))
	mock.ExpectExec("INSERT INTO quality_policy_versions").
		WithArgs(sqlmock.AnyArg(), int64(17), `{"minimum_coverage":0.8,"minimum_confidence":0.7,"minimum_margin":0.15,"minimum_samples_per_dimension":3,"observe_delta_pp":5,"suspected_delta_pp":10,"high_risk_delta_pp":20,"freshness_hours":24}`, int64(77)).
		WillReturnResult(sqlmock.NewResult(1, 0))
	mock.ExpectCommit()

	ctx := service.WithRadarTenant(context.Background(), 17)
	item, err := (&radarGovernanceRepository{db: db}).RegisterTrackedModel(
		ctx, "gpt-5.6-terra", 77,
	)
	require.NoError(t, err)
	require.Equal(t, "gpt-5.6-terra", item.ModelAlias)
	require.Equal(t, int64(9), item.CreatedBy)
	require.Equal(t, createdAt, item.CreatedAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRegisterTrackedModelCreatesDefaultQualityPolicy(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	createdAt := time.Date(2026, 8, 10, 1, 0, 0, 0, time.UTC)
	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery("INSERT INTO evaluation_tracked_models").
		WithArgs(int64(17), "gpt-5.6-terra", int64(77)).
		WillReturnRows(sqlmock.NewRows([]string{"model_alias", "created_by", "created_at"}).
			AddRow("gpt-5.6-terra", int64(77), createdAt))
	mock.ExpectExec("INSERT INTO quality_policy_versions").
		WithArgs(sqlmock.AnyArg(), int64(17), `{"minimum_coverage":0.8,"minimum_confidence":0.7,"minimum_margin":0.15,"minimum_samples_per_dimension":3,"observe_delta_pp":5,"suspected_delta_pp":10,"high_risk_delta_pp":20,"freshness_hours":24}`, int64(77)).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	ctx := service.WithRadarTenant(context.Background(), 17)
	_, err = (&radarGovernanceRepository{db: db}).RegisterTrackedModel(ctx, "gpt-5.6-terra", 77)

	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUntrackModelDeletesOnlyCurrentTenantRow(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectExec("DELETE FROM evaluation_tracked_models WHERE tenant_id=\\$1 AND model_alias=\\$2").
		WithArgs(int64(17), "gpt-4-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err = (&radarGovernanceRepository{db: db}).UntrackModel(
		service.WithRadarTenant(context.Background(), 17), "gpt-4-1", 77,
	)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUntrackModelReturnsNotFoundForUnknownAlias(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectExec("DELETE FROM evaluation_tracked_models WHERE tenant_id=\\$1 AND model_alias=\\$2").
		WithArgs(int64(17), "unknown-model").
		WillReturnResult(sqlmock.NewResult(0, 0))

	err = (&radarGovernanceRepository{db: db}).UntrackModel(
		service.WithRadarTenant(context.Background(), 17), "unknown-model", 77,
	)
	require.ErrorIs(t, err, service.ErrTrackedModelNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestListDatasetsIncludesProvenanceForCurrentTenant(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	datasetID := uuid.New()
	createdAt := time.Date(2026, 8, 11, 4, 0, 0, 0, time.UTC)
	mock.ExpectQuery("SELECT d.id, d.dataset_key, d.version, d.status, COUNT\\(c.id\\), d.source_type").
		WithArgs(int64(17)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "dataset_key", "version", "status", "cases", "source_type", "created_by", "tenant_id", "created_at",
		}).AddRow(datasetID, "quality-v1", "2026-08-11", "published", 8, "synthetic", int64(77), int64(17), createdAt))

	items, err := (&radarGovernanceRepository{db: db}).ListDatasets(service.WithRadarTenant(context.Background(), 17))
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, "synthetic", items[0].SourceType)
	require.Equal(t, int64(77), items[0].CreatedBy)
	require.Equal(t, int64(17), items[0].TenantID)
	require.NoError(t, mock.ExpectationsWereMet())
}
