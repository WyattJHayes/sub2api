//go:build unit

package service

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestCreateAsyncVideoBillingTaskInputNormalize(t *testing.T) {
	input := CreateAsyncVideoBillingTaskInput{Provider: "grok", UpstreamTaskID: "task", UserID: 1, APIKeyID: 2}
	require.ErrorIs(t, input.Normalize(), ErrAsyncVideoBillingProviderUnsupported)
	input.Provider = AsyncVideoBillingProviderSeedance
	input.AccountID = 3
	input.Model = " video "
	input.RequestPayloadHash = strings.Repeat("a", 64)
	input.PricingAt = time.Now()
	input.PollDeadlineAt = input.PricingAt.Add(time.Hour)
	require.NoError(t, input.Normalize())
	require.Equal(t, "video", input.Model)
	require.Equal(t, "video", input.BillingModel)
	require.Equal(t, "seedance:task", input.TaskKey)
}

func TestAsyncVideoBillingLeaseIsValid(t *testing.T) {
	require.False(t, (AsyncVideoBillingLease{}).Valid())
	require.True(t, (AsyncVideoBillingLease{TaskID: 1, Token: uuid.New(), Epoch: 1}).Valid())
}
