package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const radarOutboxE2ETimeout = 30 * time.Second

func TestRadarOutboxConsumerMultiCellToGateAndRunCompletion(t *testing.T) {
	database := radarRevisionDatabase(t)
	fixture := createRadarRevisionFixture(t, database.db)
	sealRadarRevisionEvidence(t, database.db, fixture)
	grading := repository.NewEvaluationGradingRepository(database.db)

	scores := submitRadarScores(t, grading, fixture.workerID, []string{"0.80", "0.70", "0.90", "0.75"})
	require.Len(t, scores, 4)
	runtime := startRadarOutboxConsumer(t, database.db, service.EvaluationOutboxConsumerModeCore)
	completeRadarConsumerAnalysis(t, database.db, fixture, grading, uuid.Nil)
	gate := waitRadarOutboxEvent(t, database.db, fixture.runID, "gate_reevaluation", uuid.Nil)
	requireRadarOutboxDrained(t, database.db, fixture.runID, uuid.Nil)
	runtime.Stop()

	var cellJobs, globalJobs, gateEvents int
	require.NoError(t, database.db.QueryRow(`
		SELECT
			(SELECT COUNT(*) FROM evaluation_analysis_jobs WHERE run_id=$1 AND scope='cell'),
			(SELECT COUNT(*) FROM evaluation_analysis_jobs WHERE run_id=$1 AND scope='global'),
			(SELECT COUNT(*) FROM evaluation_outbox_events WHERE run_id=$1 AND event_type='gate_reevaluation')`,
		fixture.runID).Scan(&cellJobs, &globalJobs, &gateEvents))
	require.Equal(t, len(fixture.domains), cellJobs)
	require.Equal(t, 1, globalJobs)
	require.Equal(t, 1, gateEvents)
	require.NotEqual(t, uuid.Nil, gate.ID)

	var runStatus string
	require.NoError(t, database.db.QueryRow(`SELECT status FROM evaluation_runs WHERE id=$1`, fixture.runID).Scan(&runStatus))
	require.Equal(t, "completed", runStatus)
}

func TestRadarOutboxConsumerSingleCellCompatibilityAndReplay(t *testing.T) {
	database := radarRevisionDatabase(t)
	fixture := createRadarRevisionFixtureWithDomains(t, database.db, []string{"coding"})
	sealRadarRevisionEvidence(t, database.db, fixture)
	grading := repository.NewEvaluationGradingRepository(database.db)

	scores := submitRadarScores(t, grading, fixture.workerID, []string{"0.80", "0.75"})
	require.Len(t, scores, 2)
	runtime := startRadarOutboxConsumer(t, database.db, service.EvaluationOutboxConsumerModeCore)
	completeRadarConsumerAnalysis(t, database.db, fixture, grading, uuid.Nil)
	gate := waitRadarOutboxEvent(t, database.db, fixture.runID, "gate_reevaluation", uuid.Nil)
	requireRadarOutboxDrained(t, database.db, fixture.runID, uuid.Nil)
	waitRadarRunStatus(t, database.db, fixture.runID, "completed")

	var cellJobs, globalJobs, decisions int
	require.NoError(t, database.db.QueryRow(`
		SELECT
			(SELECT COUNT(*) FROM evaluation_analysis_jobs WHERE run_id=$1 AND scope='cell'),
			(SELECT COUNT(*) FROM evaluation_analysis_jobs WHERE run_id=$1 AND scope='global'),
			(SELECT COUNT(*) FROM evaluation_gate_decisions WHERE run_id=$1)`, fixture.runID).
		Scan(&cellJobs, &globalJobs, &decisions))
	require.Equal(t, 1, cellJobs)
	require.Zero(t, globalJobs)
	require.Zero(t, decisions)

	cellEventID, attemptBefore := replayRadarCompletedCellEvent(t, database.db, fixture.runID)
	waitRadarOutboxEventID(t, database.db, cellEventID)
	requireRadarOutboxDrained(t, database.db, fixture.runID, uuid.Nil)
	var replayedCellJobs, attemptAfter int
	require.NoError(t, database.db.QueryRow(`
		SELECT
			(SELECT COUNT(*) FROM evaluation_analysis_jobs WHERE run_id=$1 AND scope='cell'),
			(SELECT attempt FROM evaluation_outbox_events WHERE id=$2)`, fixture.runID, cellEventID).
		Scan(&replayedCellJobs, &attemptAfter))
	require.Equal(t, cellJobs, replayedCellJobs)
	require.Greater(t, attemptAfter, attemptBefore)

	historical := enqueueHistoricalSingleCellGlobal(t, database.db, fixture.runID)
	waitRadarOutboxEventID(t, database.db, historical.ID)
	requireRadarOutboxDrained(t, database.db, fixture.runID, uuid.Nil)
	var compatibleGlobalJobs, gateEvents int
	require.NoError(t, database.db.QueryRow(`
		SELECT
			(SELECT COUNT(*) FROM evaluation_analysis_jobs WHERE run_id=$1 AND scope='global'),
			(SELECT COUNT(*) FROM evaluation_outbox_events WHERE run_id=$1 AND event_type='gate_reevaluation')`,
		fixture.runID).Scan(&compatibleGlobalJobs, &gateEvents))
	require.Zero(t, compatibleGlobalJobs)
	require.Equal(t, 1, gateEvents)
	require.NotEqual(t, uuid.Nil, gate.ID)
	runtime.Stop()
}

