package integration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

const radarRevisionPostgresImage = "postgres:18.1-alpine3.23"

func TestRadar199CutoverCloseBeforeOutboxTableExists(t *testing.T) {
	database := radarRevisionDatabase(t)
	_, err := database.db.Exec(`DROP TABLE evaluation_outbox_events CASCADE`)
	require.NoError(t, err)

	require.NoError(t, runRadar199Cutover(t, database, "drain"))
	require.NoError(t, runRadar199Cutover(t, database, "close"))
	requireCutoverState(t, database.db, "closed:audit:1")
}

func TestRadarRevisionPipelineE2E(t *testing.T) {
	database := radarRevisionDatabase(t)
	require.NoError(t, runRadar199Cutover(t, database, "audit"))
	activeKeyID := uuid.New()
	_, err := database.db.Exec(`
		INSERT INTO evaluation_evidence_signing_keys (id, key_reference, status, state_epoch)
		VALUES ($1, $2, 'active', 1)`, activeKeyID, "kms://radar/e2e/active")
	require.NoError(t, err)

	writerKinds := []string{"gateway", "replacement", "grader", "statistics", "reaper", "scheduler", "outbox"}
	for index, kind := range writerKinds {
		_, err := database.db.Exec(`
			INSERT INTO evaluation_writer_sessions (
				instance_id, writer_kind, protocol_version, active_lease_count,
				heartbeat_expires_at, last_transaction_at
			) VALUES (
				('00000000-0000-0000-0000-' || lpad($1::text, 12, '0'))::uuid,
				$2, 1, 1, NOW() + INTERVAL '5 minutes', NOW()
			)`, index+1, kind)
		require.NoError(t, err)
	}
	require.NoError(t, runRadar199Cutover(t, database, "drain"))
	requireCutoverState(t, database.db, "draining:audit:1")

	err = runRadar199Cutover(t, database, "close")
	require.Error(t, err)
	require.Contains(t, err.Error(), "active writer session count is 7")
	requireCutoverState(t, database.db, "draining:audit:1")
	_, err = database.db.Exec(`UPDATE evaluation_writer_sessions SET active_lease_count=0, heartbeat_expires_at=NOW()-INTERVAL '1 second'`)
	require.NoError(t, err)

	tx, err := database.db.Begin()
	require.NoError(t, err)
	_, err = tx.Exec(`SELECT 1`)
	require.NoError(t, err)
	err = runRadar199Cutover(t, database, "close")
	require.Error(t, err)
	require.Contains(t, err.Error(), "active database transaction count is 1")
	requireCutoverState(t, database.db, "draining:audit:1")
	require.NoError(t, tx.Rollback())

	lockConn, err := database.db.Conn(context.Background())
	require.NoError(t, err)
	_, err = lockConn.ExecContext(context.Background(), `SELECT pg_advisory_lock(199201)`)
	require.NoError(t, err)
	err = runRadar199Cutover(t, database, "close")
	require.Error(t, err)
	require.Contains(t, err.Error(), "advisory lock count is 1")
	requireCutoverState(t, database.db, "draining:audit:1")
	_, err = lockConn.ExecContext(context.Background(), `SELECT pg_advisory_unlock(199201)`)
	require.NoError(t, err)
	require.NoError(t, lockConn.Close())

	require.NoError(t, runRadar199Cutover(t, database, "close"))
	requireCutoverState(t, database.db, "closed:audit:1")

	validMigrationsDir := database.migrationsDir
	database.migrationsDir = corruptRadarMigrationCopy(t, validMigrationsDir)
	err = runRadar199Cutover(t, database, "migrate")
	require.Error(t, err)
	require.Contains(t, err.Error(), "migration checksum mismatch")
	requireCutoverState(t, database.db, "closed:audit:1")
	database.migrationsDir = validMigrationsDir

	require.NoError(t, runRadar199Cutover(t, database, "migrate"))
	requireCutoverState(t, database.db, "closed:audit:2")
	requireMigrationChecksums(t, database.db, "199_add_radar_evidence_revision_pipeline.sql", "200_add_score_idempotency_score_ref.sql", "201_add_revision_batch_events.sql")
	require.NoError(t, runRadar199Cutover(t, database, "enforce"))
	requireCutoverState(t, database.db, "closed:enforce:2")

	err = runRadar199Cutover(t, database, "reopen")
	require.Error(t, err)
	require.Contains(t, err.Error(), "protocol 2 writer session is required")
	requireCutoverState(t, database.db, "closed:enforce:2")
	require.NoError(t, runRadar199Cutover(t, database, "register"))
	require.NoError(t, runRadar199Cutover(t, database, "reopen"))
	requireCutoverState(t, database.db, "open:enforce:2")
	t.Setenv("RADAR_WRITER_INSTANCE_ID", database.writerInstanceID)

	rotated, err := repository.NewRadarGovernanceRepository(database.db).RotateEvidenceSigningKey(
		context.Background(),
		service.RotateEvidenceSigningKeyInput{
			ID: uuid.New(), KeyReference: "kms://radar/e2e/rotated",
			ExpectedActiveKeyID: activeKeyID, ExpectedActiveStateEpoch: 1,
		},
	)
	require.NoError(t, err)
	require.Equal(t, service.EvidenceSigningKeyActive, rotated.Status)
	proveRadarRevisionPipeline(t, database.db)
}

