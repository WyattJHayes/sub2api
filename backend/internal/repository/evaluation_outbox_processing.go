package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

type evaluationOutboxDomainRepository struct {
	db *sql.DB
}

func NewEvaluationOutboxDomainRepository(db *sql.DB) service.EvaluationOutboxDomainRepository {
	return &evaluationOutboxDomainRepository{db: db}
}

func (r *evaluationOutboxDomainRepository) ValidateSealedRouteEvidence(ctx context.Context, event service.EvaluationOutboxEvent) error {
	if r == nil || r.db == nil {
		return service.ErrEvaluationOutboxInvalid
	}
	var payload struct {
		RouteTraceID     string `json:"route_trace_id"`
		SchemaVersion    string `json:"schema_version"`
		EvidenceRevision int64  `json:"evidence_revision"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return service.ErrEvaluationOutboxInvalid
	}
	var runID uuid.UUID
	var schemaVersion, payloadHash string
	var evidenceRevision int64
	var sealedAt sql.NullTime
	err := r.db.QueryRowContext(ctx, `
		SELECT evaluation_run_id, schema_version, evidence_revision, payload_hash, sealed_at
		FROM evaluation_route_evidence WHERE route_trace_id=$1`, event.SourceID).Scan(
		&runID, &schemaVersion, &evidenceRevision, &payloadHash, &sealedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrRouteEvidenceSealedConflict
	}
	if err != nil {
		return fmt.Errorf("load sealed route evidence for outbox: %w", err)
	}
	if payload.RouteTraceID != event.SourceID || runID != event.RunID || !sealedAt.Valid ||
		payload.SchemaVersion != schemaVersion || payload.EvidenceRevision != evidenceRevision ||
		payloadHash != event.SourceHash {
		return service.ErrRouteEvidenceSealedConflict
	}
	return nil
}

func (r *evaluationOutboxDomainRepository) EnsureCellAnalysisJob(ctx context.Context, request service.CellAnalysisJobRequest) (*service.AnalysisJobRevision, error) {
	if r == nil {
		return nil, service.ErrAggregateRevisionInvalid
	}
	return (&evaluationAggregateRepository{db: r.db}).EnsureCellAnalysisJob(ctx, request)
}

func (r *evaluationOutboxDomainRepository) EnsureGlobalAnalysisJob(ctx context.Context, request service.GlobalAnalysisJobRequest) (*service.AnalysisJobRevision, error) {
	if r == nil {
		return nil, service.ErrAggregateRevisionInvalid
	}
	return (&evaluationAggregateRepository{db: r.db}).EnsureGlobalAnalysisJob(ctx, request)
}

func (r *evaluationOutboxDomainRepository) ResolveRadarGateTarget(ctx context.Context, runID uuid.UUID) (*service.RadarGateTarget, error) {
	if r == nil || r.db == nil || runID == uuid.Nil {
		return nil, service.ErrEvaluationOutboxInvalid
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin resolve Radar Gate target: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `
		SELECT subject.id, head.policy_id, run.tenant_id, subject.tenant_id, policy.tenant_id
		FROM evaluation_runs run
		JOIN evaluation_release_subjects subject ON subject.run_id=run.id
		JOIN LATERAL (
			SELECT event_type, effective_at, expires_at
			FROM evaluation_release_subject_events event
			WHERE event.release_subject_id=subject.id
			ORDER BY event.sequence DESC LIMIT 1
		) current_event ON current_event.event_type='activated'
			AND current_event.effective_at <= transaction_timestamp()
			AND current_event.expires_at > transaction_timestamp()
		LEFT JOIN evaluation_gate_policy_heads head
		  ON head.tenant_id=subject.tenant_id
		 AND head.environment=subject.canonical_subject->>'deployment_environment'
		 AND head.scope_type=subject.canonical_subject->>'scope_type'
		 AND head.scope_id=subject.canonical_subject->>'scope_id'
		LEFT JOIN evaluation_gate_policies policy ON policy.id=head.policy_id
		WHERE run.id=$1
		ORDER BY subject.created_at DESC`, runID)
	if err != nil {
		return nil, fmt.Errorf("load Radar Gate target: %w", err)
	}
	defer rows.Close()
	type candidate struct {
		subjectID                uuid.UUID
		policyID                 uuid.NullUUID
		runTenant, subjectTenant int64
		policyTenant             sql.NullInt64
	}
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.subjectID, &item.policyID, &item.runTenant, &item.subjectTenant, &item.policyTenant); err != nil {
			return nil, fmt.Errorf("scan Radar Gate target: %w", err)
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Radar Gate targets: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close Radar Gate targets: %w", err)
	}
	if len(candidates) > 1 {
		return nil, service.ErrGovernanceHeadConflict
	}
	if len(candidates) == 0 {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit empty Radar Gate target: %w", err)
		}
		return nil, nil
	}
	item := candidates[0]
	if item.runTenant <= 0 || item.subjectTenant != item.runTenant {
		return nil, service.ErrGovernanceHeadConflict
	}
	if !item.policyID.Valid {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit Radar Gate target without policy: %w", err)
		}
		return nil, nil
	}
	if !item.policyTenant.Valid || item.policyTenant.Int64 != item.runTenant {
		return nil, service.ErrGovernanceHeadConflict
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit Radar Gate target: %w", err)
	}
	return &service.RadarGateTarget{
		ReleaseSubjectID: item.subjectID,
		PolicyID:         item.policyID.UUID,
		TenantID:         item.runTenant,
	}, nil
}

func (r *evaluationOutboxDomainRepository) EvaluateAndProjectRadarGate(ctx context.Context, outcome service.AutomatedRadarGateOutcome) (*service.RadarGateDecisionRecord, error) {
	if r == nil || r.db == nil || outcome.EventID == uuid.Nil || outcome.EventRunID == uuid.Nil ||
		outcome.Target.ReleaseSubjectID == uuid.Nil || outcome.Target.PolicyID == uuid.Nil || outcome.Target.TenantID <= 0 ||
		!validLowerHexSHA256(outcome.CauseSetHash) {
		return nil, service.ErrEvaluationOutboxInvalid
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, fmt.Errorf("begin automated Radar Gate outcome: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var eventRunID uuid.UUID
	var eventCauseSetHash string
	if err := tx.QueryRowContext(ctx, `
		SELECT run_id, cause_set_hash FROM evaluation_outbox_events
		WHERE id=$1`, outcome.EventID).Scan(&eventRunID, &eventCauseSetHash); err != nil ||
		eventRunID != outcome.EventRunID || eventCauseSetHash != outcome.CauseSetHash {
		return nil, service.ErrGovernanceHeadConflict
	}
	if replayed, err := loadAutomatedRadarGateReplay(ctx, tx, outcome); err != nil {
		return nil, err
	} else if replayed != nil {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit replayed automated Radar Gate outcome: %w", err)
		}
		return replayed, nil
	}

	target, err := loadAutomatedRadarGateTarget(ctx, tx, outcome)
	if err != nil {
		return nil, err
	}
	loaded, err := loadRadarGateReliabilityContext(ctx, tx, outcome.EventRunID, outcome.Target.PolicyID)
	if err != nil {
		return nil, err
	}
	if loaded.ReleaseSubjectHash != target.releaseSubjectHash {
		return nil, service.ErrGovernanceHeadConflict
	}
	if err := validateCurrentGateAuthority(ctx, tx, outcome.EventRunID, outcome.Target.PolicyID, target.releaseSubjectHash); err != nil {
		return nil, err
	}
	if err := validateGateReliabilityWatermark(ctx, tx, outcome.EventRunID, outcome.Target.PolicyID, loaded.SourceWatermark); err != nil {
		return nil, err
	}

	gateDecision := service.EvaluateRadarGate(loaded.Policy, loaded.Input)
	evidence, evidenceHash, err := service.BuildRadarGateEvidenceEnvelope(
		outcome.EventRunID, outcome.Target.PolicyID, loaded.PolicyHash, loaded.ObservedAt,
		loaded.Input, loaded.Evidence,
	)
	if err != nil {
		return nil, fmt.Errorf("build automated Radar Gate evidence: %w", err)
	}
	decision, err := recordRadarGateDecisionTx(ctx, tx, service.RadarGateDecisionInput{
		RunID: outcome.EventRunID, BaselineID: &target.subject.BaselineID, PolicyID: outcome.Target.PolicyID,
		Status: gateDecision.Status, RuleIDs: []string{gateDecision.RuleID}, Evidence: evidence,
		EvidenceHash: evidenceHash, ReleaseSubjectHash: target.releaseSubjectHash,
		SourceWatermark: loaded.SourceWatermark, SupersedesDecisionID: loaded.SupersedesDecisionID,
		CauseSetHash: outcome.CauseSetHash,
	}, target.tenantID)
	if err != nil {
		return nil, err
	}
	watermarkHash, err := service.DigestCanonicalJSON(decision.SourceWatermark)
	if err != nil {
		return nil, fmt.Errorf("hash automated Radar Gate watermark: %w", err)
	}
	projectionStatus := "blocked"
	if decision.Status == service.RadarGatePassed || decision.Status == service.RadarGateRecorded {
		projectionStatus = "pending"
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_release_projections (
			release_subject_id,run_id,release_subject_hash,decision_id,authorization_id,
			status,source_watermark,cause_set_hash,last_outbox_event_id
		) VALUES ($1,$2,$3,$4,NULL,$5,$6,$7,$8)
		ON CONFLICT (release_subject_id) DO UPDATE SET
			run_id=EXCLUDED.run_id, release_subject_hash=EXCLUDED.release_subject_hash,
			decision_id=EXCLUDED.decision_id, authorization_id=NULL, status=EXCLUDED.status,
			source_watermark=EXCLUDED.source_watermark, cause_set_hash=EXCLUDED.cause_set_hash,
			last_outbox_event_id=EXCLUDED.last_outbox_event_id, updated_at=transaction_timestamp()`,
		outcome.Target.ReleaseSubjectID, outcome.EventRunID, target.releaseSubjectHash, decision.ID,
		projectionStatus, watermarkHash, outcome.CauseSetHash, outcome.EventID); err != nil {
		return nil, fmt.Errorf("project automated Radar Gate release: %w", err)
	}
	if err := projectAutomatedRadarGateAlert(ctx, tx, outcome, target, loaded.Input, decision); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit automated Radar Gate outcome: %w", err)
	}
	return decision, nil
}

