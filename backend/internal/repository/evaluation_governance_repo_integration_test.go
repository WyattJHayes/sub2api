//go:build integration

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRecordGateDecisionRejectsReliabilityWatermarkWithoutSnapshotRefs(t *testing.T) {
	tx := testTx(t)
	fixture := insertRadarControlPlaneConstraintFixture(t, tx)
	require.NoError(t, tx.Commit())
	ctx := context.Background()
	policyID := uuid.New()
	_, err := integrationDB.ExecContext(ctx, `
		INSERT INTO evaluation_gate_policies
			(id,version,policy,policy_hash,enforcement_starts_at,created_by)
		VALUES ($1,$2,'{}'::jsonb,$3,NOW() - INTERVAL '1 hour',$4)`,
		policyID, 920001, hashString("gate-policy"), fixture.actorID)
	require.NoError(t, err)
	watermark, err := json.Marshal(radarGateReliabilityWatermark{
		Version: "radar-gate-reliability-watermark-v1", RunID: fixture.runID,
		PolicyID: policyID, PolicyHash: hashString("gate-policy"), ObservedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, err = NewRadarGovernanceRepository(integrationDB).RecordGateDecision(ctx, service.RadarGateDecisionInput{
		RunID: fixture.runID, PolicyID: policyID, Status: service.RadarGatePassed,
		RuleIDs: []string{"pass"}, Evidence: json.RawMessage(`{"version":"radar-gate-evidence-v1"}`),
		EvidenceHash: hashString("gate-evidence"), ReleaseSubjectHash: hashString("release-subject"),
		SourceWatermark: watermark, CauseSetHash: hashString("cause-set"),
	})
	require.ErrorIs(t, err, service.ErrGovernanceHeadConflict)
}

func TestRecordGateDecisionRejectsReliabilityWatermarkWithStalePolicyHash(t *testing.T) {
	tx := testTx(t)
	fixture := insertRadarControlPlaneConstraintFixture(t, tx)
	require.NoError(t, tx.Commit())
	ctx := context.Background()
	policyID := uuid.New()
	currentPolicyHash := hashString("current-gate-policy")
	_, err := integrationDB.ExecContext(ctx, `
		INSERT INTO evaluation_gate_policies
			(id,version,policy,policy_hash,enforcement_starts_at,created_by)
		VALUES ($1,$2,'{}'::jsonb,$3,NOW() - INTERVAL '1 hour',$4)`,
		policyID, 920002, currentPolicyHash, fixture.actorID)
	require.NoError(t, err)
	ref := publishGateReliabilityHead(t, fixture.runID, "gate-policy-v1", "region:global")
	watermark, err := json.Marshal(radarGateReliabilityWatermark{
		Version: "radar-gate-reliability-watermark-v1", RunID: fixture.runID,
		PolicyID: policyID, PolicyHash: hashString("retired-gate-policy"), ObservedAt: time.Now().UTC(),
		SnapshotRefs: []service.RadarGateReliabilitySnapshotRef{ref},
	})
	require.NoError(t, err)

	_, err = NewRadarGovernanceRepository(integrationDB).RecordGateDecision(ctx, service.RadarGateDecisionInput{
		RunID: fixture.runID, PolicyID: policyID, Status: service.RadarGatePassed,
		RuleIDs: []string{"pass"}, Evidence: json.RawMessage(`{"version":"radar-gate-evidence-v1"}`),
		EvidenceHash: hashString("gate-evidence-stale-policy"), ReleaseSubjectHash: hashString("release-subject"),
		SourceWatermark: watermark, CauseSetHash: hashString("cause-set"),
	})
	require.ErrorIs(t, err, service.ErrGovernanceHeadConflict)
}

func TestLoadRadarGateReliabilityRejectsMissingActiveReleaseSubject(t *testing.T) {
	tx := testTx(t)
	fixture := insertRadarControlPlaneConstraintFixture(t, tx)
	require.NoError(t, tx.Commit())
	ctx := context.Background()
	policyID := uuid.New()
	policy := json.RawMessage(`{}`)
	policyHash, err := service.DigestCanonicalJSON(policy)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `
		INSERT INTO evaluation_gate_policies
			(id,version,policy,policy_hash,enforcement_starts_at,created_by)
		VALUES ($1,$2,$3::jsonb,$4,NOW() - INTERVAL '1 hour',$5)`,
		policyID, 920003, string(policy), policyHash, fixture.actorID)
	require.NoError(t, err)

	_, err = NewRadarGovernanceRepository(integrationDB).(service.RadarGateReliabilityLoader).
		LoadRadarGateReliability(ctx, fixture.runID, policyID)
	require.ErrorIs(t, err, service.ErrGovernanceHeadConflict)
}

func TestLoadReliabilityGateSnapshotRejectsMalformedMetrics(t *testing.T) {
	tx := testTx(t)
	fixture := insertRadarControlPlaneConstraintFixture(t, tx)
	require.NoError(t, tx.Commit())
	ref := publishGateReliabilityHead(t, fixture.runID, "gate-malformed-v1", "region:global")
	corruptTx, err := integrationDB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = corruptTx.Rollback() }()
	_, err = corruptTx.Exec(`SET LOCAL session_replication_role = replica`)
	require.NoError(t, err)
	_, err = corruptTx.Exec(`
		UPDATE evaluation_reliability_snapshots
		SET metrics='{"request_count":"corrupt"}'::jsonb
		WHERE id=$1`, ref.SnapshotID)
	require.NoError(t, err)
	require.NoError(t, corruptTx.Commit())

	readTx, err := integrationDB.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = readTx.Rollback() }()
	_, err = loadReliabilityGateSnapshot(
		context.Background(), readTx, fixture.runID,
		reliabilityGateSliceRequirement{ProfileID: ref.ProfileID, SliceKey: ref.SliceKey},
		time.Now().UTC(), map[string]struct{}{"reliability-query-v1": {}},
	)
	require.Error(t, err)
}

