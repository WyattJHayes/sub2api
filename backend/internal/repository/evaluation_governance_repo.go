package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

type radarGovernanceRepository struct{ db *sql.DB }

func NewRadarGovernanceRepository(db *sql.DB) service.RadarGovernanceRepository {
	return &radarGovernanceRepository{db: db}
}

func (r *radarGovernanceRepository) valid() error {
	if r == nil || r.db == nil {
		return errors.New("nil radar governance repository")
	}
	return nil
}

func (r *radarGovernanceRepository) ListPermissions(ctx context.Context, actorID int64) ([]service.RadarPermission, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT role FROM evaluation_role_bindings
		WHERE actor_id = $1 AND enabled = TRUE AND scope = '{}'::jsonb`, actorID)
	if err != nil {
		return nil, fmt.Errorf("list radar roles: %w", err)
	}
	defer rows.Close()
	seen := map[service.RadarPermission]bool{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan radar role: %w", err)
		}
		for _, p := range radarPermissions(service.RadarRole(raw)) {
			seen[p] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate radar roles: %w", err)
	}
	if len(seen) == 0 {
		var bootstrap bool
		if err := r.db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM users u
				WHERE u.id = $1 AND u.role = 'admin' AND u.status = 'active' AND u.deleted_at IS NULL
				  AND NOT EXISTS (SELECT 1 FROM evaluation_role_bindings WHERE enabled = TRUE)
			)`, actorID).Scan(&bootstrap); err != nil {
			return nil, fmt.Errorf("check radar RBAC bootstrap: %w", err)
		}
		if !bootstrap {
			return nil, service.ErrRadarForbidden
		}
		for _, permission := range radarPermissions(service.RolePlatformAdmin) {
			seen[permission] = true
		}
	}
	out := make([]service.RadarPermission, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	// Keep ordering deterministic without exporting the static role map.
	sortRadarPermissions(out)
	return out, nil
}

func (r *radarGovernanceRepository) Require(ctx context.Context, actorID int64, permission service.RadarPermission) error {
	permissions, err := r.ListPermissions(ctx, actorID)
	if err != nil {
		return service.ErrRadarForbidden
	}
	for _, p := range permissions {
		if p == permission {
			return nil
		}
	}
	return service.ErrRadarForbidden
}

func radarPermissions(role service.RadarRole) []service.RadarPermission {
	all := map[service.RadarRole][]service.RadarPermission{
		service.RoleViewer:         {service.PermissionView},
		service.RoleTestOperator:   {service.PermissionView, service.PermissionRunStart, service.PermissionRunRetry, service.PermissionWorkerManage},
		service.RoleQualityAdmin:   {service.PermissionView, service.PermissionDatasetManage, service.PermissionDatasetPublish, service.PermissionPolicyManage, service.PermissionBaselineQualityApprove},
		service.RoleReleaseManager: {service.PermissionView, service.PermissionGateDecide, service.PermissionGateWaive, service.PermissionBaselineReleaseApprove},
		service.RolePlatformAdmin:  {service.PermissionView, service.PermissionRoleManage, service.PermissionRouteAction, service.PermissionEvaluationKeyManage},
	}
	return all[role]
}

