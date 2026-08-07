package repository

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

const RadarEvidenceSigningKeyReference = "env:RADAR_EVIDENCE_HASH_KEY"

type evaluationRouteEvidenceRepository struct {
	sql                sqlExecutor
	db                 *sql.DB
	semanticsVerifiers *service.RequestSemanticsVerifierRegistry
	evidenceKeys       service.EvidenceSigningKeyResolver
}

func NewEvaluationRouteEvidenceRepository(db *sql.DB) service.EvaluationEvidenceRepository {
	return NewEvaluationRouteEvidenceRepositoryWithVerifiers(db, service.NewRequestSemanticsVerifierRegistry())
}

func NewEvaluationRouteEvidenceRepositoryWithVerifiers(db *sql.DB, registry *service.RequestSemanticsVerifierRegistry) service.EvaluationEvidenceRepository {
	if registry == nil {
		registry = service.NewRequestSemanticsVerifierRegistry()
	}
	return &evaluationRouteEvidenceRepository{
		sql: db, db: db, semanticsVerifiers: registry,
	}
}

func ProvideEvaluationRouteEvidenceRepository(db *sql.DB, cfg *config.Config) service.EvaluationEvidenceRepository {
	base := NewEvaluationRouteEvidenceRepository(db)
	repo, ok := base.(*evaluationRouteEvidenceRepository)
	if !ok {
		return base
	}
	if cfg != nil && len([]byte(strings.TrimSpace(cfg.Radar.HashingSecret))) >= 32 {
		key := []byte(strings.TrimSpace(cfg.Radar.HashingSecret))
		repo.evidenceKeys = service.EvidenceSigningKeyResolverFunc(func(_ context.Context, reference string) ([]byte, error) {
			return resolveConfiguredEvidenceSigningKey(reference, key)
		})
	}
	return repo
}