func TestLoadReliabilityGateSnapshotRejectsUnreconciledRouteBilling(t *testing.T) {
	tx := testTx(t)
	fixture := insertRadarControlPlaneConstraintFixture(t, tx)
	require.NoError(t, tx.Commit())
	ctx := context.Background()
	ref := publishGateReliabilityHead(t, fixture.runID, "gate-billing-v1", "region:global")
	var apiKeyID int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		INSERT INTO api_keys (user_id,key) VALUES ($1,$2) RETURNING id`,
		fixture.actorID, "billing-gate-key-"+uuid.NewString()).Scan(&apiKeyID))
	_, err := integrationDB.ExecContext(ctx, `
		INSERT INTO evaluation_route_evidence (
			route_trace_id, evaluation_run_id, sample_id, api_key_id, request_id,
			requested_model, route_profile_version, region, transport_status, started_at,
			billed_amount, billing_status
		) VALUES ($1,$2,$3,$4,$5,'route-a','route-v1','default','succeeded',NOW() - INTERVAL '90 seconds',0.01,'incomplete')`,
		uuid.NewString(), fixture.runID, fixture.sampleID, apiKeyID, "billing-request-"+uuid.NewString())
	require.NoError(t, err)

	readTx, err := integrationDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = readTx.Rollback() }()
	loaded, err := loadReliabilityGateSnapshot(
		ctx, readTx, fixture.runID,
		reliabilityGateSliceRequirement{ProfileID: ref.ProfileID, SliceKey: ref.SliceKey},
		time.Now().UTC(), map[string]struct{}{"reliability-query-v1": {}},
	)
	require.NoError(t, err)
	require.False(t, loaded.BillingReconciled)
}

func TestCreateGatePolicyComputesCanonicalHashInsideRepository(t *testing.T) {
	tx := testTx(t)
	fixture := insertRadarControlPlaneConstraintFixture(t, tx)
	require.NoError(t, tx.Commit())
	policy := json.RawMessage(`{"observation_days":14,"aggregate_delta_pp":-2}`)
	wantHash, err := service.DigestCanonicalJSON(policy)
	require.NoError(t, err)
	enforcementStartsAt := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)

	record, err := NewRadarGovernanceRepository(integrationDB).CreateGatePolicy(context.Background(), service.RadarGatePolicyInput{
		Version: 920004, Policy: policy, PolicyHash: hashString("untrusted-policy-hash"),
		EnforcementStartsAt: enforcementStartsAt, ApprovalExpiresAt: enforcementStartsAt.Add(time.Hour),
		CreatedBy: fixture.actorID,
	})
	require.NoError(t, err)
	require.Equal(t, wantHash, record.PolicyHash)
	var storedHash string
	require.NoError(t, integrationDB.QueryRow(`SELECT policy_hash FROM evaluation_gate_policies WHERE id=$1`, record.ID).Scan(&storedHash))
	require.Equal(t, wantHash, storedHash)
}

func TestCreateGatePolicyRecordsOneCreatedEventForIdenticalRetry(t *testing.T) {
	tx := testTx(t)
	fixture := insertRadarControlPlaneConstraintFixture(t, tx)
	require.NoError(t, tx.Commit())

	policy := json.RawMessage(`{"aggregate_delta_pp":-2,"observation_days":14}`)
	wantHash, err := service.DigestCanonicalJSON(policy)
	require.NoError(t, err)
	enforcementStartsAt := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	input := service.RadarGatePolicyInput{
		Version:             920005,
		Policy:              policy,
		PolicyHash:          hashString("untrusted-policy-hash"),
		EnforcementStartsAt: enforcementStartsAt,
		ApprovalExpiresAt:   enforcementStartsAt.Add(time.Hour),
		CreatedBy:           fixture.actorID,
	}
	repo := NewRadarGovernanceRepository(integrationDB)

	first, err := repo.CreateGatePolicy(context.Background(), input)
	require.NoError(t, err)
	second, err := repo.CreateGatePolicy(context.Background(), input)
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID)

	var eventCount int
	var eventPolicyHash string
	var eventActorID int64
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), `
		SELECT COUNT(*), MIN(policy_hash), MIN(actor_id)
		FROM evaluation_gate_policy_events
		WHERE policy_id=$1 AND event_type='created'`, first.ID).
		Scan(&eventCount, &eventPolicyHash, &eventActorID))
	require.Equal(t, 1, eventCount)
	require.Equal(t, wantHash, eventPolicyHash)
	require.Equal(t, fixture.actorID, eventActorID)
}

func publishGateReliabilityHead(t *testing.T, runID uuid.UUID, profileID, sliceKey string) service.RadarGateReliabilitySnapshotRef {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	snapshot, err := NewEvaluationReliabilityRepository(integrationDB).Publish(ctx, ReliabilitySnapshotInput{
		RunID: runID, ProfileID: profileID, SliceKey: sliceKey,
		WindowStart: now.Add(-2 * time.Minute), WindowEnd: now.Add(-time.Minute),
		QueryVersion: "reliability-query-v1", SourceHash: hashString("gate-reliability-source" + uuid.NewString()),
		FreshUntil: now.Add(time.Hour),
		Metrics: ReliabilityMetrics{
			RequestCount: 1, SuccessCount: 1, SuccessfulLatencyCount: 1,
			HistogramOrSketchHash: hashString("gate-reliability-histogram"),
		},
	})
	require.NoError(t, err)
	var headEventID uuid.UUID
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT head_event_id FROM evaluation_reliability_heads
		WHERE run_id=$1 AND reliability_profile_id=$2 AND slice_key=$3`,
		runID, profileID, sliceKey).Scan(&headEventID))
	return service.RadarGateReliabilitySnapshotRef{
		SnapshotID: snapshot.ID, HeadEventID: headEventID, ProfileID: profileID, SliceKey: sliceKey,
		SnapshotHash: snapshot.SnapshotHash, SourceHash: snapshot.SourceHash, CreatedAt: snapshot.CreatedAt,
	}
}