type radarRevisionFixture struct {
	userID   int64
	apiKeyID int64
	planID   uuid.UUID
	workerID uuid.UUID
	runID    uuid.UUID
	domains  []string
}

func proveRadarRevisionPipeline(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	fixture := createRadarRevisionFixture(t, db)
	grading := repository.NewEvaluationGradingRepository(db)
	sealRadarRevisionEvidence(t, db, fixture)

	initialScores := submitRadarScores(t, grading, fixture.workerID, []string{"0.80", "0.70", "0.90", "0.75"})
	require.Len(t, initialScores, 4)
	initialRuntime := startRadarOutboxConsumer(t, db, service.EvaluationOutboxConsumerModeCore)
	initialDecision, policyID, releaseSubjectHash := completeRadarAnalysisAndDecision(
		t, db, fixture, grading, uuid.Nil, nil,
	)
	waitRadarRunStatus(t, db, fixture.runID, "completed")
	initialRuntime.Stop()

	var runStatus string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT status FROM evaluation_runs WHERE id=$1`, fixture.runID).Scan(&runStatus))
	require.Equal(t, "completed", runStatus)

	batchRepo := repository.NewRadarGovernanceRepository(db).(service.RevisionBatchRepository)
	batch, err := batchRepo.CreateRevisionBatch(ctx, service.CreateRevisionBatchInput{
		RunID: fixture.runID, Reason: "e2e model quality regression", RequestedBy: fixture.userID,
		IdempotencyKey: radarRevisionHash("batch:" + fixture.runID.String()),
	})
	require.NoError(t, err)
	require.Equal(t, service.RevisionBatchRunning, batch.Status)

	setRadarWorkerMode(t, db, fixture.workerID, "grader", []string{"grader"})
	regradedScores := submitRadarScores(t, grading, fixture.workerID, []string{"0.65", "0.60", "0.70", "0.55"})
	require.Len(t, regradedScores, 4)
	for _, score := range regradedScores {
		require.Equal(t, 2, score.HeadVersion)
	}
	var regradedCount int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_grading_jobs
		WHERE revision_batch_id=$1 AND status='completed'
		  AND score_id IS NOT NULL AND score_created_at IS NOT NULL`, batch.ID).Scan(&regradedCount))
	require.Equal(t, 4, regradedCount)

	regradeRuntime := startRadarOutboxConsumer(t, db, service.EvaluationOutboxConsumerModeCore)
	regradeDecision, _, _ := completeRadarAnalysisAndDecision(
		t, db, fixture, grading, batch.ID, &radarDecisionSeed{
			policyID: policyID, releaseSubjectHash: releaseSubjectHash, supersedes: initialDecision.ID,
		},
	)
	waitRadarRevisionBatchStatus(t, db, batch.ID, service.RevisionBatchCompleted)
	regradeRuntime.Stop()
	require.NotEqual(t, initialDecision.ID, regradeDecision.ID)

	var currentDecisionID uuid.UUID
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT decision_id FROM evaluation_gate_decision_heads
		WHERE run_id=$1 AND policy_id=$2 AND release_subject_hash=$3`,
		fixture.runID, policyID, releaseSubjectHash).Scan(&currentDecisionID))
	require.Equal(t, regradeDecision.ID, currentDecisionID)
	var batchStatus service.RevisionBatchStatus
	require.NoError(t, db.QueryRowContext(ctx, `SELECT status FROM evaluation_revision_batches WHERE id=$1`, batch.ID).Scan(&batchStatus))
	require.Equal(t, service.RevisionBatchCompleted, batchStatus)

	var leased int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM evaluation_assignments WHERE status IN ('leased','running')) +
		       (SELECT COUNT(*) FROM evaluation_grading_jobs WHERE status='leased') +
		       (SELECT COUNT(*) FROM evaluation_analysis_jobs WHERE status='leased') +
		       (SELECT COUNT(*) FROM evaluation_outbox_events WHERE status='leased') +
		       (SELECT COALESCE(SUM(active_lease_count),0) FROM evaluation_writer_sessions)`).Scan(&leased))
	require.Zero(t, leased)
}

