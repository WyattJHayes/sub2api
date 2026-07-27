package service

import (
	"context"
	"errors"
	"sort"
	"sync"
)

type RadarRole string

const (
	RoleViewer         RadarRole = "viewer"
	RoleTestOperator   RadarRole = "test_operator"
	RoleQualityAdmin   RadarRole = "quality_admin"
	RoleReleaseManager RadarRole = "release_manager"
	RolePlatformAdmin  RadarRole = "platform_admin"
)

type RadarPermission string

const (
	PermissionView                   RadarPermission = "view"
	PermissionRunStart               RadarPermission = "run_start"
	PermissionRunRetry               RadarPermission = "run_retry"
	PermissionWorkerManage           RadarPermission = "worker_manage"
	PermissionDatasetManage          RadarPermission = "dataset_manage"
	PermissionDatasetPublish         RadarPermission = "dataset_publish"
	PermissionPolicyManage           RadarPermission = "policy_manage"
	PermissionBaselineQualityApprove RadarPermission = "baseline_quality_approve"
	PermissionGateDecide             RadarPermission = "gate_decide"
	PermissionGateWaive              RadarPermission = "gate_waive"
	PermissionBaselineReleaseApprove RadarPermission = "baseline_release_approve"
	PermissionRoleManage             RadarPermission = "role_manage"
	PermissionRouteAction            RadarPermission = "route_action"
	PermissionEvaluationKeyManage    RadarPermission = "evaluation_key_manage"
)

var ErrRadarForbidden = errors.New("radar permission denied")

var radarRolePermissions = map[RadarRole][]RadarPermission{
	RoleViewer:         {PermissionView},
	RoleTestOperator:   {PermissionView, PermissionRunStart, PermissionRunRetry, PermissionWorkerManage},
	RoleQualityAdmin:   {PermissionView, PermissionDatasetManage, PermissionDatasetPublish, PermissionPolicyManage, PermissionBaselineQualityApprove},
	RoleReleaseManager: {PermissionView, PermissionGateDecide, PermissionGateWaive, PermissionBaselineReleaseApprove},
	RolePlatformAdmin:  {PermissionView, PermissionRoleManage, PermissionRouteAction, PermissionEvaluationKeyManage},
}

type RadarAuthorizer interface {
	Require(ctx context.Context, actorID int64, permission RadarPermission) error
	ListPermissions(ctx context.Context, actorID int64) ([]RadarPermission, error)
}

type StaticRadarAuthorizer struct {
	mu      sync.RWMutex
	roles   map[int64][]RadarRole
	enabled map[int64]bool
}

func NewStaticRadarAuthorizer(roles map[int64][]RadarRole) *StaticRadarAuthorizer {
	copyRoles := make(map[int64][]RadarRole, len(roles))
	for actor, values := range roles {
		copyRoles[actor] = append([]RadarRole(nil), values...)
	}
	return &StaticRadarAuthorizer{roles: copyRoles, enabled: map[int64]bool{}}
}

func (a *StaticRadarAuthorizer) SetEnabled(actorID int64, enabled bool) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.enabled[actorID] = enabled
}

func (a *StaticRadarAuthorizer) ListPermissions(ctx context.Context, actorID int64) ([]RadarPermission, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a == nil {
		return nil, ErrRadarForbidden
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if enabled, ok := a.enabled[actorID]; ok && !enabled {
		return nil, ErrRadarForbidden
	}
	seen := map[RadarPermission]struct{}{}
	for _, role := range a.roles[actorID] {
		for _, permission := range radarRolePermissions[role] {
			seen[permission] = struct{}{}
		}
	}
	out := make([]RadarPermission, 0, len(seen))
	for permission := range seen {
		out = append(out, permission)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	if len(out) == 0 {
		return nil, ErrRadarForbidden
	}
	return out, nil
}

func (a *StaticRadarAuthorizer) Require(ctx context.Context, actorID int64, permission RadarPermission) error {
	permissions, err := a.ListPermissions(ctx, actorID)
	if err != nil {
		return ErrRadarForbidden
	}
	for _, candidate := range permissions {
		if candidate == permission {
			return nil
		}
	}
	return ErrRadarForbidden
}