func TestGovernanceHeadCASAndRepeatedActivationOnPostgres(t *testing.T) {
	tx := testTx(t)
	fixture := insertRadarControlPlaneConstraintFixture(t, tx)
	qualityApproverID, releaseApproverID := insertRadarGatePolicyApprovers(t, tx, fixture.actorID)
	require.NoError(t, tx.Commit())
	ctx := context.Background()
	repo := NewRadarGovernanceRepository(integrationDB)
	now := time.Now().UTC()
	policyA, policyB := uuid.New(), uuid.New()
	for index, policyID := range []uuid.UUID{policyA, policyB} {
		hash := hashString(policyID.String())
		_, err := integrationDB.ExecContext(ctx, `
			INSERT INTO evaluation_gate_policies
				(id,version,policy,policy_hash,enforcement_starts_at,created_by,tenant_id)
			VALUES ($1,$2,'{}'::jsonb,$3,$4,$5,$5)`, policyID, 9000+index, hash, now.Add(-time.Hour), fixture.actorID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, `
			INSERT INTO evaluation_gate_policy_approvals
				(id,policy_id,approver_id,role,policy_hash,evidence_hash,effective_at,expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, uuid.New(), policyID, qualityApproverID,
			"quality_admin", hash, hashString("quality-approval:"+policyID.String()), now.Add(-time.Hour), now.Add(time.Hour))
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, `
			INSERT INTO evaluation_gate_policy_approvals
				(id,policy_id,approver_id,role,policy_hash,evidence_hash,effective_at,expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, uuid.New(), policyID, releaseApproverID,
			"release_manager", hash, hashString("release-approval:"+policyID.String()), now.Add(-time.Hour), now.Add(time.Hour))
		require.NoError(t, err)
	}

	headA, err := repo.ActivateGatePolicy(ctx, service.RadarGatePolicyActivationInput{
		PolicyID: policyA, Scope: service.RadarGovernanceScope{Environment: "cas-a", ScopeType: "global", ScopeID: "global"}, ActorID: fixture.actorID,
	})
	require.NoError(t, err)
	headB, err := repo.ActivateGatePolicy(ctx, service.RadarGatePolicyActivationInput{
		PolicyID: policyB, Scope: headA.Scope, ActorID: fixture.actorID, ExpectedPolicyID: &policyA,
	})
	require.NoError(t, err)
	_, err = repo.ActivateGatePolicy(ctx, service.RadarGatePolicyActivationInput{
		PolicyID: policyA, Scope: headA.Scope, ActorID: fixture.actorID, ExpectedPolicyID: &policyB,
	})
	require.NoError(t, err)
	var activationCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_gate_policy_events
		WHERE policy_id=$1 AND event_type='activated' AND environment='cas-a'`, policyA).Scan(&activationCount))
	require.Equal(t, 2, activationCount)
	require.NotEqual(t, headA.EventID, headB.EventID)

	policyC, policyD := uuid.New(), uuid.New()
	for index, policyID := range []uuid.UUID{policyC, policyD} {
		eventID := uuid.New()
		_, err := integrationDB.ExecContext(ctx, `
			INSERT INTO evaluation_gate_policies
				(id,version,policy,policy_hash,enforcement_starts_at,created_by,tenant_id)
			VALUES ($1,$2,'{}'::jsonb,$3,$4,$5,$5)`, policyID, 9010+index, hashString(policyID.String()), now.Add(-time.Hour), fixture.actorID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, `
			INSERT INTO evaluation_gate_policy_events
				(id,policy_id,event_type,policy_hash,environment,scope_type,scope_id,actor_id)
			SELECT $1,id,'activated',policy_hash,'cas-empty','global','global',$3
			FROM evaluation_gate_policies WHERE id=$2`, eventID, policyID, fixture.actorID)
		require.NoError(t, err)
	}
	eventRows, err := integrationDB.QueryContext(ctx, `SELECT id,policy_id FROM evaluation_gate_policy_events WHERE environment='cas-empty' ORDER BY policy_id`)
	require.NoError(t, err)
	type candidate struct{ eventID, policyID uuid.UUID }
	var candidates []candidate
	for eventRows.Next() {
		var c candidate
		require.NoError(t, eventRows.Scan(&c.eventID, &c.policyID))
		candidates = append(candidates, c)
	}
	require.NoError(t, eventRows.Close())
	require.Len(t, candidates, 2)
	results := make(chan bool, 2)
	errors := make(chan error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, c := range candidates {
		wg.Add(1)
		go func(c candidate) {
			defer wg.Done()
			<-start
			var advanced bool
			err := integrationDB.QueryRowContext(ctx, `SELECT advance_evaluation_gate_policy_head($1,$2,$3,$4,$5,NULL)`,
				"cas-empty", "global", "global", c.policyID, c.eventID).Scan(&advanced)
			results <- advanced
			errors <- err
		}(c)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	advancedCount := 0
	for advanced := range results {
		if advanced {
			advancedCount++
		}
	}
	require.Equal(t, 1, advancedCount)
}

func TestGovernancePolicyHeadCASIsTenantScopedOnPostgres(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()
	fixture := insertRadarControlPlaneConstraintFixture(t, tx)
	require.NoError(t, tx.Commit())

	type candidate struct {
		tenantID int64
		policyID uuid.UUID
		eventID  uuid.UUID
	}
	candidates := make([]candidate, 0, 2)
	now := time.Now().UTC()
	for index, tenantID := range []int64{fixture.actorID + 1000, fixture.actorID + 2000} {
		policyID, eventID := uuid.New(), uuid.New()
		hash := hashString("tenant-head:" + uuid.NewString())
		_, err := integrationDB.ExecContext(ctx, `
			INSERT INTO evaluation_gate_policies
				(id,version,policy,policy_hash,enforcement_starts_at,created_by,tenant_id)
			VALUES ($1,$2,'{}'::jsonb,$3,$4,$5,$6)`, policyID, 940000+index, hash, now.Add(-time.Hour), fixture.actorID, tenantID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, `
			INSERT INTO evaluation_gate_policy_events
				(id,policy_id,event_type,policy_hash,environment,scope_type,scope_id,actor_id)
			VALUES ($1,$2,'activated',$3,'tenant-shared','global','global',$4)`, eventID, policyID, hash, fixture.actorID)
		require.NoError(t, err)
		candidates = append(candidates, candidate{tenantID: tenantID, policyID: policyID, eventID: eventID})
	}
	for _, item := range candidates {
		var advanced bool
		err := integrationDB.QueryRowContext(ctx, `
			SELECT advance_evaluation_gate_policy_head($1,$2,$3,$4,$5,$6,NULL)`,
			item.tenantID, "tenant-shared", "global", "global", item.policyID, item.eventID).Scan(&advanced)
		require.NoError(t, err)
		require.True(t, advanced)
	}

	var headCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_gate_policy_heads
		WHERE environment='tenant-shared' AND scope_type='global' AND scope_id='global'
		  AND tenant_id IN ($1,$2)`, candidates[0].tenantID, candidates[1].tenantID).Scan(&headCount))
	require.Equal(t, 2, headCount)
}