func (r *radarGovernanceRepository) EnableEvaluationKey(ctx context.Context, keyID, actorID int64) (*service.RadarEvaluationKeyRecord, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	if keyID <= 0 || actorID <= 0 {
		return nil, errors.New("evaluation API key and actor are required")
	}
	tx, err := beginEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("control"))
	if err != nil {
		return nil, fmt.Errorf("begin evaluation API key enablement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	record := &service.RadarEvaluationKeyRecord{}
	var groupID sql.NullInt64
	err = tx.QueryRowContext(ctx, `
		UPDATE api_keys k
		SET is_evaluation = TRUE, updated_at = NOW()
		WHERE k.id = $1 AND k.status = 'active' AND k.deleted_at IS NULL
		  AND (k.expires_at IS NULL OR k.expires_at > NOW())
		  AND (k.quota = 0 OR k.quota_used < k.quota)
		  AND EXISTS (
		    SELECT 1 FROM users u
		    LEFT JOIN groups g ON g.id = k.group_id
		    WHERE u.id = k.user_id AND u.status = 'active' AND u.deleted_at IS NULL
		      AND (g.id IS NULL OR (g.status = 'active' AND g.deleted_at IS NULL))
		  )
		RETURNING k.id, k.name, k.user_id, k.group_id, k.is_evaluation`, keyID).Scan(
		&record.ID, &record.Name, &record.UserID, &groupID, &record.IsEvaluation,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("enable evaluation API key: %w", err)
	}
	if groupID.Valid {
		record.GroupID = &groupID.Int64
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_key_events (id, api_key_id, action, actor_id)
		VALUES ($1, $2, 'enabled', $3)`, uuid.New(), keyID, actorID); err != nil {
		return nil, fmt.Errorf("audit evaluation API key enablement: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit evaluation API key enablement: %w", err)
	}
	return record, nil
}

func sortRadarPermissions(values []service.RadarPermission) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func (r *radarGovernanceRepository) CreateRoleBinding(ctx context.Context, input service.RadarRoleBindingInput) (*service.RadarRoleBinding, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	if input.ActorID <= 0 || input.CreatedBy <= 0 || len(radarPermissions(input.Role)) == 0 {
		return nil, errors.New("invalid radar role binding")
	}
	if len(strings.TrimSpace(string(input.Scope))) == 0 {
		input.Scope = json.RawMessage(`{}`)
	}
	var scope map[string]any
	if err := json.Unmarshal(input.Scope, &scope); err != nil || len(scope) != 0 {
		return nil, errors.New("scoped radar role bindings are not supported")
	}
	input.Scope = json.RawMessage(`{}`)
	id := uuid.New()
	var out service.RadarRoleBinding
	var createdBy sql.NullInt64
	err := WithEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("control"), func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
		INSERT INTO evaluation_role_bindings (id, actor_id, role, scope, created_by)
		VALUES ($1, $2, $3, $4::jsonb, $5)
		ON CONFLICT (actor_id, role, md5(scope::text)) WHERE enabled DO UPDATE SET enabled = TRUE
		RETURNING id, actor_id, role, scope, enabled, created_by, created_at, disabled_at`, id, input.ActorID, input.Role, string(input.Scope), input.CreatedBy).
			Scan(&out.ID, &out.ActorID, &out.Role, &out.Scope, &out.Enabled, &createdBy, &out.CreatedAt, &out.DisabledAt)
	})
	if err != nil {
		return nil, fmt.Errorf("create radar role binding: %w", err)
	}
	if createdBy.Valid {
		out.CreatedBy = &createdBy.Int64
	}
	return &out, nil
}

func (r *radarGovernanceRepository) DisableRoleBinding(ctx context.Context, id uuid.UUID, actorID int64) error {
	if err := r.valid(); err != nil {
		return err
	}
	var result sql.Result
	err := WithEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("control"), func(tx *sql.Tx) error {
		var err error
		result, err = tx.ExecContext(ctx, `UPDATE evaluation_role_bindings SET enabled = FALSE, disabled_at = COALESCE(disabled_at, NOW()) WHERE id = $1 AND enabled = TRUE`, id)
		return err
	})
	if err != nil {
		return fmt.Errorf("disable radar role binding: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (r *radarGovernanceRepository) ListRoleBindings(ctx context.Context, actorID *int64) ([]service.RadarRoleBinding, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	query := `SELECT id, actor_id, role, scope, enabled, created_by, created_at, disabled_at FROM evaluation_role_bindings`
	args := []any{}
	if actorID != nil {
		query += ` WHERE actor_id = $1`
		args = append(args, *actorID)
	}
	query += ` ORDER BY created_at DESC`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list radar role bindings: %w", err)
	}
	defer rows.Close()
	var out []service.RadarRoleBinding
	for rows.Next() {
		var b service.RadarRoleBinding
		var createdBy sql.NullInt64
		var disabled sql.NullTime
		if err := rows.Scan(&b.ID, &b.ActorID, &b.Role, &b.Scope, &b.Enabled, &createdBy, &b.CreatedAt, &disabled); err != nil {
			return nil, err
		}
		if createdBy.Valid {
			b.CreatedBy = &createdBy.Int64
		}
		if disabled.Valid {
			b.DisabledAt = &disabled.Time
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (r *radarGovernanceRepository) ProposeBaseline(ctx context.Context, input service.RadarBaselineInput) (*service.RadarBaseline, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	id := uuid.New()
	var b service.RadarBaseline
	err := WithEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("control"), func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `INSERT INTO evaluation_baselines (id, model_route, run_id, dataset_manifest_sha256, evidence_hash, route_profile_version, policy_version, proposed_by) VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id,model_route,run_id,dataset_manifest_sha256,evidence_hash,route_profile_version,policy_version,status,proposed_by,proposed_at,activated_at,retired_at`, id, input.ModelRoute, input.RunID, input.DatasetManifestSHA256, input.EvidenceHash, input.RouteProfileVersion, input.PolicyVersion, input.ProposedBy).Scan(&b.ID, &b.ModelRoute, &b.RunID, &b.DatasetManifestSHA256, &b.EvidenceHash, &b.RouteProfileVersion, &b.PolicyVersion, &b.Status, &b.ProposedBy, &b.ProposedAt, &b.ActivatedAt, &b.RetiredAt)
	})
	if err != nil {
		return nil, fmt.Errorf("propose radar baseline: %w", err)
	}
	return &b, nil
}

