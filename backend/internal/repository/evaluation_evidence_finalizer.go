package repository

import (
	"context"
	"crypto/hmac"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type routeEvidenceFinalizationRow struct {
	envelope                               service.RouteEvidenceEnvelope
	fallback                               []service.RouteFallbackEntry
	revision                               int64
	terminalAt                             sql.NullTime
	sealedAt                               sql.NullTime
	payloadHash, signingKeyID, payloadHMAC sql.NullString
}

func (r *evaluationRouteEvidenceRepository) FinalizeRouteEvidence(ctx context.Context, input service.FinalizeRouteEvidenceInput) (service.SealedRouteEvidence, error) {
	var sealed service.SealedRouteEvidence
	err := r.withWriter(ctx, func(exec sqlExecutor) error {
		tx, ok := exec.(*sql.Tx)
		if !ok {
			return errors.New("finalize route evidence requires a database transaction")
		}
		var runID, assignmentID uuid.UUID
		var evidenceLease int64
		var evidenceSealedAt sql.NullTime
		if err := tx.QueryRowContext(ctx, `SELECT evaluation_run_id, assignment_id, lease_epoch, sealed_at FROM evaluation_route_evidence WHERE route_trace_id=$1`, input.RouteTraceID).Scan(&runID, &assignmentID, &evidenceLease, &evidenceSealedAt); err != nil {
			return service.ErrRouteEvidenceNotOpen
		}
		var runStatus string
		var controlEpoch int64
		if err := tx.QueryRowContext(ctx, `SELECT status, control_epoch FROM evaluation_runs WHERE id=$1 FOR UPDATE`, runID).Scan(&runStatus, &controlEpoch); err != nil {
			return service.ErrLeaseFenced
		}
		if evidenceLease != input.LeaseEpoch {
			return service.ErrLeaseFenced
		}
		if evidenceSealedAt.Valid {
			row, err := loadRouteEvidenceForFinalization(ctx, tx, input.RouteTraceID)
			if err != nil {
				return err
			}
			sealed, err = verifySealedRouteEvidenceRow(ctx, tx, row, r.evidenceKeys)
			return err
		}
		if controlEpoch != input.LeaseEpoch {
			return service.ErrLeaseFenced
		}
		if runStatus != "running" && runStatus != "budget_paused" && runStatus != "paused" {
			return service.ErrLeaseFenced
		}
		var assignmentStatus string
		if err := tx.QueryRowContext(ctx, `
			SELECT a.status FROM evaluation_assignments a
			WHERE a.id=$1 AND a.lease_epoch=$2
			  AND NOT EXISTS (SELECT 1 FROM evaluation_assignments newer WHERE newer.sample_id=a.sample_id AND newer.attempt>a.attempt)
			FOR UPDATE`, assignmentID, input.LeaseEpoch).Scan(&assignmentStatus); err != nil {
			return service.ErrLeaseFenced
		}
		if assignmentStatus != "leased" && assignmentStatus != "running" {
			return service.ErrLeaseFenced
		}
		row, err := loadRouteEvidenceForFinalization(ctx, tx, input.RouteTraceID)
		if err != nil {
			return err
		}
		if row.sealedAt.Valid {
			sealed, err = verifySealedRouteEvidenceRow(ctx, tx, row, r.evidenceKeys)
			return err
		}
		if row.revision != input.ExpectedRevision {
			return &service.RouteEvidenceRevisionConflict{CurrentRevision: row.revision}
		}
		if row.envelope.TransportStatus == "started" || (row.envelope.TransportStatus == "succeeded" && row.envelope.BillingStatus != "complete") {
			return service.ErrRouteEvidenceNotOpen
		}
		keyID, key, err := resolveActiveEvidenceSigningKey(ctx, tx, r.evidenceKeys)
		if err != nil {
			return err
		}
		var sealedAt time.Time
		if err := tx.QueryRowContext(ctx, `SELECT transaction_timestamp()`).Scan(&sealedAt); err != nil {
			return err
		}
		sealed, err = sealRouteEvidenceRow(ctx, tx, input.RouteTraceID, row, sealedAt, sealedAt, keyID, key)
		return err
	})
	return sealed, err
}

func (r *evaluationRouteEvidenceRepository) FinalizeRouteEvidenceFromTerminalization(ctx context.Context, input service.FinalizeRouteEvidenceFromTerminalizationInput) (int, error) {
	if input.EventID == uuid.Nil || input.RunID == uuid.Nil || input.ControlEpoch < 1 || r == nil || r.db == nil {
		return 0, service.ErrLeaseFenced
	}
	sealedCount := 0
	err := withEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("system"), func(tx *sql.Tx) error {
		var runStatus string
		var runEpoch int64
		if err := tx.QueryRowContext(ctx, `SELECT status, control_epoch FROM evaluation_runs WHERE id=$1 FOR UPDATE`, input.RunID).Scan(&runStatus, &runEpoch); err != nil || runEpoch != input.ControlEpoch {
			return service.ErrLeaseFenced
		}
		var terminalStatus string
		var eventEpoch int64
		var processedAt sql.NullTime
		if err := tx.QueryRowContext(ctx, `SELECT terminal_status, control_epoch, processed_at FROM evaluation_route_evidence_terminalization_outbox WHERE id=$1 AND run_id=$2 FOR UPDATE`, input.EventID, input.RunID).Scan(&terminalStatus, &eventEpoch, &processedAt); err != nil || eventEpoch != input.ControlEpoch {
			return service.ErrLeaseFenced
		}
		if processedAt.Valid {
			return nil
		}
		if (terminalStatus == "cancelled" && runStatus != "cancelled") || (terminalStatus == "failed" && runStatus != "failed") {
			return service.ErrLeaseFenced
		}
		assignmentRows, err := tx.QueryContext(ctx, `SELECT a.id FROM evaluation_assignments a JOIN evaluation_samples s ON s.id=a.sample_id WHERE s.run_id=$1 ORDER BY a.id FOR UPDATE OF a`, input.RunID)
		if err != nil {
			return err
		}
		for assignmentRows.Next() {
			var ignored uuid.UUID
			if err := assignmentRows.Scan(&ignored); err != nil {
				_ = assignmentRows.Close()
				return err
			}
		}
		if err := assignmentRows.Close(); err != nil {
			return err
		}
		traceRows, err := tx.QueryContext(ctx, `SELECT route_trace_id FROM evaluation_route_evidence WHERE evaluation_run_id=$1 AND assignment_id IS NOT NULL AND sealed_at IS NULL ORDER BY assignment_id, request_ordinal FOR UPDATE`, input.RunID)
		if err != nil {
			return err
		}
		var traceIDs []string
		for traceRows.Next() {
			var traceID string
			if err := traceRows.Scan(&traceID); err != nil {
				_ = traceRows.Close()
				return err
			}
			traceIDs = append(traceIDs, traceID)
		}
		if err := traceRows.Close(); err != nil {
			return err
		}
		keyID, key, err := resolveActiveEvidenceSigningKey(ctx, tx, r.evidenceKeys)
		if err != nil {
			return err
		}
		var transactionTime time.Time
		if err := tx.QueryRowContext(ctx, `SELECT transaction_timestamp()`).Scan(&transactionTime); err != nil {
			return err
		}
		transportStatus, outcome, reason := "gateway_failed", "gateway_failed", "run_failed"
		if terminalStatus == "cancelled" {
			transportStatus, outcome, reason = "client_cancelled", "cancelled", "run_cancelled"
		}
		for _, traceID := range traceIDs {
			row, err := loadRouteEvidenceForFinalization(ctx, tx, traceID)
			if err != nil {
				return err
			}
			if last := len(row.fallback) - 1; last >= 0 {
				finished := transactionTime
				if finished.Before(row.fallback[last].StartedAt) {
					finished = row.fallback[last].StartedAt
				}
				row.fallback[last].FinishedAt = &finished
				row.fallback[last].Outcome = outcome
			}
			fallbackJSON, err := json.Marshal(row.fallback)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE evaluation_route_evidence SET transport_status=$2, fallback_chain=$3::jsonb, finished_at=COALESCE(finished_at,$4), incomplete_reason=COALESCE(incomplete_reason,$5), updated_at=$4 WHERE route_trace_id=$1 AND sealed_at IS NULL`, traceID, transportStatus, fallbackJSON, transactionTime, reason); err != nil {
				return err
			}
			row.envelope.TransportStatus = transportStatus
			row.envelope.IncompleteReason = &reason
			if row.envelope.FinishedAt == nil {
				value := service.FormatRouteEvidenceTime(transactionTime)
				row.envelope.FinishedAt = &value
			}
			row.fallback = append([]service.RouteFallbackEntry(nil), row.fallback...)
			if _, err := sealRouteEvidenceRow(ctx, tx, traceID, row, transactionTime, transactionTime, keyID, key); err != nil {
				return err
			}
			sealedCount++
		}
		result, err := tx.ExecContext(ctx, `UPDATE evaluation_route_evidence_terminalization_outbox SET processed_at=transaction_timestamp() WHERE id=$1 AND processed_at IS NULL`, input.EventID)
		if err != nil {
			return err
		}
		affected, _ := result.RowsAffected()
		if affected != 1 {
			return service.ErrLeaseFenced
		}
		return nil
	})
	return sealedCount, err
}

func resolveActiveEvidenceSigningKey(ctx context.Context, tx *sql.Tx, resolver service.EvidenceSigningKeyResolver) (uuid.UUID, []byte, error) {
	if resolver == nil {
		return uuid.Nil, nil, service.ErrEvidenceSigningKeyUnavailable
	}
	var keyID uuid.UUID
	var keyReference string
	if err := tx.QueryRowContext(ctx, `SELECT id, key_reference FROM evaluation_evidence_signing_keys WHERE status='active' FOR SHARE`).Scan(&keyID, &keyReference); err != nil {
		return uuid.Nil, nil, service.ErrEvidenceSigningKeyUnavailable
	}
	key, err := resolver.ResolveEvidenceSigningKey(ctx, keyReference)
	if err != nil || len(key) < 32 {
		return uuid.Nil, nil, service.ErrEvidenceSigningKeyUnavailable
	}
	return keyID, key, nil
}

func sealRouteEvidenceRow(ctx context.Context, tx *sql.Tx, traceID string, row routeEvidenceFinalizationRow, terminalAt, sealedAt time.Time, keyID uuid.UUID, key []byte) (service.SealedRouteEvidence, error) {
	canonical, err := canonicalizeRouteEvidenceRow(row, row.revision+1, terminalAt, sealedAt, keyID)
	if err != nil {
		return service.SealedRouteEvidence{}, err
	}
	payloadHMAC := service.SignEvidence(row.envelope.SchemaVersion, canonical.Bytes, key)
	result, err := tx.ExecContext(ctx, `UPDATE evaluation_route_evidence SET evidence_revision=$2, terminal_at=$3, sealed_at=$4, payload_hash=$5, signing_key_id=$6, payload_hmac=$7, updated_at=$4 WHERE route_trace_id=$1 AND evidence_revision=$8 AND sealed_at IS NULL`, traceID, row.revision+1, terminalAt, sealedAt, canonical.SHA256, keyID, payloadHMAC, row.revision)
	if err != nil {
		return service.SealedRouteEvidence{}, fmt.Errorf("seal route evidence: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return service.SealedRouteEvidence{}, &service.RouteEvidenceRevisionConflict{CurrentRevision: row.revision}
	}
	runID, err := uuid.Parse(row.envelope.EvaluationRunID)
	if err != nil {
		return service.SealedRouteEvidence{}, service.ErrEvaluationOutboxInvalid
	}
	payload, err := json.Marshal(struct {
		RouteTraceID  string `json:"route_trace_id"`
		SchemaVersion string `json:"schema_version"`
		Revision      int64  `json:"evidence_revision"`
	}{traceID, row.envelope.SchemaVersion, row.revision + 1})
	if err != nil {
		return service.SealedRouteEvidence{}, fmt.Errorf("marshal sealed evidence outbox payload: %w", err)
	}
	if _, err := enqueueEvaluationOutbox(ctx, tx, service.EnqueueEvaluationOutboxInput{
		EventType: "route_evidence_sealed", RunID: runID,
		ScopeKey:        row.envelope.AssignmentID + "/" + fmt.Sprint(row.envelope.RequestOrdinal),
		AnalysisVersion: row.envelope.SchemaVersion, SourceType: "route_evidence",
		SourceID: traceID, SourceHash: canonical.SHA256, Payload: payload,
	}); err != nil {
		return service.SealedRouteEvidence{}, fmt.Errorf("enqueue sealed route evidence: %w", err)
	}
	return service.SealedRouteEvidence{Revision: row.revision + 1, PayloadHash: canonical.SHA256, SigningKeyID: keyID.String(), PayloadHMAC: payloadHMAC, SealedAt: sealedAt}, nil
}

func canonicalizeRouteEvidenceRow(row routeEvidenceFinalizationRow, revision int64, terminalAt, sealedAt time.Time, keyID uuid.UUID) (service.CanonicalRouteEvidenceEnvelope, error) {
	row.envelope.EvidenceRevision = revision
	row.envelope.TerminalAt = service.FormatRouteEvidenceTime(terminalAt)
	row.envelope.SealedAt = service.FormatRouteEvidenceTime(sealedAt)
	row.envelope.SigningKeyID = keyID.String()
	row.envelope.FallbackChain = make([]service.RouteEvidenceAttemptEnvelope, len(row.fallback))
	for index, attempt := range row.fallback {
		if attempt.FinishedAt == nil {
			return service.CanonicalRouteEvidenceEnvelope{}, service.ErrRouteEvidenceFallbackInvalid
		}
		row.envelope.FallbackChain[index] = service.RouteEvidenceAttemptEnvelope{
			AttemptIndex: attempt.Ordinal, ParentAttemptIndex: attempt.ParentAttemptIndex, DispatchMode: attempt.DispatchMode,
			RouteRuleHash: attempt.RouteRuleHash, RequestedModel: attempt.RequestedModel, ResolvedModel: attempt.ResolvedModel,
			Provider: attempt.Provider, ChannelRef: attempt.ChannelRef, AccountPoolRef: attempt.AccountPoolRef, Region: attempt.Region,
			Outcome: attempt.Outcome, ErrorCode: nullableEnvelopeString(attempt.ErrorCode),
			StartedAt: service.FormatRouteEvidenceTime(attempt.StartedAt), FinishedAt: service.FormatRouteEvidenceTime(*attempt.FinishedAt),
		}
	}
	return service.CanonicalizeRouteEvidenceEnvelope(row.envelope)
}

func verifySealedRouteEvidenceRow(ctx context.Context, tx *sql.Tx, row routeEvidenceFinalizationRow, resolver service.EvidenceSigningKeyResolver) (service.SealedRouteEvidence, error) {
	if !row.terminalAt.Valid || !row.sealedAt.Valid || !row.payloadHash.Valid || !row.signingKeyID.Valid || !row.payloadHMAC.Valid || resolver == nil {
		return service.SealedRouteEvidence{}, service.ErrRouteEvidenceSealedConflict
	}
	keyID, err := uuid.Parse(row.signingKeyID.String)
	if err != nil {
		return service.SealedRouteEvidence{}, service.ErrRouteEvidenceSealedConflict
	}
	var keyReference, status string
	if err := tx.QueryRowContext(ctx, `SELECT key_reference, status FROM evaluation_evidence_signing_keys WHERE id=$1 FOR SHARE`, keyID).Scan(&keyReference, &status); err != nil {
		return service.SealedRouteEvidence{}, service.ErrEvidenceSigningKeyUnavailable
	}
	if status == "revoked" {
		return service.SealedRouteEvidence{}, service.ErrEvidenceSigningKeyRevoked
	}
	if status != "active" && status != "verify_only" {
		return service.SealedRouteEvidence{}, service.ErrEvidenceSigningKeyUnavailable
	}
	key, err := resolver.ResolveEvidenceSigningKey(ctx, keyReference)
	if err != nil || len(key) < 32 {
		return service.SealedRouteEvidence{}, service.ErrEvidenceSigningKeyUnavailable
	}
	canonical, err := canonicalizeRouteEvidenceRow(row, row.revision, row.terminalAt.Time, row.sealedAt.Time, keyID)
	if err != nil {
		return service.SealedRouteEvidence{}, err
	}
	wantHMAC := service.SignEvidence(row.envelope.SchemaVersion, canonical.Bytes, key)
	if canonical.SHA256 != row.payloadHash.String || !hmac.Equal([]byte(wantHMAC), []byte(row.payloadHMAC.String)) {
		return service.SealedRouteEvidence{}, service.ErrRouteEvidenceSealedConflict
	}
	return service.SealedRouteEvidence{Revision: row.revision, PayloadHash: row.payloadHash.String, SigningKeyID: keyID.String(), PayloadHMAC: row.payloadHMAC.String, SealedAt: row.sealedAt.Time}, nil
}

func loadRouteEvidenceForFinalization(ctx context.Context, tx *sql.Tx, traceID string) (routeEvidenceFinalizationRow, error) {
	var row routeEvidenceFinalizationRow
	var resolved, provider, channel, account, errorCode, finishReason, amount, incomplete sql.NullString
	var semanticsPolicy sql.NullString
	var inputTokens, outputTokens, ttft, latency sql.NullInt64
	var finished sql.NullTime
	var started time.Time
	var fallbackJSON []byte
	err := tx.QueryRowContext(ctx, `
		SELECT schema_version, canonicalization_version, route_trace_id, evaluation_run_id::text, sample_id::text,
			assignment_id::text, request_ordinal, lease_epoch, request_manifest_id::text, request_manifest_sha256,
			request_slot_id, request_semantics_id::text, request_semantics_sha256, request_semantics_policy_sha256,
			request_tool_schema_sha256, request_allowed_tool_set_sha256, evidence_revision, api_key_id, request_id,
			requested_model, resolved_model, route_profile_version, gateway_image_digest, provider, channel_ref,
			account_pool_ref, region, attempts, fallback_chain, transport_status, error_code, finish_reason,
			input_tokens, output_tokens, ttft_ms, latency_ms, billing_status, billed_amount::text, incomplete_reason,
			started_at, finished_at, terminal_at, sealed_at, payload_hash, signing_key_id::text, payload_hmac
		FROM evaluation_route_evidence WHERE route_trace_id=$1 FOR UPDATE`, traceID).Scan(
		&row.envelope.SchemaVersion, &row.envelope.CanonicalizationVersion, &row.envelope.RouteTraceID, &row.envelope.EvaluationRunID, &row.envelope.SampleID,
		&row.envelope.AssignmentID, &row.envelope.RequestOrdinal, &row.envelope.LeaseEpoch, &row.envelope.RequestManifestID, &row.envelope.RequestManifestSHA256,
		&row.envelope.RequestSlotID, &row.envelope.RequestSemanticsID, &row.envelope.RequestSemanticsSHA256, &semanticsPolicy,
		&row.envelope.RequestToolSchemaSHA256, &row.envelope.RequestAllowedToolSetSHA256, &row.revision, &row.envelope.APIKeyID, &row.envelope.RequestID,
		&row.envelope.RequestedModel, &resolved, &row.envelope.RouteProfileVersion, &row.envelope.GatewayImageDigest, &provider, &channel,
		&account, &row.envelope.Region, &row.envelope.Attempts, &fallbackJSON, &row.envelope.TransportStatus, &errorCode, &finishReason,
		&inputTokens, &outputTokens, &ttft, &latency, &row.envelope.BillingStatus, &amount, &incomplete,
		&started, &finished, &row.terminalAt, &row.sealedAt, &row.payloadHash, &row.signingKeyID, &row.payloadHMAC)
	if err != nil {
		return row, err
	}
	row.envelope.StartedAt = service.FormatRouteEvidenceTime(started)
	row.envelope.ResolvedModel = nullableSQLString(resolved)
	row.envelope.Provider = nullableSQLString(provider)
	row.envelope.ChannelRef = nullableSQLString(channel)
	row.envelope.AccountPoolRef = nullableSQLString(account)
	row.envelope.ErrorCode = nullableSQLString(errorCode)
	row.envelope.RequestSemanticsPolicySHA256 = nullableSQLString(semanticsPolicy)
	row.envelope.FinishReason = nullableSQLString(finishReason)
	row.envelope.IncompleteReason = nullableSQLString(incomplete)
	row.envelope.InputTokens = nullableSQLInt(inputTokens)
	row.envelope.OutputTokens = nullableSQLInt(outputTokens)
	row.envelope.TTFTMS = nullableSQLInt(ttft)
	row.envelope.LatencyMS = nullableSQLInt(latency)
	if amount.Valid {
		parsed, err := decimal.NewFromString(amount.String)
		if err != nil {
			return row, err
		}
		value := parsed.StringFixed(8)
		row.envelope.BilledAmount = &value
	}
	if finished.Valid {
		value := service.FormatRouteEvidenceTime(finished.Time)
		row.envelope.FinishedAt = &value
	}
	if err := json.Unmarshal(fallbackJSON, &row.fallback); err != nil {
		return row, err
	}
	return row, nil
}

func nullableSQLString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	v := strings.TrimSpace(value.String)
	if v == "" {
		return nil
	}
	return &v
}
func nullableSQLInt(value sql.NullInt64) *int {
	if !value.Valid {
		return nil
	}
	v := int(value.Int64)
	return &v
}
func nullableEnvelopeString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}