func insertRadarGatePolicyApprovers(t *testing.T, tx *sql.Tx, tenantID int64) (qualityAdminID, releaseManagerID int64) {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()
	for _, approver := range []struct {
		email string
		role  service.RadarRole
		out   *int64
	}{
		{email: "quality-approver-" + suffix + "@example.com", role: service.RoleQualityAdmin, out: &qualityAdminID},
		{email: "release-approver-" + suffix + "@example.com", role: service.RoleReleaseManager, out: &releaseManagerID},
	} {
		require.NoError(t, tx.QueryRowContext(ctx, `
			INSERT INTO users (email, password_hash, role, balance, concurrency, status)
			VALUES ($1, 'radar-approval-test', 'admin', 0, 1, 'active')
			RETURNING id`, approver.email).Scan(approver.out))
		require.NoError(t, execRadarFixtureSQL(ctx, tx, `
			INSERT INTO evaluation_role_bindings
				(id, actor_id, role, scope, enabled, created_by, tenant_id)
			VALUES ($1, $2, $3, '{}'::jsonb, TRUE, $4, $5)`,
			uuid.New(), *approver.out, approver.role, tenantID, tenantID))
	}
	return qualityAdminID, releaseManagerID
}

func insertRadarGatePolicyApproval(t *testing.T, tx *sql.Tx, policyID uuid.UUID, approverID int64, role service.RadarRole, policyHash, evidenceHash string, effectiveAt, expiresAt time.Time) {
	t.Helper()
	require.NoError(t, execRadarFixtureSQL(context.Background(), tx, `
		INSERT INTO evaluation_gate_policy_approvals
			(id, policy_id, approver_id, role, policy_hash, evidence_hash, effective_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		uuid.New(), policyID, approverID, role, policyHash, evidenceHash, effectiveAt, expiresAt))
}

func insertRadarGatePolicyScenario(t *testing.T, tx *sql.Tx, fixture radarControlPlaneConstraintFixture, version int, enforcementStartsAt time.Time) (uuid.UUID, string, int64, int64) {
	t.Helper()
	policyID := uuid.New()
	policy := json.RawMessage(`{}`)
	policyHash, err := service.DigestCanonicalJSON(policy)
	require.NoError(t, err)
	require.NoError(t, execRadarFixtureSQL(context.Background(), tx, `
		INSERT INTO evaluation_gate_policies
			(id, version, policy, policy_hash, enforcement_starts_at, created_by, tenant_id)
		VALUES ($1, $2, $3::jsonb, $4, $5, $6, $6)`,
		policyID, version, string(policy), policyHash, enforcementStartsAt, fixture.actorID))
	qualityApproverID, releaseApproverID := insertRadarGatePolicyApprovers(t, tx, fixture.actorID)
	return policyID, policyHash, qualityApproverID, releaseApproverID
}

func TestActivateGatePolicyRequiresIndependentCurrentApprovalsOnPostgres(t *testing.T) {
	t.Run("only quality approval", func(t *testing.T) {
		tx := testTx(t)
		fixture := insertRadarControlPlaneConstraintFixture(t, tx)
		now := time.Now().UTC()
		policyID, policyHash, qualityApproverID, _ := insertRadarGatePolicyScenario(t, tx, fixture, 930001, now.Add(-time.Hour))
		insertRadarGatePolicyApproval(t, tx, policyID, qualityApproverID, service.RoleQualityAdmin, policyHash, hashString("quality-only"), now.Add(-2*time.Hour), now.Add(2*time.Hour))
		require.NoError(t, tx.Commit())

		_, err := NewRadarGovernanceRepository(integrationDB).ActivateGatePolicy(context.Background(), service.RadarGatePolicyActivationInput{
			PolicyID: policyID, Scope: service.RadarGovernanceScope{Environment: "approval-only-quality", ScopeType: "global", ScopeID: "global"}, ActorID: fixture.actorID,
		})
		require.ErrorContains(t, err, "activation window")
	})

	t.Run("same actor holds both roles", func(t *testing.T) {
		tx := testTx(t)
		fixture := insertRadarControlPlaneConstraintFixture(t, tx)
		now := time.Now().UTC()
		policyID, policyHash, qualityApproverID, _ := insertRadarGatePolicyScenario(t, tx, fixture, 930002, now.Add(-time.Hour))
		require.NoError(t, execRadarFixtureSQL(context.Background(), tx, `
			INSERT INTO evaluation_role_bindings
				(id, actor_id, role, scope, enabled, created_by, tenant_id)
			VALUES ($1, $2, 'release_manager', '{}'::jsonb, TRUE, $3, $3)`, uuid.New(), qualityApproverID, fixture.actorID))
		insertRadarGatePolicyApproval(t, tx, policyID, qualityApproverID, service.RoleQualityAdmin, policyHash, hashString("same-actor-quality"), now.Add(-2*time.Hour), now.Add(2*time.Hour))
		insertRadarGatePolicyApproval(t, tx, policyID, qualityApproverID, service.RoleReleaseManager, policyHash, hashString("same-actor-release"), now.Add(-2*time.Hour), now.Add(2*time.Hour))
		require.NoError(t, tx.Commit())

		_, err := NewRadarGovernanceRepository(integrationDB).ActivateGatePolicy(context.Background(), service.RadarGatePolicyActivationInput{
			PolicyID: policyID, Scope: service.RadarGovernanceScope{Environment: "approval-same-actor", ScopeType: "global", ScopeID: "global"}, ActorID: fixture.actorID,
		})
		require.ErrorContains(t, err, "activation window")
	})

	t.Run("creator approvals are excluded", func(t *testing.T) {
		tx := testTx(t)
		fixture := insertRadarControlPlaneConstraintFixture(t, tx)
		now := time.Now().UTC()
		policyID, policyHash, _, _ := insertRadarGatePolicyScenario(t, tx, fixture, 930003, now.Add(-time.Hour))
		for _, role := range []service.RadarRole{service.RoleQualityAdmin, service.RoleReleaseManager} {
			require.NoError(t, execRadarFixtureSQL(context.Background(), tx, `
				INSERT INTO evaluation_role_bindings
					(id, actor_id, role, scope, enabled, created_by, tenant_id)
				VALUES ($1, $2, $3, '{}'::jsonb, TRUE, $2, $2)`, uuid.New(), fixture.actorID, role))
			insertRadarGatePolicyApproval(t, tx, policyID, fixture.actorID, role, policyHash, hashString("creator-"+string(role)), now.Add(-2*time.Hour), now.Add(2*time.Hour))
		}
		require.NoError(t, tx.Commit())

		_, err := NewRadarGovernanceRepository(integrationDB).ActivateGatePolicy(context.Background(), service.RadarGatePolicyActivationInput{
			PolicyID: policyID, Scope: service.RadarGovernanceScope{Environment: "approval-creator", ScopeType: "global", ScopeID: "global"}, ActorID: fixture.actorID,
		})
		require.ErrorContains(t, err, "activation window")
	})

	t.Run("policy hash mismatch is rejected", func(t *testing.T) {
		tx := testTx(t)
		fixture := insertRadarControlPlaneConstraintFixture(t, tx)
		now := time.Now().UTC()
		policyID, _, qualityApproverID, releaseApproverID := insertRadarGatePolicyScenario(t, tx, fixture, 930004, now.Add(-time.Hour))
		wrongHash := hashString("retired-policy")
		insertRadarGatePolicyApproval(t, tx, policyID, qualityApproverID, service.RoleQualityAdmin, wrongHash, hashString("stale-quality"), now.Add(-2*time.Hour), now.Add(2*time.Hour))
		insertRadarGatePolicyApproval(t, tx, policyID, releaseApproverID, service.RoleReleaseManager, wrongHash, hashString("stale-release"), now.Add(-2*time.Hour), now.Add(2*time.Hour))
		require.NoError(t, tx.Commit())

		_, err := NewRadarGovernanceRepository(integrationDB).ActivateGatePolicy(context.Background(), service.RadarGatePolicyActivationInput{
			PolicyID: policyID, Scope: service.RadarGovernanceScope{Environment: "approval-stale-hash", ScopeType: "global", ScopeID: "global"}, ActorID: fixture.actorID,
		})
		require.ErrorContains(t, err, "activation window")
	})

	t.Run("two distinct approvals activate", func(t *testing.T) {
		tx := testTx(t)
		fixture := insertRadarControlPlaneConstraintFixture(t, tx)
		now := time.Now().UTC()
		policyID, policyHash, qualityApproverID, releaseApproverID := insertRadarGatePolicyScenario(t, tx, fixture, 930005, now.Add(-time.Hour))
		require.NoError(t, tx.Commit())
		repo := &radarGovernanceRepository{db: integrationDB}
		for _, approval := range []service.RadarGatePolicyApprovalInput{
			{PolicyID: policyID, ApproverID: qualityApproverID, Role: service.RoleQualityAdmin, PolicyHash: policyHash, EvidenceHash: hashString("success-quality"), EffectiveAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(2 * time.Hour)},
			{PolicyID: policyID, ApproverID: releaseApproverID, Role: service.RoleReleaseManager, PolicyHash: policyHash, EvidenceHash: hashString("success-release"), EffectiveAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(2 * time.Hour)},
		} {
			_, err := repo.ApproveGatePolicy(context.Background(), approval)
			require.NoError(t, err)
		}

		head, err := repo.ActivateGatePolicy(context.Background(), service.RadarGatePolicyActivationInput{
			PolicyID: policyID, Scope: service.RadarGovernanceScope{Environment: "approval-success", ScopeType: "global", ScopeID: "global"}, ActorID: fixture.actorID,
		})
		require.NoError(t, err)
		require.Equal(t, policyID, head.PolicyID)
	})
}

func TestActivateGatePolicyAllowsFutureEnforcementWithCurrentApprovalsOnPostgres(t *testing.T) {
	tx := testTx(t)
	fixture := insertRadarControlPlaneConstraintFixture(t, tx)
	now := time.Now().UTC()
	policyID, policyHash, qualityApproverID, releaseApproverID := insertRadarGatePolicyScenario(t, tx, fixture, 930006, now.Add(24*time.Hour))
	insertRadarGatePolicyApproval(t, tx, policyID, qualityApproverID, service.RoleQualityAdmin, policyHash, hashString("future-quality"), now.Add(-time.Hour), now.Add(48*time.Hour))
	insertRadarGatePolicyApproval(t, tx, policyID, releaseApproverID, service.RoleReleaseManager, policyHash, hashString("future-release"), now.Add(-time.Hour), now.Add(48*time.Hour))
	require.NoError(t, tx.Commit())

	head, err := NewRadarGovernanceRepository(integrationDB).ActivateGatePolicy(context.Background(), service.RadarGatePolicyActivationInput{
		PolicyID: policyID, Scope: service.RadarGovernanceScope{Environment: "approval-future-enforcement", ScopeType: "global", ScopeID: "global"}, ActorID: fixture.actorID,
	})
	require.NoError(t, err)
	require.Equal(t, policyID, head.PolicyID)
}

func TestApproveGatePolicyRejectsCreatorOnPostgres(t *testing.T) {
	tx := testTx(t)
	fixture := insertRadarControlPlaneConstraintFixture(t, tx)
	now := time.Now().UTC()
	policyID, policyHash, _, _ := insertRadarGatePolicyScenario(t, tx, fixture, 930008, now.Add(-time.Hour))
	require.NoError(t, execRadarFixtureSQL(context.Background(), tx, `
		INSERT INTO evaluation_role_bindings
			(id, actor_id, role, scope, enabled, created_by, tenant_id)
		VALUES ($1, $2, 'quality_admin', '{}'::jsonb, TRUE, $2, $2)`, uuid.New(), fixture.actorID))
	require.NoError(t, tx.Commit())

	_, err := (&radarGovernanceRepository{db: integrationDB}).ApproveGatePolicy(context.Background(), service.RadarGatePolicyApprovalInput{
		PolicyID: policyID, ApproverID: fixture.actorID, Role: service.RoleQualityAdmin,
		PolicyHash: policyHash, EvidenceHash: hashString("creator-approval"),
		EffectiveAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour),
	})
	require.ErrorContains(t, err, "creator cannot approve")
}

func TestGateLoaderFailsClosedAfterApprovalExpiryOnPostgres(t *testing.T) {
	tx := testTx(t)
	fixture := insertRadarControlPlaneConstraintFixture(t, tx)
	now := time.Now().UTC()
	policyID, policyHash, qualityApproverID, releaseApproverID := insertRadarGatePolicyScenario(t, tx, fixture, 930007, now.Add(-time.Hour))
	insertRadarGatePolicyApproval(t, tx, policyID, qualityApproverID, service.RoleQualityAdmin, policyHash, hashString("expired-quality"), now.Add(-2*time.Hour), now.Add(-time.Minute))
	insertRadarGatePolicyApproval(t, tx, policyID, releaseApproverID, service.RoleReleaseManager, policyHash, hashString("expired-release"), now.Add(-2*time.Hour), now.Add(-time.Minute))
	require.NoError(t, tx.Commit())

	_, err := (&radarGovernanceRepository{db: integrationDB}).LoadRadarGateReliability(context.Background(), fixture.runID, policyID)
	require.ErrorIs(t, err, service.ErrGovernanceHeadConflict)
}

func TestGovernanceReevaluationTargetsActiveMatchingReleaseOnPostgres(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()
	matching := insertRadarControlPlaneConstraintFixture(t, tx)
	revoked := insertRadarControlPlaneConstraintFixture(t, tx)
	expired := insertRadarControlPlaneConstraintFixture(t, tx)
	now := time.Now().UTC()
	type releaseEvent struct {
		eventType   string
		effectiveAt time.Time
		expiresAt   any
	}
	type releaseFixture struct {
		runID      uuid.UUID
		actorID    int64
		modelRoute string
		events     []releaseEvent
	}
	for index, release := range []releaseFixture{
		{runID: matching.runID, actorID: matching.actorID, modelRoute: "deepseek", events: []releaseEvent{
			{eventType: "activated", effectiveAt: now.Add(-time.Hour), expiresAt: now.Add(time.Hour)},
			{eventType: "activated", effectiveAt: now.Add(time.Hour), expiresAt: now.Add(2 * time.Hour)},
		}},
		{runID: matching.runID, actorID: matching.actorID, modelRoute: "qwen", events: []releaseEvent{
			{eventType: "activated", effectiveAt: now.Add(-time.Hour), expiresAt: now.Add(time.Hour)},
		}},
		{runID: revoked.runID, actorID: revoked.actorID, modelRoute: "deepseek", events: []releaseEvent{
			{eventType: "activated", effectiveAt: now.Add(-2 * time.Hour), expiresAt: now.Add(time.Hour)},
			{eventType: "revoked", effectiveAt: now.Add(-time.Hour)},
		}},
		{runID: expired.runID, actorID: expired.actorID, modelRoute: "deepseek", events: []releaseEvent{
			{eventType: "activated", effectiveAt: now.Add(-2 * time.Hour), expiresAt: now.Add(-time.Hour)},
		}},
	} {
		baselineID := uuid.New()
		subjectID := uuid.New()
		subjectHash := hashString(subjectID.String())
		releaseJSON := fmt.Sprintf(`{"baseline_id":%q,"deployment_environment":"production","scope_type":"global","scope_id":"global"}`, baselineID.String())
		require.NoError(t, execRadarFixtureSQL(ctx, tx, `
			INSERT INTO evaluation_baselines
				(id,model_route,run_id,dataset_manifest_sha256,evidence_hash,route_profile_version,policy_version,proposed_by)
			VALUES ($1,$2,$3,$4,$5,'radar-route-profile-v1',$6,$7)`,
			baselineID, release.modelRoute, release.runID, hashString("dataset"), hashString("evidence"), 9900+index, release.actorID))
		require.NoError(t, execRadarFixtureSQL(ctx, tx, `
			INSERT INTO evaluation_release_subjects
				(id,run_id,subject_hash,canonical_subject,canonical_subject_bytes)
			VALUES ($1,$2,$3,$4::jsonb,convert_to($4::text,'UTF8'))`, subjectID, release.runID, subjectHash, releaseJSON))
		for _, event := range release.events {
			require.NoError(t, execRadarFixtureSQL(ctx, tx, `
				INSERT INTO evaluation_release_subject_events
					(id,release_subject_id,event_type,actor_id,effective_at,expires_at)
				VALUES ($1,$2,$3,$4,$5,$6)`, uuid.New(), subjectID, event.eventType, release.actorID, event.effectiveAt, event.expiresAt))
		}
	}

	eventID := uuid.New()
	require.NoError(t, enqueueGovernanceReevaluation(ctx, tx, "baseline_head", uuid.New(), eventID,
		service.RadarGovernanceScope{Environment: "production", ScopeType: "global", ScopeID: "global"}, "deepseek"))
	var count int
	var runID uuid.UUID
	require.NoError(t, tx.QueryRowContext(ctx, `
		SELECT COUNT(*),MIN(run_id::text)::uuid
		FROM evaluation_gate_reevaluation_outbox
		WHERE payload->>'event_id'=$1`, eventID.String()).Scan(&count, &runID))
	require.Equal(t, 1, count)
	require.Equal(t, matching.runID, runID)
}

func TestEvidenceSigningKeyRotationAndRevocationPropagateOnPostgres(t *testing.T) {
	ctx := context.Background()
	evidenceRepo, lease, semantics := createOpenRouteEvidenceFixture(t)
	const tenantID int64 = 731001
	_, err := integrationDB.ExecContext(ctx, `UPDATE evaluation_runs SET tenant_id=$2 WHERE id=$1`, lease.RunID, tenantID)
	require.NoError(t, err)
	oldKeyID := configureTestEvidenceSigningKey(t, evidenceRepo)
	semantics.PromptHash = hashString("mismatched-prompt")

	_, err = evidenceRepo.CreateOpen(ctx, createOpenRouteEvidenceInput(lease, semantics))
	require.ErrorIs(t, err, service.ErrRequestSemanticsMismatch)

	var oldEpoch int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT state_epoch FROM evaluation_evidence_signing_keys WHERE id=$1`, oldKeyID).Scan(&oldEpoch))
	newKeyID := uuid.New()
	governance := NewRadarGovernanceRepository(integrationDB)
	rotated, err := governance.RotateEvidenceSigningKey(ctx, service.RotateEvidenceSigningKeyInput{
		ID: newKeyID, KeyReference: "test:evidence-key:" + uuid.NewString(),
		ExpectedActiveKeyID: oldKeyID, ExpectedActiveStateEpoch: oldEpoch,
	})
	require.NoError(t, err)
	require.Equal(t, service.EvidenceSigningKeyActive, rotated.Status)
	require.Equal(t, int64(1), rotated.StateEpoch)

	var oldStatus string
	var verifyEpoch int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT status, state_epoch FROM evaluation_evidence_signing_keys WHERE id=$1`, oldKeyID).
		Scan(&oldStatus, &verifyEpoch))
	require.Equal(t, "verify_only", oldStatus)
	require.Equal(t, oldEpoch+1, verifyEpoch)
	var activeCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_evidence_signing_keys WHERE status='active'`).Scan(&activeCount))
	require.Equal(t, 1, activeCount)

	revoked, err := governance.TransitionEvidenceSigningKey(ctx, service.TransitionEvidenceSigningKeyInput{
		ID: oldKeyID, ExpectedStateEpoch: verifyEpoch, Status: service.EvidenceSigningKeyRevoked,
	})
	require.NoError(t, err)
	require.Equal(t, service.EvidenceSigningKeyRevoked, revoked.Status)
	require.Equal(t, verifyEpoch+1, revoked.StateEpoch)
	require.NotNil(t, revoked.RevokedAt)

	var reevaluationCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_gate_reevaluation_outbox
		WHERE run_id=$1 AND cause_type='evidence' AND cause_id=$2
		  AND (payload->>'state_epoch')::bigint=$3`, lease.RunID, oldKeyID, revoked.StateEpoch).
		Scan(&reevaluationCount))
	require.Equal(t, 1, reevaluationCount)
	var unifiedCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_outbox_events
		WHERE run_id=$1 AND event_type='gate_reevaluation'
		  AND source_type='evidence_signing_key_state' AND source_id=$2`,
		lease.RunID, oldKeyID.String()+":"+fmt.Sprint(revoked.StateEpoch)).Scan(&unifiedCount))
	require.Equal(t, 1, unifiedCount)

	var alertCount, eventCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_alerts
		WHERE model_route='route-a' AND capability_domain='coding'
		  AND cause='insufficient_evidence' AND status='open' AND tenant_id=$1`, tenantID).Scan(&alertCount))
	require.Equal(t, 1, alertCount)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM evaluation_alert_events event
		JOIN evaluation_alerts alert ON alert.id=event.alert_id
		WHERE alert.model_route='route-a' AND alert.capability_domain='coding'
		  AND event.payload->>'signing_key_id'=$1
		  AND (event.payload->>'state_epoch')::bigint=$2`, oldKeyID.String(), revoked.StateEpoch).
		Scan(&eventCount))
	require.Equal(t, 1, eventCount)
}