func (r *radarGovernanceRepository) GetBaseline(ctx context.Context, id uuid.UUID) (*service.RadarBaseline, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	var b service.RadarBaseline
	err := r.db.QueryRowContext(ctx, `SELECT id,model_route,run_id,dataset_manifest_sha256,evidence_hash,route_profile_version,policy_version,status,proposed_by,proposed_at,activated_at,retired_at FROM evaluation_baselines WHERE id=$1`, id).Scan(&b.ID, &b.ModelRoute, &b.RunID, &b.DatasetManifestSHA256, &b.EvidenceHash, &b.RouteProfileVersion, &b.PolicyVersion, &b.Status, &b.ProposedBy, &b.ProposedAt, &b.ActivatedAt, &b.RetiredAt)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (r *radarGovernanceRepository) ApproveBaseline(ctx context.Context, input service.RadarBaselineApprovalInput) (*service.RadarBaselineApproval, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	id := uuid.New()
	var a service.RadarBaselineApproval
	err := WithEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("control"), func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `INSERT INTO evaluation_baseline_approvals (id,baseline_id,approver_id,role,evidence_hash) VALUES ($1,$2,$3,$4,$5) ON CONFLICT (baseline_id,approver_id,role) DO UPDATE SET evidence_hash=EXCLUDED.evidence_hash RETURNING id,baseline_id,approver_id,role,evidence_hash,created_at`, id, input.BaselineID, input.ApproverID, input.Role, input.EvidenceHash).Scan(&a.ID, &a.BaselineID, &a.ApproverID, &a.Role, &a.EvidenceHash, &a.CreatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("approve radar baseline: %w", err)
	}
	return &a, nil
}