func TestRadarOutboxConsumerRevisionBatchEpochFencing(t *testing.T) {
	database := radarRevisionDatabase(t)
	fixture := createRadarRevisionFixture(t, database.db)
	sealRadarRevisionEvidence(t, database.db, fixture)
	grading := repository.NewEvaluationGradingRepository(database.db)

	submitRadarScores(t, grading, fixture.workerID, []string{"0.80", "0.70", "0.90", "0.75"})
	initialRuntime := startRadarOutboxConsumer(t, database.db, service.EvaluationOutboxConsumerModeCore)
	initialDecision, policyID, releaseSubjectHash := completeRadarAnalysisAndDecision(
		t, database.db, fixture, grading, uuid.Nil, nil,
	)
	waitRadarRunStatus(t, database.db, fixture.runID, "completed")
	initialRuntime.Stop()

	governance, ok := repository.NewRadarGovernanceRepository(database.db).(service.RevisionBatchRepository)
	require.True(t, ok)
	batch, err := governance.CreateRevisionBatch(context.Background(), service.CreateRevisionBatchInput{
		RunID: fixture.runID, Reason: "consumer epoch fencing",
		RequestedBy: fixture.userID, IdempotencyKey: radarRevisionHash("epoch-batch:" + fixture.runID.String()),
	})
	require.NoError(t, err)
	setRadarWorkerMode(t, database.db, fixture.workerID, "grader", []string{"grader"})
	submitRadarScores(t, grading, fixture.workerID, []string{"0.65", "0.60", "0.70", "0.55"})

	outbox := repository.NewEvaluationOutboxRepository(database.db)
	outboxWorkerID, err := outbox.EnsureConsumerWorker(context.Background(), "radar-control-plane-outbox")
	require.NoError(t, err)
	claimed, err := outbox.Claim(context.Background(), outboxWorkerID, []string{"cell_recompute"}, 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, batch.ID, claimed[0].RevisionBatchID)
	require.Equal(t, batch.ControlEpoch, claimed[0].LeaseEpoch)

	fenced, err := governance.FenceRevisionBatch(context.Background(), service.RevisionBatchControlInput{
		BatchID: batch.ID, Reason: "invalidate stale outbox lease", ActorID: fixture.userID,
		IdempotencyKey: radarRevisionHash("epoch-fence:" + batch.ID.String()),
	})
	require.NoError(t, err)
	require.Greater(t, fenced.ControlEpoch, claimed[0].LeaseEpoch)
	require.ErrorIs(t,
		outbox.Complete(context.Background(), claimed[0].ID, claimed[0].LeaseToken, claimed[0].LeaseEpoch),
		service.ErrEvaluationOutboxFenced,
	)

	regradeRuntime := startRadarOutboxConsumer(t, database.db, service.EvaluationOutboxConsumerModeCore)
	completeRadarAnalysisAndDecision(t, database.db, fixture, grading, batch.ID, &radarDecisionSeed{
		policyID: policyID, releaseSubjectHash: releaseSubjectHash, supersedes: initialDecision.ID,
	})
	waitRadarRevisionBatchStatus(t, database.db, batch.ID, service.RevisionBatchCompleted)
	regradeRuntime.Stop()
}