func createRadarRevisionFixture(t *testing.T, db *sql.DB) radarRevisionFixture {
	return createRadarRevisionFixtureWithDomains(t, db, []string{"coding", "reasoning"})
}

func createRadarRevisionFixtureWithDomains(t *testing.T, db *sql.DB, domains []string) radarRevisionFixture {
	t.Helper()
	require.NotEmpty(t, domains)
	ctx := context.Background()
	suffix := uuid.NewString()
	fixture := radarRevisionFixture{
		planID: uuid.New(), workerID: uuid.New(), domains: append([]string(nil), domains...),
	}
	require.NoError(t, db.QueryRowContext(ctx, `
		INSERT INTO users (email,username,password_hash,role,balance,concurrency,status)
		VALUES ($1,$2,'not-a-login-secret','admin',100,0,'active') RETURNING id`,
		"radar-revision-"+suffix+"@example.com", "radar-revision-"+suffix).Scan(&fixture.userID))
	require.NoError(t, db.QueryRowContext(ctx, `
		INSERT INTO api_keys (user_id,key,name,status,is_evaluation)
		VALUES ($1,$2,$3,'active',TRUE) RETURNING id`, fixture.userID,
		"sk-radar-revision-"+suffix, "radar-revision-"+suffix).Scan(&fixture.apiKeyID))
	datasetID := uuid.New()
	require.NoError(t, withRadarWriterTx(ctx, db, "api", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO evaluation_dataset_versions (
				id,dataset_key,version,manifest_sha256,source_type,status,created_by,tenant_id
			) VALUES ($1,$2,'v1',$3,'synthetic','draft',$4,$4)`,
			datasetID, "radar-revision-"+suffix, strings.Repeat("1", 64), fixture.userID); err != nil {
			return err
		}
		for index, domain := range domains {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO evaluation_cases (
					id,dataset_version_id,case_key,capability_domain,priority,weight,sample_count,
					prompt_spec,expected_spec,execution_spec,grader_id,grader_version,
					content_sha256,confidentiality,estimated_cost
				) VALUES ($1,$2,$3,$4,'P0',1,1,'{}','{}','{}','grader','v1',$5,'synthetic',0.01)`,
				uuid.New(), datasetID, fmt.Sprintf("case-%d", index), domain,
				radarRevisionHash(fmt.Sprintf("case:%s:%d", datasetID, index))); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE evaluation_dataset_versions SET status='published',published_at=NOW() WHERE id=$1`, datasetID); err != nil {
			return err
		}
		matrix := `[{
			"route":"route-a",
			"baseline":{"route":"route-a-baseline"},
			"candidate":{"route":"route-a-candidate"}
		}]`
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO evaluation_plans (
				id,name,dataset_version_id,gateway_api_key_id,trigger_type,model_matrix,
				max_run_cost,daily_cost_limit,max_concurrency,created_by,tenant_id
			) VALUES ($1,$2,$3,$4,'manual',$5::jsonb,100,100,2,$6,$6)`,
			fixture.planID, "radar-revision-"+suffix, datasetID, fixture.apiKeyID, matrix, fixture.userID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO evaluation_workers (id,name,worker_kind,token_hash,capabilities,image_digest,tenant_id)
			VALUES ($1,$2,'grader',$3,ARRAY['grader'],$4,$5)`, fixture.workerID,
			"radar-revision-worker-"+suffix, radarRevisionHash("worker:"+suffix),
			"worker@sha256:"+strings.Repeat("3", 64), fixture.userID)
		return err
	}))
	run, err := repository.NewEvaluationRepository(db).CreateRunWithMatrix(ctx, service.CreateRunInput{
		PlanID: fixture.planID, TriggerSource: "manual", CreatedBy: fixture.userID,
	})
	require.NoError(t, err)
	fixture.runID = run.ID
	return fixture
}

func sealRadarRevisionEvidence(t *testing.T, db *sql.DB, fixture radarRevisionFixture) {
	t.Helper()
	ctx := context.Background()
	startRadarRevisionRun(t, db, fixture)
	require.NoError(t, withRadarWriterTx(ctx, db, "gateway", func(tx *sql.Tx) error {
		semanticsID := uuid.New()
		semanticsHash := radarRevisionHash("semantics:" + fixture.runID.String())
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO evaluation_request_semantics (
				id,schema_version,canonical_semantics_bytes,request_semantics_sha256
			) VALUES ($1,'radar-request-semantics-v1',convert_to('{}','UTF8'),$2)`,
			semanticsID, semanticsHash); err != nil {
			return err
		}
		var signingKeyID uuid.UUID
		if err := tx.QueryRowContext(ctx, `SELECT id FROM evaluation_evidence_signing_keys WHERE status='active'`).Scan(&signingKeyID); err != nil {
			if err != sql.ErrNoRows {
				return err
			}
			signingKeyID = uuid.New()
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO evaluation_evidence_signing_keys (id,key_reference,status,state_epoch)
				VALUES ($1,$2,'active',1)`, signingKeyID,
				"kms://radar/e2e/"+signingKeyID.String()); err != nil {
				return err
			}
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT assignment.id,assignment.sample_id,sample.model_route,
			       pair_spec.request_manifest_id,pair_spec.request_manifest_sha256
			FROM evaluation_assignments assignment
			JOIN evaluation_samples sample ON sample.id=assignment.sample_id
			JOIN evaluation_side_specs side_spec ON side_spec.sample_id=sample.id
			JOIN evaluation_pair_specs pair_spec ON pair_spec.id=side_spec.pair_spec_id
			WHERE sample.run_id=$1 ORDER BY sample.model_route`, fixture.runID)
		if err != nil {
			return err
		}
		type assignment struct {
			id, sampleID, manifestID uuid.UUID
			modelRoute, manifestHash string
		}
		var assignments []assignment
		for rows.Next() {
			var item assignment
			if err := rows.Scan(&item.id, &item.sampleID, &item.modelRoute, &item.manifestID, &item.manifestHash); err != nil {
				rows.Close()
				return err
			}
			assignments = append(assignments, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		expectedAssignments := len(fixture.domains) * 2
		if len(assignments) != expectedAssignments {
			return fmt.Errorf("expected %d paired assignments, got %d", expectedAssignments, len(assignments))
		}
		for index, item := range assignments {
			routeTraceID := "revision-evidence-" + uuid.NewString()
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO evaluation_route_evidence (
					route_trace_id,evaluation_run_id,sample_id,api_key_id,request_id,
					requested_model,resolved_model,route_profile_version,provider,region,
					attempts,fallback_chain,finish_reason,input_tokens,output_tokens,
					latency_ms,billed_amount,transport_status,started_at,finished_at,
					schema_version,canonicalization_version,assignment_id,request_ordinal,
					lease_epoch,request_manifest_id,request_manifest_sha256,request_slot_id,
					request_semantics_id,request_semantics_sha256,
					request_semantics_policy_sha256,request_tool_schema_sha256,
					request_allowed_tool_set_sha256,evidence_revision,terminal_at,sealed_at,
					payload_hash,signing_key_id,payload_hmac,billing_status,gateway_image_digest
				) VALUES (
					$1,$2,$3,$4,$5,$6,'model-a','route-v1','provider-a','region-a',
					1,'[]','stop',1,1,1,0.00000001,'succeeded',NOW(),NOW(),
					'radar-route-evidence-v1','rfc8785-v1',$7,0,0,$8,$9,'request-0',
					$10,$11,$12,$13,$14,1,NOW(),NOW(),$15,$16,$17,'complete','sha256:gateway'
				)`, routeTraceID, fixture.runID, item.sampleID,
				fixture.apiKeyID, "request-"+uuid.NewString(), item.modelRoute, item.id,
				item.manifestID, item.manifestHash, semanticsID, semanticsHash,
				strings.Repeat("4", 64), strings.Repeat("5", 64), strings.Repeat("6", 64),
				radarRevisionHash(fmt.Sprintf("evidence:%s:%d", fixture.runID, index)),
				signingKeyID, strings.Repeat("7", 64)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE evaluation_assignments
				SET status='evidence_uploaded',lease_epoch=0,evidence_manifest='{}',
				    lease_token_hash=NULL,leased_by=NULL,lease_expires_at=NULL,heartbeat_at=NULL,
				    updated_at=NOW()
				WHERE id=$1`, item.id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE evaluation_samples
				SET status='evidence_uploaded',route_trace_id=$2,updated_at=NOW()
				WHERE id=$1`, item.sampleID, routeTraceID); err != nil {
				return err
			}
		}
		return nil
	}))
}

func startRadarRevisionRun(t *testing.T, db *sql.DB, fixture radarRevisionFixture) {
	t.Helper()
	setRadarWorkerMode(t, db, fixture.workerID, "runner", fixture.domains)
	lease, err := repository.NewEvaluationRepository(db).ClaimAssignment(
		context.Background(), fixture.workerID, fixture.domains, time.Minute,
	)
	require.NoError(t, err)
	require.NotNil(t, lease)
	require.Equal(t, fixture.runID, lease.RunID)
	setRadarWorkerMode(t, db, fixture.workerID, "grader", []string{"grader"})
}

func submitRadarScores(t *testing.T, grading service.EvaluationGradingRepository, workerID uuid.UUID, values []string) []*service.Score {
	t.Helper()
	ctx := context.Background()
	scores := make([]*service.Score, 0, len(values))
	for _, value := range values {
		lease, err := grading.ClaimGradingLease(ctx, workerID, []string{"grader"}, time.Minute)
		require.NoError(t, err)
		require.NotNil(t, lease)
		score, err := grading.SubmitScore(ctx, lease.ID, lease.Token, service.ScoreSubmission{
			Score: decimal.RequireFromString(value), LeaseEpoch: lease.LeaseEpoch,
		})
		require.NoError(t, err)
		scores = append(scores, score)
	}
	return scores
}

type radarDecisionSeed struct {
	policyID           uuid.UUID
	releaseSubjectHash string
	supersedes         uuid.UUID
}

func completeRadarAnalysisAndDecision(
	t *testing.T,
	db *sql.DB,
	fixture radarRevisionFixture,
	grading service.EvaluationGradingRepository,
	batchID uuid.UUID,
	seed *radarDecisionSeed,
) (*service.RadarGateDecisionRecord, uuid.UUID, string) {
	t.Helper()
	ctx := context.Background()
	completeRadarConsumerAnalysis(t, db, fixture, grading, batchID)
	gateEvent := waitRadarOutboxEvent(t, db, fixture.runID, "gate_reevaluation", batchID)
	requireRadarOutboxDrained(t, db, fixture.runID, batchID)

	policyID := uuid.New()
	releaseSubjectHash := radarRevisionHash("release:" + fixture.runID.String())
	var supersedes *uuid.UUID
	if seed == nil {
		require.NoError(t, withRadarWriterTx(ctx, db, "api", func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO evaluation_gate_policies (
					id,version,policy,policy_hash,enforcement_starts_at,created_by
				) VALUES ($1,$2,'{}',$3,NOW(),$4)`, policyID,
				int(time.Now().UnixNano()%1_000_000_000)+1, radarRevisionHash("policy:"+policyID.String()), fixture.userID)
			return err
		}))
	} else {
		policyID = seed.policyID
		releaseSubjectHash = seed.releaseSubjectHash
		supersedes = &seed.supersedes
	}
	decision, err := repository.NewRadarGovernanceRepository(db).RecordGateDecision(ctx, service.RadarGateDecisionInput{
		RunID: fixture.runID, PolicyID: policyID, Status: service.RadarGatePassed,
		RuleIDs:  []string{"pass"},
		Evidence: json.RawMessage(`{}`), EvidenceHash: radarRevisionHash("decision:" + gateEvent.ID.String()),
		ReleaseSubjectHash: releaseSubjectHash, SourceWatermark: json.RawMessage(`{}`),
		SupersedesDecisionID: supersedes, CauseSetHash: gateEvent.CauseSetHash,
	})
	require.NoError(t, err)
	return decision, policyID, releaseSubjectHash
}