func (r *radarGovernanceRepository) ActivateBaseline(ctx context.Context, baselineID uuid.UUID, actorID int64) (*service.RadarBaseline, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	tx, err := beginEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("control"))
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var quality, release int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FILTER (WHERE role='quality_admin'), COUNT(*) FILTER (WHERE role='release_manager') FROM evaluation_baseline_approvals WHERE baseline_id=$1`, baselineID).Scan(&quality, &release); err != nil {
		return nil, err
	}
	if quality == 0 || release == 0 {
		return nil, errors.New("baseline requires quality_admin and release_manager approvals")
	}
	var b service.RadarBaseline
	var oldID uuid.UUID
	if err := tx.QueryRowContext(ctx, `SELECT model_route FROM evaluation_baselines WHERE id=$1 FOR UPDATE`, baselineID).Scan(&b.ModelRoute); err != nil {
		return nil, err
	}
	_ = actorID
	if err := tx.QueryRowContext(ctx, `UPDATE evaluation_baselines SET status='retired', retired_at=NOW() WHERE model_route=$1 AND status='active' AND id<>$2 RETURNING id`, b.ModelRoute, baselineID).Scan(&oldID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	err = tx.QueryRowContext(ctx, `UPDATE evaluation_baselines SET status='active', activated_at=COALESCE(activated_at,NOW()) WHERE id=$1 RETURNING id,model_route,run_id,dataset_manifest_sha256,evidence_hash,route_profile_version,policy_version,status,proposed_by,proposed_at,activated_at,retired_at`, baselineID).Scan(&b.ID, &b.ModelRoute, &b.RunID, &b.DatasetManifestSHA256, &b.EvidenceHash, &b.RouteProfileVersion, &b.PolicyVersion, &b.Status, &b.ProposedBy, &b.ProposedAt, &b.ActivatedAt, &b.RetiredAt)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &b, nil
}

func (r *radarGovernanceRepository) CreateGatePolicy(ctx context.Context, input service.RadarGatePolicyInput) (*service.RadarGatePolicyRecord, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	id := uuid.New()
	var p service.RadarGatePolicyRecord
	err := WithEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("control"), func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `INSERT INTO evaluation_gate_policies (id,version,policy,policy_hash,enforcement_starts_at,created_by) VALUES ($1,$2,$3::jsonb,$4,$5,$6) ON CONFLICT (version) DO UPDATE SET policy=EXCLUDED.policy,policy_hash=EXCLUDED.policy_hash RETURNING id,version,policy,policy_hash,enforcement_starts_at,created_by,created_at,retired_at`, id, input.Version, string(input.Policy), input.PolicyHash, input.EnforcementStartsAt, input.CreatedBy).Scan(&p.ID, &p.Version, &p.Policy, &p.PolicyHash, &p.EnforcementStartsAt, &p.CreatedBy, &p.CreatedAt, &p.RetiredAt)
	})
	if err != nil {
		return nil, fmt.Errorf("create radar gate policy: %w", err)
	}
	return &p, nil
}

func (r *radarGovernanceRepository) RecordGateDecision(ctx context.Context, input service.RadarGateDecisionInput) (*service.RadarGateDecisionRecord, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	id := uuid.New()
	var d service.RadarGateDecisionRecord
	err := WithEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("control"), func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `INSERT INTO evaluation_gate_decisions (id,run_id,baseline_id,policy_id,status,rule_ids,evidence,evidence_hash) VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8) ON CONFLICT (run_id,policy_id) DO UPDATE SET status=EXCLUDED.status,rule_ids=EXCLUDED.rule_ids,evidence=EXCLUDED.evidence,evidence_hash=EXCLUDED.evidence_hash RETURNING id,run_id,baseline_id,policy_id,status,rule_ids,evidence,evidence_hash,created_at`, id, input.RunID, input.BaselineID, input.PolicyID, input.Status, pq.Array(input.RuleIDs), string(input.Evidence), input.EvidenceHash).Scan(&d.ID, &d.RunID, &d.BaselineID, &d.PolicyID, &d.Status, &pqStringArray{&d.RuleIDs}, &d.Evidence, &d.EvidenceHash, &d.CreatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("record radar gate decision: %w", err)
	}
	return &d, nil
}

// pqStringArray lets sql.Scan populate []string without exposing pq types in the service package.
type pqStringArray struct{ dst *[]string }

func (a *pqStringArray) Scan(src any) error {
	var v pq.StringArray
	if err := v.Scan(src); err != nil {
		return err
	}
	*a.dst = []string(v)
	return nil
}

func (r *radarGovernanceRepository) WaiveGateDecision(ctx context.Context, input service.RadarGateWaiverInput) (*service.RadarGateWaiverRecord, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	tx, err := beginEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("control"))
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	id := uuid.New()
	var w service.RadarGateWaiverRecord
	err = tx.QueryRowContext(ctx, `INSERT INTO evaluation_gate_waivers (id,decision_id,business_reason,risk_owner_user_id,mitigation,retest_plan,expires_at,approved_by) VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id,decision_id,business_reason,risk_owner_user_id,mitigation,retest_plan,expires_at,approved_by,created_at`, id, input.DecisionID, input.BusinessReason, input.RiskOwnerUserID, input.Mitigation, input.RetestPlan, input.ExpiresAt, input.ApprovedBy).Scan(&w.ID, &w.DecisionID, &w.BusinessReason, &w.RiskOwnerUserID, &w.Mitigation, &w.RetestPlan, &w.ExpiresAt, &w.ApprovedBy, &w.CreatedAt)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE evaluation_gate_decisions SET status='waived' WHERE id=$1`, input.DecisionID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &w, nil
}