func TestRadarOutboxConsumerFullModeProjectsGateAtomicallyAndIdempotently(t *testing.T) {
	database := radarRevisionDatabase(t)
	fixture := createRadarRevisionFixture(t, database.db)
	sealRadarRevisionEvidence(t, database.db, fixture)
	enableRadarTrustedGateStorage(t, database.db)
	target := prepareRadarFullGateTarget(t, database.db, fixture)
	grading := repository.NewEvaluationGradingRepository(database.db)

	submitRadarScores(t, grading, fixture.workerID, []string{"0.80", "0.70", "0.90", "0.75"})
	runtime := startRadarOutboxConsumer(t, database.db, service.EvaluationOutboxConsumerModeFull)
	completeRadarConsumerAnalysis(t, database.db, fixture, grading, uuid.Nil)
	gate := waitRadarOutboxEvent(t, database.db, fixture.runID, "gate_reevaluation", uuid.Nil)
	requireRadarOutboxDrained(t, database.db, fixture.runID, uuid.Nil)
	waitRadarRunStatus(t, database.db, fixture.runID, "completed")
	runtime.Stop()

	decisionID := requireRadarFullGateProjection(t, database.db, fixture.runID, gate.ID, target)
	var observedEvents int
	require.NoError(t, database.db.QueryRow(`
		SELECT COUNT(*) FROM evaluation_alert_events event
		JOIN evaluation_alerts alert ON alert.id=event.alert_id
		WHERE alert.tenant_id=$1 AND event.kind='observed'
		  AND event.payload->>'outbox_event_id'=$2`, target.tenantID, gate.ID.String()).Scan(&observedEvents))
	require.Equal(t, 1, observedEvents)

	replayRadarCompletedOutboxEvent(t, database.db, gate.ID)
	replayRuntime := startRadarOutboxConsumer(t, database.db, service.EvaluationOutboxConsumerModeFull)
	waitRadarOutboxEventID(t, database.db, gate.ID)
	requireRadarOutboxDrained(t, database.db, fixture.runID, uuid.Nil)
	replayRuntime.Stop()
	replayedDecisionID := requireRadarFullGateProjection(t, database.db, fixture.runID, gate.ID, target)
	require.Equal(t, decisionID, replayedDecisionID)
	require.NoError(t, database.db.QueryRow(`
		SELECT COUNT(*) FROM evaluation_alert_events event
		JOIN evaluation_alerts alert ON alert.id=event.alert_id
		WHERE alert.tenant_id=$1 AND event.kind='observed'
		  AND event.payload->>'outbox_event_id'=$2`, target.tenantID, gate.ID.String()).Scan(&observedEvents))
	require.Equal(t, 1, observedEvents)
}

type radarFullGateTarget struct {
	tenantID         int64
	policyID         uuid.UUID
	releaseSubjectID uuid.UUID
}

