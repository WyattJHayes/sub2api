//go:build integration

package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent/apikey"
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyCreateLimitConcurrentInstances(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := testEntClient(t)
	owner, err := client.User.Create().
		SetEmail(fmt.Sprintf("key-cap-%d@test.com", time.Now().UnixNano())).
		SetPasswordHash("synthetic-hash").SetStatus(service.StatusActive).SetRole(service.RoleUser).Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupCtx := mixins.SkipSoftDelete(context.Background())
		_, cleanupErr := client.APIKey.Delete().Where(apikey.UserIDEQ(owner.ID)).Exec(cleanupCtx)
		require.NoError(t, cleanupErr)
		require.NoError(t, client.User.DeleteOneID(owner.ID).Exec(cleanupCtx))
	})
	// Independent repository instances still share a database row lock.
	repos := []service.APIKeyRepository{
		NewAPIKeyRepository(client, integrationDB), NewAPIKeyRepository(client, integrationDB),
	}
	seed := &service.APIKey{UserID: owner.ID, Key: fmt.Sprintf("synthetic-cap-seed-%d", owner.ID), Name: "disabled", Status: service.StatusDisabled}
	require.NoError(t, repos[0].CreateWithActiveLimit(ctx, seed, 2))
	const requests = 16
	keys := make([]*service.APIKey, requests)
	errs := make([]error, requests)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range requests {
		keys[i] = &service.APIKey{UserID: owner.ID, Key: fmt.Sprintf("synthetic-cap-%d-%d", owner.ID, i), Name: "concurrent", Status: service.StatusActive}
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			errs[index] = repos[index%len(repos)].CreateWithActiveLimit(ctx, keys[index], 2)
		}(i)
	}
	close(start)
	wg.Wait()
	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
		} else {
			require.ErrorIs(t, err, service.ErrAPIKeyCountExceeded)
		}
	}
	require.Equal(t, 1, succeeded)
	count, err := repos[0].CountByUserID(ctx, owner.ID)
	require.NoError(t, err)
	require.Equal(t, int64(2), count)
	// Disabled keys count toward the cap; soft-deleting one frees its slot.
	require.NoError(t, repos[0].DeleteWithAudit(ctx, seed.ID))
	replacement := &service.APIKey{UserID: owner.ID, Key: fmt.Sprintf("synthetic-cap-replacement-%d", owner.ID), Name: "replacement", Status: service.StatusActive}
	require.NoError(t, repos[1].CreateWithActiveLimit(ctx, replacement, 2))
	count, err = repos[1].CountByUserID(ctx, owner.ID)
	require.NoError(t, err)
	require.Equal(t, int64(2), count)
}
