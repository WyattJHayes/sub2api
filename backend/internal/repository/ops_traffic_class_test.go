package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpsInsertErrorLogArgsIncludesNormalizedTrafficClass(t *testing.T) {
	input := &service.OpsInsertErrorLogInput{TrafficClass: service.TrafficClassSynthetic}
	args := opsInsertErrorLogArgs(input)

	// The class is persisted next to request metadata, after request_type and
	// before the user-agent/error fields in the stable insert contract.
	require.Contains(t, insertOpsErrorLogSQL, "traffic_class")
	require.Contains(t, args, service.TrafficClassSynthetic)
}