func TestFrozenReleaseSubjectBindingSelectsBaselineRouteOnPostgres(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()
	fixture := insertRadarControlPlaneConstraintFixture(t, tx)
	const routeProfile = "radar-route-profile-v1"
	const runnerDigest = "sha256:runner"
	const graderDigest = "sha256:grader"
	const statisticsDigest = "sha256:statistics"
	candidateHashes := map[string]string{
		"deepseek": hashString("deepseek-candidate"),
		"qwen":     hashString("qwen-candidate"),
	}

	require.NoError(t, execRadarFixtureSQL(ctx, tx, `UPDATE evaluation_runs SET route_profile_version=$2 WHERE id=$1`, fixture.runID, routeProfile))
	manifestID := uuid.New()
	manifestHash := hashString("request-manifest")
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_request_manifests
			(id,schema_version,interaction_type,canonical_manifest_bytes,manifest_sha256)
		VALUES ($1,'radar-request-manifest-v1','single',$2,$3)`, manifestID, []byte(`{}`), manifestHash))

	var runnerAssignmentID uuid.UUID
	for index, route := range []string{"deepseek", "qwen"} {
		pairID := uuid.New()
		baselineSideID := uuid.New()
		candidateSideID := uuid.New()
		baselineSampleID := uuid.New()
		candidateSampleID := uuid.New()
		require.NoError(t, execRadarFixtureSQL(ctx, tx, `
			INSERT INTO evaluation_pair_specs
				(id,run_id,case_id,sample_index,repeat_index,request_manifest_id,request_manifest_sha256,canonical_spec,pair_spec_hash)
			VALUES ($1,$2,$3,0,$4,$5,$6,jsonb_build_object('region','default'),$7)`,
			pairID, fixture.runID, fixture.caseID, index, manifestID, manifestHash, hashString("pair-"+route)))
		for _, sample := range []struct {
			id         uuid.UUID
			modelRoute string
		}{
			{id: baselineSampleID, modelRoute: "baseline:" + route},
			{id: candidateSampleID, modelRoute: "candidate:" + route},
		} {
			require.NoError(t, execRadarFixtureSQL(ctx, tx, `
				INSERT INTO evaluation_samples
					(id,run_id,case_id,model_route,sample_index,priority,status,estimated_cost)
				VALUES ($1,$2,$3,$4,0,'P0','completed',0.01)`, sample.id, fixture.runID, fixture.caseID, sample.modelRoute))
		}
		for _, side := range []struct {
			id          uuid.UUID
			sampleID    uuid.UUID
			name        string
			modelRoute  string
			modelConfig string
		}{
			{id: baselineSideID, sampleID: baselineSampleID, name: "baseline", modelRoute: "baseline:" + route, modelConfig: hashString("baseline-" + route)},
			{id: candidateSideID, sampleID: candidateSampleID, name: "candidate", modelRoute: "candidate:" + route, modelConfig: candidateHashes[route]},
		} {
			require.NoError(t, execRadarFixtureSQL(ctx, tx, `
				INSERT INTO evaluation_side_specs
					(id,pair_spec_id,sample_id,side,canonical_spec,side_spec_hash)
				VALUES ($1,$2,$3,$4,jsonb_build_object(
					'model_route',$5::text,'model_config_sha256',$6::text,'route_profile_version',$7::text
				),$8)`, side.id, pairID, side.sampleID, side.name, side.modelRoute, side.modelConfig, routeProfile, hashString("side-"+route+side.name)))
		}
		require.NoError(t, execRadarFixtureSQL(ctx, tx, `
			INSERT INTO evaluation_pair_bindings
				(id,pair_spec_id,baseline_side_spec_id,candidate_side_spec_id,pair_binding_hash)
			VALUES ($1,$2,$3,$4,$5)`, uuid.New(), pairID, baselineSideID, candidateSideID, hashString("binding-"+route)))
		if route == "deepseek" {
			runnerAssignmentID = uuid.New()
			require.NoError(t, execRadarFixtureSQL(ctx, tx, `
				INSERT INTO evaluation_assignments
					(id,sample_id,attempt,idempotency_key,status,worker_image_digest)
				VALUES ($1,$2,1,$3,'completed',$4)`, runnerAssignmentID, candidateSampleID, hashString("assignment"), runnerDigest))
		}
	}
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		UPDATE evaluation_grading_jobs
		SET status='completed', worker_image_digest=$2
		WHERE assignment_id=$1`, runnerAssignmentID, graderDigest))
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_analysis_jobs
			(id,run_id,capability_domain,model_route,"window",analysis_version,window_start,status,worker_image_digest)
		VALUES ($1,$2,'protocol','candidate:deepseek','release','radar-analysis-v1',date_trunc('day',NOW()),'completed',$3)`,
		uuid.New(), fixture.runID, statisticsDigest))

	baselineID := uuid.New()
	datasetManifest := fmt.Sprintf("%064d", 1)
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_baselines
			(id,model_route,run_id,dataset_manifest_sha256,evidence_hash,route_profile_version,policy_version,proposed_by)
		VALUES ($1,'deepseek',$2,$3,$4,$5,9900,$6)`,
		baselineID, fixture.runID, datasetManifest, hashString("baseline-evidence"), routeProfile, fixture.actorID))
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `SELECT set_config('app.evaluation_head_cas','1',true)`))
	baselineEventID := uuid.New()
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_baseline_events
			(id,baseline_id,event_type,evidence_hash,environment,scope_type,scope_id,actor_id)
		VALUES ($1,$2,'activated',$3,'production','global','global',$4)`,
		baselineEventID, baselineID, hashString("baseline-evidence"), fixture.actorID))
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_baseline_heads
			(environment,scope_type,scope_id,model_route,baseline_id,event_id)
		VALUES ('production','global','global','deepseek',$1,$2)`, baselineID, baselineEventID))

	subject := releaseSubjectFixture()
	subject.BaselineID = baselineID
	subject.CandidateModelConfigSHA256 = candidateHashes["deepseek"]
	subject.DatasetManifestSHA256 = datasetManifest
	subject.RouteProfileVersion = routeProfile
	subject.RunnerImageDigests = []string{runnerDigest}
	subject.GraderImageDigests = []string{graderDigest}
	subject.StatisticsImageDigests = []string{statisticsDigest}
	valid, err := validateFrozenReleaseSubjectBinding(ctx, tx, fixture.runID, subject)
	require.NoError(t, err)
	require.True(t, valid, "selected deepseek branch must ignore qwen candidate identity")

	subject.CandidateModelConfigSHA256 = candidateHashes["qwen"]
	valid, err = validateFrozenReleaseSubjectBinding(ctx, tx, fixture.runID, subject)
	require.NoError(t, err)
	require.False(t, valid, "selected deepseek branch must reject qwen candidate identity")
}

