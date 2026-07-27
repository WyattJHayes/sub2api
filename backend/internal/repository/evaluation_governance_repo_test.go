package repository

import (
	"context"
	"encoding/json"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
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