func (r *radarGovernanceRepository) ObserveAlert(ctx context.Context, input service.RadarAlertObservationInput) (*service.RadarAlertRecord, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	tx, err := beginEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("control"))
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	id := uuid.New()
	var a service.RadarAlertRecord
	observed := input.ObservedAt
	if observed.IsZero() {
		observed = time.Now().UTC()
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO evaluation_alerts (id,model_route,capability_domain,cause,policy_version,status,severity,attribution_confidence,first_seen_at) VALUES ($1,$2,$3,$4,$5,'open',$6,$7,$8) ON CONFLICT (model_route,capability_domain,cause,policy_version) DO UPDATE SET status=CASE WHEN evaluation_alerts.status='resolved' THEN 'open' ELSE evaluation_alerts.status END, first_seen_at=CASE WHEN evaluation_alerts.status='resolved' THEN EXCLUDED.first_seen_at ELSE evaluation_alerts.first_seen_at END RETURNING id,model_route,capability_domain,cause,policy_version,status,severity,attribution_confidence,first_seen_at,acknowledged_at,resolved_at,recovery_test_id`, id, input.ModelRoute, input.CapabilityDomain, input.Cause, input.PolicyVersion, input.Severity, input.Confidence, observed).Scan(&a.ID, &a.ModelRoute, &a.CapabilityDomain, &a.Cause, &a.PolicyVersion, &a.Status, &a.Severity, &a.AttributionConfidence, &a.FirstSeenAt, &a.AcknowledgedAt, &a.ResolvedAt, &a.RecoveryTestID)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO evaluation_alert_events (id,alert_id,kind,payload,created_at) VALUES ($1,$2,'observed',$3::jsonb,$4)`, uuid.New(), a.ID, string(input.Payload), observed); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &a, nil
}