func TestTrustedGovernanceRecordsAreImmutable(t *testing.T) {
	tx := testTx(t)
	fixture := insertRadarControlPlaneConstraintFixture(t, tx)
	ctx := context.Background()
	policyID := uuid.New()
	baselineID := uuid.New()
	subjectID := uuid.New()
	decisionID := uuid.New()
	hashA := hashString("governance-a")
	hashB := hashString("governance-b")

	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_gate_policies
			(id, version, policy, policy_hash, enforcement_starts_at, created_by)
		VALUES ($1, 1001, '{}'::jsonb, $2, NOW(), $3)`, policyID, hashA, fixture.actorID))
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_baselines
			(id, model_route, run_id, dataset_manifest_sha256, evidence_hash, route_profile_version, policy_version, proposed_by)
		VALUES ($1, 'candidate', $2, $3, $4, 'route-v1', 1001, $5)`, baselineID, fixture.runID, hashA, hashB, fixture.actorID))
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_release_subjects (id, run_id, subject_hash, canonical_subject, canonical_subject_bytes)
		VALUES ($1, $2, $3, '{}'::jsonb, convert_to('{}', 'UTF8'))`, subjectID, fixture.runID, hashA))
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_gate_decisions
			(id, run_id, baseline_id, policy_id, status, evidence_hash, release_subject_hash, cause_set_hash)
		VALUES ($1, $2, $3, $4, 'insufficient_evidence', $5, $6, $7)`,
		decisionID, fixture.runID, baselineID, policyID, hashA, hashB, hashA))

	for name, statement := range map[string]string{
		"decision update": `UPDATE evaluation_gate_decisions SET status = 'blocked' WHERE id = $1`,
		"policy update":   `UPDATE evaluation_gate_policies SET policy_hash = repeat('f', 64) WHERE id = $1`,
		"baseline update": `UPDATE evaluation_baselines SET status = 'active' WHERE id = $1`,
		"subject update":  `UPDATE evaluation_release_subjects SET canonical_subject = '{"changed":true}'::jsonb WHERE id = $1`,
		"decision delete": `DELETE FROM evaluation_gate_decisions WHERE id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tx.ExecContext(ctx, `SAVEPOINT immutable_attempt`)
			require.NoError(t, err)
			id := decisionID
			switch name {
			case "policy update":
				id = policyID
			case "baseline update":
				id = baselineID
			case "subject update":
				id = subjectID
			}
			_, err = tx.ExecContext(ctx, statement, id)
			require.Error(t, err)
			_, rollbackErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT immutable_attempt`)
			require.NoError(t, rollbackErr)
		})
	}
}

