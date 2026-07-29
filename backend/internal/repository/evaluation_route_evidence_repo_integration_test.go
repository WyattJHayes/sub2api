//go:build integration

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestEvaluationRouteEvidenceRepository_OutOfOrderWritesConvergeOnPostgres(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.NewString()
	user := mustCreateUser(t, integrationEntClient, &service.User{Email: "route-evidence-" + suffix + "@example.com"})
	apiKey := mustCreateApiKey(t, integrationEntClient, &service.APIKey{
		UserID: user.ID, Key: "sk-route-evidence-" + suffix, Name: "route evidence",
	})
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM evaluation_route_evidence WHERE api_key_id = $1", apiKey.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM api_keys WHERE id = $1", apiKey.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM users WHERE id = $1", user.ID)
	})

	repo := &evaluationRouteEvidenceRepository{sql: integrationDB}
	for _, transportFirst := range []bool{true, false} {
		name := "billing_then_transport"
		if transportFirst {
			name = "transport_then_billing"
		}
		t.Run(name, func(t *testing.T) {
			traceID := "trace-" + uuid.NewString()
			runID := uuid.NewString()
			sampleID := uuid.NewString()
			evalCtx := service.WithEvaluationContext(ctx, service.EvaluationContext{
				RunID: runID, SampleID: sampleID, APIKeyID: apiKey.ID, RouteTraceID: traceID,
				ExpectedModelAlias: "expected-placeholder", ExpectedRouteProfile: "route-placeholder",
				IssuedAt: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
			})
			startedAt := time.Date(2026, 7, 25, 12, 1, 0, 0, time.UTC)
			finishedAt := startedAt.Add(1250 * time.Millisecond)
			transport := service.RouteEvidence{
				RouteTraceID: traceID, EvaluationRunID: runID, SampleID: sampleID, APIKeyID: apiKey.ID,
				RequestID: "request-real", RequestedModel: "public-coder-alias", ResolvedModel: "qwen3-coder-2026-07",
				RouteProfileVersion: "route-v43-real", Provider: "qwen", ChannelRef: "channel_redacted",
				AccountPoolRef: "account_redacted", Region: "cn-east-real", Attempts: 2,
				FallbackChain: []service.RouteFallbackEntry{
					{Ordinal: 1, Provider: "qwen", AccountPoolRef: "account_first", ChannelRef: "channel_redacted", ResolvedModel: "qwen3-coder-2026-07", Region: "cn-east-real", ErrorCode: "429"},
					{Ordinal: 2, Provider: "qwen", AccountPoolRef: "account_redacted", ChannelRef: "channel_redacted", ResolvedModel: "qwen3-coder-2026-07", Region: "cn-east-real"},
				},
				TransportStatus: "succeeded", StartedAt: startedAt, FinishedAt: &finishedAt,
			}
			ttft := 123
			latency := 1250
			usage := service.RouteUsageEvidence{
				InputTokens: 101, OutputTokens: 37, TTFT: &ttft, Latency: &latency,
				BilledAmount: decimal.RequireFromString("0.00012345"), FinishReason: "stop",
			}

			writeTransport := func() { require.NoError(t, repo.UpsertTransport(evalCtx, transport)) }
			writeBilling := func() { require.NoError(t, repo.AttachBilling(evalCtx, traceID, usage)) }
			if transportFirst {
				writeTransport()
				writeTransport()
				writeBilling()
				writeBilling()
			} else {
				writeBilling()
				writeBilling()
				writeTransport()
				writeTransport()
			}

			conflict := transport
			conflict.EvaluationRunID = uuid.NewString()
			require.ErrorIs(t, repo.UpsertTransport(evalCtx, conflict), service.ErrRouteEvidenceIdentityConflict)

			assertEvaluationRouteEvidenceRow(t, ctx, traceID, transport, usage)
		})
	}
}

