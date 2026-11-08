package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRadarV0220TwoGBProfileLoadsBoundedGatewayWithMCPDisabled(t *testing.T) {
	resetViperWithJWTSecret(t)
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "radar", "profiles", "v0.2.20-2g.json"))
	require.NoError(t, err)
	var environment map[string]string
	require.NoError(t, json.Unmarshal(data, &environment))
	for key, value := range environment {
		t.Setenv(key, value)
	}
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, int64(134217728), cfg.Gateway.RequestBodyMemoryBudgetBytes)
	require.Equal(t, 120, cfg.Gateway.RequestBodyReadTimeoutSeconds)
	require.Equal(t, 120, cfg.Gateway.DownstreamWriteTimeoutSeconds)
	require.False(t, cfg.MCP.Enabled())
	require.Equal(t, int64(268435456), cfg.Gateway.MaxBodySize)
	require.Equal(t, 524288000, cfg.Gateway.MaxLineSize)
}