func (r *radarGovernanceRepository) AcknowledgeAlert(ctx context.Context, alertID uuid.UUID, actorID int64) error {
	return r.transitionAlert(ctx, alertID, actorID, "acknowledged", `status='acknowledged',acknowledged_at=COALESCE(acknowledged_at,NOW())`)
}
func (r *radarGovernanceRepository) transitionAlert(ctx context.Context, id uuid.UUID, actorID int64, kind, update string) error {
	if err := r.valid(); err != nil {
		return err
	}
	tx, err := beginEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("control"))
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE evaluation_alerts SET `+update+` WHERE id=$1 AND status<>'resolved'`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO evaluation_alert_events (id,alert_id,kind,actor_id) VALUES ($1,$2,$3,$4)`, uuid.New(), id, kind, actorID); err != nil {
		return err
	}
	return tx.Commit()
}
func (r *radarGovernanceRepository) RecordAlertRecovery(ctx context.Context, id, recoveryTestID uuid.UUID, passed bool, actorID int64, payload json.RawMessage) error {
	if err := r.valid(); err != nil {
		return err
	}
	tx, err := beginEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("control"))
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE evaluation_alerts SET recovery_test_id=$2 WHERE id=$1`, id, recoveryTestID); err != nil {
		return err
	}
	event := struct {
		Passed  bool            `json:"passed"`
		Payload json.RawMessage `json:"payload,omitempty"`
	}{passed, payload}
	body, _ := json.Marshal(event)
	if _, err = tx.ExecContext(ctx, `INSERT INTO evaluation_alert_events (id,alert_id,kind,actor_id,payload) VALUES ($1,$2,'recovery_test',$3,$4::jsonb)`, uuid.New(), id, actorID, string(body)); err != nil {
		return err
	}
	return tx.Commit()
}
func (r *radarGovernanceRepository) ResolveAlert(ctx context.Context, id uuid.UUID, actorID int64) error {
	return r.transitionAlert(ctx, id, actorID, "resolved", `status='resolved',resolved_at=NOW()`)
}
func (r *radarGovernanceRepository) RecordAttribution(ctx context.Context, input service.RadarAttributionInput) (*service.RadarAttributionRecord, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	id := uuid.New()
	var a service.RadarAttributionRecord
	err := WithEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("control"), func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `INSERT INTO evaluation_attributions (id,alert_id,cause,confidence,route_slices,evidence_hash) VALUES ($1,$2,$3,$4,$5::jsonb,$6) RETURNING id,alert_id,cause,confidence,route_slices,evidence_hash,created_at`, id, input.AlertID, input.Cause, input.Confidence, string(input.RouteSlices), input.EvidenceHash).Scan(&a.ID, &a.AlertID, &a.Cause, &a.Confidence, &a.RouteSlices, &a.EvidenceHash, &a.CreatedAt)
	})
	if err != nil {
		return nil, err
	}
	return &a, nil
}
func (r *radarGovernanceRepository) GetAlert(ctx context.Context, id uuid.UUID) (*service.RadarAlertRecord, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	var a service.RadarAlertRecord
	err := r.db.QueryRowContext(ctx, `SELECT id,model_route,capability_domain,cause,policy_version,status,severity,attribution_confidence,first_seen_at,acknowledged_at,resolved_at,recovery_test_id FROM evaluation_alerts WHERE id=$1`, id).Scan(&a.ID, &a.ModelRoute, &a.CapabilityDomain, &a.Cause, &a.PolicyVersion, &a.Status, &a.Severity, &a.AttributionConfidence, &a.FirstSeenAt, &a.AcknowledgedAt, &a.ResolvedAt, &a.RecoveryTestID)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

var _ service.RadarGovernanceRepository = (*radarGovernanceRepository)(nil)

var _ service.RadarProjectionRepository = (*radarGovernanceRepository)(nil)

func nullableProjectionFloat(raw sql.NullString) *float64 {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return nil
	}
	value, err := strconv.ParseFloat(raw.String, 64)
	if err != nil {
		return nil
	}
	return &value
}

func projectionHealthState(delta, ciHigh *float64, sampleCount int) string {
	if delta == nil || ciHigh == nil || sampleCount <= 0 {
		return "insufficient_evidence"
	}
	if *delta <= 0 && *ciHigh < 0 {
		return "degraded"
	}
	return "healthy"
}

func (r *radarGovernanceRepository) ListModelHealth(ctx context.Context) ([]service.RadarModelHealthProjection, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT model_route, capability_domain,
		       aggregate->>'baseline_score', aggregate->>'candidate_score',
		       aggregate->>'delta_pp', aggregate->>'ci_low_pp', aggregate->>'ci_high_pp',
		       aggregate->>'effective_pair_count', window_start
		FROM (
			SELECT DISTINCT ON (model_route, capability_domain)
			       model_route, capability_domain, aggregate, window_start
			FROM evaluation_aggregate_snapshots
			ORDER BY model_route, capability_domain, window_start DESC, created_at DESC
		) latest
		ORDER BY model_route, capability_domain`)
	if err != nil {
		return nil, fmt.Errorf("list radar model health: %w", err)
	}
	defer rows.Close()
	var out []service.RadarModelHealthProjection
	for rows.Next() {
		var model, domain string
		var baseline, candidate, delta, low, high, countRaw sql.NullString
		var freshness time.Time
		if err := rows.Scan(&model, &domain, &baseline, &candidate, &delta, &low, &high, &countRaw, &freshness); err != nil {
			return nil, fmt.Errorf("scan radar model health: %w", err)
		}
		count := 0
		if countRaw.Valid {
			count, _ = strconv.Atoi(countRaw.String)
		}
		out = append(out, service.RadarModelHealthProjection{
			ModelRoute: model, CapabilityDomain: domain,
			HealthState:   projectionHealthState(nullableProjectionFloat(delta), nullableProjectionFloat(high), count),
			BaselineScore: nullableProjectionFloat(baseline), CandidateScore: nullableProjectionFloat(candidate),
			DeltaPP: nullableProjectionFloat(delta), CILowPP: nullableProjectionFloat(low), CIHighPP: nullableProjectionFloat(high),
			SampleCount: func() *int {
				if countRaw.Valid {
					return &count
				}
				return nil
			}(), Freshness: freshness,
		})
	}
	return out, rows.Err()
}

