package repository

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

func TestRadarPermissionsUseOnlyGlobalRoleBindings(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(`SELECT role FROM evaluation_role_bindings.*scope = '\{\}'::jsonb`).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("viewer"))

	repo := &radarGovernanceRepository{db: db}
	permissions, err := repo.ListPermissions(context.Background(), 7)
	if err != nil {
		t.Fatalf("ListPermissions() error = %v", err)
	}
	if len(permissions) != 1 || permissions[0] != service.PermissionView {
		t.Fatalf("permissions = %v, want [view]", permissions)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRadarPermissionsAllowAdminBootstrapOnlyWhenNoBindingsExist(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery("SELECT role FROM evaluation_role_bindings").
		WithArgs(int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{"role"}))
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs(int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	repo := &radarGovernanceRepository{db: db}
	permissions, err := repo.ListPermissions(context.Background(), 1)
	if err != nil {
		t.Fatalf("ListPermissions() error = %v", err)
	}
	want := map[service.RadarPermission]bool{
		service.PermissionView: true, service.PermissionRoleManage: true,
		service.PermissionRouteAction: true,
	}
	for _, permission := range permissions {
		delete(want, permission)
	}
	if len(want) != 0 {
		t.Fatalf("bootstrap permissions missing %v", want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateRadarRoleBindingRejectsUnsupportedScope(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := &radarGovernanceRepository{db: db}

	_, err = repo.CreateRoleBinding(context.Background(), service.RadarRoleBindingInput{
		ActorID: 2, Role: service.RoleViewer, Scope: json.RawMessage(`{"model":"qwen"}`), CreatedBy: 1,
	})
	if err == nil {
		t.Fatal("CreateRoleBinding() error = nil, want scoped binding rejection")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListRunsReturnsContractStatusAndManifestIdentity(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	runID := uuid.New()
	planID := uuid.New()
	manifestID := uuid.New()
	createdAt := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT r\.id, r\.plan_id, r\.trigger_source, r\.status`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "plan_id", "trigger_source", "status", "created_at", "started_at", "finished_at",
			"contract_status", "manifest_id", "manifest_sha256",
		}).AddRow(runID, planID, "manual", "pending", createdAt, nil, nil, "legacy-unbound", nil, nil).
			AddRow(uuid.New(), planID, "release", "completed", createdAt, createdAt, createdAt, "bound", manifestID, serviceContractHash))

	repo := &radarGovernanceRepository{db: db}
	runs, err := repo.ListRuns(context.Background())
	if err != nil {
		t.Fatalf("ListRuns() error = %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("len(runs) = %d, want 2", len(runs))
	}
	if runs[0].ContractStatus != "legacy-unbound" {
		t.Fatalf("legacy contract status = %q", runs[0].ContractStatus)
	}
	if runs[1].ContractStatus != "bound" || runs[1].RequestManifestID == nil || *runs[1].RequestManifestID != manifestID {
		t.Fatalf("bound run projection = %+v", runs[1])
	}
	if runs[1].RequestManifestSHA256 != serviceContractHash {
		t.Fatalf("manifest hash = %q", runs[1].RequestManifestSHA256)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