func setRadarWorkerMode(t *testing.T, db *sql.DB, workerID uuid.UUID, kind string, capabilities []string) {
	t.Helper()
	require.NoError(t, withRadarWriterTx(context.Background(), db, "api", func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			UPDATE evaluation_workers
			SET worker_kind=$2,capabilities=$3,updated_at=NOW() WHERE id=$1`,
			workerID, kind, pq.Array(capabilities))
		return err
	}))
}

func withRadarWriterTx(ctx context.Context, db *sql.DB, kind string, fn func(*sql.Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	instanceID := uuid.New()
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO evaluation_writer_sessions (
			instance_id,writer_kind,protocol_version,heartbeat_expires_at,last_transaction_at
		) VALUES ($1,$2,2,NOW()+INTERVAL '5 minutes',NOW())
		ON CONFLICT (instance_id) DO UPDATE SET heartbeat_expires_at=EXCLUDED.heartbeat_expires_at`, instanceID, kind); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"app.evaluation_writer_instance_id": instanceID.String(),
		"app.evaluation_writer_protocol":    "2",
		"app.evaluation_writer_kind":        kind,
	} {
		if _, err = tx.ExecContext(ctx, `SELECT set_config($1,$2,true)`, name, value); err != nil {
			return err
		}
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func radarRevisionHash(value string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func requireCutoverState(t *testing.T, db *sql.DB, want string) {
	t.Helper()
	var state string
	require.NoError(t, db.QueryRow(`
		SELECT write_mode || ':' || guard_mode || ':' || minimum_protocol_version::text
		FROM evaluation_schema_cutovers WHERE id=1`).Scan(&state))
	require.Equal(t, want, strings.TrimSpace(state))
}

func requireMigrationChecksums(t *testing.T, db *sql.DB, names ...string) {
	t.Helper()
	for _, name := range names {
		contents, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
		require.NoError(t, err)
		want := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimSpace(string(contents)))))
		var got string
		require.NoError(t, db.QueryRow(`SELECT checksum FROM schema_migrations WHERE filename=$1`, name).Scan(&got))
		require.Equal(t, want, got, name)
	}
}