type automatedRadarGateTarget struct {
	tenantID           int64
	releaseSubjectHash string
	modelRoute         string
	policyVersion      int
	subject            service.ReleaseSubject
}

func loadAutomatedRadarGateTarget(ctx context.Context, tx *sql.Tx, outcome service.AutomatedRadarGateOutcome) (automatedRadarGateTarget, error) {
	var target automatedRadarGateTarget
	var runTenantID, subjectTenantID, policyTenantID int64
	var subjectJSON []byte
	err := tx.QueryRowContext(ctx, `
		SELECT run.tenant_id, subject.tenant_id, policy.tenant_id,
		       subject.subject_hash, subject.canonical_subject, baseline.model_route, policy.version
		FROM evaluation_runs run
		JOIN evaluation_release_subjects subject
		  ON subject.id=$2 AND subject.run_id=run.id
		JOIN LATERAL (
			SELECT event_type,effective_at,expires_at
			FROM evaluation_release_subject_events event
			WHERE event.release_subject_id=subject.id
			ORDER BY event.sequence DESC LIMIT 1
		) current_event ON current_event.event_type='activated'
			AND current_event.effective_at <= transaction_timestamp()
			AND current_event.expires_at > transaction_timestamp()
		JOIN evaluation_gate_policy_heads head
		  ON head.tenant_id=subject.tenant_id
		 AND head.environment=subject.canonical_subject->>'deployment_environment'
		 AND head.scope_type=subject.canonical_subject->>'scope_type'
		 AND head.scope_id=subject.canonical_subject->>'scope_id'
		 AND head.policy_id=$3
		JOIN evaluation_gate_policies policy ON policy.id=head.policy_id AND policy.retired_at IS NULL
		JOIN evaluation_baselines baseline
		  ON baseline.id=(subject.canonical_subject->>'baseline_id')::uuid
		 AND baseline.tenant_id=subject.tenant_id
		WHERE run.id=$1
		FOR SHARE OF run,subject,policy,baseline`, outcome.EventRunID, outcome.Target.ReleaseSubjectID, outcome.Target.PolicyID).Scan(
		&runTenantID, &subjectTenantID, &policyTenantID, &target.releaseSubjectHash,
		&subjectJSON, &target.modelRoute, &target.policyVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return automatedRadarGateTarget{}, service.ErrGovernanceHeadConflict
	}
	if err != nil {
		return automatedRadarGateTarget{}, fmt.Errorf("load automated Radar Gate target: %w", err)
	}
	if runTenantID <= 0 || runTenantID != outcome.Target.TenantID || subjectTenantID != runTenantID || policyTenantID != runTenantID {
		return automatedRadarGateTarget{}, service.ErrGovernanceHeadConflict
	}
	if err := json.Unmarshal(subjectJSON, &target.subject); err != nil || target.subject.BaselineID == uuid.Nil {
		return automatedRadarGateTarget{}, service.ErrGovernanceHeadConflict
	}
	target.tenantID = runTenantID
	return target, nil
}