func prepareRadarFullGateTarget(t *testing.T, db *sql.DB, fixture radarRevisionFixture) radarFullGateTarget {
	t.Helper()
	target := radarFullGateTarget{
		tenantID: fixture.userID, policyID: uuid.New(), releaseSubjectID: uuid.New(),
	}
	baselineID := uuid.New()
	datasetHash := radarRevisionHash("full-gate-dataset:" + fixture.runID.String())
	policy := json.RawMessage(`{
		"observation_days":0,
		"critical_domain_delta_pp":2,
		"aggregate_delta_pp":2,
		"confidence_level":0.95,
		"require_ci_exclude_zero":false
	}`)
	policyHash, err := service.DigestCanonicalJSON(policy)
	require.NoError(t, err)
	canonicalSubject, err := service.CanonicalizeReleaseSubject(service.ReleaseSubject{
		CandidateModelConfigSHA256: radarRevisionHash("full-gate-candidate:" + fixture.runID.String()),
		BaselineID:                 baselineID, DatasetManifestSHA256: datasetHash,
		RouteProfileVersion: "radar-route-profile-v1", GatewayImageDigest: "sha256:gateway",
		ControlPlaneImageDigest: "sha256:control-plane", RunnerImageDigests: []string{"sha256:runner"},
		GraderImageDigests: []string{"sha256:grader"}, StatisticsImageDigests: []string{"sha256:statistics"},
		AnalysisVersion: "v1", RegionSet: []string{"global"}, DeploymentEnvironment: "staging",
		ScopeType: "global", ScopeID: "global",
	})
	require.NoError(t, err)

	now := time.Now().UTC()
	require.NoError(t, withRadarWriterTx(context.Background(), db, "api", func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE evaluation_plans SET tenant_id=$2 WHERE id=$1`, fixture.planID, target.tenantID); err != nil {
			return err
		}
		if _, err := tx.Exec(`
			UPDATE evaluation_runs SET tenant_id=$2,started_at=$3,updated_at=NOW() WHERE id=$1`,
			fixture.runID, target.tenantID, now.Add(-24*time.Hour)); err != nil {
			return err
		}
		var policyVersion int
		if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0)+1 FROM evaluation_gate_policies`).Scan(&policyVersion); err != nil {
			return err
		}
		if _, err := tx.Exec(`
			INSERT INTO evaluation_gate_policies
				(id,version,policy,policy_hash,enforcement_starts_at,created_by,tenant_id)
			VALUES ($1,$2,$3::jsonb,$4,$5,$6,$6)`, target.policyID, policyVersion,
			string(policy), policyHash, now.Add(-30*time.Minute), fixture.userID); err != nil {
			return err
		}
		for _, approver := range []struct {
			role service.RadarRole
			name string
		}{
			{role: service.RoleQualityAdmin, name: "quality"},
			{role: service.RoleReleaseManager, name: "release"},
		} {
			var approverID int64
			if err := tx.QueryRow(`
				INSERT INTO users (email,password_hash,role,balance,concurrency,status)
				VALUES ($1,'radar-full-gate-approval','admin',0,1,'active') RETURNING id`,
				approver.name+"-"+uuid.NewString()+"@example.com").Scan(&approverID); err != nil {
				return err
			}
			if _, err := tx.Exec(`
				INSERT INTO evaluation_role_bindings
					(id,actor_id,role,scope,enabled,created_by,tenant_id)
				VALUES ($1,$2,$3,'{}'::jsonb,TRUE,$4,$4)`,
				uuid.New(), approverID, approver.role, target.tenantID); err != nil {
				return err
			}
			if _, err := tx.Exec(`
				INSERT INTO evaluation_gate_policy_approvals
					(id,policy_id,approver_id,role,policy_hash,evidence_hash,effective_at,expires_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, uuid.New(), target.policyID,
				approverID, approver.role, policyHash,
				radarRevisionHash("full-gate-approval:"+approver.name+":"+target.policyID.String()),
				now.Add(-time.Hour), now.Add(time.Hour)); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`
			INSERT INTO evaluation_baselines
				(id,model_route,run_id,dataset_manifest_sha256,evidence_hash,route_profile_version,
				 policy_version,proposed_by,tenant_id)
			VALUES ($1,'route-a',$2,$3,$4,'radar-route-profile-v1',$5,$6,$6)`,
			baselineID, fixture.runID, datasetHash,
			radarRevisionHash("full-gate-baseline:"+fixture.runID.String()), policyVersion, target.tenantID); err != nil {
			return err
		}
		if _, err := tx.Exec(`
			INSERT INTO evaluation_release_subjects
				(id,run_id,subject_hash,canonical_subject,canonical_subject_bytes,tenant_id)
			VALUES ($1,$2,$3,$4::jsonb,$5,$6)`, target.releaseSubjectID, fixture.runID,
			canonicalSubject.SHA256, string(canonicalSubject.Bytes), []byte(canonicalSubject.Bytes), target.tenantID); err != nil {
			return err
		}
		if _, err := tx.Exec(`
			INSERT INTO evaluation_release_subject_events
				(id,release_subject_id,event_type,actor_id,effective_at,expires_at)
			VALUES ($1,$2,'activated',$3,$4,$5)`, uuid.New(), target.releaseSubjectID,
			fixture.userID, now.Add(-time.Minute), now.Add(time.Hour)); err != nil {
			return err
		}
		policyEventID := uuid.New()
		if _, err := tx.Exec(`
			INSERT INTO evaluation_gate_policy_events
				(id,policy_id,event_type,policy_hash,environment,scope_type,scope_id,actor_id)
			VALUES ($1,$2,'activated',$3,'staging','global','global',$4)`,
			policyEventID, target.policyID, policyHash, fixture.userID); err != nil {
			return err
		}
		var advanced bool
		if err := tx.QueryRow(`
			SELECT advance_evaluation_gate_policy_head($1,'staging','global','global',$2,$3,NULL)`,
			target.tenantID, target.policyID, policyEventID).Scan(&advanced); err != nil {
			return err
		}
		if !advanced {
			return fmt.Errorf("full Gate policy head did not advance")
		}
		return nil
	}))
	return target
}

func requireRadarFullGateProjection(
	t *testing.T,
	db *sql.DB,
	runID, gateEventID uuid.UUID,
	target radarFullGateTarget,
) uuid.UUID {
	t.Helper()
	var decisionID, headDecisionID, projectionDecisionID, lastEventID uuid.UUID
	var decisionStatus service.RadarGateDecisionStatus
	var decisionRuleID string
	require.NoError(t, db.QueryRow(`
		SELECT id,status,COALESCE(rule_ids[1],'') FROM evaluation_gate_decisions
		WHERE run_id=$1 AND policy_id=$2`, runID, target.policyID).
		Scan(&decisionID, &decisionStatus, &decisionRuleID))
	require.Equal(t, service.RadarGateBlocked, decisionStatus, "rule_id=%s", decisionRuleID)
	require.NoError(t, db.QueryRow(`
		SELECT decision_id FROM evaluation_gate_decision_heads
		WHERE run_id=$1 AND policy_id=$2`, runID, target.policyID).Scan(&headDecisionID))
	require.Equal(t, decisionID, headDecisionID)
	var projectionStatus string
	require.NoError(t, db.QueryRow(`
		SELECT decision_id,status,last_outbox_event_id FROM evaluation_release_projections
		WHERE release_subject_id=$1`, target.releaseSubjectID).
		Scan(&projectionDecisionID, &projectionStatus, &lastEventID))
	require.Equal(t, decisionID, projectionDecisionID)
	require.Equal(t, "blocked", projectionStatus)
	require.Equal(t, gateEventID, lastEventID)
	var alertStatus service.RadarAlertStatus
	require.NoError(t, db.QueryRow(`
		SELECT status FROM evaluation_alerts
		WHERE tenant_id=$1 AND model_route='route-a' AND capability_domain='global'`, target.tenantID).
		Scan(&alertStatus))
	require.Equal(t, service.RadarAlertStatusOpen, alertStatus)
	return decisionID
}

func startRadarOutboxConsumer(t *testing.T, db *sql.DB, mode service.EvaluationOutboxConsumerMode) *service.EvaluationOutboxConsumerRuntime {
	t.Helper()
	dispatcher := service.NewEvaluationOutboxDispatcher(repository.NewEvaluationOutboxDomainRepository(db), mode)
	runtime := service.NewEvaluationOutboxConsumerRuntime(
		repository.NewEvaluationOutboxRepository(db),
		dispatcher,
		service.EvaluationOutboxConsumerRuntimeOptions{
			PollInterval:      10 * time.Millisecond,
			ClaimBatch:        16,
			MaxConcurrency:    4,
			LeaseDuration:     3 * time.Second,
			HeartbeatInterval: 250 * time.Millisecond,
			HandlerTimeout:    2 * time.Second,
			WorkerName:        "radar-control-plane-outbox",
			Mode:              mode,
		},
	)
	runtime.Start()
	t.Cleanup(runtime.Stop)
	return runtime
}

func completeRadarConsumerAnalysis(
	t *testing.T,
	db *sql.DB,
	fixture radarRevisionFixture,
	grading service.EvaluationGradingRepository,
	batchID uuid.UUID,
) {
	t.Helper()
	setRadarWorkerMode(t, db, fixture.workerID, "statistics", append(append([]string(nil), fixture.domains...), "global"))
	for _, domain := range fixture.domains {
		lease := claimRadarAnalysisLease(t, grading, fixture.workerID, fixture.runID, domain, batchID)
		require.Equal(t, "cell", lease.Scope)
		completeRadarAnalysisLease(t, grading, lease)
	}
	if len(fixture.domains) > 1 {
		waitRadarOutboxTypeDrained(t, db, fixture.runID, batchID, "global_recompute")
		completed := 0
		for {
			lease, err := grading.ClaimAnalysisJob(
				context.Background(), fixture.workerID, []string{"global"}, 3*time.Second,
			)
			require.NoError(t, err)
			if lease == nil {
				break
			}
			require.Equal(t, fixture.runID, lease.RunID)
			require.Equal(t, "global", lease.Scope)
			require.Equal(t, "global", lease.CapabilityDomain)
			require.Equal(t, batchID, lease.RevisionBatchID)
			completeRadarAnalysisLease(t, grading, lease)
			completed++
		}
		require.Positive(t, completed)
	}
}

func claimRadarAnalysisLease(
	t *testing.T,
	grading service.EvaluationGradingRepository,
	workerID, runID uuid.UUID,
	capability string,
	batchID uuid.UUID,
) *service.AnalysisJobLease {
	t.Helper()
	deadline := time.Now().Add(radarOutboxE2ETimeout)
	for time.Now().Before(deadline) {
		lease, err := grading.ClaimAnalysisJob(context.Background(), workerID, []string{capability}, 3*time.Second)
		require.NoError(t, err)
		if lease == nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		require.Equal(t, runID, lease.RunID)
		require.Equal(t, capability, lease.CapabilityDomain)
		require.Equal(t, batchID, lease.RevisionBatchID)
		return lease
	}
	t.Fatalf("analysis job %s for run %s and revision batch %s was not created", capability, runID, batchID)
	return nil
}

func completeRadarAnalysisLease(t *testing.T, grading service.EvaluationGradingRepository, lease *service.AnalysisJobLease) {
	t.Helper()
	aggregate, err := json.Marshal(map[string]any{
		"delta_pp":             1,
		"ci_high_pp":           1,
		"effective_pair_count": max(1, len(lease.ScoreRefs)),
		"evidence_sufficiency": "sufficient",
	})
	require.NoError(t, err)
	_, err = grading.CompleteAnalysisJob(context.Background(), lease.ID, lease.Token, service.AggregateSubmission{
		RunID: lease.RunID, ScoreRefs: lease.ScoreRefs, SnapshotRefs: lease.SnapshotRefs,
		InputSetHash: lease.InputSetHash, Aggregate: aggregate, LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)
}

func waitRadarOutboxEvent(t *testing.T, db *sql.DB, runID uuid.UUID, eventType string, batchID uuid.UUID) service.EvaluationOutboxEvent {
	t.Helper()
	deadline := time.Now().Add(radarOutboxE2ETimeout)
	var lastStatus, lastError string
	for time.Now().Before(deadline) {
		var event service.EvaluationOutboxEvent
		var batch uuid.NullUUID
		var errorCode sql.NullString
		err := db.QueryRow(`
			SELECT id,run_id,revision_batch_id,event_type,cause_set_hash,status,last_error_code
			FROM evaluation_outbox_events
			WHERE run_id=$1 AND event_type=$2
			  AND (($3::uuid IS NULL AND revision_batch_id IS NULL) OR revision_batch_id=$3)
			ORDER BY sequence DESC LIMIT 1`, runID, eventType, nullableRadarBatchID(batchID)).Scan(
			&event.ID, &event.RunID, &batch, &event.EventType, &event.CauseSetHash, &event.Status, &errorCode)
		if err == nil {
			if batch.Valid {
				event.RevisionBatchID = batch.UUID
			}
			lastStatus = string(event.Status)
			lastError = errorCode.String
			if event.Status == service.EvaluationOutboxCompleted {
				return event
			}
			if event.Status == service.EvaluationOutboxDeadLetter {
				t.Fatalf("outbox event %s for run %s was dead lettered with %s", eventType, runID, lastError)
			}
		} else if err != sql.ErrNoRows {
			require.NoError(t, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf(
		"outbox event %s for run %s did not complete, last status %q, error %q, pipeline %s",
		eventType, runID, lastStatus, lastError, radarPipelineState(t, db, runID),
	)
	return service.EvaluationOutboxEvent{}
}

func waitRadarOutboxEventID(t *testing.T, db *sql.DB, eventID uuid.UUID) {
	t.Helper()
	deadline := time.Now().Add(radarOutboxE2ETimeout)
	var status service.EvaluationOutboxStatus
	var lastError sql.NullString
	for time.Now().Before(deadline) {
		err := db.QueryRow(`
			SELECT status,last_error_code FROM evaluation_outbox_events WHERE id=$1`, eventID).
			Scan(&status, &lastError)
		if err == nil {
			if status == service.EvaluationOutboxCompleted {
				return
			}
			if status == service.EvaluationOutboxDeadLetter {
				t.Fatalf("outbox event %s was dead lettered with %s", eventID, lastError.String)
			}
		} else if err != sql.ErrNoRows {
			require.NoError(t, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("outbox event %s did not complete, last status %q, error %q", eventID, status, lastError.String)
}

func replayRadarCompletedCellEvent(t *testing.T, db *sql.DB, runID uuid.UUID) (uuid.UUID, int) {
	t.Helper()
	var eventID uuid.UUID
	var attempt int
	require.NoError(t, db.QueryRow(`
		SELECT id,attempt FROM evaluation_outbox_events
		WHERE run_id=$1 AND event_type='cell_recompute' AND status='completed'
		ORDER BY sequence LIMIT 1`, runID).Scan(&eventID, &attempt))
	replayRadarCompletedOutboxEvent(t, db, eventID)
	return eventID, attempt
}

func replayRadarCompletedOutboxEvent(t *testing.T, db *sql.DB, eventID uuid.UUID) {
	t.Helper()
	require.NoError(t, withRadarWriterTx(context.Background(), db, "outbox", func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			UPDATE evaluation_outbox_events
			SET status='pending',available_at=NOW(),lease_token_hash=NULL,lease_owner=NULL,
				lease_expires_at=NULL,last_error_code=NULL,updated_at=NOW()
			WHERE id=$1`, eventID)
		return err
	}))
}