type radarRevisionTestDatabase struct {
	db               *sql.DB
	dsn              string
	psqlBin          string
	migrationsDir    string
	writerInstanceID string
}

func radarRevisionDatabase(t *testing.T) *radarRevisionTestDatabase {
	t.Helper()
	ctx := context.Background()
	testcontainers.SkipIfProviderIsNotHealthy(t)
	container, err := tcpostgres.Run(
		ctx,
		radarRevisionPostgresImage,
		tcpostgres.WithDatabase("sub2api_revision_test"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, container.Terminate(context.Background())) })
	dsn, err := container.ConnectionString(ctx, "sslmode=disable", "TimeZone=UTC")
	require.NoError(t, err)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.Eventually(t, func() bool { return db.PingContext(ctx) == nil }, 30*time.Second, 250*time.Millisecond)
	require.NoError(t, repository.ApplyMigrations(ctx, db))
	containerID := container.GetContainerID()
	require.NotEmpty(t, containerID)
	psqlBin := filepath.Join(t.TempDir(), "psql")
	wrapper := "#!/usr/bin/env bash\nexec docker exec -i " + containerID + " psql \"$@\"\n"
	require.NoError(t, os.WriteFile(psqlBin, []byte(wrapper), 0o700))
	return &radarRevisionTestDatabase{
		db:               db,
		dsn:              "postgres://postgres:postgres@localhost:5432/sub2api_revision_test?sslmode=disable",
		psqlBin:          psqlBin,
		migrationsDir:    mustAbs(t, filepath.Join("..", "..", "migrations")),
		writerInstanceID: uuid.NewString(),
	}
}