func loadAutomatedRadarGateReplay(ctx context.Context, tx *sql.Tx, outcome service.AutomatedRadarGateOutcome) (*service.RadarGateDecisionRecord, error) {
	var releaseSubjectID uuid.UUID
	var tenantID int64
	var decision service.RadarGateDecisionRecord
	err := tx.QueryRowContext(ctx, `
		SELECT projection.release_subject_id, decision.tenant_id,
		       decision.id,decision.run_id,decision.baseline_id,decision.policy_id,decision.status,
		       decision.rule_ids,decision.evidence,decision.evidence_hash,decision.release_subject_hash,
		       decision.source_watermark,decision.supersedes_decision_id,decision.cause_set_hash,decision.created_at
		FROM evaluation_release_projections projection
		JOIN evaluation_gate_decisions decision ON decision.id=projection.decision_id
		WHERE projection.last_outbox_event_id=$1`, outcome.EventID).Scan(
		&releaseSubjectID, &tenantID, &decision.ID, &decision.RunID, &decision.BaselineID,
		&decision.PolicyID, &decision.Status, &pqStringArray{&decision.RuleIDs}, &decision.Evidence,
		&decision.EvidenceHash, &decision.ReleaseSubjectHash, &decision.SourceWatermark,
		&decision.SupersedesDecisionID, &decision.CauseSetHash, &decision.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load replayed automated Radar Gate outcome: %w", err)
	}
	if releaseSubjectID != outcome.Target.ReleaseSubjectID || tenantID != outcome.Target.TenantID ||
		decision.RunID != outcome.EventRunID || decision.PolicyID != outcome.Target.PolicyID ||
		decision.CauseSetHash != outcome.CauseSetHash {
		return nil, service.ErrGovernanceHeadConflict
	}
	return &decision, nil
}

