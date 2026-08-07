package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEvaluationVocabularyValidation(t *testing.T) {
	tests := []struct {
		name    string
		valid   bool
		unknown bool
	}{
		{name: "dataset status", valid: DatasetStatusPublished.Valid(), unknown: DatasetStatus("unknown").Valid()},
		{name: "run status", valid: RunStatusRunning.Valid(), unknown: RunStatus("unknown").Valid()},
		{name: "assignment status", valid: AssignmentStatusEvidenceUploaded.Valid(), unknown: AssignmentStatus("unknown").Valid()},
		{name: "failure class", valid: FailureClassInfrastructure.Valid(), unknown: FailureClass("unknown").Valid()},
		{name: "capability domain", valid: CapabilityDomainLongContext.Valid(), unknown: CapabilityDomain("unknown").Valid()},
		{name: "case priority", valid: CasePriorityP0.Valid(), unknown: CasePriority("unknown").Valid()},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.True(t, test.valid)
			require.False(t, test.unknown)
		})
	}
}