func TestEvaluationRouteEvidence_SealedIdentityIsImmutable(t *testing.T) {
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	gradingRepo := NewEvaluationGradingRepository(integrationDB)
	run, err := gradingRepo.(*evaluationGradingRepository).createRunForGradingTest(ctx, fixture.planID)
	require.NoError(t, err)
	tx := testTx(t)

	var assignmentID, sampleID, manifestID uuid.UUID
	var manifestHash string
	require.NoError(t, tx.QueryRowContext(ctx, `
		SELECT a.id, a.sample_id, ps.request_manifest_id, ps.request_manifest_sha256
		FROM evaluation_assignments a
		JOIN evaluation_samples s ON s.id = a.sample_id
		JOIN evaluation_side_specs ss ON ss.sample_id = s.id
		JOIN evaluation_pair_specs ps ON ps.id = ss.pair_spec_id
		WHERE s.run_id = $1
		ORDER BY a.id
		LIMIT 1`, run.ID).Scan(&assignmentID, &sampleID, &manifestID, &manifestHash))

	semanticsID := uuid.New()
	signingKeyID := uuid.New()
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_request_semantics (
			id, schema_version, canonical_semantics_bytes, request_semantics_sha256
		) VALUES ($1, 'radar-request-semantics-v1', convert_to('{}', 'UTF8'), $2)`,
		semanticsID, strings.Repeat("1", 64)))
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_evidence_signing_keys (
			id, key_reference, status, state_epoch
		) VALUES ($1, $2, 'active', 1)`, signingKeyID, "test-key-"+uuid.NewString()))

	traceID := "sealed-" + uuid.NewString()
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_route_evidence (
			route_trace_id, evaluation_run_id, sample_id, api_key_id, request_id,
			requested_model, resolved_model, route_profile_version, provider, region,
			attempts, fallback_chain, finish_reason, input_tokens, output_tokens,
			latency_ms, billed_amount, transport_status, started_at, finished_at,
			schema_version, canonicalization_version, assignment_id, request_ordinal,
			lease_epoch, request_manifest_id, request_manifest_sha256, request_slot_id,
			request_semantics_id, request_semantics_sha256,
			request_semantics_policy_sha256, request_tool_schema_sha256,
			request_allowed_tool_set_sha256, evidence_revision, terminal_at, sealed_at,
			payload_hash, signing_key_id, payload_hmac, billing_status,
			gateway_image_digest
		) VALUES (
			$1, $2, $3, $4, 'request-1',
			'route-a', 'model-a', 'route-v1', 'provider-a', 'region-a',
			1, '[]'::jsonb, 'stop', 1, 1,
			1, 0.00000001, 'succeeded', NOW(), NOW(),
			'radar-route-evidence-v1', 'rfc8785-v1', $5, 0,
			0, $6, $7, 'slot-0',
			$8, $9,
			$10, $11, $12, 2, NOW(), NOW(),
			$13, $14, $15, 'complete',
			'sha256:gateway'
		)`, traceID, run.ID, sampleID, fixture.apiKeyID, assignmentID,
		manifestID, manifestHash, semanticsID, strings.Repeat("1", 64),
		strings.Repeat("2", 64), strings.Repeat("3", 64), strings.Repeat("4", 64),
		strings.Repeat("5", 64), signingKeyID, strings.Repeat("6", 64)))

	requireSQLRejectedWithinSavepoint(t, tx, "sealed_evidence_update", `
		UPDATE evaluation_route_evidence SET requested_model = 'mutated' WHERE route_trace_id = $1`, traceID)
	requireSQLRejectedWithinSavepoint(t, tx, "sealed_evidence_delete", `
		DELETE FROM evaluation_route_evidence WHERE route_trace_id = $1`, traceID)
}

func TestCreateOpenRejectsExactSlotMismatchBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	repo, lease, semantics := createOpenRouteEvidenceFixture(t)
	configureTestEvidenceSigningKey(t, repo)
	semantics.PromptHash = strings.Repeat("f", 64)
	input := createOpenRouteEvidenceInput(lease, semantics)

	_, err := repo.CreateOpen(ctx, input)
	require.ErrorIs(t, err, service.ErrRequestSemanticsMismatch)

	var status, reason string
	var terminalAt, sealedAt sql.NullTime
	var payloadHash, payloadHMAC sql.NullString
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT transport_status, COALESCE(incomplete_reason, ''), terminal_at, sealed_at, payload_hash, payload_hmac
		FROM evaluation_route_evidence WHERE route_trace_id = $1`, lease.RouteTraceID).
		Scan(&status, &reason, &terminalAt, &sealedAt, &payloadHash, &payloadHMAC))
	require.Equal(t, "protocol_failed", status)
	require.Equal(t, "request_semantics_mismatch", reason)
	require.True(t, terminalAt.Valid)
	require.True(t, sealedAt.Valid)
	require.Len(t, payloadHash.String, 64)
	require.Len(t, payloadHMAC.String, 64)
}