func enqueueHistoricalSingleCellGlobal(t *testing.T, db *sql.DB, runID uuid.UUID) *service.EvaluationOutboxEvent {
	t.Helper()
	var snapshotID uuid.UUID
	var domain, route, aggregateHash string
	require.NoError(t, db.QueryRow(`
		SELECT snapshot_id,capability_domain,canonical_model_route,aggregate_hash
		FROM evaluation_aggregate_heads
		WHERE run_id=$1 AND capability_domain<>'global'`, runID).
		Scan(&snapshotID, &domain, &route, &aggregateHash))
	payload, err := json.Marshal(map[string]any{
		"snapshot_id": snapshotID, "capability_domain": domain,
		"model_route": route, "analysis_version": "v1",
	})
	require.NoError(t, err)
	event, err := repository.NewEvaluationOutboxRepository(db).Enqueue(context.Background(), service.EnqueueEvaluationOutboxInput{
		EventType: "global_recompute", RunID: runID, ScopeKey: domain + "/" + route,
		AnalysisVersion: "v1", SourceType: "aggregate_head", SourceID: snapshotID.String(),
		SourceHash: radarRevisionHash("historical-global:" + snapshotID.String() + ":" + aggregateHash),
		Payload:    payload, WorkOrigin: "initial",
	})
	require.NoError(t, err)
	return event
}

