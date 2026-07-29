package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestEvaluationOutboxRepositoryRejectsNilParameters(t *testing.T) {
	repo := &evaluationOutboxRepository{}
	ctx := context.Background()

	_, err := repo.Enqueue(ctx, service.EnqueueEvaluationOutboxInput{})
	require.ErrorIs(t, err, service.ErrEvaluationOutboxInvalid)

	_, err = repo.Claim(ctx, uuid.Nil, nil, 1, time.Minute)
	require.ErrorIs(t, err, service.ErrEvaluationOutboxInvalid)

	err = repo.Heartbeat(ctx, uuid.Nil, "", 0, time.Minute)
	require.ErrorIs(t, err, service.ErrEvaluationOutboxInvalid)

	err = repo.Complete(ctx, uuid.Nil, "", 0)
	require.ErrorIs(t, err, service.ErrEvaluationOutboxInvalid)

	err = repo.DeadLetter(ctx, uuid.Nil, "", 0, "handler_failed")
	require.ErrorIs(t, err, service.ErrEvaluationOutboxInvalid)

	_, err = repo.ReplayDeadLetter(ctx, uuid.Nil)
	require.ErrorIs(t, err, service.ErrEvaluationOutboxInvalid)
}