type automatedRadarGateAlertPayload struct {
	Source             string    `json:"source"`
	RunID              uuid.UUID `json:"run_id"`
	DecisionID         uuid.UUID `json:"decision_id"`
	PolicyID           uuid.UUID `json:"policy_id"`
	RuleID             string    `json:"rule_id"`
	EvidenceHash       string    `json:"evidence_hash"`
	OutboxEventID      uuid.UUID `json:"outbox_event_id"`
	CauseSetHash       string    `json:"cause_set_hash"`
	ReleaseSubjectHash string    `json:"release_subject_hash"`
}

func projectAutomatedRadarGateAlert(
	ctx context.Context,
	tx *sql.Tx,
	outcome service.AutomatedRadarGateOutcome,
	target automatedRadarGateTarget,
	input service.RadarGateInput,
	decision *service.RadarGateDecisionRecord,
) error {
	if decision == nil || len(decision.RuleIDs) == 0 {
		return service.ErrGovernanceHeadConflict
	}
	ruleID := decision.RuleIDs[0]
	payload, err := json.Marshal(automatedRadarGateAlertPayload{
		Source: "automated_gate", RunID: outcome.EventRunID, DecisionID: decision.ID,
		PolicyID: outcome.Target.PolicyID, RuleID: ruleID, EvidenceHash: decision.EvidenceHash,
		OutboxEventID: outcome.EventID, CauseSetHash: outcome.CauseSetHash,
		ReleaseSubjectHash: decision.ReleaseSubjectHash,
	})
	if err != nil {
		return fmt.Errorf("marshal automated Radar Gate alert: %w", err)
	}
	if decision.Status == service.RadarGatePassed || decision.Status == service.RadarGateRecorded {
		rows, err := tx.QueryContext(ctx, `
			UPDATE evaluation_alerts alert
			SET status='resolved', resolved_at=COALESCE(resolved_at,transaction_timestamp())
			WHERE alert.tenant_id=$1 AND alert.model_route=$2 AND alert.capability_domain='global'
			  AND alert.policy_version=$3 AND alert.status<>'resolved'
			  AND EXISTS (
				SELECT 1 FROM evaluation_alert_events event
				WHERE event.alert_id=alert.id AND event.payload->>'source'='automated_gate'
				  AND event.payload->>'run_id'=$4 AND event.payload->>'policy_id'=$5
				  AND event.payload->>'release_subject_hash'=$6
			  )
			RETURNING alert.id`, target.tenantID, target.modelRoute, target.policyVersion,
			outcome.EventRunID.String(), outcome.Target.PolicyID.String(), decision.ReleaseSubjectHash)
		if err != nil {
			return fmt.Errorf("resolve automated Radar Gate alerts: %w", err)
		}
		var alertIDs []uuid.UUID
		for rows.Next() {
			var alertID uuid.UUID
			if err := rows.Scan(&alertID); err != nil {
				rows.Close()
				return fmt.Errorf("scan resolved automated Radar Gate alert: %w", err)
			}
			alertIDs = append(alertIDs, alertID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("iterate resolved automated Radar Gate alerts: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close resolved automated Radar Gate alerts: %w", err)
		}
		for _, alertID := range alertIDs {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO evaluation_alert_events (id,alert_id,kind,payload)
				VALUES ($1,$2,'resolved',$3::jsonb)`, uuid.New(), alertID, string(payload)); err != nil {
				return fmt.Errorf("record resolved automated Radar Gate alert: %w", err)
			}
		}
		return nil
	}

	signal := service.RadarAlertSignal{
		ModelRoute: target.modelRoute, CapabilityDomain: "global", PolicyVersion: target.policyVersion,
		QualityRegressed:       strings.HasPrefix(ruleID, "quality.") || ruleID == "p0.new_failure",
		ReliabilitySLOBreached: input.ReliabilitySLOBreached || strings.HasPrefix(ruleID, "slo.") || strings.HasPrefix(ruleID, "billing."),
	}
	cause := service.AttributeRadarCause(signal)
	severity := "P1"
	if decision.Status == service.RadarGateInsufficientEvidence || ruleID == "p0.new_failure" {
		cause = service.RadarAlertCauseInsufficientEvidence
		severity = "P0"
	}
	var alertID uuid.UUID
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO evaluation_alerts (
			id,tenant_id,model_route,capability_domain,cause,policy_version,status,severity,first_seen_at
		) VALUES ($1,$2,$3,'global',$4,$5,'open',$6,transaction_timestamp())
		ON CONFLICT (tenant_id,model_route,capability_domain,cause,policy_version)
		DO UPDATE SET status=CASE WHEN evaluation_alerts.status='resolved' THEN 'open' ELSE evaluation_alerts.status END,
			first_seen_at=CASE WHEN evaluation_alerts.status='resolved' THEN transaction_timestamp() ELSE evaluation_alerts.first_seen_at END,
			acknowledged_at=CASE WHEN evaluation_alerts.status='resolved' THEN NULL ELSE evaluation_alerts.acknowledged_at END,
			resolved_at=CASE WHEN evaluation_alerts.status='resolved' THEN NULL ELSE evaluation_alerts.resolved_at END,
			severity=EXCLUDED.severity
		RETURNING id`, uuid.New(), target.tenantID, target.modelRoute, cause, target.policyVersion, severity).Scan(&alertID); err != nil {
		return fmt.Errorf("observe automated Radar Gate alert: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_alert_events (id,alert_id,kind,payload)
		VALUES ($1,$2,'observed',$3::jsonb)`, uuid.New(), alertID, string(payload)); err != nil {
		return fmt.Errorf("record automated Radar Gate alert: %w", err)
	}
	return nil
}

func (r *evaluationOutboxDomainRepository) ReconcileEvaluationRun(ctx context.Context, runID uuid.UUID) error {
	if r == nil || r.db == nil {
		return service.ErrEvaluationOutboxInvalid
	}
	_, err := (&evaluationRepository{db: r.db}).ReconcileEvaluationRun(ctx, runID)
	return err
}
