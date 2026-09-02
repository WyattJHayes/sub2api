package migrations

import (
	"crypto/sha256"
	"fmt"
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

func TestRadarQualityReportsMigration222DeclaresTenantScopedSanitizedSchema(t *testing.T) {
	sqlBytes, err := os.ReadFile(filepath.Join("222_add_radar_quality_reports.sql"))
	if err != nil {
		t.Fatalf("read migration 222: %v", err)
	}
	sql := string(sqlBytes)
	for _, fragment := range []string{
		"CREATE TABLE IF NOT EXISTS quality_reports",
		"CREATE TABLE IF NOT EXISTS quality_dimension_results",
		"CREATE TABLE IF NOT EXISTS quality_source_attributions",
		"CREATE TABLE IF NOT EXISTS quality_probe_observations",
		"CREATE TABLE IF NOT EXISTS quality_policy_versions",
		"UNIQUE (tenant_id, run_id, model_alias)",
		"UNIQUE (id, tenant_id)",
		"uq_evaluation_runs_id_tenant",
		"FOREIGN KEY (run_id, tenant_id) REFERENCES evaluation_runs(id, tenant_id)",
		"coverage NUMERIC(8,6)",
		"jsonb_array_length(alternate_candidates) > 0",
		"idx_quality_reports_latest_public",
		"knowledge_freshness",
		"stream_completeness",
		"insufficient_evidence",
		"evidence_code VARCHAR(64) NOT NULL",
		"quality_dimension TEXT",
		"quality_probe_spec JSONB NOT NULL DEFAULT '{}'::jsonb",
		"observation_hash CHAR(64)",
		"event_digest CHAR(64)",
		"FOREIGN KEY (dimension_result_id, tenant_id) REFERENCES quality_dimension_results(id, tenant_id)",
		"FOREIGN KEY (tenant_id, policy_version) REFERENCES quality_policy_versions(tenant_id, version)",
	} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("migration 222 missing %q", fragment)
		}
	}
	for _, prohibited := range []string{"prompt", "completion", "route_trace_id", "artifact", "evidence_summary", "JSONB"} {
		if prohibited == "JSONB" {
			continue
		}
		if strings.Contains(strings.ToLower(sql), prohibited) {
			t.Errorf("migration 222 must not store %q", prohibited)
		}
	}
}

func TestRadarQualityObservationContextMigration223SeedsDefaultPolicy(t *testing.T) {
	sqlBytes, err := os.ReadFile(filepath.Join("223_add_quality_observation_context.sql"))
	if err != nil {
		t.Fatalf("read migration 223: %v", err)
	}
	sql := string(sqlBytes)
	for _, fragment := range []string{
		"INSERT INTO quality_policy_versions",
		"'quality-v1'",
		`'{"minimum_coverage":0.8,"minimum_confidence":0.7,"minimum_margin":0.15,"minimum_samples_per_dimension":3,"observe_delta_pp":5,"suspected_delta_pp":10,"high_risk_delta_pp":20,"freshness_hours":24}'::jsonb`,
		"u.id",
		"ON CONFLICT (tenant_id, version) DO NOTHING",
		"CREATE INDEX IF NOT EXISTS idx_evaluation_cases_quality_dimension",
		"ON evaluation_cases (quality_dimension) WHERE quality_dimension IS NOT NULL",
	} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("migration 223 missing %q", fragment)
		}
	}
}

func TestRadarQualityReportsMigration224PreservesAggregateRevisionHistory(t *testing.T) {
	sqlBytes, err := os.ReadFile(filepath.Join("224_add_quality_report_aggregate_revision.sql"))
	if err != nil {
		t.Fatalf("read migration 224: %v", err)
	}
	sql := string(sqlBytes)
	for _, fragment := range []string{
		"ADD COLUMN IF NOT EXISTS aggregate_revision BIGINT NOT NULL DEFAULT 0",
		"CHECK (aggregate_revision >= 0)",
		"UNIQUE (tenant_id, run_id, model_alias, aggregate_revision)",
		"uq_quality_reports_tenant_run_model_revision",
		"DROP INDEX IF EXISTS idx_quality_reports_latest_public",
		"ON quality_reports (tenant_id, model_alias, aggregate_revision DESC, generated_at DESC)",
		"JOIN pg_index i ON i.indexrelid = c.conindid",
		"JOIN pg_attribute a",
		"c.contype = 'u'",
		"cardinality(c.conkey) = 3",
		"format('ALTER TABLE quality_reports DROP CONSTRAINT %I', legacy_constraint.conname)",
	} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("migration 224 missing %q", fragment)
		}
	}
}

func TestGroupModelPricingMigration221PreservesExistingLongContextPricing(t *testing.T) {
	sqlBytes, err := os.ReadFile(filepath.Join("221_group_model_pricing.sql"))
	if err != nil {
		t.Fatalf("read migration 221 group pricing: %v", err)
	}
	sql := string(sqlBytes)
	for _, fragment := range []string{
		"ADD COLUMN IF NOT EXISTS long_context_pricing_enabled BOOLEAN NOT NULL DEFAULT TRUE",
		"ADD COLUMN IF NOT EXISTS model_pricing JSONB",
		"SET long_context_pricing_enabled = TRUE",
		"WHERE long_context_pricing_enabled IS DISTINCT FROM TRUE",
	} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("migration 225 missing %q", fragment)
		}
	}
}

func TestV020RadarMigrationInventoryPreservesAppliedFiles(t *testing.T) {
	paths, err := filepath.Glob("*.sql")
	if err != nil {
		t.Fatalf("list SQL migrations: %v", err)
	}
	if len(paths) != 305 {
		t.Fatalf("controlled source must contain 305 SQL migrations, found %d", len(paths))
	}

	expected := map[string]string{
		"221_add_radar_tracked_models.sql":              "a2d328c1940225315478b843690dd74f7e169cf712b64b7e94005c23bdd782b2",
		"222_add_radar_quality_reports.sql":             "8bbe1a4aef7270cc875cb1f5954e3ed36560b9a9883ac97490c790d14d35ddbf",
		"223_add_quality_observation_context.sql":       "b553fb40adc85f27397bd7966659043d595bd148245fe7a7d418b4627e853bca",
		"224_add_quality_report_aggregate_revision.sql": "e882dbddb0ef055e29ba22b60aede1584a381cf9b57a202068dbb170997dc6f6",
	}
	for name, want := range expected {
		contents, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read immutable migration %s: %v", name, err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(contents)); got != want {
			t.Errorf("immutable migration %s SHA256 = %s, want %s", name, got, want)
		}
	}
}
