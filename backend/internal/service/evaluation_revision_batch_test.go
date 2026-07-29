package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRevisionBatchStatusesAreFinite(t *testing.T) {
	statuses := []RevisionBatchStatus{
		RevisionBatchPending, RevisionBatchRunning, RevisionBatchBlocked,
		RevisionBatchCompleted, RevisionBatchFailed, RevisionBatchCancelled,
	}
	for _, status := range statuses {
		require.True(t, status.Valid(), string(status))
	}
	require.False(t, RevisionBatchStatus("unknown").Valid())
}

func TestRevisionBatchIdempotencyKeyRequiresLowercaseSHA256(t *testing.T) {
	require.NoError(t, ValidateRevisionBatchIdempotencyKey("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	require.Error(t, ValidateRevisionBatchIdempotencyKey("short"))
	require.Error(t, ValidateRevisionBatchIdempotencyKey("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"))
}