func requireRadarOutboxDrained(t *testing.T, db *sql.DB, runID, batchID uuid.UUID) {
	t.Helper()
	var pending, leased, dead int
	var queryErr error
	require.Eventually(t, func() bool {
		queryErr = db.QueryRow(`
			SELECT COUNT(*) FILTER (WHERE status='pending'),
			       COUNT(*) FILTER (WHERE status='leased'),
			       COUNT(*) FILTER (WHERE status='dead_letter')
			FROM evaluation_outbox_events
			WHERE run_id=$1
			  AND (($2::uuid IS NULL AND revision_batch_id IS NULL) OR revision_batch_id=$2)`,
			runID, nullableRadarBatchID(batchID)).Scan(&pending, &leased, &dead)
		return queryErr == nil && pending == 0 && leased == 0 && dead == 0
	}, radarOutboxE2ETimeout, 20*time.Millisecond,
		"outbox did not drain for run %s and batch %s: pending=%d leased=%d dead=%d err=%v",
		runID, batchID, pending, leased, dead, queryErr)
	require.NoError(t, queryErr)
}

func waitRadarOutboxTypeDrained(t *testing.T, db *sql.DB, runID, batchID uuid.UUID, eventType string) {
	t.Helper()
	var total, pending, leased, dead int
	var queryErr error
	require.Eventually(t, func() bool {
		queryErr = db.QueryRow(`
			SELECT COUNT(*),
			       COUNT(*) FILTER (WHERE status='pending'),
			       COUNT(*) FILTER (WHERE status='leased'),
			       COUNT(*) FILTER (WHERE status='dead_letter')
			FROM evaluation_outbox_events
			WHERE run_id=$1 AND event_type=$2
			  AND (($3::uuid IS NULL AND revision_batch_id IS NULL) OR revision_batch_id=$3)`,
			runID, eventType, nullableRadarBatchID(batchID)).Scan(&total, &pending, &leased, &dead)
		return queryErr == nil && total > 0 && pending == 0 && leased == 0 && dead == 0
	}, radarOutboxE2ETimeout, 20*time.Millisecond,
		"outbox type %s did not drain for run %s and batch %s: total=%d pending=%d leased=%d dead=%d err=%v",
		eventType, runID, batchID, total, pending, leased, dead, queryErr)
	require.NoError(t, queryErr)
}

