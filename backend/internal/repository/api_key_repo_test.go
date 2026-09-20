package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyRepository_GetByIDIncludeDeleted(t *testing.T) {
	repo, client := newAPIKeyRepoSQLite(t)
	ctx := context.Background()
	user := mustCreateAPIKeyRepoUser(t, ctx, client, "deleted-billing-owner@test.com")
	group, err := client.Group.Create().
		SetName("deleted-billing-owner").
		SetPlatform(service.PlatformOpenAI).
		SetStatus(service.StatusActive).
		SetSubscriptionType(service.SubscriptionTypeStandard).
		SetRateMultiplier(1).
		Save(ctx)
	require.NoError(t, err)

	key := &service.APIKey{
		UserID:  user.ID,
		GroupID: &group.ID,
		Key:     "sk-deleted-billing-owner",
		Name:    "Deleted Billing Owner",
		Status:  service.StatusActive,
	}
	require.NoError(t, repo.Create(ctx, key))
	require.NoError(t, repo.DeleteWithAudit(ctx, key.ID))

	deleted, err := repo.GetByIDIncludeDeleted(ctx, key.ID)
	require.NoError(t, err)
	require.Equal(t, key.ID, deleted.ID)
	require.Equal(t, key.UserID, deleted.UserID)
	require.NotNil(t, deleted.User)
	require.NotNil(t, deleted.Group)

	_, err = repo.GetByID(ctx, key.ID)
	require.ErrorIs(t, err, service.ErrAPIKeyNotFound)
}
