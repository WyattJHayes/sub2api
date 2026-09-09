package service

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestOutboxDedupKeyIsStableAcrossRetry(t *testing.T) {
	runID := uuid.MustParse("00000000-0000-0000-0000-000000000001")

	first, err := OutboxDedupKey("cell_recompute", runID, "coding/route-a", "v1", strings.Repeat("a", 64))
	require.NoError(t, err)
	second, err := OutboxDedupKey("cell_recompute", runID, "coding/route-a", "v1", strings.Repeat("a", 64))
	require.NoError(t, err)

	require.Equal(t, first, second)
	require.Len(t, first, 64)
}

func TestOutboxDedupKeyChangesWhenSourceIdentityChanges(t *testing.T) {
	runID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	first, err := OutboxDedupKey("cell_recompute", runID, "coding/route-a", "v1", strings.Repeat("a", 64))
	require.NoError(t, err)
	second, err := OutboxDedupKey("cell_recompute", runID, "coding/route-a", "v1", strings.Repeat("b", 64))
	require.NoError(t, err)

	require.NotEqual(t, first, second)
}

func TestCauseSetHashIsStableAcrossInputOrder(t *testing.T) {
	firstID := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
	secondID := uuid.MustParse("00000000-0000-0000-0000-000000000001")

	forward, err := CauseSetHash([]uuid.UUID{firstID, secondID, firstID})
	require.NoError(t, err)
	reverse, err := CauseSetHash([]uuid.UUID{secondID, firstID})
	require.NoError(t, err)

	require.Equal(t, forward, reverse)
	require.Equal(t, "8cf19c4ab7202caf2af0ad83672e25048cca6172517a7bc3acdf15b7568f97db", forward)
}

func TestCauseSetHashRejectsEmptyOrNilCause(t *testing.T) {
	_, err := CauseSetHash(nil)
	require.ErrorIs(t, err, ErrEvaluationOutboxInvalid)
	_, err = CauseSetHash([]uuid.UUID{uuid.Nil})
	require.ErrorIs(t, err, ErrEvaluationOutboxInvalid)
}