func (r *radarGovernanceRepository) ListRuns(ctx context.Context) ([]service.RadarRunProjection, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT r.id, r.plan_id, r.trigger_source, r.status, r.created_at, r.started_at, r.finished_at,
			CASE
				WHEN contract.manifest_id IS NULL THEN 'legacy-unbound'
				WHEN contract.pair_count = contract.binding_count AND contract.pair_count > 0 THEN 'bound'
				ELSE 'incomplete'
			END AS contract_status,
			contract.manifest_id, contract.manifest_sha256
		FROM evaluation_runs r
		LEFT JOIN LATERAL (
			SELECT
				(SELECT ps.request_manifest_id
				 FROM evaluation_pair_specs ps
				 WHERE ps.run_id = r.id
				 ORDER BY ps.created_at, ps.id
				 LIMIT 1) AS manifest_id,
				(SELECT ps.request_manifest_sha256
				 FROM evaluation_pair_specs ps
				 WHERE ps.run_id = r.id
				 ORDER BY ps.created_at, ps.id
				 LIMIT 1) AS manifest_sha256,
				(SELECT COUNT(*) FROM evaluation_pair_specs ps WHERE ps.run_id = r.id) AS pair_count,
				(SELECT COUNT(*)
				 FROM evaluation_pair_specs ps
				 JOIN evaluation_pair_bindings pb ON pb.pair_spec_id = ps.id
				 WHERE ps.run_id = r.id) AS binding_count
		) contract ON TRUE
		ORDER BY r.created_at DESC
		LIMIT 200`)
	if err != nil {
		return nil, fmt.Errorf("list radar runs: %w", err)
	}
	defer rows.Close()
	var out []service.RadarRunProjection
	for rows.Next() {
		var item service.RadarRunProjection
		var manifestHash sql.NullString
		if err := rows.Scan(&item.ID, &item.PlanID, &item.TriggerSource, &item.Status, &item.CreatedAt, &item.StartedAt, &item.FinishedAt,
			&item.ContractStatus, &item.RequestManifestID, &manifestHash); err != nil {
			return nil, err
		}
		if manifestHash.Valid {
			item.RequestManifestSHA256 = manifestHash.String
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r *radarGovernanceRepository) ListAlerts(ctx context.Context) ([]service.RadarAlertProjection, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id, model_route, capability_domain, cause, severity, status, attribution_confidence, first_seen_at FROM evaluation_alerts ORDER BY first_seen_at DESC LIMIT 200`)
	if err != nil {
		return nil, fmt.Errorf("list radar alerts: %w", err)
	}
	defer rows.Close()
	var out []service.RadarAlertProjection
	for rows.Next() {
		var item service.RadarAlertProjection
		if err := rows.Scan(&item.ID, &item.ModelRoute, &item.CapabilityDomain, &item.Cause, &item.Severity, &item.Status, &item.AttributionConfidence, &item.FirstSeenAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r *radarGovernanceRepository) ListGates(ctx context.Context) ([]service.RadarGateProjection, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id, run_id, status, COALESCE(rule_ids[1], ''), created_at FROM evaluation_gate_decisions ORDER BY created_at DESC LIMIT 200`)
	if err != nil {
		return nil, fmt.Errorf("list radar gates: %w", err)
	}
	defer rows.Close()
	var out []service.RadarGateProjection
	for rows.Next() {
		var item service.RadarGateProjection
		if err := rows.Scan(&item.ID, &item.RunID, &item.Status, &item.RuleID, &item.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r *radarGovernanceRepository) ListWorkers(ctx context.Context) ([]service.RadarWorkerProjection, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id, name, worker_kind, status, last_heartbeat_at, capabilities FROM evaluation_workers ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list radar workers: %w", err)
	}
	defer rows.Close()
	var out []service.RadarWorkerProjection
	for rows.Next() {
		var item service.RadarWorkerProjection
		var capabilities pq.StringArray
		if err := rows.Scan(&item.ID, &item.Name, &item.WorkerKind, &item.Status, &item.LastHeartbeatAt, &capabilities); err != nil {
			return nil, err
		}
		item.Capabilities = []string(capabilities)
		if item.Status == "active" && (item.LastHeartbeatAt == nil || time.Since(item.LastHeartbeatAt.UTC()) > 2*time.Minute) {
			item.Status = "stale"
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r *radarGovernanceRepository) ListDatasets(ctx context.Context) ([]service.RadarDatasetProjection, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT d.id, d.dataset_key, d.version, d.status, COUNT(c.id), d.created_at
		FROM evaluation_dataset_versions d
		LEFT JOIN evaluation_cases c ON c.dataset_version_id = d.id
		GROUP BY d.id, d.dataset_key, d.version, d.status, d.created_at
		ORDER BY d.created_at DESC LIMIT 200`)
	if err != nil {
		return nil, fmt.Errorf("list radar datasets: %w", err)
	}
	defer rows.Close()
	var out []service.RadarDatasetProjection
	for rows.Next() {
		var item service.RadarDatasetProjection
		if err := rows.Scan(&item.ID, &item.Name, &item.Version, &item.Status, &item.Cases, &item.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// Avoid an accidental unused import when a build tags out pq in downstream tools.
var _ = strings.TrimSpace