func TestCreateOpenRejectsInvalidGatewayImageDigestBeforeInsert(t *testing.T) {
	ctx := context.Background()
	repo, lease, semantics := createOpenRouteEvidenceFixture(t)
	input := createOpenRouteEvidenceInput(lease, semantics)
	input.GatewayImageDigest = "sha256:gateway"

	_, err := repo.CreateOpen(ctx, input)
	require.Error(t, err)

	var count int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_route_evidence WHERE route_trace_id = $1`, lease.RouteTraceID).Scan(&count))
	require.Zero(t, count)
}

func TestPatchRouteEvidenceRejectsTerminalProtocolFailureOnPostgres(t *testing.T) {
	ctx := context.Background()
	repo, lease, semantics := createOpenRouteEvidenceFixture(t)
	configureTestEvidenceSigningKey(t, repo)
	semantics.PromptHash = strings.Repeat("f", 64)

	terminal, err := repo.CreateOpen(ctx, createOpenRouteEvidenceInput(lease, semantics))
	require.ErrorIs(t, err, service.ErrRequestSemanticsMismatch)
	require.True(t, terminal.Terminal)

	_, err = repo.PatchRouteEvidence(ctx, lease.RouteTraceID, service.RouteEvidencePatch{
		ExpectedRevision: terminal.Revision,
		Transport:        &service.TransportPatch{Provider: testStringPointer("qwen")},
	})
	require.ErrorIs(t, err, service.ErrRouteEvidenceNotOpen)
}

func TestPatchRouteEvidenceRejectsStaleRevisionOnPostgres(t *testing.T) {
	ctx := context.Background()
	repo, lease, semantics := createOpenRouteEvidenceFixture(t)
	opened, err := repo.CreateOpen(ctx, createOpenRouteEvidenceInput(lease, semantics))
	require.NoError(t, err)

	first, err := repo.PatchRouteEvidence(ctx, lease.RouteTraceID, service.RouteEvidencePatch{
		ExpectedRevision: opened.Revision,
		Transport:        &service.TransportPatch{Provider: testStringPointer("qwen")},
	})
	require.NoError(t, err)
	_, err = repo.PatchRouteEvidence(ctx, lease.RouteTraceID, service.RouteEvidencePatch{
		ExpectedRevision: opened.Revision,
		Transport:        &service.TransportPatch{ResolvedModel: testStringPointer("qwen3-coder")},
	})
	var conflict *service.RouteEvidenceRevisionConflict
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, first.Revision, conflict.CurrentRevision)
}

func TestPatchRouteEvidenceAllowsNullToValueOnceOnPostgres(t *testing.T) {
	ctx := context.Background()
	repo, lease, semantics := createOpenRouteEvidenceFixture(t)
	opened, err := repo.CreateOpen(ctx, createOpenRouteEvidenceInput(lease, semantics))
	require.NoError(t, err)

	patched, err := repo.PatchRouteEvidence(ctx, lease.RouteTraceID, service.RouteEvidencePatch{
		ExpectedRevision: opened.Revision,
		Transport: &service.TransportPatch{
			Provider:      testStringPointer("deepseek"),
			ResolvedModel: testStringPointer("deepseek-v3.2"),
		},
	})
	require.NoError(t, err)
	require.Equal(t, opened.Revision+1, patched.Revision)

	_, err = repo.PatchRouteEvidence(ctx, lease.RouteTraceID, service.RouteEvidencePatch{
		ExpectedRevision: patched.Revision,
		Transport:        &service.TransportPatch{Provider: testStringPointer("qwen")},
	})
	require.ErrorIs(t, err, service.ErrRouteEvidenceFieldImmutable)
}

func TestPatchRouteEvidenceCannotMutateIdentityOrClearFieldOnPostgres(t *testing.T) {
	ctx := context.Background()
	repo, lease, semantics := createOpenRouteEvidenceFixture(t)
	opened, err := repo.CreateOpen(ctx, createOpenRouteEvidenceInput(lease, semantics))
	require.NoError(t, err)
	patched, err := repo.PatchRouteEvidence(ctx, lease.RouteTraceID, service.RouteEvidencePatch{
		ExpectedRevision: opened.Revision,
		Transport:        &service.TransportPatch{Provider: testStringPointer("qwen")},
	})
	require.NoError(t, err)

	identity := patched.Identity
	identity.SampleID = uuid.NewString()
	_, err = repo.PatchRouteEvidence(ctx, lease.RouteTraceID, service.RouteEvidencePatch{
		ExpectedRevision: patched.Revision,
		Identity:         &identity,
	})
	require.ErrorIs(t, err, service.ErrRouteEvidenceIdentityConflict)

	_, err = repo.PatchRouteEvidence(ctx, lease.RouteTraceID, service.RouteEvidencePatch{
		ExpectedRevision: patched.Revision,
		Transport:        &service.TransportPatch{Provider: testStringPointer("")},
	})
	require.ErrorIs(t, err, service.ErrRouteEvidenceFieldImmutable)
}

func TestFinalizeSealsRouteEvidenceWithActiveSigningKeyOnPostgres(t *testing.T) {
	ctx := context.Background()
	repo, lease, semantics := createOpenRouteEvidenceFixture(t)
	keyID := configureTestEvidenceSigningKey(t, repo)

	opened, err := repo.CreateOpen(ctx, createOpenRouteEvidenceInput(lease, semantics))
	require.NoError(t, err)
	start := time.Date(2026, 7, 28, 1, 0, 0, 0, time.UTC)
	finish := start.Add(time.Second)
	transportStatus := "succeeded"
	provider := "qwen"
	model := "model-a"
	channel, account := "channel_1", "account_1"
	attempts := 1
	chain := []service.RouteFallbackEntry{{
		Ordinal: 1, DispatchMode: "primary", RouteRuleHash: strings.Repeat("1", 64), RequestedModel: "route-a",
		ResolvedModel: model, Provider: provider, AccountPoolRef: "account_1", ChannelRef: "channel_1", Region: "default",
		Outcome: "succeeded", StartedAt: start, FinishedAt: &finish,
	}}
	patched, err := repo.PatchRouteEvidence(ctx, lease.RouteTraceID, service.RouteEvidencePatch{
		ExpectedRevision: opened.Revision,
		Transport: &service.TransportPatch{ResolvedModel: &model, Provider: &provider, ChannelRef: &channel, AccountPoolRef: &account, Attempts: &attempts,
			FallbackChain: &chain, TransportStatus: &transportStatus, FinishedAt: &finish},
	})
	require.NoError(t, err)
	inputTokens, outputTokens, latency := 11, 7, 1000
	amount := decimal.RequireFromString("0.00012345")
	finishReason, billingStatus := "stop", "complete"
	patched, err = repo.PatchRouteEvidence(ctx, lease.RouteTraceID, service.RouteEvidencePatch{
		ExpectedRevision: patched.Revision,
		Billing: &service.BillingPatch{InputTokens: &inputTokens, OutputTokens: &outputTokens, Latency: &latency,
			BilledAmount: &amount, FinishReason: &finishReason, BillingStatus: &billingStatus},
	})
	require.NoError(t, err)

	sealed, err := repo.FinalizeRouteEvidence(ctx, service.FinalizeRouteEvidenceInput{
		RouteTraceID: lease.RouteTraceID, ExpectedRevision: patched.Revision, LeaseEpoch: opened.Identity.LeaseEpoch,
	})
	require.NoError(t, err)
	require.Equal(t, patched.Revision+1, sealed.Revision)
	require.Len(t, sealed.PayloadHash, 64)
	require.Equal(t, keyID.String(), sealed.SigningKeyID)
	require.Len(t, sealed.PayloadHMAC, 64)

	var terminalAt, sealedAt sql.NullTime
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT terminal_at, sealed_at FROM evaluation_route_evidence WHERE route_trace_id=$1`, lease.RouteTraceID).Scan(&terminalAt, &sealedAt))
	require.True(t, terminalAt.Valid)
	require.True(t, sealedAt.Valid)
	var sealedOutbox int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_outbox_events
		WHERE event_type='route_evidence_sealed' AND source_type='route_evidence'
		  AND source_id=$1 AND source_hash=$2`, lease.RouteTraceID, sealed.PayloadHash).Scan(&sealedOutbox))
	require.Equal(t, 1, sealedOutbox)

	retried, err := repo.FinalizeRouteEvidence(ctx, service.FinalizeRouteEvidenceInput{
		RouteTraceID: lease.RouteTraceID, ExpectedRevision: patched.Revision, LeaseEpoch: opened.Identity.LeaseEpoch,
	})
	require.NoError(t, err)
	require.Equal(t, sealed, retried)

	_, err = integrationDB.ExecContext(ctx, `
		UPDATE evaluation_evidence_signing_keys
		SET status='revoked', state_epoch=state_epoch+1, revoked_at=transaction_timestamp(), updated_at=transaction_timestamp()
		WHERE id=$1`, keyID)
	require.NoError(t, err)
	_, err = repo.FinalizeRouteEvidence(ctx, service.FinalizeRouteEvidenceInput{
		RouteTraceID: lease.RouteTraceID, ExpectedRevision: patched.Revision, LeaseEpoch: opened.Identity.LeaseEpoch,
	})
	require.ErrorIs(t, err, service.ErrEvidenceSigningKeyRevoked)
}

func TestCancelThenFinalizeReturnsLeaseFenced(t *testing.T) {
	ctx := context.Background()
	repo, lease, semantics := createOpenRouteEvidenceFixture(t)
	opened, err := repo.CreateOpen(ctx, createOpenRouteEvidenceInput(lease, semantics))
	require.NoError(t, err)
	var actorID int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT created_by FROM evaluation_runs WHERE id=$1`, lease.RunID).Scan(&actorID))
	governance := NewRadarGovernanceRepository(integrationDB).(*radarGovernanceRepository)
	blocker := lockEvaluationAssignmentForRace(t, lease.ID)

	type cancelResult struct{ err error }
	cancelStarted := make(chan struct{})
	cancelDone := make(chan cancelResult, 1)
	go func() {
		close(cancelStarted)
		_, cancelErr := governance.CancelRun(ctx, lease.RunID, "operator", actorID, strings.Repeat("7", 64))
		cancelDone <- cancelResult{err: cancelErr}
	}()
	<-cancelStarted
	waitForBlockedEvaluationQuery(t, "UPDATE evaluation_assignments SET status='cancelled'")

	finalizeStarted := make(chan struct{})
	finalizeDone := make(chan error, 1)
	go func() {
		close(finalizeStarted)
		_, finalizeErr := repo.FinalizeRouteEvidence(ctx, service.FinalizeRouteEvidenceInput{
			RouteTraceID: lease.RouteTraceID, ExpectedRevision: opened.Revision, LeaseEpoch: opened.Identity.LeaseEpoch,
		})
		finalizeDone <- finalizeErr
	}()
	<-finalizeStarted
	require.NoError(t, blocker.Rollback())

	require.NoError(t, (<-cancelDone).err)
	require.ErrorIs(t, <-finalizeDone, service.ErrLeaseFenced)
}

