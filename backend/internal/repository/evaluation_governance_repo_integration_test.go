//go:build integration

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestGovernanceHeadCASAndRepeatedActivationOnPostgres(t *testing.T) {
	tx := testTx(t)
	fixture := insertRadarControlPlaneConstraintFixture(t, tx)
	require.NoError(t, tx.Commit())
	ctx := context.Background()
	repo := NewRadarGovernanceRepository(integrationDB)
	now := time.Now().UTC()
	policyA, policyB := uuid.New(), uuid.New()
	for index, policyID := range []uuid.UUID{policyA, policyB} {
		hash := hashString(policyID.String())
		_, err := integrationDB.ExecContext(ctx, `
			INSERT INTO evaluation_gate_policies (id,version,policy,policy_hash,enforcement_starts_at,created_by)
			VALUES ($1,$2,'{}'::jsonb,$3,$4,$5)`, policyID, 9000+index, hash, now.Add(-time.Hour), fixture.actorID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, `
			INSERT INTO evaluation_gate_policy_approvals
				(id,policy_id,approver_id,role,effective_at,expires_at)
			VALUES ($1,$2,$3,'quality_admin',$4,$5)`, uuid.New(), policyID, fixture.actorID, now.Add(-time.Hour), now.Add(time.Hour))
		require.NoError(t, err)
	}
	_, err := integrationDB.ExecContext(ctx, `
		INSERT INTO evaluation_role_bindings (id,actor_id,role,created_by)
		VALUES ($1,$2,'quality_admin',$2)`, uuid.New(), fixture.actorID)
	require.NoError(t, err)

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
			INSERT INTO evaluation_gate_policies (id,version,policy,policy_hash,enforcement_starts_at,created_by)
			VALUES ($1,$2,'{}'::jsonb,$3,$4,$5)`, policyID, 9010+index, hashString(policyID.String()), now.Add(-time.Hour), fixture.actorID)
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
	oldKeyID := configureTestEvidenceSigningKey(t, evidenceRepo)
	semantics.PromptHash = hashString("mismatched-prompt")

	_, err := evidenceRepo.CreateOpen(ctx, createOpenRouteEvidenceInput(lease, semantics))
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
		  AND cause='insufficient_evidence' AND status='open'`).Scan(&alertCount))
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
