package repository

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestEvaluationProviderRejectsMissingRouteProfileBeforeDatabaseWrite(t *testing.T) {
	for _, cfg := range []*config.Config{nil, {}} {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		_, err = ProvideEvaluationRepository(db, cfg).CreateRunWithMatrix(context.Background(), service.CreateRunInput{
			PlanID: uuid.New(), TriggerSource: "cron", CreatedBy: 1,
		})
		require.ErrorContains(t, err, "evaluation route profile version is required")
		require.NoError(t, mock.ExpectationsWereMet())
	}
}