func resolveConfiguredEvidenceSigningKey(reference string, defaultKey []byte) ([]byte, error) {
	reference = strings.TrimSpace(reference)
	if reference == RadarEvidenceSigningKeyReference {
		return append([]byte(nil), defaultKey...), nil
	}
	const versionedPrefix = "env:RADAR_EVIDENCE_HASH_KEY_"
	if !strings.HasPrefix(reference, versionedPrefix) {
		return nil, service.ErrEvidenceSigningKeyUnavailable
	}
	suffix := strings.TrimPrefix(reference, versionedPrefix)
	if suffix == "" {
		return nil, service.ErrEvidenceSigningKeyUnavailable
	}
	for _, character := range suffix {
		if (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '_' {
			return nil, service.ErrEvidenceSigningKeyUnavailable
		}
	}
	secret, ok := os.LookupEnv(strings.TrimPrefix(reference, "env:"))
	secret = strings.TrimSpace(secret)
	if !ok || len([]byte(secret)) < 32 {
		return nil, service.ErrEvidenceSigningKeyUnavailable
	}
	return []byte(secret), nil
}

type createOpenBinding struct {
	assignmentID uuid.UUID
	leaseEpoch   int64
	manifestID   uuid.UUID
	manifestHash string
	manifest     service.RequestManifest
	slot         service.RequestSlot
	semantics    service.CanonicalRequestSemantics
}

func (r *evaluationRouteEvidenceRepository) CreateOpen(ctx context.Context, input service.CreateOpenRouteEvidenceInput) (service.RouteEvidencePatchState, error) {
	var state service.RouteEvidencePatchState
	var businessErr error
	err := r.withWriter(ctx, func(exec sqlExecutor) error {
		tx, ok := exec.(*sql.Tx)
		if !ok {
			return errors.New("create open route evidence requires a database transaction")
		}
		binding, err := r.lockAndValidateCreateOpen(ctx, tx, input)
		if err != nil {
			return err
		}
		semanticsID, err := insertRequestSemantics(ctx, tx, binding.semantics)
		if err != nil {
			return err
		}
		state = service.RouteEvidencePatchState{
			Identity: service.RouteEvidenceIdentity{
				RouteTraceID: input.RouteTraceID, RunID: input.RunID, SampleID: input.SampleID,
				APIKeyID: input.APIKeyID, AssignmentID: binding.assignmentID.String(), RequestOrdinal: input.RequestOrdinal,
				LeaseEpoch: binding.leaseEpoch,
			},
			Revision:  0,
			Transport: service.TransportPatch{TransportStatus: routeEvidenceStringPointer("started")},
			Billing:   service.BillingPatch{BillingStatus: routeEvidenceStringPointer("incomplete")},
		}
		if err := validateCreateOpenSlot(ctx, r.semanticsVerifiers, binding); err != nil {
			if insertErr := insertOpenRouteEvidence(ctx, tx, input, binding, semanticsID, true); insertErr != nil {
				return insertErr
			}
			keyID, key, keyErr := resolveActiveEvidenceSigningKey(ctx, tx, r.evidenceKeys)
			if keyErr != nil {
				return keyErr
			}
			var sealedAt time.Time
			if timeErr := tx.QueryRowContext(ctx, `SELECT transaction_timestamp()`).Scan(&sealedAt); timeErr != nil {
				return timeErr
			}
			row, loadErr := loadRouteEvidenceForFinalization(ctx, tx, input.RouteTraceID)
			if loadErr != nil {
				return loadErr
			}
			sealed, sealErr := sealRouteEvidenceRow(ctx, tx, input.RouteTraceID, row, sealedAt, sealedAt, keyID, key)
			if sealErr != nil {
				return sealErr
			}
			state.Transport.TransportStatus = routeEvidenceStringPointer("protocol_failed")
			state.Terminal = true
			state.Sealed = true
			state.Revision = sealed.Revision
			businessErr = err
			return nil
		}
		if err := insertOpenRouteEvidence(ctx, tx, input, binding, semanticsID, false); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return service.RouteEvidencePatchState{}, err
	}
	return state, businessErr
}

func (r *evaluationRouteEvidenceRepository) lockAndValidateCreateOpen(ctx context.Context, tx *sql.Tx, input service.CreateOpenRouteEvidenceInput) (createOpenBinding, error) {
	if strings.TrimSpace(input.GatewayServiceIdentity) != "sub2api-gateway" ||
		!validGatewayImageDigest(input.GatewayImageDigest) || strings.TrimSpace(input.RequestID) == "" ||
		strings.TrimSpace(input.RouteTraceID) == "" || strings.TrimSpace(input.RequestedModel) == "" ||
		strings.TrimSpace(input.Region) == "" || input.APIKeyID <= 0 || input.RequestOrdinal < 0 || input.StartedAt.IsZero() {
		return createOpenBinding{}, errors.New("create open route evidence identity is incomplete")
	}
	runID, err := uuid.Parse(strings.TrimSpace(input.RunID))
	if err != nil {
		return createOpenBinding{}, fmt.Errorf("parse route evidence run id: %w", err)
	}
	sampleID, err := uuid.Parse(strings.TrimSpace(input.SampleID))
	if err != nil {
		return createOpenBinding{}, fmt.Errorf("parse route evidence sample id: %w", err)
	}
	canonicalSemantics, err := service.CanonicalizeRequestSemantics(input.Semantics)
	if err != nil {
		return createOpenBinding{}, err
	}

	var (
		runStatus, runRouteProfile, assignmentStatus string
		runEpoch, leaseEpoch                         int64
		assignmentID, manifestID                     uuid.UUID
		manifestHash                                 string
		manifestBytes, sideSpecBytes                 []byte
	)
	err = tx.QueryRowContext(ctx, `
		SELECT r.status, r.control_epoch, r.route_profile_version,
			a.id, a.status, a.lease_epoch,
			ps.request_manifest_id, ps.request_manifest_sha256,
			m.canonical_manifest_bytes, ss.canonical_spec
		FROM evaluation_runs r
		JOIN evaluation_samples s ON s.run_id = r.id AND s.id = $2
		JOIN evaluation_assignments a ON a.sample_id = s.id
		JOIN evaluation_side_specs ss ON ss.sample_id = s.id
		JOIN evaluation_pair_specs ps ON ps.id = ss.pair_spec_id
		JOIN evaluation_request_manifests m
			ON m.id = ps.request_manifest_id AND m.manifest_sha256 = ps.request_manifest_sha256
		JOIN evaluation_plans p ON p.id = r.plan_id AND p.gateway_api_key_id = $4
		WHERE r.id = $1 AND s.route_trace_id = $3
		ORDER BY a.attempt DESC
		LIMIT 1
		FOR UPDATE OF r, a`, runID, sampleID, input.RouteTraceID, input.APIKeyID).Scan(
		&runStatus, &runEpoch, &runRouteProfile,
		&assignmentID, &assignmentStatus, &leaseEpoch,
		&manifestID, &manifestHash, &manifestBytes, &sideSpecBytes,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return createOpenBinding{}, service.ErrLeaseFenced
	}
	if err != nil {
		return createOpenBinding{}, fmt.Errorf("lock route evidence run and assignment: %w", err)
	}
	if runStatus != "running" || (assignmentStatus != "leased" && assignmentStatus != "running") || leaseEpoch != runEpoch {
		return createOpenBinding{}, service.ErrLeaseFenced
	}
	if strings.TrimSpace(runRouteProfile) != strings.TrimSpace(input.RouteProfileVersion) {
		return createOpenBinding{}, service.ErrRouteEvidenceIdentityConflict
	}

	var side service.SideSpec
	if err := json.Unmarshal(sideSpecBytes, &side); err != nil {
		return createOpenBinding{}, fmt.Errorf("decode route evidence side spec: %w", err)
	}
	if side.ExpectedModelAlias != strings.TrimSpace(input.RequestedModel) || side.RouteProfileVersion != strings.TrimSpace(input.RouteProfileVersion) {
		return createOpenBinding{}, service.ErrRouteEvidenceIdentityConflict
	}
	var manifest service.RequestManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return createOpenBinding{}, fmt.Errorf("decode route evidence request manifest: %w", err)
	}
	recomputed, err := service.CanonicalizeRequestManifest(manifest)
	if err != nil {
		return createOpenBinding{}, err
	}
	if recomputed.SHA256 != manifestHash || !bytes.Equal(recomputed.Bytes, manifestBytes) {
		return createOpenBinding{}, errors.New("frozen request manifest digest mismatch")
	}
	var matches []service.RequestSlot
	for _, slot := range manifest.RequestSlots {
		if input.RequestOrdinal >= slot.OrdinalMin && input.RequestOrdinal <= slot.OrdinalMax {
			matches = append(matches, slot)
		}
	}
	if len(matches) != 1 {
		return createOpenBinding{}, service.ErrRequestSemanticsMismatch
	}
	var existingOccurrences int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_route_evidence
		WHERE assignment_id = $1 AND request_slot_id = $2`, assignmentID, matches[0].SlotID).Scan(&existingOccurrences); err != nil {
		return createOpenBinding{}, fmt.Errorf("count route evidence slot occurrences: %w", err)
	}
	if existingOccurrences >= matches[0].MaxOccurrences {
		return createOpenBinding{}, service.ErrRequestSemanticsMismatch
	}
	return createOpenBinding{
		assignmentID: assignmentID, leaseEpoch: leaseEpoch, manifestID: manifestID,
		manifestHash: manifestHash, manifest: manifest, slot: matches[0], semantics: canonicalSemantics,
	}, nil
}

func validGatewayImageDigest(value string) bool {
	const prefix = "sub2api-gateway@sha256:"
	value = strings.TrimSpace(value)
	encoded := strings.TrimPrefix(value, prefix)
	if encoded == value || len(encoded) != 64 || strings.ToLower(encoded) != encoded {
		return false
	}
	decoded, err := hex.DecodeString(encoded)
	return err == nil && len(decoded) == 32
}

func validateCreateOpenSlot(ctx context.Context, registry *service.RequestSemanticsVerifierRegistry, binding createOpenBinding) error {
	semantics := binding.semantics.Semantics
	slot := binding.slot
	if semantics.InteractionType != binding.manifest.InteractionType || semantics.SlotID != slot.SlotID ||
		semantics.RequestOrdinal < slot.OrdinalMin || semantics.RequestOrdinal > slot.OrdinalMax ||
		semantics.Phase != slot.Phase || semantics.ToolSchemaHash != slot.ToolSchemaSHA256 ||
		semantics.ProvidedToolSetHash != slot.AllowedToolSetSHA256 {
		return service.ErrRequestSemanticsMismatch
	}
	switch slot.SemanticsMode {
	case "exact":
		if binding.semantics.SHA256 != slot.ExpectedRequestSemanticsSHA256 {
			return service.ErrRequestSemanticsMismatch
		}
	case "adapter_policy":
		if err := registry.Verify(ctx, slot.RequestSemanticsPolicySHA256, binding.semantics); err != nil {
			return err
		}
	default:
		return service.ErrRequestSemanticsMismatch
	}
	return nil
}

func insertRequestSemantics(ctx context.Context, tx *sql.Tx, semantics service.CanonicalRequestSemantics) (uuid.UUID, error) {
	semanticsID := uuid.New()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_request_semantics (
			id, schema_version, canonical_semantics_bytes, request_semantics_sha256
		) VALUES ($1, $2, $3, $4)
		ON CONFLICT (request_semantics_sha256) DO NOTHING`,
		semanticsID, semantics.Semantics.SchemaVersion, semantics.Bytes, semantics.SHA256); err != nil {
		return uuid.Nil, fmt.Errorf("insert request semantics: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT id FROM evaluation_request_semantics WHERE request_semantics_sha256 = $1`, semantics.SHA256).Scan(&semanticsID); err != nil {
		return uuid.Nil, fmt.Errorf("load request semantics: %w", err)
	}
	return semanticsID, nil
}

func insertOpenRouteEvidence(ctx context.Context, tx *sql.Tx, input service.CreateOpenRouteEvidenceInput, binding createOpenBinding, semanticsID uuid.UUID, protocolFailed bool) error {
	status := "started"
	var incompleteReason any
	if protocolFailed {
		status = "protocol_failed"
		incompleteReason = "request_semantics_mismatch"
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_route_evidence (
			route_trace_id, evaluation_run_id, sample_id, api_key_id, request_id,
			requested_model, route_profile_version, region, transport_status, started_at,
			schema_version, canonicalization_version, assignment_id, request_ordinal,
			lease_epoch, request_manifest_id, request_manifest_sha256, request_slot_id,
			request_semantics_id, request_semantics_sha256, request_semantics_policy_sha256,
			request_tool_schema_sha256, request_allowed_tool_set_sha256,
			evidence_revision, billing_status, gateway_image_digest, terminal_at, incomplete_reason
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9, $10,
			'radar-route-evidence-v1', 'rfc8785-v1', $11, $12,
			$13, $14, $15, $16,
			$17, $18, $19,
			$20, $21,
			0, 'incomplete', $22, $23, $24
		)
		ON CONFLICT DO NOTHING`,
		input.RouteTraceID, input.RunID, input.SampleID, input.APIKeyID, input.RequestID,
		input.RequestedModel, input.RouteProfileVersion, input.Region, status, input.StartedAt,
		binding.assignmentID, input.RequestOrdinal, binding.leaseEpoch,
		binding.manifestID, binding.manifestHash, binding.slot.SlotID,
		semanticsID, binding.semantics.SHA256, binding.slot.RequestSemanticsPolicySHA256,
		binding.slot.ToolSchemaSHA256, binding.slot.AllowedToolSetSHA256,
		input.GatewayImageDigest, nil, incompleteReason,
	)
	if err != nil {
		return fmt.Errorf("insert open route evidence: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return service.ErrRouteEvidenceIdentityConflict
	}
	return nil
}

func (r *evaluationRouteEvidenceRepository) PatchRouteEvidence(ctx context.Context, traceID string, patch service.RouteEvidencePatch) (service.RouteEvidencePatchState, error) {
	var updated service.RouteEvidencePatchState
	err := r.withWriter(ctx, func(exec sqlExecutor) error {
		tx, ok := exec.(*sql.Tx)
		if !ok {
			return errors.New("patch route evidence requires a database transaction")
		}
		current, err := loadRouteEvidencePatchState(ctx, tx, traceID)
		if err != nil {
			return err
		}
		if err := ensureRouteEvidenceExecutionScope(ctx, tx, current.Identity.RunID); err != nil {
			return err
		}
		updated, err = service.MergeRouteEvidencePatch(current, patch)
		if err != nil {
			return err
		}
		if updated.Revision == current.Revision {
			return nil
		}
		return persistRouteEvidencePatchState(ctx, tx, traceID, current.Revision, updated)
	})
	return updated, err
}

func ensureRouteEvidenceExecutionScope(ctx context.Context, exec sqlExecutor, runIDText string) error {
	if _, scoped := radarTenant(ctx); !scoped {
		if _, bound := service.RadarWorkerID(ctx); !bound {
			return nil
		}
	}
	runID, err := uuid.Parse(strings.TrimSpace(runIDText))
	if err != nil || runID == uuid.Nil {
		return service.ErrRadarForbidden
	}
	tx, ok := exec.(*sql.Tx)
	if !ok {
		return errors.New("route evidence execution scope requires a database transaction")
	}
	return ensureRadarExecutionScope(ctx, tx, runID)
}

func loadRouteEvidencePatchState(ctx context.Context, tx *sql.Tx, traceID string) (service.RouteEvidencePatchState, error) {
	var (
		state                                                          service.RouteEvidencePatchState
		resolvedModel, provider, channelRef, accountPoolRef, errorCode sql.NullString
		transportStatus, billingStatus                                 string
		attempts                                                       int
		fallbackJSON                                                   []byte
		finishedAt, terminalAt, sealedAt                               sql.NullTime
		inputTokens, outputTokens, ttft, latency                       sql.NullInt64
		billedAmount, finishReason                                     sql.NullString
	)
	err := tx.QueryRowContext(ctx, `
		SELECT route_trace_id, evaluation_run_id::text, sample_id::text, api_key_id,
			assignment_id::text, request_ordinal, lease_epoch, evidence_revision, terminal_at, sealed_at,
			resolved_model, provider, channel_ref, account_pool_ref, attempts, fallback_chain,
			transport_status, error_code, finished_at,
			input_tokens, output_tokens, ttft_ms, latency_ms, billed_amount::text,
			finish_reason, billing_status
		FROM evaluation_route_evidence
		WHERE route_trace_id = $1 AND assignment_id IS NOT NULL
		FOR UPDATE`, traceID).Scan(
		&state.Identity.RouteTraceID, &state.Identity.RunID, &state.Identity.SampleID, &state.Identity.APIKeyID,
		&state.Identity.AssignmentID, &state.Identity.RequestOrdinal, &state.Identity.LeaseEpoch, &state.Revision, &terminalAt, &sealedAt,
		&resolvedModel, &provider, &channelRef, &accountPoolRef, &attempts, &fallbackJSON,
		&transportStatus, &errorCode, &finishedAt,
		&inputTokens, &outputTokens, &ttft, &latency, &billedAmount,
		&finishReason, &billingStatus,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return state, service.ErrRouteEvidenceNotOpen
	}
	if err != nil {
		return state, fmt.Errorf("load route evidence patch state: %w", err)
	}
	state.Terminal = terminalAt.Valid
	state.Sealed = sealedAt.Valid
	state.Transport.TransportStatus = routeEvidenceStringPointer(transportStatus)
	state.Billing.BillingStatus = routeEvidenceStringPointer(billingStatus)
	assignNullableString(&state.Transport.ResolvedModel, resolvedModel)
	assignNullableString(&state.Transport.Provider, provider)
	assignNullableString(&state.Transport.ChannelRef, channelRef)
	assignNullableString(&state.Transport.AccountPoolRef, accountPoolRef)
	assignNullableString(&state.Transport.ErrorCode, errorCode)
	assignNullableString(&state.Billing.FinishReason, finishReason)
	if attempts > 0 {
		state.Transport.Attempts = &attempts
	}
	var fallback []service.RouteFallbackEntry
	if err := json.Unmarshal(fallbackJSON, &fallback); err != nil {
		return state, fmt.Errorf("decode route evidence fallback chain: %w", err)
	}
	if len(fallback) > 0 {
		state.Transport.FallbackChain = &fallback
	}
	if finishedAt.Valid {
		state.Transport.FinishedAt = &finishedAt.Time
	}
	assignNullableInt(&state.Billing.InputTokens, inputTokens)
	assignNullableInt(&state.Billing.OutputTokens, outputTokens)
	assignNullableInt(&state.Billing.TTFT, ttft)
	assignNullableInt(&state.Billing.Latency, latency)
	if billedAmount.Valid {
		amount, err := decimal.NewFromString(billedAmount.String)
		if err != nil {
			return state, fmt.Errorf("decode route evidence billed amount: %w", err)
		}
		state.Billing.BilledAmount = &amount
	}
	return state, nil
}

func persistRouteEvidencePatchState(ctx context.Context, tx *sql.Tx, traceID string, previousRevision int64, state service.RouteEvidencePatchState) error {
	fallbackJSON := any(nil)
	if state.Transport.FallbackChain != nil {
		encoded, err := json.Marshal(*state.Transport.FallbackChain)
		if err != nil {
			return fmt.Errorf("encode route evidence fallback chain: %w", err)
		}
		fallbackJSON = encoded
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE evaluation_route_evidence SET
			resolved_model=$2, provider=$3, channel_ref=$4, account_pool_ref=$5,
			attempts=COALESCE($6, attempts), fallback_chain=COALESCE($7::jsonb, fallback_chain),
			transport_status=$8, error_code=$9, finished_at=$10,
			input_tokens=$11, output_tokens=$12, ttft_ms=$13, latency_ms=$14,
			billed_amount=$15, finish_reason=$16, billing_status=$17,
			evidence_revision=$18, updated_at=transaction_timestamp()
		WHERE route_trace_id=$1 AND evidence_revision=$19 AND terminal_at IS NULL AND sealed_at IS NULL`,
		traceID,
		state.Transport.ResolvedModel, state.Transport.Provider, state.Transport.ChannelRef, state.Transport.AccountPoolRef,
		state.Transport.Attempts, fallbackJSON, state.Transport.TransportStatus, state.Transport.ErrorCode, state.Transport.FinishedAt,
		state.Billing.InputTokens, state.Billing.OutputTokens, state.Billing.TTFT, state.Billing.Latency,
		state.Billing.BilledAmount, state.Billing.FinishReason, state.Billing.BillingStatus,
		state.Revision, previousRevision,
	)
	if err != nil {
		return fmt.Errorf("patch route evidence: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return &service.RouteEvidenceRevisionConflict{CurrentRevision: previousRevision}
	}
	return nil
}

func assignNullableString(target **string, value sql.NullString) {
	if value.Valid {
		*target = routeEvidenceStringPointer(value.String)
	}
}

func assignNullableInt(target **int, value sql.NullInt64) {
	if value.Valid {
		converted := int(value.Int64)
		*target = &converted
	}
}

func routeEvidenceStringPointer(value string) *string {
	return &value
}

func (r *evaluationRouteEvidenceRepository) UpsertTransport(ctx context.Context, evidence service.RouteEvidence) error {
	fallbackEntries := evidence.FallbackChain
	if fallbackEntries == nil {
		fallbackEntries = []service.RouteFallbackEntry{}
	}
	fallbackChain, err := json.Marshal(fallbackEntries)
	if err != nil {
		return fmt.Errorf("marshal route fallback chain: %w", err)
	}

	write := func(exec sqlExecutor) error {
		if err := ensureRouteEvidenceExecutionScope(ctx, exec, evidence.EvaluationRunID); err != nil {
			return err
		}
		result, err := exec.ExecContext(ctx, `
		INSERT INTO evaluation_route_evidence (
			route_trace_id, evaluation_run_id, sample_id, api_key_id, request_id,
			requested_model, resolved_model, route_profile_version, provider,
			channel_ref, account_pool_ref, region, attempts, fallback_chain,
			transport_status, error_code, started_at, finished_at
		) VALUES (
			$1, $2, $3, $4, NULLIF($5, ''),
			$6, NULLIF($7, ''), $8, NULLIF($9, ''),
			NULLIF($10, ''), NULLIF($11, ''), $12, $13, $14,
			$15, NULLIF($16, ''), $17, $18
		)
		ON CONFLICT (route_trace_id) DO UPDATE SET
			request_id = EXCLUDED.request_id,
			requested_model = EXCLUDED.requested_model,
			resolved_model = EXCLUDED.resolved_model,
			route_profile_version = EXCLUDED.route_profile_version,
			provider = EXCLUDED.provider,
			channel_ref = EXCLUDED.channel_ref,
			account_pool_ref = EXCLUDED.account_pool_ref,
			region = EXCLUDED.region,
			attempts = EXCLUDED.attempts,
			fallback_chain = EXCLUDED.fallback_chain,
			transport_status = EXCLUDED.transport_status,
			error_code = EXCLUDED.error_code,
			started_at = EXCLUDED.started_at,
			finished_at = EXCLUDED.finished_at,
			updated_at = NOW()
		WHERE evaluation_route_evidence.evaluation_run_id = EXCLUDED.evaluation_run_id
			AND evaluation_route_evidence.sample_id = EXCLUDED.sample_id
			AND evaluation_route_evidence.api_key_id = EXCLUDED.api_key_id`,
			evidence.RouteTraceID,
			evidence.EvaluationRunID,
			evidence.SampleID,
			evidence.APIKeyID,
			evidence.RequestID,
			evidence.RequestedModel,
			evidence.ResolvedModel,
			evidence.RouteProfileVersion,
			evidence.Provider,
			evidence.ChannelRef,
			evidence.AccountPoolRef,
			evidence.Region,
			evidence.Attempts,
			fallbackChain,
			evidence.TransportStatus,
			evidence.ErrorCode,
			evidence.StartedAt,
			evidence.FinishedAt,
		)
		if err != nil {
			return fmt.Errorf("upsert route transport evidence: %w", err)
		}
		return r.checkIdentityConflictWith(ctx, exec, result, evidence.RouteTraceID, evidence.EvaluationRunID, evidence.SampleID, evidence.APIKeyID)
	}
	return r.withWriter(ctx, write)
}

func (r *evaluationRouteEvidenceRepository) AttachBilling(ctx context.Context, traceID string, usage service.RouteUsageEvidence) error {
	evaluation, ok := service.EvaluationContextFromContext(ctx)
	if !ok {
		return fmt.Errorf("attach route billing evidence: evaluation context missing")
	}

	write := func(exec sqlExecutor) error {
		if err := ensureRouteEvidenceExecutionScope(ctx, exec, evaluation.RunID); err != nil {
			return err
		}
		result, err := exec.ExecContext(ctx, `
		INSERT INTO evaluation_route_evidence (
			route_trace_id, evaluation_run_id, sample_id, api_key_id,
			requested_model, route_profile_version, region, started_at,
			input_tokens, output_tokens, ttft_ms, latency_ms, billed_amount, finish_reason,
			transport_status
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8,
			$9, $10, $11, $12, $13, NULLIF($14, ''),
			'started'
		)
		ON CONFLICT (route_trace_id) DO UPDATE SET
			input_tokens = COALESCE(EXCLUDED.input_tokens, evaluation_route_evidence.input_tokens),
			output_tokens = COALESCE(EXCLUDED.output_tokens, evaluation_route_evidence.output_tokens),
			ttft_ms = COALESCE(EXCLUDED.ttft_ms, evaluation_route_evidence.ttft_ms),
			latency_ms = COALESCE(EXCLUDED.latency_ms, evaluation_route_evidence.latency_ms),
			billed_amount = COALESCE(EXCLUDED.billed_amount, evaluation_route_evidence.billed_amount),
			finish_reason = COALESCE(EXCLUDED.finish_reason, evaluation_route_evidence.finish_reason),
			updated_at = NOW()
		WHERE evaluation_route_evidence.evaluation_run_id = EXCLUDED.evaluation_run_id
			AND evaluation_route_evidence.sample_id = EXCLUDED.sample_id
			AND evaluation_route_evidence.api_key_id = EXCLUDED.api_key_id`,
			traceID,
			evaluation.RunID,
			evaluation.SampleID,
			evaluation.APIKeyID,
			evaluation.ExpectedModelAlias,
			evaluation.ExpectedRouteProfile,
			"",
			evaluation.IssuedAt,
			usage.InputTokens,
			usage.OutputTokens,
			usage.TTFT,
			usage.Latency,
			usage.BilledAmount,
			usage.FinishReason,
		)
		if err != nil {
			return fmt.Errorf("attach route billing evidence: %w", err)
		}
		return r.checkIdentityConflictWith(ctx, exec, result, traceID, evaluation.RunID, evaluation.SampleID, evaluation.APIKeyID)
	}
	return r.withWriter(ctx, write)
}

func (r *evaluationRouteEvidenceRepository) withWriter(ctx context.Context, fn func(sqlExecutor) error) error {
	if r.db == nil {
		return fn(r.sql)
	}
	identity := defaultEvaluationWriterIdentity("gateway")
	return withEvaluationWriterTx(ctx, r.db, identity, func(tx *sql.Tx) error {
		return fn(tx)
	})
}

func (r *evaluationRouteEvidenceRepository) checkIdentityConflictWith(
	ctx context.Context,
	exec sqlExecutor,
	result sql.Result,
	traceID string,
	runID string,
	sampleID string,
	apiKeyID int64,
) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read route evidence upsert result: %w", err)
	}
	if affected != 0 {
		return nil
	}

	var existingRunID string
	var existingSampleID string
	var existingAPIKeyID int64
	rows, err := exec.QueryContext(ctx, `
		SELECT evaluation_run_id::text, sample_id::text, api_key_id
		FROM evaluation_route_evidence
		WHERE route_trace_id = $1`, traceID)
	if err != nil {
		return fmt.Errorf("%w: load route evidence identity: %w", service.ErrRouteEvidenceIdentityConflict, err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return fmt.Errorf("%w: load route evidence identity: %w", service.ErrRouteEvidenceIdentityConflict, err)
		}
		return fmt.Errorf("%w: route evidence disappeared after guarded upsert: %w", service.ErrRouteEvidenceIdentityConflict, sql.ErrNoRows)
	}
	if err := rows.Scan(&existingRunID, &existingSampleID, &existingAPIKeyID); err != nil {
		return fmt.Errorf("%w: scan route evidence identity: %w", service.ErrRouteEvidenceIdentityConflict, err)
	}
	if existingRunID != runID || existingSampleID != sampleID || existingAPIKeyID != apiKeyID {
		return fmt.Errorf("%w: route_trace_id %q", service.ErrRouteEvidenceIdentityConflict, traceID)
	}
	return fmt.Errorf("%w: guarded upsert affected no rows for matching identity %q", service.ErrRouteEvidenceIdentityConflict, traceID)
}