func waitRadarRunStatus(t *testing.T, db *sql.DB, runID uuid.UUID, want string) {
	t.Helper()
	var status string
	var queryErr error
	require.Eventually(t, func() bool {
		queryErr = db.QueryRow(`SELECT status FROM evaluation_runs WHERE id=$1`, runID).Scan(&status)
		return queryErr == nil && status == want
	}, radarOutboxE2ETimeout, 20*time.Millisecond,
		"run %s did not reach %s, last status %q, outbox %s", runID, want, status, radarOutboxCounts(t, db, runID))
	require.NoError(t, queryErr)
}

func waitRadarRevisionBatchStatus(
	t *testing.T,
	db *sql.DB,
	batchID uuid.UUID,
	want service.RevisionBatchStatus,
) {
	t.Helper()
	var status service.RevisionBatchStatus
	var queryErr error
	require.Eventually(t, func() bool {
		queryErr = db.QueryRow(`SELECT status FROM evaluation_revision_batches WHERE id=$1`, batchID).Scan(&status)
		return queryErr == nil && status == want
	}, radarOutboxE2ETimeout, 20*time.Millisecond,
		"revision batch %s did not reach %s, last status %q, err=%v", batchID, want, status, queryErr)
	require.NoError(t, queryErr)
}

func nullableRadarBatchID(batchID uuid.UUID) any {
	if batchID == uuid.Nil {
		return nil
	}
	return batchID
}

