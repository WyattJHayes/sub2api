package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolvePluginOutboundIdentityRejectsInactiveAccounts(t *testing.T) {
	for _, status := range []string{StatusDisabled, StatusError} {
		t.Run(status, func(t *testing.T) {
			repo := stubOpenAIAccountRepo{accounts: []Account{{
				ID:       7301,
				Platform: PlatformOpenAI,
				Type:     AccountTypeOAuth,
				Status:   status,
				Credentials: map[string]any{
					"access_token": "test-only-token",
				},
			}}}
			svc := &OpenAIGatewayService{accountRepo: repo}

			identity, err := svc.ResolvePluginOutboundIdentity(context.Background(), 7301)

			require.NoError(t, err)
			require.Nil(t, identity)
		})
	}
}