func corruptRadarMigrationCopy(t *testing.T, sourceDir string) string {
	t.Helper()
	targetDir := t.TempDir()
	for _, name := range []string{
		"199_add_radar_evidence_revision_pipeline.sql",
		"200_add_score_idempotency_score_ref.sql",
		"201_add_revision_batch_events.sql",
	} {
		contents, err := os.ReadFile(filepath.Join(sourceDir, name))
		require.NoError(t, err)
		if strings.HasPrefix(name, "199_") {
			contents = append(contents, []byte("\nSELECT 199;\n")...)
		}
		require.NoError(t, os.WriteFile(filepath.Join(targetDir, name), contents, 0o600))
	}
	return targetDir
}

func runRadar199Cutover(t *testing.T, database *radarRevisionTestDatabase, phase string) error {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "radar_migration_199_cutover.sh"))
	require.NoError(t, err)
	cmd := exec.Command(script, phase)
	cmd.Env = append(os.Environ(),
		"RADAR_DATABASE_URL="+database.dsn,
		"RADAR_PSQL_BIN="+database.psqlBin,
		"RADAR_MIGRATIONS_DIR="+database.migrationsDir,
		"RADAR_WRITER_INSTANCE_ID="+database.writerInstanceID,
		"RADAR_WRITER_KIND=api",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return &cutoverCommandError{phase: phase, output: string(output), err: err}
	}
	return nil
}

func mustAbs(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	require.NoError(t, err)
	return abs
}

type cutoverCommandError struct {
	phase  string
	output string
	err    error
}

func (e *cutoverCommandError) Error() string {
	return "radar migration 199 cutover " + e.phase + " failed: " + e.err.Error() + ": " + e.output
}

func (e *cutoverCommandError) Unwrap() error { return e.err }
