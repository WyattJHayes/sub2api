package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
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
	rolesQuery := `
		SELECT role FROM evaluation_role_bindings
		WHERE actor_id = $1 AND enabled = TRUE AND scope = '{}'::jsonb`
	rolesArgs := []any{actorID}
	if tenantID, scoped := radarTenant(ctx); scoped {
		rolesQuery += ` AND tenant_id = $2`
		rolesArgs = append(rolesArgs, tenantID)
	}
	rows, err := r.db.QueryContext(ctx, rolesQuery, rolesArgs...)
	if err != nil {
		return nil, fmt.Errorf("list radar roles: %w", err)
	}
	defer func() { _ = rows.Close() }()
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
		bootstrapQuery := `
			SELECT EXISTS (
				SELECT 1 FROM users u
				WHERE u.id = $1 AND u.role = 'admin' AND u.status = 'active' AND u.deleted_at IS NULL
			  AND NOT EXISTS (SELECT 1 FROM evaluation_role_bindings WHERE enabled = TRUE`
		bootstrapArgs := []any{actorID}
		if tenantID, scoped := radarTenant(ctx); scoped {
			bootstrapQuery += ` AND tenant_id = $2`
			bootstrapArgs = append(bootstrapArgs, tenantID)
		}
		bootstrapQuery += `)
			)`
		if err := r.db.QueryRowContext(ctx, bootstrapQuery, bootstrapArgs...).Scan(&bootstrap); err != nil {
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
		service.RoleTestOperator:   {service.PermissionView, service.PermissionRunStart, service.PermissionRunRetry, service.PermissionRunControl, service.PermissionWorkerManage},
		service.RoleQualityAdmin:   {service.PermissionView, service.PermissionDatasetManage, service.PermissionDatasetPublish, service.PermissionPolicyManage, service.PermissionPolicyApprove, service.PermissionBaselineQualityApprove, service.PermissionLoadPlanManage},
		service.RoleReleaseManager: {service.PermissionView, service.PermissionGateDecide, service.PermissionGateWaive, service.PermissionPolicyApprove, service.PermissionBaselineReleaseApprove},
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
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
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
	if tenantID, scoped := radarTenant(ctx); scoped {
		if authenticatedActor, bound := service.RadarActorID(ctx); bound {
			if input.CreatedBy != authenticatedActor {
				return nil, service.ErrRadarForbidden
			}
		} else if input.ActorID != tenantID || input.CreatedBy != tenantID {
			// Calls without an authenticated actor may only create the legacy
			// self-binding. Cross-actor writes require the request identity.
			return nil, service.ErrRadarForbidden
		}
	}
	if len(strings.TrimSpace(string(input.Scope))) == 0 {
		input.Scope = json.RawMessage(`{}`)
	}
	var scope map[string]any
	if err := json.Unmarshal(input.Scope, &scope); err != nil || len(scope) != 0 {
		return nil, errors.New("scoped radar role bindings are not supported")
	}
	input.Scope = json.RawMessage(`{}`)
	tenantID := input.ActorID
	if scopedTenant, scoped := radarTenant(ctx); scoped {
		tenantID = scopedTenant
	}
	id := uuid.New()
	var out service.RadarRoleBinding
	var createdBy sql.NullInt64
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, fmt.Errorf("begin create radar role binding: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	err = tx.QueryRowContext(ctx, `
		INSERT INTO evaluation_role_bindings (id, actor_id, role, scope, created_by, tenant_id)
		VALUES ($1, $2, $3, $4::jsonb, $5, $6)
		ON CONFLICT (tenant_id, actor_id, role, md5(scope::text)) WHERE enabled DO UPDATE SET enabled = TRUE
		RETURNING id, actor_id, role, scope, enabled, created_by, created_at, disabled_at`, id, input.ActorID, input.Role, string(input.Scope), input.CreatedBy, tenantID).
		Scan(&out.ID, &out.ActorID, &out.Role, &out.Scope, &out.Enabled, &createdBy, &out.CreatedAt, &out.DisabledAt)
	if err != nil {
		return nil, fmt.Errorf("create radar role binding: %w", err)
	}
	if createdBy.Valid {
		out.CreatedBy = &createdBy.Int64
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit create radar role binding: %w", err)
	}
	return &out, nil
}

func (r *radarGovernanceRepository) DisableRoleBinding(ctx context.Context, id uuid.UUID, actorID int64) error {
	if err := r.valid(); err != nil {
		return err
	}
	if authenticatedActor, bound := service.RadarActorID(ctx); bound && actorID != authenticatedActor {
		return service.ErrRadarForbidden
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return fmt.Errorf("begin disable radar role binding: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	query := `UPDATE evaluation_role_bindings SET enabled = FALSE, disabled_at = COALESCE(disabled_at, NOW()) WHERE id = $1 AND enabled = TRUE`
	args := []any{id}
	if tenantID, scoped := radarTenant(ctx); scoped {
		query += ` AND tenant_id = $2`
		args = append(args, tenantID)
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("disable radar role binding: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit disable radar role binding: %w", err)
	}
	return nil
}

func (r *radarGovernanceRepository) ListRoleBindings(ctx context.Context, actorID *int64) ([]service.RadarRoleBinding, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	query := `SELECT id, actor_id, role, scope, enabled, created_by, created_at, disabled_at FROM evaluation_role_bindings`
	args := []any{}
	conditions := []string{}
	if actorID != nil {
		conditions = append(conditions, `actor_id = $1`)
		args = append(args, *actorID)
	}
	if tenantID, scoped := radarTenant(ctx); scoped {
		conditions = append(conditions, fmt.Sprintf("tenant_id = $%d", len(args)+1))
		args = append(args, tenantID)
	}
	if len(conditions) > 0 {
		query += ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	query += ` ORDER BY created_at DESC`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list radar role bindings: %w", err)
	}
	defer func() { _ = rows.Close() }()
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
	if tenantID, scoped := radarTenant(ctx); scoped && input.ProposedBy != tenantID {
		return nil, service.ErrRadarForbidden
	}
	id := uuid.New()
	baselineTenantID := input.ProposedBy
	if tenantID, scoped := radarTenant(ctx); scoped {
		baselineTenantID = tenantID
	}
	var b service.RadarBaseline
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, fmt.Errorf("begin propose radar baseline: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	err = tx.QueryRowContext(ctx, `INSERT INTO evaluation_baselines (id, model_route, run_id, dataset_manifest_sha256, evidence_hash, route_profile_version, policy_version, proposed_by, tenant_id) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id,model_route,run_id,dataset_manifest_sha256,evidence_hash,route_profile_version,policy_version,status,proposed_by,proposed_at,activated_at,retired_at`, id, input.ModelRoute, input.RunID, input.DatasetManifestSHA256, input.EvidenceHash, input.RouteProfileVersion, input.PolicyVersion, input.ProposedBy, baselineTenantID).Scan(&b.ID, &b.ModelRoute, &b.RunID, &b.DatasetManifestSHA256, &b.EvidenceHash, &b.RouteProfileVersion, &b.PolicyVersion, &b.Status, &b.ProposedBy, &b.ProposedAt, &b.ActivatedAt, &b.RetiredAt)
	if err != nil {
		return nil, fmt.Errorf("propose radar baseline: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_baseline_events (id, baseline_id, event_type, evidence_hash, actor_id)
		VALUES ($1, $2, 'proposed', $3, $4)`, uuid.New(), b.ID, b.EvidenceHash, input.ProposedBy); err != nil {
		return nil, fmt.Errorf("record radar baseline proposal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit propose radar baseline: %w", err)
	}
	return &b, nil
}

