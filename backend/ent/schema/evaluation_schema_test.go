package schema

import (
	"testing"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"github.com/stretchr/testify/require"
)

func TestEvaluationConfigurationSchemaImmutabilityAndIdentity(t *testing.T) {
	requireUniqueIndexFields(t, EvaluationDatasetVersion{}.Indexes(), "dataset_key", "version")

	caseFields := EvaluationCase{}.Fields()
	for _, fieldName := range []string{"prompt_spec", "expected_spec", "encrypted_spec", "content_sha256"} {
		require.True(t, requireEntFieldDescriptor(t, caseFields, fieldName).Immutable,
			"EvaluationCase.%s should be immutable", fieldName)
	}
}

func TestEvaluationCaseQualityProbeFieldsAreOptional(t *testing.T) {
	caseFields := EvaluationCase{}.Fields()
	qualityDimension := requireEntFieldDescriptor(t, caseFields, "quality_dimension")
	require.True(t, qualityDimension.Optional, "EvaluationCase.quality_dimension should be optional")
	require.True(t, qualityDimension.Immutable, "EvaluationCase.quality_dimension should be immutable")

	qualityProbeSpec := requireEntFieldDescriptor(t, caseFields, "quality_probe_spec")
	require.False(t, qualityProbeSpec.Optional, "EvaluationCase.quality_probe_spec must match the non-null SQL column")
	require.True(t, qualityProbeSpec.Immutable, "EvaluationCase.quality_probe_spec should be immutable")
	require.NotNil(t, qualityProbeSpec.Default,
		"EvaluationCase.quality_probe_spec should default to an empty JSON object")
}

func requireUniqueIndexFields(t *testing.T, indexes []ent.Index, fields ...string) {
	t.Helper()
	for _, entIndex := range indexes {
		descriptor := entIndex.Descriptor()
		if descriptor.Unique && len(descriptor.Fields) == len(fields) && equalStrings(descriptor.Fields, fields) {
			return
		}
	}
	require.Failf(t, "missing unique index", "expected unique index on %v", fields)
}

func requireEntFieldDescriptor(t *testing.T, fields []ent.Field, name string) *field.Descriptor {
	t.Helper()
	for _, entField := range fields {
		descriptor := entField.Descriptor()
		if descriptor.Name == name {
			return descriptor
		}
	}
	require.Failf(t, "missing field", "expected field %s", name)
	return nil
}

func equalStrings(left, right []string) bool {
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
