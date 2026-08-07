package migrations

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRadarReliabilityMigration200DeclaresAppendOnlyRecoverySchema(t *testing.T) {
	path := filepath.Join("200_add_radar_reliability_and_dr.sql")
	sqlBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration 200: %v", err)
	}
	sql := string(sqlBytes)
	for _, fragment := range []string{
		"CREATE TABLE IF NOT EXISTS evaluation_load_plans",
		"CREATE TABLE IF NOT EXISTS evaluation_fault_experiments",
		"CREATE TABLE IF NOT EXISTS evaluation_fault_experiment_events",
		"CREATE TABLE IF NOT EXISTS evaluation_recovery_evidence",
		"evaluation_reliability_snapshots_denominator_check",
		"trg_evaluation_recovery_evidence_immutable",
		"audit_evaluation_writer_protocol()",
	} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("migration 200 missing %q", fragment)
		}
	}
	if strings.Contains(sql, "SET tenant_id = COALESCE(NULLIF(e.requested_by") {
		t.Fatal("fault experiment tenant backfill must not treat requested_by as a tenant")
	}
	if strings.Contains(sql, "SELECT worker_id, MAX(actor_id)") {
		t.Fatal("worker tenant backfill must not treat event actor_id as a tenant")
	}
	if strings.Contains(sql, "SELECT alert_id, MAX(actor_id)") {
		t.Fatal("alert tenant backfill must not treat event actor_id as a tenant")
	}
	for _, fragment := range []string{"WITH worker_run_tenants AS", "wt.tenant_count = 1"} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("migration 200 missing safe worker tenant backfill fragment %q", fragment)
		}
	}
	for _, fragment := range []string{
		"ALTER TABLE %I DISABLE TRIGGER USER",
		"ALTER TABLE %I ENABLE TRIGGER USER",
	} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("migration 200 must restore immutable triggers around tenant backfill: missing %q", fragment)
		}
	}
	for _, fragment := range []string{
		"DROP CONSTRAINT IF EXISTS evaluation_alerts_model_route_capability_domain_cause_policy_version_key",
		"uq_evaluation_alerts_tenant_scope",
		"UNIQUE (tenant_id, model_route, capability_domain, cause, policy_version)",
	} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("migration 200 missing tenant-scoped alert uniqueness fragment %q", fragment)
		}
	}
}