func radarOutboxCounts(t *testing.T, db *sql.DB, runID uuid.UUID) string {
	t.Helper()
	rows, err := db.Query(`
		SELECT event_type,status,COUNT(*)
		FROM evaluation_outbox_events WHERE run_id=$1
		GROUP BY event_type,status ORDER BY event_type,status`, runID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var result string
	for rows.Next() {
		var eventType, status string
		var count int
		require.NoError(t, rows.Scan(&eventType, &status, &count))
		result += fmt.Sprintf("%s/%s=%d ", eventType, status, count)
	}
	require.NoError(t, rows.Err())
	return result
}

func radarPipelineState(t *testing.T, db *sql.DB, runID uuid.UUID) string {
	t.Helper()
	result := "outbox[" + radarOutboxCounts(t, db, runID) + "]"
	rows, err := db.Query(`
		SELECT scope,capability_domain,status,aggregate_revision,
		       COALESCE(revision_batch_id::text,'')
		FROM evaluation_analysis_jobs WHERE run_id=$1
		ORDER BY created_at,id`, runID)
	require.NoError(t, err)
	for rows.Next() {
		var scope, domain, status, batchID string
		var revision int64
		require.NoError(t, rows.Scan(&scope, &domain, &status, &revision, &batchID))
		result += fmt.Sprintf(" job[%s/%s/%s/r%d/b%s]", scope, domain, status, revision, batchID)
	}
	require.NoError(t, rows.Close())

	rows, err = db.Query(`
		SELECT capability_domain,canonical_model_route,aggregate_revision,
		       COALESCE(revision_batch_id::text,'')
		FROM evaluation_aggregate_heads WHERE run_id=$1
		ORDER BY capability_domain,canonical_model_route`, runID)
	require.NoError(t, err)
	for rows.Next() {
		var domain, route, batchID string
		var revision int64
		require.NoError(t, rows.Scan(&domain, &route, &revision, &batchID))
		result += fmt.Sprintf(" head[%s/%s/r%d/b%s]", domain, route, revision, batchID)
	}
	require.NoError(t, rows.Close())

	rows, err = db.Query(`
		SELECT id,status,control_epoch FROM evaluation_revision_batches
		WHERE run_id=$1 ORDER BY started_at,id`, runID)
	require.NoError(t, err)
	for rows.Next() {
		var batchID uuid.UUID
		var status service.RevisionBatchStatus
		var epoch int64
		require.NoError(t, rows.Scan(&batchID, &status, &epoch))
		result += fmt.Sprintf(" batch[%s/%s/e%d]", batchID, status, epoch)
	}
	require.NoError(t, rows.Close())
	return result
}

func enableRadarTrustedGateStorage(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`
		UPDATE evaluation_gate_storage_modes
		SET mode='trusted',updated_at=NOW() WHERE id=1`)
	require.NoError(t, err)
	var mode string
	require.NoError(t, db.QueryRow(`SELECT mode FROM evaluation_gate_storage_modes WHERE id=1`).Scan(&mode))
	require.Equal(t, "trusted", mode)
}