func TestFinalizeThenCancelKeepsSealedEvidence(t *testing.T) {
	ctx := context.Background()
	repo, lease, semantics := createOpenRouteEvidenceFixture(t)
	configureTestEvidenceSigningKey(t, repo)
	opened, err := repo.CreateOpen(ctx, createOpenRouteEvidenceInput(lease, semantics))
	require.NoError(t, err)
	start := time.Date(2026, 7, 28, 1, 0, 0, 0, time.UTC)
	finish := start.Add(time.Second)
	transportStatus, provider, model, attempts := "succeeded", "qwen", "model-a", 1
	channel, account := "channel_1", "account_1"
	chain := []service.RouteFallbackEntry{{
		Ordinal: 1, DispatchMode: "primary", RouteRuleHash: strings.Repeat("1", 64), RequestedModel: "route-a",
		ResolvedModel: model, Provider: provider, AccountPoolRef: "account_1", ChannelRef: "channel_1", Region: "default",
		Outcome: "succeeded", StartedAt: start, FinishedAt: &finish,
	}}
	patched, err := repo.PatchRouteEvidence(ctx, lease.RouteTraceID, service.RouteEvidencePatch{
		ExpectedRevision: opened.Revision,
		Transport: &service.TransportPatch{ResolvedModel: &model, Provider: &provider, ChannelRef: &channel, AccountPoolRef: &account, Attempts: &attempts,
			FallbackChain: &chain, TransportStatus: &transportStatus, FinishedAt: &finish},
	})
	require.NoError(t, err)
	inputTokens, outputTokens, latency := 11, 7, 1000
	amount := decimal.RequireFromString("0.00012345")
	finishReason, billingStatus := "stop", "complete"
	patched, err = repo.PatchRouteEvidence(ctx, lease.RouteTraceID, service.RouteEvidencePatch{
		ExpectedRevision: patched.Revision,
		Billing: &service.BillingPatch{InputTokens: &inputTokens, OutputTokens: &outputTokens, Latency: &latency,
			BilledAmount: &amount, FinishReason: &finishReason, BillingStatus: &billingStatus},
	})
	require.NoError(t, err)
	var actorID int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT created_by FROM evaluation_runs WHERE id=$1`, lease.RunID).Scan(&actorID))
	governance := NewRadarGovernanceRepository(integrationDB).(*radarGovernanceRepository)
	blocker := lockEvaluationAssignmentForRace(t, lease.ID)

	finalizeStarted := make(chan struct{})
	finalizeDone := make(chan struct {
		sealed service.SealedRouteEvidence
		err    error
	}, 1)
	go func() {
		close(finalizeStarted)
		result, finalizeErr := repo.FinalizeRouteEvidence(ctx, service.FinalizeRouteEvidenceInput{
			RouteTraceID: lease.RouteTraceID, ExpectedRevision: patched.Revision, LeaseEpoch: opened.Identity.LeaseEpoch,
		})
		finalizeDone <- struct {
			sealed service.SealedRouteEvidence
			err    error
		}{sealed: result, err: finalizeErr}
	}()
	<-finalizeStarted
	waitForBlockedEvaluationQuery(t, "SELECT a.status FROM evaluation_assignments a")

	cancelStarted := make(chan struct{})
	cancelDone := make(chan error, 1)
	go func() {
		close(cancelStarted)
		_, cancelErr := governance.CancelRun(ctx, lease.RunID, "operator", actorID, strings.Repeat("9", 64))
		cancelDone <- cancelErr
	}()
	<-cancelStarted
	require.NoError(t, blocker.Rollback())

	finalized := <-finalizeDone
	require.NoError(t, finalized.err)
	require.NoError(t, <-cancelDone)
	sealed := finalized.sealed

	retried, err := repo.FinalizeRouteEvidence(ctx, service.FinalizeRouteEvidenceInput{
		RouteTraceID: lease.RouteTraceID, ExpectedRevision: patched.Revision, LeaseEpoch: opened.Identity.LeaseEpoch,
	})
	require.NoError(t, err)
	require.Equal(t, sealed, retried)
	var sealedCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_route_evidence
		WHERE route_trace_id=$1 AND evidence_revision=$2 AND payload_hash=$3 AND payload_hmac=$4`,
		lease.RouteTraceID, sealed.Revision, sealed.PayloadHash, sealed.PayloadHMAC).Scan(&sealedCount))
	require.Equal(t, 1, sealedCount)
}

