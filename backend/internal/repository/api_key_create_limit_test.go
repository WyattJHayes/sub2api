package repository

import (
	"context"
	"errors"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyRepositoryCreateWithActiveLimit(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		count     int64
		countErr  error
		insertErr error
		commitErr error
		wantErr   error
	}{
		{name: "available slot commits", count: 1},
		{name: "full cap rolls back", count: 2, wantErr: service.ErrAPIKeyCountExceeded},
		{name: "count failure rolls back", countErr: errors.New("count failed")},
		{name: "insert failure rolls back", insertErr: errors.New("insert failed")},
		{name: "commit failure leaves caller unchanged", commitErr: errors.New("commit failed")},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { _ = client.Close() })
			repo := NewAPIKeyRepository(client, db)
			key := &service.APIKey{UserID: 7, Key: "synthetic-key", Name: "limited", Status: service.StatusActive}

			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT .* FROM "users".*FOR UPDATE`).WithArgs(int64(7)).
				WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(7)))
			count := mock.ExpectQuery(`SELECT COUNT\([^)]+\) FROM "api_keys".*"deleted_at" IS NULL.*"user_id" = \$1`).WithArgs(int64(7))
			if scenario.countErr != nil {
				count.WillReturnError(scenario.countErr)
			} else {
				count.WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(scenario.count))
			}
			if scenario.countErr == nil && scenario.count < 2 {
				insert := mock.ExpectQuery(`INSERT INTO "api_keys".*RETURNING "id"`)
				if scenario.insertErr != nil {
					insert.WillReturnError(scenario.insertErr)
				} else {
					insert.WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(11)))
				}
			}
			if scenario.countErr != nil || scenario.count >= 2 || scenario.insertErr != nil {
				mock.ExpectRollback()
			} else if scenario.commitErr != nil {
				mock.ExpectCommit().WillReturnError(scenario.commitErr)
			} else {
				mock.ExpectCommit()
			}

			err = repo.CreateWithActiveLimit(context.Background(), key, 2)
			if scenario.wantErr != nil {
				require.ErrorIs(t, err, scenario.wantErr)
			} else if scenario.countErr != nil || scenario.insertErr != nil || scenario.commitErr != nil {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, int64(11), key.ID)
			}
			if err != nil {
				require.Zero(t, key.ID)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