func (r *radarGovernanceRepository) GetBaseline(ctx context.Context, id uuid.UUID) (*service.RadarBaseline, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	var b service.RadarBaseline
	query := `
		SELECT b.id,b.model_route,b.run_id,b.dataset_manifest_sha256,b.evidence_hash,b.route_profile_version,
		       b.policy_version,CASE WHEN EXISTS (SELECT 1 FROM evaluation_baseline_heads h WHERE h.baseline_id=b.id) THEN 'active' ELSE b.status END,
		       b.proposed_by,b.proposed_at,
		       COALESCE(b.activated_at,(SELECT MIN(e.created_at) FROM evaluation_baseline_events e WHERE e.baseline_id=b.id AND e.event_type='activated')),
		       b.retired_at
		FROM evaluation_baselines b WHERE b.id=$1`
	args := []any{id}
	if tenantID, scoped := radarTenant(ctx); scoped {
		query += ` AND b.tenant_id=$2`
		args = append(args, tenantID)
	}
	err := r.db.QueryRowContext(ctx, query, args...).Scan(&b.ID, &b.ModelRoute, &b.RunID, &b.DatasetManifestSHA256, &b.EvidenceHash, &b.RouteProfileVersion, &b.PolicyVersion, &b.Status, &b.ProposedBy, &b.ProposedAt, &b.ActivatedAt, &b.RetiredAt)
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
	if input.EffectiveAt.IsZero() {
		input.EffectiveAt = time.Now().UTC()
	}
	if !input.ExpiresAt.After(input.EffectiveAt) {
		return nil, errors.New("baseline approval expiry must follow its effective time")
	}
	var a service.RadarBaselineApproval
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, fmt.Errorf("begin approve radar baseline: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if tenantID, scoped := radarTenant(ctx); scoped {
		var ownerID int64
		if err := tx.QueryRowContext(ctx, `SELECT tenant_id FROM evaluation_baselines WHERE id=$1`, input.BaselineID).Scan(&ownerID); err != nil {
			return nil, err
		}
		if ownerID != tenantID {
			return nil, service.ErrRadarForbidden
		}
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO evaluation_baseline_approvals (id,baseline_id,approver_id,role,evidence_hash,effective_at,expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (baseline_id,approver_id,role) DO NOTHING RETURNING id,baseline_id,approver_id,role,evidence_hash,effective_at,expires_at,created_at`, id, input.BaselineID, input.ApproverID, input.Role, input.EvidenceHash, input.EffectiveAt, input.ExpiresAt).Scan(&a.ID, &a.BaselineID, &a.ApproverID, &a.Role, &a.EvidenceHash, &a.EffectiveAt, &a.ExpiresAt, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `SELECT id,baseline_id,approver_id,role,evidence_hash,effective_at,expires_at,created_at FROM evaluation_baseline_approvals WHERE baseline_id=$1 AND approver_id=$2 AND role=$3`, input.BaselineID, input.ApproverID, input.Role).Scan(&a.ID, &a.BaselineID, &a.ApproverID, &a.Role, &a.EvidenceHash, &a.EffectiveAt, &a.ExpiresAt, &a.CreatedAt)
		if err == nil && (a.EvidenceHash != input.EvidenceHash || !a.ExpiresAt.Equal(input.ExpiresAt)) {
			return nil, errors.New("baseline approval already exists with different evidence")
		}
	}
	if err != nil {
		return nil, fmt.Errorf("approve radar baseline: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_baseline_events (id, baseline_id, event_type, evidence_hash, actor_id)
		VALUES ($1, $2, 'approved', $3, $4)
		ON CONFLICT DO NOTHING`, uuid.New(), a.BaselineID, a.EvidenceHash, a.ApproverID); err != nil {
		return nil, fmt.Errorf("record radar baseline approval: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit approve radar baseline: %w", err)
	}
	return &a, nil
}

func (r *radarGovernanceRepository) ActivateBaseline(ctx context.Context, baselineID uuid.UUID, actorID int64) (*service.RadarBaseline, error) {
	if _, err := r.ActivateBaselineHead(ctx, service.RadarBaselineActivationInput{
		BaselineID: baselineID,
		Scope: service.RadarGovernanceScope{
			Environment: "production",
			ScopeType:   "global",
			ScopeID:     service.GlobalReleaseScopeID,
		},
		ActorID: actorID,
	}); err != nil {
		return nil, err
	}
	return r.GetBaseline(ctx, baselineID)
}

func (r *radarGovernanceRepository) CreateGatePolicy(ctx context.Context, input service.RadarGatePolicyInput) (*service.RadarGatePolicyRecord, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	if tenantID, scoped := radarTenant(ctx); scoped && input.CreatedBy != tenantID {
		return nil, service.ErrRadarForbidden
	}
	id := uuid.New()
	policyTenantID := input.CreatedBy
	if tenantID, scoped := radarTenant(ctx); scoped {
		policyTenantID = tenantID
	}
	if !input.ApprovalExpiresAt.After(input.EnforcementStartsAt) {
		return nil, errors.New("gate policy approval must outlive enforcement start")
	}
	policyHash, err := service.DigestCanonicalJSON(input.Policy)
	if err != nil {
		return nil, fmt.Errorf("canonicalize radar gate policy: %w", err)
	}
	var p service.RadarGatePolicyRecord
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, fmt.Errorf("begin create radar gate policy: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	created := true
	err = tx.QueryRowContext(ctx, `INSERT INTO evaluation_gate_policies (id,version,policy,policy_hash,enforcement_starts_at,created_by,tenant_id) VALUES ($1,$2,$3::jsonb,$4,$5,$6,$7) ON CONFLICT (version) DO NOTHING RETURNING id,version,policy,policy_hash,enforcement_starts_at,created_by,created_at,retired_at`, id, input.Version, string(input.Policy), policyHash, input.EnforcementStartsAt, input.CreatedBy, policyTenantID).Scan(&p.ID, &p.Version, &p.Policy, &p.PolicyHash, &p.EnforcementStartsAt, &p.CreatedBy, &p.CreatedAt, &p.RetiredAt)
	if errors.Is(err, sql.ErrNoRows) {
		created = false
		err = tx.QueryRowContext(ctx, `SELECT id,version,policy,policy_hash,enforcement_starts_at,created_by,created_at,retired_at FROM evaluation_gate_policies WHERE version=$1`, input.Version).Scan(&p.ID, &p.Version, &p.Policy, &p.PolicyHash, &p.EnforcementStartsAt, &p.CreatedBy, &p.CreatedAt, &p.RetiredAt)
		if err == nil && p.PolicyHash != policyHash {
			return nil, errors.New("gate policy version already exists with different content")
		}
		if err == nil {
			if tenantID, scoped := radarTenant(ctx); scoped {
				var existingTenantID int64
				if err := tx.QueryRowContext(ctx, `SELECT tenant_id FROM evaluation_gate_policies WHERE id=$1`, p.ID).Scan(&existingTenantID); err != nil {
					return nil, fmt.Errorf("load existing gate policy tenant: %w", err)
				}
				if existingTenantID != tenantID {
					return nil, service.ErrRadarForbidden
				}
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("create radar gate policy: %w", err)
	}
	if created {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO evaluation_gate_policy_events (id, policy_id, event_type, policy_hash, actor_id)
			VALUES ($1, $2, 'created', $3, $4)`, uuid.New(), p.ID, p.PolicyHash, p.CreatedBy); err != nil {
			return nil, fmt.Errorf("record radar gate policy creation: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit create radar gate policy: %w", err)
	}
	return &p, nil
}

func (r *radarGovernanceRepository) ApproveGatePolicy(ctx context.Context, input service.RadarGatePolicyApprovalInput) (*service.RadarGatePolicyApprovalRecord, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	if input.PolicyID == uuid.Nil || input.ApproverID <= 0 {
		return nil, errors.New("gate policy and approver are required")
	}
	if input.Role != service.RoleQualityAdmin && input.Role != service.RoleReleaseManager {
		return nil, errors.New("gate policy approval role must be quality_admin or release_manager")
	}
	if !validLowerHexSHA256(input.PolicyHash) || !validLowerHexSHA256(input.EvidenceHash) {
		return nil, errors.New("gate policy approval hashes must be lowercase SHA256")
	}
	if input.EffectiveAt.IsZero() || input.ExpiresAt.IsZero() || !input.ExpiresAt.After(input.EffectiveAt) {
		return nil, errors.New("gate policy approval window is invalid")
	}
	if input.EffectiveAt.After(time.Now().UTC()) {
		return nil, errors.New("gate policy approval cannot begin in the future")
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, fmt.Errorf("begin gate policy approval: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var creatorID, policyTenantID int64
	var currentHash string
	var enforcementStartsAt time.Time
	var retiredAt sql.NullTime
	if err := tx.QueryRowContext(ctx, `
		SELECT created_by, tenant_id, policy_hash, enforcement_starts_at, retired_at
		FROM evaluation_gate_policies WHERE id=$1 FOR SHARE`, input.PolicyID).
		Scan(&creatorID, &policyTenantID, &currentHash, &enforcementStartsAt, &retiredAt); err != nil {
		return nil, fmt.Errorf("load gate policy for approval: %w", err)
	}
	if retiredAt.Valid {
		return nil, errors.New("retired gate policy cannot be approved")
	}
	if creatorID == input.ApproverID {
		return nil, errors.New("gate policy creator cannot approve the same policy")
	}
	if currentHash != input.PolicyHash {
		return nil, errors.New("gate policy approval hash does not match current policy")
	}
	if input.EffectiveAt.After(enforcementStartsAt) || !input.ExpiresAt.After(enforcementStartsAt) {
		return nil, errors.New("gate policy approval must cover enforcement start")
	}
	if tenantID, scoped := radarTenant(ctx); scoped && policyTenantID != tenantID {
		return nil, service.ErrRadarForbidden
	}
	var roleBound bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM evaluation_role_bindings
			WHERE actor_id=$1 AND role=$2 AND enabled=TRUE AND scope='{}'::jsonb AND tenant_id=$3
		)`, input.ApproverID, input.Role, policyTenantID).Scan(&roleBound); err != nil {
		return nil, fmt.Errorf("check gate policy approver role: %w", err)
	}
	if !roleBound {
		return nil, service.ErrRadarForbidden
	}
	var out service.RadarGatePolicyApprovalRecord
	var created bool
	err = tx.QueryRowContext(ctx, `
		INSERT INTO evaluation_gate_policy_approvals
			(id,policy_id,approver_id,role,policy_hash,evidence_hash,effective_at,expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (policy_id,approver_id,role) DO NOTHING
		RETURNING id,policy_id,approver_id,role,policy_hash,evidence_hash,effective_at,expires_at,created_at`,
		uuid.New(), input.PolicyID, input.ApproverID, input.Role, input.PolicyHash, input.EvidenceHash,
		input.EffectiveAt, input.ExpiresAt).
		Scan(&out.ID, &out.PolicyID, &out.ApproverID, &out.Role, &out.PolicyHash, &out.EvidenceHash, &out.EffectiveAt, &out.ExpiresAt, &out.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `
			SELECT id,policy_id,approver_id,role,policy_hash,evidence_hash,effective_at,expires_at,created_at
			FROM evaluation_gate_policy_approvals
			WHERE policy_id=$1 AND approver_id=$2 AND role=$3`, input.PolicyID, input.ApproverID, input.Role).
			Scan(&out.ID, &out.PolicyID, &out.ApproverID, &out.Role, &out.PolicyHash, &out.EvidenceHash, &out.EffectiveAt, &out.ExpiresAt, &out.CreatedAt)
		if err == nil && (out.PolicyHash != input.PolicyHash || out.EvidenceHash != input.EvidenceHash ||
			!out.EffectiveAt.Equal(input.EffectiveAt) || !out.ExpiresAt.Equal(input.ExpiresAt)) {
			return nil, errors.New("gate policy approval already exists with different evidence or validity")
		}
	} else {
		created = err == nil
	}
	if err != nil {
		return nil, fmt.Errorf("record gate policy approval: %w", err)
	}
	if created {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO evaluation_gate_policy_events
				(id,policy_id,event_type,policy_hash,environment,scope_type,scope_id,actor_id)
			VALUES ($1,$2,'approved',$3,'approval',$4,$5,$6)`, uuid.New(), input.PolicyID, input.PolicyHash,
			input.Role, strconv.FormatInt(input.ApproverID, 10), input.ApproverID); err != nil {
			return nil, fmt.Errorf("record gate policy approval event: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit gate policy approval: %w", err)
	}
	return &out, nil
}

func (r *radarGovernanceRepository) CreateReleaseSubject(ctx context.Context, input service.ReleaseSubjectInput) (*service.ReleaseSubjectRecord, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	if input.RunID == uuid.Nil {
		return nil, errors.New("release subject run is required")
	}
	canonical, err := service.CanonicalizeReleaseSubject(input.Subject)
	if err != nil {
		return nil, err
	}
	if expected := strings.TrimSpace(input.ExpectedHash); expected != "" && expected != canonical.SHA256 {
		return nil, errors.New("release subject hash does not match canonical subject")
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, fmt.Errorf("begin create release subject: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := ensureRadarRunTenant(ctx, tx, input.RunID); err != nil {
		return nil, err
	}
	validBinding, err := validateFrozenReleaseSubjectBinding(ctx, tx, input.RunID, canonical.Subject)
	if err != nil {
		return nil, fmt.Errorf("validate frozen run binding: %w", err)
	}
	if !validBinding {
		return nil, errors.New("release subject does not match frozen run binding")
	}
	record := &service.ReleaseSubjectRecord{ID: uuid.New(), RunID: input.RunID, SubjectHash: canonical.SHA256, Subject: canonical.Subject}
	var raw []byte
	err = tx.QueryRowContext(ctx, `
		INSERT INTO evaluation_release_subjects (id, run_id, subject_hash, canonical_subject, canonical_subject_bytes, tenant_id)
		VALUES ($1, $2, $3, $4::jsonb, $5, (SELECT tenant_id FROM evaluation_runs WHERE id=$2))
		ON CONFLICT (run_id, subject_hash) DO NOTHING
		RETURNING canonical_subject_bytes, created_at`, record.ID, record.RunID, record.SubjectHash, string(canonical.Bytes), []byte(canonical.Bytes)).Scan(&raw, &record.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `
			SELECT id, canonical_subject_bytes, created_at
			FROM evaluation_release_subjects WHERE run_id=$1 AND subject_hash=$2`, record.RunID, record.SubjectHash).Scan(&record.ID, &raw, &record.CreatedAt)
	}
	if err != nil {
		return nil, fmt.Errorf("persist release subject: %w", err)
	}
	if err := json.Unmarshal(raw, &record.Subject); err != nil {
		return nil, fmt.Errorf("decode release subject: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit release subject: %w", err)
	}
	return record, nil
}

func (r *radarGovernanceRepository) GetReleaseSubject(ctx context.Context, id uuid.UUID) (*service.ReleaseSubjectRecord, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	if id == uuid.Nil {
		return nil, errors.New("release subject is required")
	}
	query := `
		SELECT rs.id,rs.run_id,rs.subject_hash,rs.canonical_subject,rs.created_at,
		       COALESCE(event.event_type='activated' AND event.effective_at <= transaction_timestamp() AND event.expires_at > transaction_timestamp(), FALSE),
		       event.effective_at,event.expires_at
		FROM evaluation_release_subjects rs
		LEFT JOIN LATERAL (
			SELECT event_type,effective_at,expires_at
			FROM evaluation_release_subject_events
			WHERE release_subject_id=rs.id
			  AND effective_at <= transaction_timestamp()
			ORDER BY sequence DESC LIMIT 1
		) event ON TRUE
		WHERE rs.id=$1`
	args := []any{id}
	if tenantID, scoped := radarTenant(ctx); scoped {
		query += ` AND rs.tenant_id=$2`
		args = append(args, tenantID)
	}
	var record service.ReleaseSubjectRecord
	var raw []byte
	var effectiveAt, expiresAt sql.NullTime
	if err := r.db.QueryRowContext(ctx, query, args...).Scan(
		&record.ID, &record.RunID, &record.SubjectHash, &raw, &record.CreatedAt, &record.Active, &effectiveAt, &expiresAt,
	); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &record.Subject); err != nil {
		return nil, fmt.Errorf("decode release subject: %w", err)
	}
	if effectiveAt.Valid {
		record.EffectiveAt = &effectiveAt.Time
	}
	if expiresAt.Valid {
		record.ExpiresAt = &expiresAt.Time
	}
	return &record, nil
}

func validateFrozenReleaseSubjectBinding(ctx context.Context, tx *sql.Tx, runID uuid.UUID, subject service.ReleaseSubject) (bool, error) {
	var valid bool
	err := tx.QueryRowContext(ctx, `
		WITH frozen_run_binding AS (
			SELECT r.status, r.route_profile_version, dv.manifest_sha256
			FROM evaluation_runs r
			JOIN evaluation_plans p ON p.id = r.plan_id
			JOIN evaluation_dataset_versions dv ON dv.id = p.dataset_version_id
			WHERE r.id = $1
		), pair_binding_counts AS (
			SELECT COUNT(*) AS pair_count,
			       COUNT(pb.id) AS binding_count
			FROM evaluation_pair_specs ps
			LEFT JOIN evaluation_pair_bindings pb ON pb.pair_spec_id = ps.id
			WHERE ps.run_id = $1
		), baseline_binding AS (
			SELECT b.id, b.model_route
			FROM evaluation_baselines b
			JOIN evaluation_baseline_heads h
			  ON h.baseline_id = b.id
			 AND h.environment = $11 AND h.scope_type = $12 AND h.scope_id = $13
			 AND h.model_route = b.model_route
			WHERE b.id = $2 AND b.dataset_manifest_sha256 = $4
			  AND b.route_profile_version = $5 AND b.retired_at IS NULL
		)
		SELECT EXISTS (
			SELECT 1
			FROM frozen_run_binding fr, pair_binding_counts pc, baseline_binding bb
			WHERE fr.status = 'completed'
			  AND fr.manifest_sha256 = $4
			  AND fr.route_profile_version = $5
			  AND pc.pair_count > 0 AND pc.pair_count = pc.binding_count
			  AND EXISTS (
				SELECT 1
				FROM evaluation_pair_bindings pb
				JOIN evaluation_pair_specs ps ON ps.id = pb.pair_spec_id
				JOIN evaluation_side_specs baseline_ss
				  ON baseline_ss.id = pb.baseline_side_spec_id
				 AND baseline_ss.pair_spec_id = pb.pair_spec_id
				JOIN evaluation_side_specs candidate_ss
				  ON candidate_ss.id = pb.candidate_side_spec_id
				 AND candidate_ss.pair_spec_id = pb.pair_spec_id
				WHERE ps.run_id = $1
				  AND baseline_ss.side = 'baseline'
				  AND candidate_ss.side = 'candidate'
				  AND baseline_ss.canonical_spec->>'model_route' = 'baseline:' || bb.model_route
			  )
			  AND NOT EXISTS (
				SELECT 1
				FROM evaluation_side_specs ss
				JOIN evaluation_pair_specs ps ON ps.id = ss.pair_spec_id
				WHERE ps.run_id = $1
				  AND ss.canonical_spec->>'route_profile_version' IS DISTINCT FROM $5
			  )
			  AND NOT EXISTS (
				SELECT 1
				FROM evaluation_pair_bindings pb
				JOIN evaluation_pair_specs ps ON ps.id = pb.pair_spec_id
				JOIN evaluation_side_specs baseline_ss
				  ON baseline_ss.id = pb.baseline_side_spec_id
				 AND baseline_ss.pair_spec_id = pb.pair_spec_id
				JOIN evaluation_side_specs candidate_ss
				  ON candidate_ss.id = pb.candidate_side_spec_id
				 AND candidate_ss.pair_spec_id = pb.pair_spec_id
				WHERE ps.run_id = $1
				  AND baseline_ss.side = 'baseline'
				  AND candidate_ss.side = 'candidate'
				  AND baseline_ss.canonical_spec->>'model_route' = 'baseline:' || bb.model_route
				  AND (
					candidate_ss.canonical_spec->>'model_route' IS DISTINCT FROM 'candidate:' || bb.model_route OR
					candidate_ss.canonical_spec->>'model_config_sha256' IS DISTINCT FROM $3
				  )
			  )
			  AND ARRAY(SELECT DISTINCT a.worker_image_digest FROM evaluation_assignments a
				JOIN evaluation_samples s ON s.id=a.sample_id
				WHERE s.run_id=$1 AND a.worker_image_digest IS NOT NULL ORDER BY 1) = $6::text[]
			  AND ARRAY(SELECT DISTINCT g.worker_image_digest FROM evaluation_grading_jobs g
				WHERE g.run_id=$1 AND g.worker_image_digest IS NOT NULL ORDER BY 1) = $7::text[]
			  AND ARRAY(SELECT DISTINCT j.worker_image_digest FROM evaluation_analysis_jobs j
				WHERE j.run_id=$1 AND j.worker_image_digest IS NOT NULL ORDER BY 1) = $8::text[]
			  AND EXISTS (SELECT 1 FROM evaluation_analysis_jobs j WHERE j.run_id=$1)
			  AND NOT EXISTS (SELECT 1 FROM evaluation_analysis_jobs j WHERE j.run_id=$1 AND j.analysis_version <> $9)
			  AND ARRAY(SELECT DISTINCT ps.canonical_spec->>'region' FROM evaluation_pair_specs ps
				WHERE ps.run_id=$1 ORDER BY 1) = $10::text[]
		)`, runID, subject.BaselineID, subject.CandidateModelConfigSHA256,
		subject.DatasetManifestSHA256, subject.RouteProfileVersion,
		pq.Array(subject.RunnerImageDigests), pq.Array(subject.GraderImageDigests),
		pq.Array(subject.StatisticsImageDigests), subject.AnalysisVersion,
		pq.Array(subject.RegionSet), subject.DeploymentEnvironment, subject.ScopeType, subject.ScopeID).Scan(&valid)
	return valid, err
}

func (r *radarGovernanceRepository) ActivateReleaseSubject(ctx context.Context, input service.ReleaseSubjectActivationInput) (*service.ReleaseSubjectEvent, error) {
	if input.ReleaseSubjectID == uuid.Nil || input.ActorID <= 0 || !input.ExpiresAt.After(input.EffectiveAt) {
		return nil, errors.New("valid release subject activation is required")
	}
	return r.appendReleaseSubjectEvent(ctx, input.ReleaseSubjectID, input.ActorID, "activated", input.EffectiveAt, &input.ExpiresAt)
}

func (r *radarGovernanceRepository) RevokeReleaseSubject(ctx context.Context, subjectID uuid.UUID, actorID int64) (*service.ReleaseSubjectEvent, error) {
	if subjectID == uuid.Nil || actorID <= 0 {
		return nil, errors.New("release subject and actor are required")
	}
	now := time.Now().UTC()
	return r.appendReleaseSubjectEvent(ctx, subjectID, actorID, "revoked", now, nil)
}

func (r *radarGovernanceRepository) appendReleaseSubjectEvent(ctx context.Context, subjectID uuid.UUID, actorID int64, eventType string, effectiveAt time.Time, expiresAt *time.Time) (*service.ReleaseSubjectEvent, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if tenantID, scoped := radarTenant(ctx); scoped {
		var ownerID sql.NullInt64
		if err := tx.QueryRowContext(ctx, `
			SELECT tenant_id
			FROM evaluation_release_subjects
			WHERE id=$1`, subjectID).Scan(&ownerID); err != nil {
			return nil, err
		}
		if !ownerID.Valid || ownerID.Int64 != tenantID {
			return nil, service.ErrRadarForbidden
		}
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "release-subject:"+subjectID.String()); err != nil {
		return nil, err
	}
	event := &service.ReleaseSubjectEvent{ID: uuid.New(), ReleaseSubjectID: subjectID, EventType: eventType, ActorID: actorID, EffectiveAt: effectiveAt, ExpiresAt: expiresAt}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO evaluation_release_subject_events
			(id,release_subject_id,event_type,actor_id,effective_at,expires_at)
		SELECT $1,rs.id,$2,$3,$4,$5 FROM evaluation_release_subjects rs WHERE rs.id=$6
		RETURNING created_at`, event.ID, eventType, actorID, effectiveAt, expiresAt, subjectID).Scan(&event.CreatedAt)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return event, nil
}

func (r *radarGovernanceRepository) ActivateGatePolicy(ctx context.Context, input service.RadarGatePolicyActivationInput) (*service.RadarGatePolicyHead, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	if input.PolicyID == uuid.Nil || input.ActorID <= 0 {
		return nil, errors.New("policy and activation actor are required")
	}
	scope, err := service.CanonicalizeGovernanceScope(input.Scope)
	if err != nil {
		return nil, err
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, fmt.Errorf("begin activate gate policy: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var tenantID int64
	if err := tx.QueryRowContext(ctx, `SELECT tenant_id FROM evaluation_gate_policies WHERE id=$1`, input.PolicyID).Scan(&tenantID); err != nil {
		return nil, err
	}
	if requestTenant, scoped := radarTenant(ctx); scoped && tenantID != requestTenant {
		return nil, service.ErrRadarForbidden
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, fmt.Sprintf("policy:%d:%s:%s:%s", tenantID, scope.Environment, scope.ScopeType, scope.ScopeID)); err != nil {
		return nil, fmt.Errorf("lock gate policy lineage: %w", err)
	}
	var policyHash string
	var eligible bool
	if err := tx.QueryRowContext(ctx, `
		SELECT p.policy_hash,
		       p.retired_at IS NULL
		       AND COUNT(*) FILTER (
				WHERE rb.actor_id IS NOT NULL AND a.role='quality_admin' AND a.approver_id <> p.created_by
				  AND a.effective_at <= transaction_timestamp() AND a.expires_at > transaction_timestamp()
				  AND a.effective_at <= p.enforcement_starts_at AND a.expires_at > p.enforcement_starts_at
			) >= 1
		       AND COUNT(*) FILTER (
				WHERE rb.actor_id IS NOT NULL AND a.role='release_manager' AND a.approver_id <> p.created_by
				  AND a.effective_at <= transaction_timestamp() AND a.expires_at > transaction_timestamp()
				  AND a.effective_at <= p.enforcement_starts_at AND a.expires_at > p.enforcement_starts_at
			) >= 1
		       AND COUNT(DISTINCT a.approver_id) FILTER (
				WHERE rb.actor_id IS NOT NULL AND a.approver_id <> p.created_by
				  AND a.effective_at <= transaction_timestamp() AND a.expires_at > transaction_timestamp()
				  AND a.effective_at <= p.enforcement_starts_at AND a.expires_at > p.enforcement_starts_at
			) >= 2
		FROM evaluation_gate_policies p
		LEFT JOIN evaluation_gate_policy_approvals a
		  ON a.policy_id=p.id AND a.policy_hash=p.policy_hash
		LEFT JOIN evaluation_role_bindings rb
		  ON rb.actor_id=a.approver_id AND rb.role=a.role AND rb.enabled=TRUE
		 AND rb.scope='{}'::jsonb AND rb.tenant_id=p.tenant_id
		WHERE p.id=$1
		GROUP BY p.id,p.policy_hash,p.created_by,p.enforcement_starts_at,p.retired_at`, input.PolicyID).Scan(&policyHash, &eligible); err != nil {
		return nil, fmt.Errorf("load gate policy version: %w", err)
	}
	if !eligible {
		return nil, errors.New("gate policy is outside its activation window")
	}
	head := &service.RadarGatePolicyHead{Scope: scope, PolicyHash: policyHash}
	err = tx.QueryRowContext(ctx, `
		SELECT policy_id,event_id,updated_at FROM evaluation_gate_policy_heads
		WHERE tenant_id=$1 AND environment=$2 AND scope_type=$3 AND scope_id=$4`, tenantID, scope.Environment, scope.ScopeType, scope.ScopeID).Scan(&head.PolicyID, &head.EventID, &head.UpdatedAt)
	if err == nil && head.PolicyID == input.PolicyID {
		if input.ExpectedPolicyID != nil && *input.ExpectedPolicyID != head.PolicyID {
			return nil, service.ErrGovernanceHeadConflict
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return nil, commitErr
		}
		return head, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("load current gate policy head: %w", err)
	}
	var current any
	if err == nil {
		current = head.PolicyID
		if input.ExpectedPolicyID == nil || *input.ExpectedPolicyID != head.PolicyID {
			return nil, service.ErrGovernanceHeadConflict
		}
	} else if input.ExpectedPolicyID != nil {
		return nil, service.ErrGovernanceHeadConflict
	}
	head.EventID = uuid.New()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_gate_policy_events
			(id,policy_id,event_type,policy_hash,environment,scope_type,scope_id,actor_id)
		VALUES ($1,$2,'activated',$3,$4,$5,$6,$7)`, head.EventID, input.PolicyID, policyHash, scope.Environment, scope.ScopeType, scope.ScopeID, input.ActorID); err != nil {
		return nil, fmt.Errorf("record gate policy activation: %w", err)
	}
	var advanced bool
	if err := tx.QueryRowContext(ctx, `SELECT advance_evaluation_gate_policy_head($1,$2,$3,$4,$5,$6,$7)`,
		tenantID, scope.Environment, scope.ScopeType, scope.ScopeID, input.PolicyID, head.EventID, current).Scan(&advanced); err != nil {
		return nil, fmt.Errorf("advance gate policy head: %w", err)
	}
	if !advanced {
		return nil, service.ErrGovernanceHeadConflict
	}
	if err := enqueueGovernanceReevaluation(ctx, tx, "policy_head", input.PolicyID, head.EventID, scope, ""); err != nil {
		return nil, err
	}
	head.PolicyID = input.PolicyID
	if err := tx.QueryRowContext(ctx, `SELECT updated_at FROM evaluation_gate_policy_heads WHERE tenant_id=$1 AND environment=$2 AND scope_type=$3 AND scope_id=$4`, tenantID, scope.Environment, scope.ScopeType, scope.ScopeID).Scan(&head.UpdatedAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit gate policy activation: %w", err)
	}
	return head, nil
}

func (r *radarGovernanceRepository) ActivateBaselineHead(ctx context.Context, input service.RadarBaselineActivationInput) (*service.RadarBaselineHead, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	if input.BaselineID == uuid.Nil || input.ActorID <= 0 {
		return nil, errors.New("baseline and activation actor are required")
	}
	scope, err := service.CanonicalizeGovernanceScope(input.Scope)
	if err != nil {
		return nil, err
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, fmt.Errorf("begin activate baseline head: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if tenantID, scoped := radarTenant(ctx); scoped {
		var ownerID int64
		if err := tx.QueryRowContext(ctx, `SELECT tenant_id FROM evaluation_baselines WHERE id=$1`, input.BaselineID).Scan(&ownerID); err != nil {
			return nil, err
		}
		if ownerID != tenantID {
			return nil, service.ErrRadarForbidden
		}
	}
	var quality, release, approvers int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FILTER (WHERE a.role='quality_admin'), COUNT(*) FILTER (WHERE a.role='release_manager'),
		       COUNT(DISTINCT a.approver_id)
		FROM evaluation_baseline_approvals a
		JOIN evaluation_baselines b
		  ON b.id=a.baseline_id
		JOIN evaluation_role_bindings rb
		  ON rb.actor_id=a.approver_id AND rb.role=a.role AND rb.enabled=TRUE AND rb.scope='{}'::jsonb
		 AND rb.tenant_id=b.tenant_id
		WHERE a.baseline_id=$1 AND a.effective_at <= transaction_timestamp()
		  AND a.expires_at > transaction_timestamp()`, input.BaselineID).Scan(&quality, &release, &approvers); err != nil {
		return nil, err
	}
	if quality == 0 || release == 0 || approvers < 2 {
		return nil, errors.New("baseline requires distinct quality_admin and release_manager approvals")
	}
	head := &service.RadarBaselineHead{Scope: scope, BaselineID: input.BaselineID}
	var evidenceHash string
	if err := tx.QueryRowContext(ctx, `SELECT model_route,evidence_hash FROM evaluation_baselines WHERE id=$1`, input.BaselineID).Scan(&head.ModelRoute, &evidenceHash); err != nil {
		return nil, fmt.Errorf("load baseline version: %w", err)
	}
	lineage := "baseline:" + scope.Environment + ":" + scope.ScopeType + ":" + scope.ScopeID + ":" + head.ModelRoute
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lineage); err != nil {
		return nil, fmt.Errorf("lock baseline lineage: %w", err)
	}
	var current any
	var currentID uuid.UUID
	err = tx.QueryRowContext(ctx, `
		SELECT baseline_id,event_id,updated_at FROM evaluation_baseline_heads
		WHERE environment=$1 AND scope_type=$2 AND scope_id=$3 AND model_route=$4`, scope.Environment, scope.ScopeType, scope.ScopeID, head.ModelRoute).Scan(&currentID, &head.EventID, &head.UpdatedAt)
	if err == nil && currentID == input.BaselineID {
		if commitErr := tx.Commit(); commitErr != nil {
			return nil, commitErr
		}
		return head, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("load current baseline head: %w", err)
	}
	if err == nil {
		current = currentID
		if input.ExpectedBaselineID == nil || *input.ExpectedBaselineID != currentID {
			return nil, service.ErrGovernanceHeadConflict
		}
	} else if input.ExpectedBaselineID != nil {
		return nil, service.ErrGovernanceHeadConflict
	}
	head.EventID = uuid.New()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_baseline_events
			(id,baseline_id,event_type,evidence_hash,environment,scope_type,scope_id,actor_id)
		VALUES ($1,$2,'activated',$3,$4,$5,$6,$7)`, head.EventID, input.BaselineID, evidenceHash, scope.Environment, scope.ScopeType, scope.ScopeID, input.ActorID); err != nil {
		return nil, fmt.Errorf("record baseline activation: %w", err)
	}
	var advanced bool
	if err := tx.QueryRowContext(ctx, `SELECT advance_evaluation_baseline_head($1,$2,$3,$4,$5,$6,$7)`,
		scope.Environment, scope.ScopeType, scope.ScopeID, head.ModelRoute, input.BaselineID, head.EventID, current).Scan(&advanced); err != nil {
		return nil, fmt.Errorf("advance baseline head: %w", err)
	}
	if !advanced {
		return nil, service.ErrGovernanceHeadConflict
	}
	if err := enqueueGovernanceReevaluation(ctx, tx, "baseline_head", input.BaselineID, head.EventID, scope, head.ModelRoute); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT updated_at FROM evaluation_baseline_heads WHERE environment=$1 AND scope_type=$2 AND scope_id=$3 AND model_route=$4`, scope.Environment, scope.ScopeType, scope.ScopeID, head.ModelRoute).Scan(&head.UpdatedAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit baseline activation: %w", err)
	}
	return head, nil
}

func enqueueGovernanceReevaluation(ctx context.Context, tx *sql.Tx, causeType string, causeID, eventID uuid.UUID, scope service.RadarGovernanceScope, modelRoute string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_gate_reevaluation_outbox (id,run_id,cause_type,cause_id,idempotency_key,payload)
		SELECT gen_random_uuid(),rs.run_id,$1,$2,
		       md5(rs.run_id::text || ':' || $3::text || ':a') || md5(rs.run_id::text || ':' || $3::text || ':b'),
		       jsonb_build_object('event_id',$3,'environment',$4::text,'scope_type',$5::text,'scope_id',$6::text,'model_route',NULLIF($7::text,''))
		FROM evaluation_release_subjects rs
		JOIN LATERAL (
			SELECT e.event_type,e.effective_at,e.expires_at
			FROM evaluation_release_subject_events e
			WHERE e.release_subject_id=rs.id
			  AND e.effective_at <= transaction_timestamp()
			ORDER BY e.sequence DESC LIMIT 1
		) active_release ON active_release.event_type='activated'
		 AND active_release.expires_at > transaction_timestamp()
		WHERE rs.canonical_subject->>'deployment_environment'=$4::text
		  AND ($5::text='global' OR (rs.canonical_subject->>'scope_type'=$5::text AND rs.canonical_subject->>'scope_id'=$6::text))
		  AND ($7::text='' OR EXISTS (
			SELECT 1 FROM evaluation_baselines b
			WHERE b.id=(rs.canonical_subject->>'baseline_id')::uuid AND b.model_route=$7::text
		  ))
		ON CONFLICT (idempotency_key) DO NOTHING`, causeType, causeID, eventID, scope.Environment, scope.ScopeType, scope.ScopeID, modelRoute)
	if err != nil {
		return fmt.Errorf("enqueue governance reevaluation: %w", err)
	}
	return nil
}

func (r *radarGovernanceRepository) RotateEvidenceSigningKey(ctx context.Context, input service.RotateEvidenceSigningKeyInput) (*service.EvidenceSigningKeyRecord, error) {
	input.KeyReference = strings.TrimSpace(input.KeyReference)
	if err := r.valid(); err != nil {
		return nil, err
	}
	if input.ID == uuid.Nil || input.ExpectedActiveKeyID == uuid.Nil || input.ID == input.ExpectedActiveKeyID ||
		input.KeyReference == "" || input.ExpectedActiveStateEpoch < 1 {
		return nil, service.ErrEvidenceSigningKeyTransition
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, fmt.Errorf("begin evidence signing key rotation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	active, err := loadActiveEvidenceSigningKeyForUpdate(ctx, tx)
	if err != nil {
		return nil, err
	}
	if active.ID != input.ExpectedActiveKeyID || active.StateEpoch != input.ExpectedActiveStateEpoch {
		return nil, service.ErrEvidenceSigningKeyConflict
	}
	previous, err := updateEvidenceSigningKeyState(ctx, tx, active.ID, active.StateEpoch, service.EvidenceSigningKeyVerifyOnly)
	if err != nil {
		return nil, err
	}
	if err := enqueueEvidenceSigningKeyReevaluation(ctx, tx, previous); err != nil {
		return nil, err
	}
	record := &service.EvidenceSigningKeyRecord{}
	if err := scanEvidenceSigningKey(tx.QueryRowContext(ctx, `
		INSERT INTO evaluation_evidence_signing_keys (id, key_reference, status, state_epoch)
		VALUES ($1, $2, 'active', 1)
		RETURNING id, key_reference, status, state_epoch, created_at, updated_at, revoked_at`, input.ID, input.KeyReference), record); err != nil {
		return nil, fmt.Errorf("insert active evidence signing key: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit evidence signing key rotation: %w", err)
	}
	return record, nil
}

func (r *radarGovernanceRepository) TransitionEvidenceSigningKey(ctx context.Context, input service.TransitionEvidenceSigningKeyInput) (*service.EvidenceSigningKeyRecord, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	if input.ID == uuid.Nil || input.ExpectedStateEpoch < 1 ||
		(input.Status != service.EvidenceSigningKeyVerifyOnly && input.Status != service.EvidenceSigningKeyRevoked) {
		return nil, service.ErrEvidenceSigningKeyTransition
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, fmt.Errorf("begin evidence signing key transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	current := &service.EvidenceSigningKeyRecord{}
	if err := scanEvidenceSigningKey(tx.QueryRowContext(ctx, `
		SELECT id, key_reference, status, state_epoch, created_at, updated_at, revoked_at
		FROM evaluation_evidence_signing_keys WHERE id=$1 FOR UPDATE`, input.ID), current); err != nil {
		return nil, fmt.Errorf("lock evidence signing key: %w", err)
	}
	if current.StateEpoch != input.ExpectedStateEpoch {
		return nil, service.ErrEvidenceSigningKeyConflict
	}
	if current.Status == input.Status {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return current, nil
	}
	if current.Status == service.EvidenceSigningKeyRevoked ||
		(current.Status == service.EvidenceSigningKeyVerifyOnly && input.Status != service.EvidenceSigningKeyRevoked) {
		return nil, service.ErrEvidenceSigningKeyTransition
	}
	record, err := updateEvidenceSigningKeyState(ctx, tx, current.ID, current.StateEpoch, input.Status)
	if err != nil {
		return nil, err
	}
	if err := enqueueEvidenceSigningKeyReevaluation(ctx, tx, record); err != nil {
		return nil, err
	}
	if record.Status == service.EvidenceSigningKeyRevoked {
		if err := observeRevokedEvidenceSigningKeyAlerts(ctx, tx, record); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit evidence signing key transition: %w", err)
	}
	return record, nil
}

type evidenceSigningKeyScanner interface {
	Scan(...any) error
}

func scanEvidenceSigningKey(scanner evidenceSigningKeyScanner, record *service.EvidenceSigningKeyRecord) error {
	var status string
	var revokedAt sql.NullTime
	if err := scanner.Scan(&record.ID, &record.KeyReference, &status, &record.StateEpoch, &record.CreatedAt, &record.UpdatedAt, &revokedAt); err != nil {
		return err
	}
	record.Status = service.EvidenceSigningKeyStatus(status)
	if revokedAt.Valid {
		record.RevokedAt = &revokedAt.Time
	}
	return nil
}

func loadActiveEvidenceSigningKeyForUpdate(ctx context.Context, tx *sql.Tx) (*service.EvidenceSigningKeyRecord, error) {
	record := &service.EvidenceSigningKeyRecord{}
	if err := scanEvidenceSigningKey(tx.QueryRowContext(ctx, `
		SELECT id, key_reference, status, state_epoch, created_at, updated_at, revoked_at
		FROM evaluation_evidence_signing_keys WHERE status='active' FOR UPDATE`), record); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrEvidenceSigningKeyUnavailable
		}
		return nil, fmt.Errorf("lock active evidence signing key: %w", err)
	}
	return record, nil
}

func updateEvidenceSigningKeyState(ctx context.Context, tx *sql.Tx, keyID uuid.UUID, expectedEpoch int64, status service.EvidenceSigningKeyStatus) (*service.EvidenceSigningKeyRecord, error) {
	record := &service.EvidenceSigningKeyRecord{}
	query := `
		UPDATE evaluation_evidence_signing_keys
		SET status=$2::text, state_epoch=state_epoch+1,
			revoked_at=CASE WHEN $2::text='revoked' THEN transaction_timestamp() ELSE NULL END,
			updated_at=transaction_timestamp()
		WHERE id=$1 AND state_epoch=$3
		RETURNING id, key_reference, status, state_epoch, created_at, updated_at, revoked_at`
	if status == service.EvidenceSigningKeyVerifyOnly {
		query = `
			UPDATE evaluation_evidence_signing_keys
			SET status='verify_only', state_epoch=state_epoch+1, revoked_at=NULL, updated_at=transaction_timestamp()
			WHERE id=$1 AND state_epoch=$2
			RETURNING id, key_reference, status, state_epoch, created_at, updated_at, revoked_at`
	}
	var err error
	if status == service.EvidenceSigningKeyVerifyOnly {
		err = scanEvidenceSigningKey(tx.QueryRowContext(ctx, query, keyID, expectedEpoch), record)
	} else {
		err = scanEvidenceSigningKey(tx.QueryRowContext(ctx, query, keyID, string(status), expectedEpoch), record)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrEvidenceSigningKeyConflict
	}
	if err != nil {
		return nil, fmt.Errorf("transition evidence signing key: %w", err)
	}
	return record, nil
}

func enqueueEvidenceSigningKeyReevaluation(ctx context.Context, tx *sql.Tx, record *service.EvidenceSigningKeyRecord) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_gate_reevaluation_outbox
			(id, run_id, cause_type, cause_id, idempotency_key, payload)
		SELECT gen_random_uuid(), affected.run_id, 'evidence', $1,
		       md5(affected.run_id::text || ':evidence-signing-key:' || $1::text || ':' || $3::text || ':a') ||
		       md5(affected.run_id::text || ':evidence-signing-key:' || $1::text || ':' || $3::text || ':b'),
		       jsonb_build_object('signing_key_id', $1::uuid, 'status', $2::text, 'state_epoch', $3::bigint)
		FROM (
			SELECT DISTINCT evaluation_run_id AS run_id
			FROM evaluation_route_evidence WHERE signing_key_id=$1
		) affected
		ON CONFLICT (idempotency_key) DO NOTHING`, record.ID, string(record.Status), record.StateEpoch)
	if err != nil {
		return fmt.Errorf("enqueue evidence signing key reevaluation: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT evaluation_run_id
		FROM evaluation_route_evidence WHERE signing_key_id=$1
		ORDER BY evaluation_run_id`, record.ID)
	if err != nil {
		return fmt.Errorf("load signing key affected runs: %w", err)
	}
	var runIDs []uuid.UUID
	for rows.Next() {
		var runID uuid.UUID
		if err := rows.Scan(&runID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan signing key affected run: %w", err)
		}
		runIDs = append(runIDs, runID)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close signing key affected runs: %w", err)
	}
	payload, err := json.Marshal(struct {
		SigningKeyID uuid.UUID                        `json:"signing_key_id"`
		Status       service.EvidenceSigningKeyStatus `json:"status"`
		StateEpoch   int64                            `json:"state_epoch"`
	}{record.ID, record.Status, record.StateEpoch})
	if err != nil {
		return fmt.Errorf("marshal signing key state outbox payload: %w", err)
	}
	sourceID := record.ID.String() + ":" + fmt.Sprint(record.StateEpoch)
	sourceHash := hashString(strings.Join([]string{
		"evidence-signing-key-state", record.ID.String(), string(record.Status), fmt.Sprint(record.StateEpoch),
	}, "\x00"))
	for _, runID := range runIDs {
		if _, err := enqueueEvaluationOutbox(ctx, tx, service.EnqueueEvaluationOutboxInput{
			EventType: "gate_reevaluation", RunID: runID,
			ScopeKey: "signing-key/" + record.ID.String(), AnalysisVersion: "evidence-signing-key-v1",
			SourceType: "evidence_signing_key_state", SourceID: sourceID,
			SourceHash: sourceHash, Payload: payload,
		}); err != nil {
			return fmt.Errorf("enqueue unified signing key reevaluation: %w", err)
		}
	}
	return nil
}

func observeRevokedEvidenceSigningKeyAlerts(ctx context.Context, tx *sql.Tx, record *service.EvidenceSigningKeyRecord) error {
	_, err := tx.ExecContext(ctx, `
		WITH affected AS (
			SELECT DISTINCT r.tenant_id,
			       regexp_replace(s.model_route, '^(baseline|candidate):', '') AS model_route,
			       c.capability_domain
			FROM evaluation_route_evidence e
			JOIN evaluation_assignments a ON a.id=e.assignment_id
			JOIN evaluation_samples s ON s.id=a.sample_id
			JOIN evaluation_cases c ON c.id=s.case_id
			JOIN evaluation_runs r ON r.id=e.evaluation_run_id
			WHERE e.signing_key_id=$1
		), policy AS (
			SELECT COALESCE(MAX(version), 1) AS policy_version FROM evaluation_gate_policies
		), updated AS (
			UPDATE evaluation_alerts alerts
			SET status='open', acknowledged_at=NULL, resolved_at=NULL,
			    first_seen_at=CASE WHEN alerts.status='resolved' THEN transaction_timestamp() ELSE alerts.first_seen_at END
			FROM affected, policy
			WHERE alerts.tenant_id=affected.tenant_id
			  AND alerts.model_route=affected.model_route
			  AND alerts.capability_domain=affected.capability_domain
			  AND alerts.cause='insufficient_evidence'
			  AND alerts.policy_version=policy.policy_version
			RETURNING alerts.id
		), inserted AS (
			INSERT INTO evaluation_alerts
				(id, tenant_id, model_route, capability_domain, cause, policy_version, status, severity, first_seen_at)
			SELECT gen_random_uuid(), affected.tenant_id, affected.model_route, affected.capability_domain,
			       'insufficient_evidence', policy.policy_version, 'open', 'P0', transaction_timestamp()
			FROM affected, policy
			WHERE NOT EXISTS (
				SELECT 1 FROM evaluation_alerts alerts
				WHERE alerts.tenant_id=affected.tenant_id
				  AND alerts.model_route=affected.model_route
				  AND alerts.capability_domain=affected.capability_domain
				  AND alerts.cause='insufficient_evidence'
				  AND alerts.policy_version=policy.policy_version
			)
			RETURNING id
		), alert_ids AS (
			SELECT id FROM updated UNION ALL SELECT id FROM inserted
		)
		INSERT INTO evaluation_alert_events (id, alert_id, kind, payload)
		SELECT gen_random_uuid(), id, 'observed',
		       jsonb_build_object('reason_code', 'evidence_signing_key_revoked',
		                          'signing_key_id', $1::uuid, 'state_epoch', $2::bigint)
		FROM alert_ids`, record.ID, record.StateEpoch)
	if err != nil {
		return fmt.Errorf("observe revoked evidence signing key alerts: %w", err)
	}
	return nil
}

func (r *radarGovernanceRepository) RecordGateDecision(ctx context.Context, input service.RadarGateDecisionInput) (*service.RadarGateDecisionRecord, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	id := uuid.New()
	var d service.RadarGateDecisionRecord
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, fmt.Errorf("begin record radar gate decision: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if input.ReleaseSubjectHash == "" {
		input.ReleaseSubjectHash = strings.Repeat("0", 64)
	}
	if input.CauseSetHash == "" {
		input.CauseSetHash = strings.Repeat("0", 64)
	}
	if len(input.SourceWatermark) == 0 {
		input.SourceWatermark = json.RawMessage(`{}`)
	}
	tenantID := int64(0)
	if scopedTenant, scoped := radarTenant(ctx); scoped {
		tenantID = scopedTenant
		if err := ensureRadarRunTenant(ctx, tx, input.RunID); err != nil {
			return nil, err
		}
		var policyOwner int64
		if err := tx.QueryRowContext(ctx, `SELECT tenant_id FROM evaluation_gate_policies WHERE id=$1`, input.PolicyID).Scan(&policyOwner); err != nil {
			return nil, err
		}
		if policyOwner != tenantID {
			return nil, service.ErrRadarForbidden
		}
	}
	if isRadarGateReliabilityWatermark(input.SourceWatermark) {
		if err := validateGateReliabilityWatermark(ctx, tx, input.RunID, input.PolicyID, input.SourceWatermark); err != nil {
			return nil, err
		}
		if err := validateCurrentGateAuthority(ctx, tx, input.RunID, input.PolicyID, input.ReleaseSubjectHash); err != nil {
			return nil, err
		}
	}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO evaluation_gate_decisions
			(id,run_id,baseline_id,policy_id,status,rule_ids,evidence,evidence_hash,release_subject_hash,source_watermark,supersedes_decision_id,cause_set_hash,tenant_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9,$10::jsonb,$11,$12,$13)
		ON CONFLICT (run_id,policy_id,evidence_hash) DO NOTHING
		RETURNING id,run_id,baseline_id,policy_id,status,rule_ids,evidence,evidence_hash,release_subject_hash,source_watermark,supersedes_decision_id,cause_set_hash,created_at`,
		id, input.RunID, input.BaselineID, input.PolicyID, input.Status, pq.Array(input.RuleIDs), string(input.Evidence), input.EvidenceHash,
		input.ReleaseSubjectHash, string(input.SourceWatermark), input.SupersedesDecisionID, input.CauseSetHash, tenantID).Scan(
		&d.ID, &d.RunID, &d.BaselineID, &d.PolicyID, &d.Status, &pqStringArray{&d.RuleIDs}, &d.Evidence, &d.EvidenceHash,
		&d.ReleaseSubjectHash, &d.SourceWatermark, &d.SupersedesDecisionID, &d.CauseSetHash, &d.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `
			SELECT id,run_id,baseline_id,policy_id,status,rule_ids,evidence,evidence_hash,release_subject_hash,source_watermark,supersedes_decision_id,cause_set_hash,created_at
			FROM evaluation_gate_decisions WHERE run_id=$1 AND policy_id=$2 AND evidence_hash=$3`, input.RunID, input.PolicyID, input.EvidenceHash).Scan(
			&d.ID, &d.RunID, &d.BaselineID, &d.PolicyID, &d.Status, &pqStringArray{&d.RuleIDs}, &d.Evidence, &d.EvidenceHash,
			&d.ReleaseSubjectHash, &d.SourceWatermark, &d.SupersedesDecisionID, &d.CauseSetHash, &d.CreatedAt)
	}
	if err != nil {
		return nil, fmt.Errorf("record radar gate decision: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_gate_decision_events (id, decision_id, event_type, supersedes_decision_id, source_watermark)
		VALUES ($1, $2, 'recorded', $3, $4::jsonb)
		ON CONFLICT DO NOTHING`, uuid.New(), d.ID, d.SupersedesDecisionID, string(d.SourceWatermark)); err != nil {
		return nil, fmt.Errorf("record radar gate decision event: %w", err)
	}
	current, err := advanceRadarGateDecisionHead(ctx, tx, &d)
	if err != nil {
		return nil, err
	}
	if current {
		if err := reconcileRevisionBatchesForGateDecision(ctx, tx, &d); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit record radar gate decision: %w", err)
	}
	return &d, nil
}

func recordRadarGateDecisionTx(ctx context.Context, tx *sql.Tx, input service.RadarGateDecisionInput, tenantID int64) (*service.RadarGateDecisionRecord, error) {
	if tx == nil {
		return nil, errors.New("radar gate decision transaction is required")
	}
	id := uuid.New()
	var d service.RadarGateDecisionRecord
	inserted := true
	err := tx.QueryRowContext(ctx, `
		INSERT INTO evaluation_gate_decisions
			(id,run_id,baseline_id,policy_id,status,rule_ids,evidence,evidence_hash,release_subject_hash,source_watermark,supersedes_decision_id,cause_set_hash,tenant_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9,$10::jsonb,$11,$12,$13)
		ON CONFLICT (run_id,policy_id,evidence_hash) DO NOTHING
		RETURNING id,run_id,baseline_id,policy_id,status,rule_ids,evidence,evidence_hash,release_subject_hash,source_watermark,supersedes_decision_id,cause_set_hash,created_at`,
		id, input.RunID, input.BaselineID, input.PolicyID, input.Status, pq.Array(input.RuleIDs), string(input.Evidence), input.EvidenceHash,
		input.ReleaseSubjectHash, string(input.SourceWatermark), input.SupersedesDecisionID, input.CauseSetHash, tenantID).Scan(
		&d.ID, &d.RunID, &d.BaselineID, &d.PolicyID, &d.Status, &pqStringArray{&d.RuleIDs}, &d.Evidence, &d.EvidenceHash,
		&d.ReleaseSubjectHash, &d.SourceWatermark, &d.SupersedesDecisionID, &d.CauseSetHash, &d.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		inserted = false
		var storedTenantID int64
		err = tx.QueryRowContext(ctx, `
			SELECT id,run_id,baseline_id,policy_id,status,rule_ids,evidence,evidence_hash,release_subject_hash,source_watermark,supersedes_decision_id,cause_set_hash,created_at,tenant_id
			FROM evaluation_gate_decisions WHERE run_id=$1 AND policy_id=$2 AND evidence_hash=$3`, input.RunID, input.PolicyID, input.EvidenceHash).Scan(
			&d.ID, &d.RunID, &d.BaselineID, &d.PolicyID, &d.Status, &pqStringArray{&d.RuleIDs}, &d.Evidence, &d.EvidenceHash,
			&d.ReleaseSubjectHash, &d.SourceWatermark, &d.SupersedesDecisionID, &d.CauseSetHash, &d.CreatedAt, &storedTenantID)
		if err == nil && (storedTenantID != tenantID || d.ReleaseSubjectHash != input.ReleaseSubjectHash || d.CauseSetHash != input.CauseSetHash) {
			return nil, service.ErrGovernanceHeadConflict
		}
	}
	if err != nil {
		return nil, fmt.Errorf("record radar gate decision: %w", err)
	}
	if inserted {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO evaluation_gate_decision_events (id, decision_id, event_type, supersedes_decision_id, source_watermark)
			VALUES ($1, $2, 'recorded', $3, $4::jsonb)`, uuid.New(), d.ID, d.SupersedesDecisionID, string(d.SourceWatermark)); err != nil {
			return nil, fmt.Errorf("record radar gate decision event: %w", err)
		}
	}
	current, err := advanceRadarGateDecisionHead(ctx, tx, &d)
	if err != nil {
		return nil, err
	}
	if current {
		if err := reconcileRevisionBatchesForGateDecision(ctx, tx, &d); err != nil {
			return nil, err
		}
	}
	return &d, nil
}

func advanceRadarGateDecisionHead(ctx context.Context, tx *sql.Tx, decision *service.RadarGateDecisionRecord) (bool, error) {
	if decision == nil {
		return false, service.ErrRevisionBatchInvalid
	}
	lockKey := strings.Join([]string{
		"gate-decision-head", decision.RunID.String(), decision.PolicyID.String(), decision.ReleaseSubjectHash,
	}, ":")
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return false, fmt.Errorf("lock radar gate decision head: %w", err)
	}
	var currentID uuid.UUID
	err := tx.QueryRowContext(ctx, `
		SELECT decision_id FROM evaluation_gate_decision_heads
		WHERE run_id=$1 AND policy_id=$2 AND release_subject_hash=$3
		FOR UPDATE`, decision.RunID, decision.PolicyID, decision.ReleaseSubjectHash).Scan(&currentID)
	if errors.Is(err, sql.ErrNoRows) {
		if decision.SupersedesDecisionID != nil {
			return false, infraerrors.Conflict("GATE_DECISION_HEAD_CONFLICT", "gate decision cannot supersede a missing head")
		}
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.evaluation_head_cas','1',true)`); err != nil {
			return false, fmt.Errorf("enable radar gate decision head mutation: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO evaluation_gate_decision_heads (
				run_id, policy_id, release_subject_hash, decision_id
			) VALUES ($1,$2,$3,$4)`, decision.RunID, decision.PolicyID,
			decision.ReleaseSubjectHash, decision.ID); err != nil {
			return false, fmt.Errorf("insert radar gate decision head: %w", err)
		}
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("load radar gate decision head: %w", err)
	}
	if currentID == decision.ID {
		return true, nil
	}
	if decision.SupersedesDecisionID == nil || *decision.SupersedesDecisionID != currentID {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.evaluation_head_cas','1',true)`); err != nil {
		return false, fmt.Errorf("enable radar gate decision head mutation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE evaluation_gate_decision_heads
		SET decision_id=$4, updated_at=transaction_timestamp()
		WHERE run_id=$1 AND policy_id=$2 AND release_subject_hash=$3`,
		decision.RunID, decision.PolicyID, decision.ReleaseSubjectHash, decision.ID); err != nil {
		return false, fmt.Errorf("advance radar gate decision head: %w", err)
	}
	return true, nil
}

func reconcileRevisionBatchesForGateDecision(ctx context.Context, tx *sql.Tx, decision *service.RadarGateDecisionRecord) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT batch.id
		FROM evaluation_revision_batches batch
		JOIN evaluation_revision_batch_requirements requirement
		  ON requirement.revision_batch_id=batch.id AND requirement.run_id=batch.run_id
		WHERE batch.run_id=$1 AND batch.status IN ('pending','running','blocked')
		  AND requirement.requirement_type='gate'
		  AND requirement.status='completed'
		  AND requirement.cause_set_hash=$2
		ORDER BY batch.id`, decision.RunID, decision.CauseSetHash)
	if err != nil {
		return fmt.Errorf("load revision batches for Gate decision: %w", err)
	}
	var batchIDs []uuid.UUID
	for rows.Next() {
		var batchID uuid.UUID
		if err := rows.Scan(&batchID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan revision batch for Gate decision: %w", err)
		}
		batchIDs = append(batchIDs, batchID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate revision batches for Gate decision: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close revision batches for Gate decision: %w", err)
	}
	for _, batchID := range batchIDs {
		if err := reconcileRevisionBatch(ctx, tx, batchID); err != nil {
			return err
		}
	}
	return nil
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
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	id := uuid.New()
	var w service.RadarGateWaiverRecord
	err = tx.QueryRowContext(ctx, `INSERT INTO evaluation_gate_waivers (id,decision_id,business_reason,risk_owner_user_id,mitigation,retest_plan,expires_at,approved_by) VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id,decision_id,business_reason,risk_owner_user_id,mitigation,retest_plan,expires_at,approved_by,created_at`, id, input.DecisionID, input.BusinessReason, input.RiskOwnerUserID, input.Mitigation, input.RetestPlan, input.ExpiresAt, input.ApprovedBy).Scan(&w.ID, &w.DecisionID, &w.BusinessReason, &w.RiskOwnerUserID, &w.Mitigation, &w.RetestPlan, &w.ExpiresAt, &w.ApprovedBy, &w.CreatedAt)
	if err != nil {
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
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	id := uuid.New()
	var a service.RadarAlertRecord
	observed := input.ObservedAt
	if observed.IsZero() {
		observed = time.Now().UTC()
	}
	tenantID := int64(0)
	if scopedTenant, scoped := radarTenant(ctx); scoped {
		tenantID = scopedTenant
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO evaluation_alerts (id,tenant_id,model_route,capability_domain,cause,policy_version,status,severity,attribution_confidence,first_seen_at) VALUES ($1,$2,$3,$4,$5,$6,'open',$7,$8,$9) ON CONFLICT (tenant_id,model_route,capability_domain,cause,policy_version) DO UPDATE SET status=CASE WHEN evaluation_alerts.status='resolved' THEN 'open' ELSE evaluation_alerts.status END, first_seen_at=CASE WHEN evaluation_alerts.status='resolved' THEN EXCLUDED.first_seen_at ELSE evaluation_alerts.first_seen_at END RETURNING id,model_route,capability_domain,cause,policy_version,status,severity,attribution_confidence,first_seen_at,acknowledged_at,resolved_at,recovery_test_id`, id, tenantID, input.ModelRoute, input.CapabilityDomain, input.Cause, input.PolicyVersion, input.Severity, input.Confidence, observed).Scan(&a.ID, &a.ModelRoute, &a.CapabilityDomain, &a.Cause, &a.PolicyVersion, &a.Status, &a.Severity, &a.AttributionConfidence, &a.FirstSeenAt, &a.AcknowledgedAt, &a.ResolvedAt, &a.RecoveryTestID)
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
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	query := `UPDATE evaluation_alerts SET ` + update + ` WHERE id=$1 AND status<>'resolved'`
	args := []any{id}
	if tenantID, scoped := radarTenant(ctx); scoped {
		query += ` AND tenant_id=$2`
		args = append(args, tenantID)
	}
	res, err := tx.ExecContext(ctx, query, args...)
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
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	query := `UPDATE evaluation_alerts SET recovery_test_id=$2 WHERE id=$1`
	args := []any{id, recoveryTestID}
	if tenantID, scoped := radarTenant(ctx); scoped {
		query += ` AND tenant_id=$3`
		args = append(args, tenantID)
	}
	if _, err = tx.ExecContext(ctx, query, args...); err != nil {
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
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, fmt.Errorf("begin record radar attribution: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	query := `INSERT INTO evaluation_attributions (id,alert_id,cause,confidence,route_slices,evidence_hash) SELECT $1,a.id,$3,$4,$5::jsonb,$6 FROM evaluation_alerts a WHERE a.id=$2`
	args := []any{id, input.AlertID, input.Cause, input.Confidence, string(input.RouteSlices), input.EvidenceHash}
	if tenantID, scoped := radarTenant(ctx); scoped {
		query += ` AND a.tenant_id=$7`
		args = append(args, tenantID)
	}
	query += ` RETURNING id,alert_id,cause,confidence,route_slices,evidence_hash,created_at`
	err = tx.QueryRowContext(ctx, query, args...).Scan(&a.ID, &a.AlertID, &a.Cause, &a.Confidence, &a.RouteSlices, &a.EvidenceHash, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit record radar attribution: %w", err)
	}
	return &a, nil
}
func (r *radarGovernanceRepository) GetAlert(ctx context.Context, id uuid.UUID) (*service.RadarAlertRecord, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	var a service.RadarAlertRecord
	query := `SELECT id,model_route,capability_domain,cause,policy_version,status,severity,attribution_confidence,first_seen_at,acknowledged_at,resolved_at,recovery_test_id FROM evaluation_alerts WHERE id=$1`
	args := []any{id}
	if tenantID, scoped := radarTenant(ctx); scoped {
		query += ` AND tenant_id=$2`
		args = append(args, tenantID)
	}
	err := r.db.QueryRowContext(ctx, query, args...).Scan(&a.ID, &a.ModelRoute, &a.CapabilityDomain, &a.Cause, &a.PolicyVersion, &a.Status, &a.Severity, &a.AttributionConfidence, &a.FirstSeenAt, &a.AcknowledgedAt, &a.ResolvedAt, &a.RecoveryTestID)
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
	query := `
		SELECT model_route, capability_domain,
		       aggregate->>'baseline_score', aggregate->>'candidate_score',
		       aggregate->>'delta_pp', aggregate->>'ci_low_pp', aggregate->>'ci_high_pp',
		       aggregate->>'effective_pair_count', window_start
		FROM (
			SELECT DISTINCT ON (model_route, capability_domain)
			       s.model_route, s.capability_domain, s.aggregate, s.window_start
			FROM evaluation_aggregate_snapshots s
			JOIN evaluation_runs r ON r.id=s.run_id
			WHERE 1=1
			`
	args := []any{}
	if tenantID, scoped := radarTenant(ctx); scoped {
		query += ` AND r.tenant_id=$1`
		args = append(args, tenantID)
	}
	query += `
			ORDER BY model_route, capability_domain, window_start DESC, created_at DESC
		) latest
		ORDER BY model_route, capability_domain`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list radar model health: %w", err)
	}
	defer func() { _ = rows.Close() }()
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
	query := `
		SELECT r.id, r.plan_id, r.trigger_source, r.status,
			CASE WHEN EXISTS (SELECT 1 FROM evaluation_pair_specs p WHERE p.run_id = r.id)
				AND (SELECT COUNT(*) FROM evaluation_pair_specs p WHERE p.run_id = r.id) =
					(SELECT COUNT(*) FROM evaluation_pair_bindings b JOIN evaluation_pair_specs p ON p.id = b.pair_spec_id WHERE p.run_id = r.id)
				AND (SELECT COUNT(*) FROM evaluation_side_specs s JOIN evaluation_pair_specs p ON p.id = s.pair_spec_id WHERE p.run_id = r.id) =
					2 * (SELECT COUNT(*) FROM evaluation_pair_specs p WHERE p.run_id = r.id)
				THEN 'bound' ELSE 'legacy-unbound' END AS contract_status,
			r.created_at, r.started_at, r.finished_at
		FROM evaluation_runs r`
	args := []any{}
	if tenantID, scoped := radarTenant(ctx); scoped {
		query += ` WHERE r.tenant_id=$1`
		args = append(args, tenantID)
	}
	query += ` ORDER BY r.created_at DESC LIMIT 200`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list radar runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []service.RadarRunProjection
	for rows.Next() {
		var item service.RadarRunProjection
		if err := rows.Scan(&item.ID, &item.PlanID, &item.TriggerSource, &item.Status, &item.ContractStatus, &item.CreatedAt, &item.StartedAt, &item.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r *radarGovernanceRepository) ListAlerts(ctx context.Context) ([]service.RadarAlertProjection, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	query := `SELECT id, model_route, capability_domain, cause, severity, status, attribution_confidence, first_seen_at FROM evaluation_alerts`
	args := []any{}
	if tenantID, scoped := radarTenant(ctx); scoped {
		query += ` WHERE tenant_id=$1`
		args = append(args, tenantID)
	}
	query += ` ORDER BY first_seen_at DESC LIMIT 200`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list radar alerts: %w", err)
	}
	defer func() { _ = rows.Close() }()
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
	query := `SELECT d.id, d.run_id, d.status, COALESCE(d.rule_ids[1], ''), d.created_at FROM evaluation_gate_decisions d`
	args := []any{}
	if tenantID, scoped := radarTenant(ctx); scoped {
		query += ` WHERE d.tenant_id=$1`
		args = append(args, tenantID)
	}
	query += ` ORDER BY d.created_at DESC LIMIT 200`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list radar gates: %w", err)
	}
	defer func() { _ = rows.Close() }()
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
	query := `SELECT id, name, worker_kind, status, last_heartbeat_at, capabilities FROM evaluation_workers`
	args := []any{}
	if tenantID, scoped := radarTenant(ctx); scoped {
		query += ` WHERE tenant_id=$1`
		args = append(args, tenantID)
	}
	query += ` ORDER BY name`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list radar workers: %w", err)
	}
	defer func() { _ = rows.Close() }()
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

func normalizeWorkerCapabilities(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func workerFingerprint(token string) string { return hashString(strings.TrimSpace(token))[:12] }

func validateWorkerRegistration(input service.RadarWorkerRegistrationInput) error {
	if strings.TrimSpace(input.Name) == "" || strings.TrimSpace(input.WorkerKind) == "" || strings.TrimSpace(input.Token) == "" {
		return infraerrors.New(http.StatusBadRequest, "WORKER_INVALID", "worker name, kind and token are required")
	}
	if input.WorkerKind != "runner" && input.WorkerKind != "grader" && input.WorkerKind != "statistics" {
		return infraerrors.New(http.StatusBadRequest, "WORKER_INVALID", "unsupported worker kind")
	}
	if input.MaxConcurrency <= 0 {
		input.MaxConcurrency = 1
	}
	if input.MaxConcurrency > 1000 {
		return infraerrors.New(http.StatusBadRequest, "WORKER_INVALID", "max concurrency is too large")
	}
	return nil
}

func scanRadarWorker(row interface{ Scan(...any) error }) (*service.RadarWorkerRecord, error) {
	var worker service.RadarWorkerRecord
	var capabilities pq.StringArray
	if err := row.Scan(&worker.ID, &worker.Name, &worker.WorkerKind, &worker.Status, &worker.ClaimMode, &worker.TokenEpoch, &worker.TokenFingerprint, &worker.Region, &worker.ImageDigest, &capabilities, &worker.MaxConcurrency, &worker.CreatedAt, &worker.UpdatedAt); err != nil {
		return nil, err
	}
	worker.Capabilities = []string(capabilities)
	return &worker, nil
}

func loadRadarWorkerEventReplay(ctx context.Context, tx *sql.Tx, key string, workerID uuid.UUID, eventType, payloadKey, payloadValue string) (*service.RadarWorkerRecord, bool, error) {
	var eventWorkerID uuid.UUID
	var existingType string
	var payload []byte
	err := tx.QueryRowContext(ctx, `SELECT worker_id, event_type, payload FROM evaluation_worker_events WHERE idempotency_key = $1 FOR UPDATE`, key).Scan(&eventWorkerID, &existingType, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load radar worker idempotency event: %w", err)
	}
	if (workerID != uuid.Nil && eventWorkerID != workerID) || existingType != eventType {
		return nil, false, infraerrors.Conflict("WORKER_IDEMPOTENCY_CONFLICT", "idempotency key belongs to another worker action")
	}
	if payloadKey != "" {
		var body map[string]any
		if err := json.Unmarshal(payload, &body); err != nil || strings.TrimSpace(fmt.Sprint(body[payloadKey])) != payloadValue {
			return nil, false, infraerrors.Conflict("WORKER_IDEMPOTENCY_CONFLICT", "idempotency key was reused with different parameters")
		}
	}
	if workerID == uuid.Nil {
		workerID = eventWorkerID
	}
	worker, err := scanRadarWorker(tx.QueryRowContext(ctx, `SELECT id, name, worker_kind, status, claim_mode, token_epoch, token_fingerprint, region, image_digest, capabilities, max_concurrency, created_at, updated_at FROM evaluation_workers WHERE id = $1 FOR UPDATE`, workerID))
	if err != nil {
		return nil, false, fmt.Errorf("load radar worker idempotent result: %w", err)
	}
	return worker, true, nil
}

func countRadarWorkerActiveLeasesTx(ctx context.Context, tx *sql.Tx, workerID uuid.UUID) (int, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM evaluation_assignments WHERE leased_by=$1 AND lease_expires_at > NOW()) + (SELECT COUNT(*) FROM evaluation_grading_jobs WHERE leased_by=$1 AND lease_expires_at > NOW()) + (SELECT COUNT(*) FROM evaluation_analysis_jobs WHERE leased_by=$1 AND lease_expires_at > NOW())`, workerID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count radar worker active leases: %w", err)
	}
	return count, nil
}

// checkRadarWorkerDrainCompletionTx is shared by control actions and lease
// release paths. The deterministic event key makes repeated checks harmless.
func checkRadarWorkerDrainCompletionTx(ctx context.Context, tx *sql.Tx, workerID uuid.UUID, actorID int64, baseKey string) (int, error) {
	var mode service.WorkerClaimMode
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT claim_mode, status FROM evaluation_workers WHERE id = $1 FOR UPDATE`, workerID).Scan(&mode, &status); err != nil {
		return 0, fmt.Errorf("load radar worker drain state: %w", err)
	}
	count, err := countRadarWorkerActiveLeasesTx(ctx, tx, workerID)
	if err != nil {
		return 0, err
	}
	if status == "active" && mode == service.WorkerClaimsDraining && count == 0 {
		var actor any
		if actorID > 0 {
			actor = actorID
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO evaluation_worker_events (id, worker_id, event_type, idempotency_key, actor_id, payload) VALUES ($1,$2,'drain_completed',$3,$4,jsonb_build_object('active_lease_count',0)) ON CONFLICT (idempotency_key) DO NOTHING`, uuid.New(), workerID, hashString(strings.TrimSpace(baseKey)+":drain_completed"), actor); err != nil {
			return 0, fmt.Errorf("record radar worker drain completion: %w", err)
		}
	}
	return count, nil
}

func (r *radarGovernanceRepository) RegisterRadarWorker(ctx context.Context, input service.RadarWorkerRegistrationInput, actorID int64, idempotencyKey string) (*service.RadarWorkerRecord, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	input.Token = strings.TrimSpace(input.Token)
	if err := validateWorkerRegistration(input); err != nil {
		return nil, err
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if len(idempotencyKey) != 64 {
		return nil, infraerrors.New(http.StatusBadRequest, "IDEMPOTENCY_KEY_INVALID", "idempotency key must be 64 characters")
	}
	input.Name, input.WorkerKind, input.Region, input.ImageDigest = strings.TrimSpace(input.Name), strings.TrimSpace(input.WorkerKind), strings.TrimSpace(input.Region), strings.TrimSpace(input.ImageDigest)
	if input.MaxConcurrency <= 0 {
		input.MaxConcurrency = 1
	}
	caps := normalizeWorkerCapabilities(input.Capabilities)
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if replay, found, err := loadRadarWorkerEventReplay(ctx, tx, idempotencyKey, uuid.Nil, "registered", "name", input.Name); err != nil {
		return nil, err
	} else if found {
		if err := ensureScopedRadarWorkerTenant(ctx, tx, replay.ID); err != nil {
			return nil, err
		}
		if replay.TokenFingerprint != workerFingerprint(input.Token) {
			return nil, infraerrors.Conflict("WORKER_IDEMPOTENCY_CONFLICT", "idempotency key was reused with different credentials")
		}
		return replay, tx.Commit()
	}
	var existing service.RadarWorkerRecord
	var existingCaps pq.StringArray
	var existingHash string
	err = tx.QueryRowContext(ctx, `SELECT id, name, worker_kind, region, image_digest, capabilities, token_hash, status, claim_mode, token_epoch, token_fingerprint, max_concurrency, created_at, updated_at FROM evaluation_workers WHERE name = $1 FOR UPDATE`, input.Name).Scan(&existing.ID, &existing.Name, &existing.WorkerKind, &existing.Region, &existing.ImageDigest, &existingCaps, &existingHash, &existing.Status, &existing.ClaimMode, &existing.TokenEpoch, &existing.TokenFingerprint, &existing.MaxConcurrency, &existing.CreatedAt, &existing.UpdatedAt)
	if err == nil {
		if err := ensureScopedRadarWorkerTenant(ctx, tx, existing.ID); err != nil {
			return nil, err
		}
		existing.Capabilities = []string(existingCaps)
		if existingHash != hashString(input.Token) || existing.WorkerKind != input.WorkerKind || existing.Region != input.Region || existing.ImageDigest != input.ImageDigest || strings.Join(existing.Capabilities, "\x00") != strings.Join(caps, "\x00") {
			return nil, infraerrors.Conflict("WORKER_IDENTITY_CONFLICT", "worker identity already exists with different credentials or metadata")
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return &existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("load radar worker: %w", err)
	}
	var tokenOwner uuid.UUID
	if tokenErr := tx.QueryRowContext(ctx, `SELECT id FROM evaluation_workers WHERE token_hash = $1 FOR UPDATE`, hashString(input.Token)).Scan(&tokenOwner); tokenErr == nil {
		return nil, infraerrors.Conflict("WORKER_IDENTITY_CONFLICT", "worker token is already registered to another identity")
	} else if !errors.Is(tokenErr, sql.ErrNoRows) {
		return nil, fmt.Errorf("check radar worker token: %w", tokenErr)
	}
	tenantID := actorID
	if scopedTenant, scoped := radarTenant(ctx); scoped {
		tenantID = scopedTenant
	}
	worker := &service.RadarWorkerRecord{}
	err = tx.QueryRowContext(ctx, `INSERT INTO evaluation_workers (id, name, worker_kind, region, image_digest, capabilities, max_concurrency, token_hash, token_fingerprint, token_epoch, status, claim_mode, tenant_id) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,0,'active','open',$10) RETURNING id, name, worker_kind, status, claim_mode, token_epoch, token_fingerprint, region, image_digest, capabilities, max_concurrency, created_at, updated_at`, uuid.New(), input.Name, input.WorkerKind, input.Region, input.ImageDigest, pq.Array(caps), input.MaxConcurrency, hashString(input.Token), workerFingerprint(input.Token), tenantID).Scan(&worker.ID, &worker.Name, &worker.WorkerKind, &worker.Status, &worker.ClaimMode, &worker.TokenEpoch, &worker.TokenFingerprint, &worker.Region, &worker.ImageDigest, &existingCaps, &worker.MaxConcurrency, &worker.CreatedAt, &worker.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("register radar worker: %w", err)
	}
	worker.Capabilities = []string(existingCaps)
	if _, err := tx.ExecContext(ctx, `INSERT INTO evaluation_worker_events (id, worker_id, event_type, idempotency_key, actor_id, payload) VALUES ($1,$2,'registered',$3,$4,jsonb_build_object('token_fingerprint',$5,'worker_kind',$6,'name',$7))`, uuid.New(), worker.ID, idempotencyKey, actorID, worker.TokenFingerprint, worker.WorkerKind, worker.Name); err != nil {
		return nil, fmt.Errorf("audit radar worker registration: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return worker, nil
}

func (r *radarGovernanceRepository) RotateRadarWorkerToken(ctx context.Context, workerID uuid.UUID, token string, actorID int64, idempotencyKey string) (*service.RadarWorkerRecord, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	token = strings.TrimSpace(token)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if workerID == uuid.Nil || token == "" || len(idempotencyKey) != 64 {
		return nil, infraerrors.New(http.StatusBadRequest, "WORKER_INVALID", "worker, token and 64 character idempotency key are required")
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := ensureScopedRadarWorkerTenant(ctx, tx, workerID); err != nil {
		return nil, err
	}
	if replay, found, err := loadRadarWorkerEventReplay(ctx, tx, idempotencyKey, workerID, "token_rotated", "token_fingerprint", workerFingerprint(token)); err != nil {
		return nil, err
	} else if found {
		return replay, tx.Commit()
	}
	var tokenOwner uuid.UUID
	if err := tx.QueryRowContext(ctx, `SELECT id FROM evaluation_workers WHERE token_hash = $1 FOR UPDATE`, hashString(token)).Scan(&tokenOwner); err == nil {
		return nil, infraerrors.Conflict("WORKER_TOKEN_CONFLICT", "worker token is already registered")
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("check radar worker token: %w", err)
	}
	worker, err := scanRadarWorker(tx.QueryRowContext(ctx, `UPDATE evaluation_workers SET token_hash=$1, token_fingerprint=$2, token_epoch=token_epoch+1, updated_at=NOW() WHERE id=$3 AND status='active' RETURNING id, name, worker_kind, status, claim_mode, token_epoch, token_fingerprint, region, image_digest, capabilities, max_concurrency, created_at, updated_at`, hashString(token), workerFingerprint(token), workerID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, infraerrors.New(http.StatusConflict, "WORKER_UNAVAILABLE", "worker is unavailable")
		}
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO evaluation_worker_events (id, worker_id, event_type, idempotency_key, actor_id, payload) VALUES ($1,$2,'token_rotated',$3,$4,jsonb_build_object('token_fingerprint',$5,'token_epoch',$6))`, uuid.New(), workerID, idempotencyKey, actorID, worker.TokenFingerprint, worker.TokenEpoch); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return worker, nil
}

func (r *radarGovernanceRepository) SetRadarWorkerClaimMode(ctx context.Context, workerID uuid.UUID, mode service.WorkerClaimMode, actorID int64, idempotencyKey string) (*service.RadarWorkerRecord, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	if workerID == uuid.Nil || len(strings.TrimSpace(idempotencyKey)) != 64 {
		return nil, infraerrors.New(http.StatusBadRequest, "WORKER_INVALID", "worker and 64 character idempotency key are required")
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	eventType := map[service.WorkerClaimMode]string{service.WorkerClaimsOpen: "claims_resumed", service.WorkerClaimsPaused: "claims_paused", service.WorkerClaimsDraining: "draining"}[mode]
	if eventType == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "WORKER_INVALID", "unsupported worker claim mode")
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := ensureScopedRadarWorkerTenant(ctx, tx, workerID); err != nil {
		return nil, err
	}
	if replay, found, err := loadRadarWorkerEventReplay(ctx, tx, idempotencyKey, workerID, eventType, "claim_mode", string(mode)); err != nil {
		return nil, err
	} else if found {
		return replay, tx.Commit()
	}
	worker, err := scanRadarWorker(tx.QueryRowContext(ctx, `UPDATE evaluation_workers SET claim_mode=$1, drain_requested_at=CASE WHEN $1='draining' THEN COALESCE(drain_requested_at,NOW()) ELSE NULL END, updated_at=NOW() WHERE id=$2 AND status='active' RETURNING id, name, worker_kind, status, claim_mode, token_epoch, token_fingerprint, region, image_digest, capabilities, max_concurrency, created_at, updated_at`, mode, workerID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, infraerrors.New(http.StatusConflict, "WORKER_UNAVAILABLE", "worker is unavailable")
		}
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO evaluation_worker_events (id, worker_id, event_type, idempotency_key, actor_id, payload) VALUES ($1,$2,$3,$4,$5,jsonb_build_object('claim_mode',$6))`, uuid.New(), workerID, eventType, idempotencyKey, actorID, mode); err != nil {
		return nil, err
	}
	if mode == service.WorkerClaimsDraining {
		var err error
		worker.ActiveLeaseCount, err = checkRadarWorkerDrainCompletionTx(ctx, tx, workerID, actorID, idempotencyKey)
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return worker, nil
}

func (r *radarGovernanceRepository) DisableRadarWorker(ctx context.Context, workerID uuid.UUID, actorID int64, idempotencyKey string) (*service.RadarWorkerRecord, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	if workerID == uuid.Nil || len(strings.TrimSpace(idempotencyKey)) != 64 {
		return nil, infraerrors.New(http.StatusBadRequest, "WORKER_INVALID", "worker and 64 character idempotency key are required")
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := ensureScopedRadarWorkerTenant(ctx, tx, workerID); err != nil {
		return nil, err
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if replay, found, err := loadRadarWorkerEventReplay(ctx, tx, idempotencyKey, workerID, "disabled", "", ""); err != nil {
		return nil, err
	} else if found {
		return replay, tx.Commit()
	}
	worker, err := scanRadarWorker(tx.QueryRowContext(ctx, `UPDATE evaluation_workers SET status='disabled', disabled_at=NOW(), updated_at=NOW() WHERE id=$1 RETURNING id, name, worker_kind, status, claim_mode, token_epoch, token_fingerprint, region, image_digest, capabilities, max_concurrency, created_at, updated_at`, workerID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, infraerrors.New(http.StatusConflict, "WORKER_UNAVAILABLE", "worker is unavailable")
		}
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO evaluation_worker_events (id, worker_id, event_type, idempotency_key, actor_id, payload) VALUES ($1,$2,'disabled',$3,$4,jsonb_build_object('token_fingerprint',$5))`, uuid.New(), workerID, idempotencyKey, actorID, worker.TokenFingerprint); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return worker, nil
}

func (r *radarGovernanceRepository) ListDatasets(ctx context.Context) ([]service.RadarDatasetProjection, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	query := `
		SELECT d.id, d.dataset_key, d.version, d.status, COUNT(c.id), d.created_at
		FROM evaluation_dataset_versions d
		LEFT JOIN evaluation_cases c ON c.dataset_version_id = d.id
		`
	args := []any{}
	if tenantID, scoped := radarTenant(ctx); scoped {
		query += ` WHERE d.tenant_id=$1`
		args = append(args, tenantID)
	}
	query += `
		GROUP BY d.id, d.dataset_key, d.version, d.status, d.created_at
		ORDER BY d.created_at DESC LIMIT 200`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list radar datasets: %w", err)
	}
	defer func() { _ = rows.Close() }()
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

var _ service.RadarWorkerRepository = (*radarGovernanceRepository)(nil)
