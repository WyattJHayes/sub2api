package repository

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestScoreSourceUsesTypedRouteEvidenceRefs(t *testing.T) {
	source := service.ScoreSource{
		RouteEvidenceRefs: []service.RouteEvidenceRef{{
			RouteTraceID: "trace-1", RequestOrdinal: 2, PayloadHash: "abc123",
		}},
	}

	encoded, err := json.Marshal(source)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"assignment_id":"00000000-0000-0000-0000-000000000000",
		"route_evidence_set_hash":"",
		"route_evidence_refs":[{
			"route_trace_id":"trace-1",
			"request_ordinal":2,
			"payload_hash":"abc123"
		}],
		"artifact_manifest_hash":""
	}`, string(encoded))
}
