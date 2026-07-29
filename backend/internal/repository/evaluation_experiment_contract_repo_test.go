package repository

import (
	"context"
	"errors"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestCreateRequestManifestUsesCanonicalHash(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()

	manifest := service.RequestManifest{
		ID:              uuid.New(),
		SchemaVersion:   service.RequestManifestSchemaVersion,
		InteractionType: service.InteractionSingle,
		OrdinalPolicy:   service.OrdinalPolicyExact,
		MinRequests:     1,
		MaxRequests:     1,
		RequestSlots: []service.RequestSlot{{
			SlotID: "slot-0", OrdinalMin: 0, OrdinalMax: 0,
			SemanticsMode:                  service.SemanticsModeExact,
			ExpectedRequestSemanticsSHA256: serviceContractHash,
			MaxOccurrences:                 1,
		}},
	}
	_, wantHash, err := service.CanonicalRequestManifest(manifest)
	require.NoError(t, err)

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO evaluation_writer_sessions`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`set_config\('app.evaluation_writer_protocol'`).WithArgs("1").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`set_config\('app.evaluation_writer_instance_id'`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`SELECT write_mode, guard_mode, minimum_protocol_version FROM evaluation_schema_cutovers`).
		WillReturnRows(sqlmock.NewRows([]string{"write_mode", "guard_mode", "minimum_protocol_version"}).AddRow("open", "audit", int64(0)))
	mock.ExpectExec(`INSERT INTO evaluation_request_manifests`).WithArgs(manifest.ID, service.RequestManifestSchemaVersion, service.InteractionSingle, sqlmock.AnyArg(), wantHash).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	repo := NewEvaluationExperimentContractRepository(db)
	record, err := repo.CreateRequestManifest(context.Background(), manifest)
	require.NoError(t, err)
	require.Equal(t, manifest.ID, record.ID)
	require.Equal(t, wantHash, record.ManifestSHA256)
	require.NoError(t, mock.ExpectationsWereMet())
}

const serviceContractHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestCreatePairContractRollsBackWhenCandidateSideInsertFails(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()

	runID := uuid.New()
	manifest := service.RequestManifest{
		ID:              uuid.New(),
		SchemaVersion:   service.RequestManifestSchemaVersion,
		InteractionType: service.InteractionSingle,
		OrdinalPolicy:   service.OrdinalPolicyExact,
		MinRequests:     1,
		MaxRequests:     1,
		RequestSlots: []service.RequestSlot{{
			SlotID: "slot-0", OrdinalMin: 0, OrdinalMax: 0,
			SemanticsMode:                  service.SemanticsModeExact,
			ExpectedRequestSemanticsSHA256: serviceContractHash,
			MaxOccurrences:                 1,
		}},
	}
	pair := service.PairSpec{
		ID:                            uuid.New(),
		DatasetVersionID:              uuid.New(),
		CaseID:                        uuid.New(),
		PromptSHA256:                  serviceContractHash,
		ToolSchemaSHA256:              serviceContractHash,
		ExpectedRequestManifestID:     manifest.ID,
		ExpectedRequestManifestSHA256: serviceContractHash,
		GraderID:                      "grader",
		GraderVersion:                 "v1",
		Region:                        "us-east",
		Protocol:                      "openai-chat",
	}
	baseline := service.SideSpec{
		ID:                       uuid.New(),
		SampleID:                 uuid.New(),
		Side:                     "baseline",
		ModelRoute:               "baseline:route",
		ModelConfigSHA256:        serviceContractHash,
		ExpectedModelAlias:       "route",
		ExpectedResolvedModel:    "route",
		RouteProfileVersion:      "route-v1",
		ProviderParametersSHA256: serviceContractHash,
	}
	candidate := baseline
	candidate.ID = uuid.New()
	candidate.SampleID = uuid.New()
	candidate.Side = "candidate"
	candidate.ModelRoute = "candidate:route"

	manifestBytes, manifestHash, err := service.CanonicalRequestManifest(manifest)
	require.NoError(t, err)
	pairBytes, pairHash, err := service.CanonicalPairSpec(pair)
	require.NoError(t, err)
	baselineBytes, baselineHash, err := service.CanonicalSideSpec(baseline)
	require.NoError(t, err)
	candidateBytes, candidateHash, err := service.CanonicalSideSpec(candidate)
	require.NoError(t, err)
	binding, err := service.BuildPairBinding(pair, baseline, candidate)
	require.NoError(t, err)

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO evaluation_writer_sessions`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`set_config\('app.evaluation_writer_protocol'`).WithArgs("1").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`set_config\('app.evaluation_writer_instance_id'`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`SELECT write_mode, guard_mode, minimum_protocol_version`).
		WillReturnRows(sqlmock.NewRows([]string{"write_mode", "guard_mode", "minimum_protocol_version"}).AddRow("open", "audit", int64(0)))
	mock.ExpectExec(`INSERT INTO evaluation_request_manifests`).WithArgs(manifest.ID, manifest.SchemaVersion, manifest.InteractionType, manifestBytes, manifestHash).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`SELECT id FROM evaluation_request_manifests`).WithArgs(manifestHash).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(manifest.ID))
	mock.ExpectExec(`INSERT INTO evaluation_pair_specs`).WithArgs(pair.ID, runID, pair.CaseID, pair.SampleIndex, pair.RepeatIndex, manifest.ID, manifestHash, service.RequestManifestSchemaVersion, pairBytes, pairHash).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO evaluation_side_specs`).WithArgs(baseline.ID, pair.ID, baseline.SampleID, baseline.Side, service.RequestManifestSchemaVersion, baselineBytes, baselineHash).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO evaluation_side_specs`).WithArgs(candidate.ID, pair.ID, candidate.SampleID, candidate.Side, service.RequestManifestSchemaVersion, candidateBytes, candidateHash).
		WillReturnError(errors.New("candidate side insert failed"))
	mock.ExpectRollback()

	repo := NewEvaluationExperimentContractRepository(db)
	_, err = repo.CreatePairContract(context.Background(), runID, manifest, pair, baseline, candidate)
	require.ErrorContains(t, err, "candidate side insert failed")
	require.NoError(t, mock.ExpectationsWereMet())
	_ = binding
}