func TestMigration198StorageCompatibility(t *testing.T) {
	tx := testTx(t)
	fixture := insertRadarControlPlaneConstraintFixture(t, tx)
	ctx := context.Background()
	policyID := uuid.New()
	decisionID := uuid.New()
	hashA := hashString("compatibility-a")
	hashB := hashString("compatibility-b")
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_gate_policies
			(id, version, policy, policy_hash, enforcement_starts_at, created_by)
		VALUES ($1, 1002, '{}'::jsonb, $2, NOW(), $3)`, policyID, hashA, fixture.actorID))
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_gate_decisions
			(id, run_id, policy_id, status, evidence_hash, release_subject_hash, cause_set_hash)
		VALUES ($1, $2, $3, 'passed', $4, $5, $6)`,
		decisionID, fixture.runID, policyID, hashA, hashB, hashA))

	var status, mode string
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT status FROM evaluation_gate_decisions WHERE id = $1`, decisionID).Scan(&status))
	require.Equal(t, "insufficient_evidence", status)
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT mode FROM evaluation_gate_storage_modes WHERE id = 1`).Scan(&mode))
	require.Equal(t, "compatibility", mode)

	var authorizationTable sql.NullString
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT to_regclass('public.evaluation_release_authorizations')::text`).Scan(&authorizationTable))
	if authorizationTable.Valid {
		var count int
		require.NoError(t, tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM evaluation_release_authorizations`).Scan(&count))
		require.Zero(t, count)
	}
}
