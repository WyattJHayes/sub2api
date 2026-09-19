//go:build unit

package repository

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestThroughputBreakdownByPlatformUsesSelectedTrafficClass(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	start := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta("AND ul.traffic_class = $3")).
		WithArgs(start, end, string(service.TrafficClassMetadata)).
		WillReturnRows(sqlmock.NewRows([]string{"platform", "request_count", "token_consumed"}).
			AddRow("openai", int64(2), int64(40)))

	items, err := (&opsRepository{db: db}).getThroughputBreakdownByPlatform(
		context.Background(), start, end, service.TrafficClassMetadata,
	)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, int64(2), items[0].RequestCount)
	require.NoError(t, mock.ExpectationsWereMet())
}