func lockEvaluationAssignmentForRace(t *testing.T, assignmentID uuid.UUID) *sql.Tx {
	t.Helper()
	tx, err := integrationDB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	var lockedID uuid.UUID
	require.NoError(t, tx.QueryRowContext(context.Background(), `
		SELECT id FROM evaluation_assignments WHERE id=$1 FOR UPDATE`, assignmentID).Scan(&lockedID))
	require.Equal(t, assignmentID, lockedID)
	return tx
}

func waitForBlockedEvaluationQuery(t *testing.T, fragment string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var blocked bool
		err := integrationDB.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE pid <> pg_backend_pid()
				  AND state='active'
				  AND wait_event_type='Lock'
				  AND position($1::text in query) > 0
			)`, fragment).Scan(&blocked)
		require.NoError(t, err)
		if blocked {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("query did not block at expected barrier: %s", fragment)
		default:
			runtime.Gosched()
		}
	}
}

func TestSystemFinalizeSealsCancelledOpenEvidence(t *testing.T) {
	ctx := context.Background()
	repo, lease, semantics := createOpenRouteEvidenceFixture(t)
	configureTestEvidenceSigningKey(t, repo)
	_, err := repo.CreateOpen(ctx, createOpenRouteEvidenceInput(lease, semantics))
	require.NoError(t, err)
	var actorID int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT created_by FROM evaluation_runs WHERE id=$1`, lease.RunID).Scan(&actorID))
	governance := NewRadarGovernanceRepository(integrationDB).(*radarGovernanceRepository)
	result, err := governance.CancelRun(ctx, lease.RunID, "operator", actorID, strings.Repeat("8", 64))
	require.NoError(t, err)
	var eventID uuid.UUID
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT id FROM evaluation_route_evidence_terminalization_outbox WHERE run_id=$1 AND control_epoch=$2`, lease.RunID, result.CurrentEpoch).Scan(&eventID))

	count, err := repo.FinalizeRouteEvidenceFromTerminalization(ctx, service.FinalizeRouteEvidenceFromTerminalizationInput{
		EventID: eventID, RunID: lease.RunID, ControlEpoch: result.CurrentEpoch,
	})
	require.NoError(t, err)
	require.Equal(t, 1, count)
	var status string
	var sealedAt, processedAt sql.NullTime
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT e.transport_status, e.sealed_at, o.processed_at
		FROM evaluation_route_evidence e
		JOIN evaluation_route_evidence_terminalization_outbox o ON o.id=$2
		WHERE e.route_trace_id=$1`, lease.RouteTraceID, eventID).Scan(&status, &sealedAt, &processedAt))
	require.Equal(t, "client_cancelled", status)
	require.True(t, sealedAt.Valid)
	require.True(t, processedAt.Valid)
}

func configureTestEvidenceSigningKey(t *testing.T, repo *evaluationRouteEvidenceRepository) uuid.UUID {
	t.Helper()
	keyID := uuid.New()
	keyReference := "test:evidence-key:" + uuid.NewString()
	err := integrationDB.QueryRowContext(context.Background(), `
		SELECT id, key_reference FROM evaluation_evidence_signing_keys WHERE status='active'`).Scan(&keyID, &keyReference)
	if errors.Is(err, sql.ErrNoRows) {
		err = integrationDB.QueryRowContext(context.Background(), `
			INSERT INTO evaluation_evidence_signing_keys (id, key_reference, status, state_epoch)
			VALUES ($1, $2, 'active', 1) RETURNING id`, keyID, keyReference).Scan(&keyID)
	}
	require.NoError(t, err)
	repo.evidenceKeys = service.EvidenceSigningKeyResolverFunc(func(_ context.Context, reference string) ([]byte, error) {
		if reference != keyReference {
			return nil, service.ErrEvidenceSigningKeyUnavailable
		}
		return []byte(strings.Repeat("k", 32)), nil
	})
	return keyID
}

func createOpenRouteEvidenceFixture(t *testing.T) (*evaluationRouteEvidenceRepository, *service.AssignmentLease, service.RequestSemantics) {
	t.Helper()
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	runRepo := NewEvaluationRepository(integrationDB)
	run, err := runRepo.CreateRunWithMatrix(ctx, service.CreateRunInput{
		PlanID: fixture.planID, TriggerSource: "manual", CreatedBy: fixture.userID,
	})
	require.NoError(t, err)
	lease, err := runRepo.ClaimAssignment(ctx, fixture.workerIDs[0], []string{"coding"}, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, lease)
	require.Equal(t, run.ID, lease.RunID)

	var promptSpecJSON []byte
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT c.prompt_spec
		FROM evaluation_side_specs ss
		JOIN evaluation_pair_specs ps ON ps.id = ss.pair_spec_id
		JOIN evaluation_cases c ON c.id = ps.case_id
		WHERE ss.sample_id = $1`, lease.SampleID).Scan(&promptSpecJSON))
	semantics, err := service.DeriveSingleRequestSemantics(promptSpecJSON)
	require.NoError(t, err)
	return &evaluationRouteEvidenceRepository{
		sql: integrationDB, db: integrationDB,
		semanticsVerifiers: service.NewRequestSemanticsVerifierRegistry(),
	}, lease, semantics
}

func createOpenRouteEvidenceInput(lease *service.AssignmentLease, semantics service.RequestSemantics) service.CreateOpenRouteEvidenceInput {
	var modelConfig struct {
		Route string `json:"route"`
	}
	if err := json.Unmarshal(lease.ModelConfig, &modelConfig); err != nil {
		panic(err)
	}
	return service.CreateOpenRouteEvidenceInput{
		RouteTraceID: lease.RouteTraceID, RunID: lease.RunID.String(), SampleID: lease.SampleID.String(),
		APIKeyID: lease.GatewayAPIKeyID, RequestID: "local:request-1", RequestedModel: modelConfig.Route,
		RouteProfileVersion: radarRouteProfileVersion, RequestOrdinal: 0,
		Semantics: semantics, GatewayServiceIdentity: "sub2api-gateway",
		GatewayImageDigest: "sub2api-gateway@sha256:" + strings.Repeat("a", 64),
		Region:             "default", StartedAt: time.Date(2026, 7, 28, 1, 0, 0, 0, time.UTC),
	}
}

func testStringPointer(value string) *string {
	return &value
}

func requireSQLRejectedWithinSavepoint(t *testing.T, tx *sql.Tx, name, query string, args ...any) {
	t.Helper()
	_, err := tx.ExecContext(context.Background(), "SAVEPOINT "+name)
	require.NoError(t, err)
	_, err = tx.ExecContext(context.Background(), query, args...)
	if err == nil {
		_, err = tx.ExecContext(context.Background(), "SET CONSTRAINTS ALL IMMEDIATE")
	}
	require.Error(t, err)
	_, rollbackErr := tx.ExecContext(context.Background(), "ROLLBACK TO SAVEPOINT "+name)
	require.NoError(t, rollbackErr)
}

func assertEvaluationRouteEvidenceRow(
	t *testing.T,
	ctx context.Context,
	traceID string,
	wantTransport service.RouteEvidence,
	wantUsage service.RouteUsageEvidence,
) {
	t.Helper()

	var count int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM evaluation_route_evidence WHERE route_trace_id = $1", traceID).Scan(&count))
	require.Equal(t, 1, count)

	var (
		runID, sampleID, requestID, requestedModel, resolvedModel, routeProfile string
		provider, channelRef, accountPoolRef, region, status, errorCode         string
		finishReason, billedAmount                                              string
		apiKeyID                                                                int64
		attempts, inputTokens, outputTokens, ttft, latency                      int
		fallbackJSON                                                            []byte
		startedAt, finishedAt                                                   time.Time
	)
	err := integrationDB.QueryRowContext(ctx, `
		SELECT evaluation_run_id::text, sample_id::text, api_key_id,
			request_id, requested_model, resolved_model, route_profile_version,
			provider, channel_ref, account_pool_ref, region, attempts, fallback_chain,
			finish_reason, input_tokens, output_tokens, ttft_ms, latency_ms,
			billed_amount::text, transport_status, COALESCE(error_code, ''), started_at, finished_at
		FROM evaluation_route_evidence
		WHERE route_trace_id = $1`, traceID).Scan(
		&runID, &sampleID, &apiKeyID,
		&requestID, &requestedModel, &resolvedModel, &routeProfile,
		&provider, &channelRef, &accountPoolRef, &region, &attempts, &fallbackJSON,
		&finishReason, &inputTokens, &outputTokens, &ttft, &latency,
		&billedAmount, &status, &errorCode, &startedAt, &finishedAt,
	)
	require.NoError(t, err)

	var fallback []service.RouteFallbackEntry
	require.NoError(t, json.Unmarshal(fallbackJSON, &fallback))
	require.Equal(t, wantTransport.EvaluationRunID, runID)
	require.Equal(t, wantTransport.SampleID, sampleID)
	require.Equal(t, wantTransport.APIKeyID, apiKeyID)
	require.Equal(t, wantTransport.RequestID, requestID)
	require.Equal(t, wantTransport.RequestedModel, requestedModel)
	require.Equal(t, wantTransport.ResolvedModel, resolvedModel)
	require.Equal(t, wantTransport.RouteProfileVersion, routeProfile)
	require.Equal(t, wantTransport.Provider, provider)
	require.Equal(t, wantTransport.ChannelRef, channelRef)
	require.Equal(t, wantTransport.AccountPoolRef, accountPoolRef)
	require.Equal(t, wantTransport.Region, region)
	require.Equal(t, wantTransport.Attempts, attempts)
	require.Equal(t, wantTransport.FallbackChain, fallback)
	require.Equal(t, wantUsage.FinishReason, finishReason)
	require.Equal(t, wantUsage.InputTokens, inputTokens)
	require.Equal(t, wantUsage.OutputTokens, outputTokens)
	require.Equal(t, *wantUsage.TTFT, ttft)
	require.Equal(t, *wantUsage.Latency, latency)
	require.Equal(t, wantUsage.BilledAmount.StringFixed(8), billedAmount, fmt.Sprintf("stored amount %s", billedAmount))
	require.Equal(t, wantTransport.TransportStatus, status)
	require.Equal(t, wantTransport.ErrorCode, errorCode)
	require.Equal(t, wantTransport.StartedAt, startedAt)
	require.Equal(t, *wantTransport.FinishedAt, finishedAt)
}
