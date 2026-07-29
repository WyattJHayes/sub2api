package repository

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

type evaluationExperimentContractRepository struct {
	db *sql.DB
}

func NewEvaluationExperimentContractRepository(db *sql.DB) *evaluationExperimentContractRepository {
	return &evaluationExperimentContractRepository{db: db}
}

func (r *evaluationExperimentContractRepository) CreateRequestManifest(ctx context.Context, manifest service.RequestManifest) (*service.RequestManifestRecord, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("nil evaluation experiment contract repository")
	}
	canonical, hash, err := service.CanonicalRequestManifest(manifest)
	if err != nil {
		return nil, err
	}
	if manifest.ID == uuid.Nil {
		manifest.ID = uuid.New()
	}
	record := &service.RequestManifestRecord{
		ID:              manifest.ID,
		SchemaVersion:   manifest.SchemaVersion,
		InteractionType: manifest.InteractionType,
		ManifestSHA256:  hash,
	}
	err = WithEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("control"), func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			INSERT INTO evaluation_request_manifests (
				id, schema_version, interaction_type, canonical_manifest_bytes, manifest_sha256
			) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (manifest_sha256) DO NOTHING`,
			manifest.ID, manifest.SchemaVersion, manifest.InteractionType, canonical, hash)
		if err != nil {
			return fmt.Errorf("insert request manifest: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read request manifest insert result: %w", err)
		}
		if affected == 0 {
			if err := tx.QueryRowContext(ctx, `
				SELECT id FROM evaluation_request_manifests WHERE manifest_sha256 = $1`, hash).Scan(&record.ID); err != nil {
				return fmt.Errorf("load existing request manifest: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return record, nil
}

func (r *evaluationExperimentContractRepository) CreatePairContract(
	ctx context.Context,
	runID uuid.UUID,
	manifest service.RequestManifest,
	pair service.PairSpec,
	baseline service.SideSpec,
	candidate service.SideSpec,
) (*service.PairBinding, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("nil evaluation experiment contract repository")
	}
	if runID == uuid.Nil {
		return nil, fmt.Errorf("evaluation run id is required")
	}
	if manifest.ID == uuid.Nil {
		manifest.ID = uuid.New()
	}
	if pair.ExpectedRequestManifestID == uuid.Nil {
		pair.ExpectedRequestManifestID = manifest.ID
	}
	if pair.ID == uuid.Nil {
		pair.ID = uuid.New()
	}
	if baseline.ID == uuid.Nil {
		baseline.ID = uuid.New()
	}
	if candidate.ID == uuid.Nil {
		candidate.ID = uuid.New()
	}
	if baseline.PairSpecID == uuid.Nil {
		baseline.PairSpecID = pair.ID
	}
	if candidate.PairSpecID == uuid.Nil {
		candidate.PairSpecID = pair.ID
	}
	if baseline.SampleID == uuid.Nil || candidate.SampleID == uuid.Nil {
		return nil, fmt.Errorf("pair side sample ids are required")
	}
	manifestBytes, manifestHash, err := service.CanonicalRequestManifest(manifest)
	if err != nil {
		return nil, err
	}
	pairBytes, pairHash, err := service.CanonicalPairSpec(pair)
	if err != nil {
		return nil, err
	}
	baselineBytes, baselineHash, err := service.CanonicalSideSpec(baseline)
	if err != nil {
		return nil, err
	}
	candidateBytes, candidateHash, err := service.CanonicalSideSpec(candidate)
	if err != nil {
		return nil, err
	}
	binding, err := service.BuildPairBinding(pair, baseline, candidate)
	if err != nil {
		return nil, err
	}
	binding.PairSpecHash = pairHash
	binding.BaselineSideHash = baselineHash
	binding.CandidateSideHash = candidateHash
	result := &service.PairBinding{}
	err = WithEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("control"), func(tx *sql.Tx) error {
		return insertPairContractTx(ctx, tx, runID, manifest, pair, baseline, candidate,
			manifestBytes, manifestHash, pairBytes, pairHash, baselineBytes, baselineHash,
			candidateBytes, candidateHash, binding, result)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func insertPairContractTx(
	ctx context.Context,
	tx *sql.Tx,
	runID uuid.UUID,
	manifest service.RequestManifest,
	pair service.PairSpec,
	baseline service.SideSpec,
	candidate service.SideSpec,
	manifestBytes []byte,
	manifestHash string,
	pairBytes []byte,
	pairHash string,
	baselineBytes []byte,
	baselineHash string,
	candidateBytes []byte,
	candidateHash string,
	binding service.PairBinding,
	result *service.PairBinding,
) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_request_manifests (
			id, schema_version, interaction_type, canonical_manifest_bytes, manifest_sha256
		) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (manifest_sha256) DO NOTHING`, manifest.ID, manifest.SchemaVersion,
		manifest.InteractionType, manifestBytes, manifestHash); err != nil {
		return fmt.Errorf("insert pair request manifest: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT id FROM evaluation_request_manifests WHERE manifest_sha256 = $1`, manifestHash).Scan(&manifest.ID); err != nil {
		return fmt.Errorf("load pair request manifest: %w", err)
	}
	if pair.ExpectedRequestManifestID != manifest.ID {
		pair.ExpectedRequestManifestID = manifest.ID
		var err error
		pairBytes, pairHash, err = service.CanonicalPairSpec(pair)
		if err != nil {
			return err
		}
		binding, err = service.BuildPairBinding(pair, baseline, candidate)
		if err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_pair_specs (
			id, run_id, case_id, sample_index, repeat_index,
			request_manifest_id, request_manifest_sha256, schema_version,
			canonical_spec, pair_spec_hash
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10)`, pair.ID,
		runID, pair.CaseID, pair.SampleIndex, pair.RepeatIndex, manifest.ID,
		manifestHash, service.RequestManifestSchemaVersion, pairBytes, pairHash); err != nil {
		return fmt.Errorf("insert pair spec: %w", err)
	}
	for _, side := range []struct {
		spec  service.SideSpec
		bytes []byte
		hash  string
	}{
		{baseline, baselineBytes, baselineHash},
		{candidate, candidateBytes, candidateHash},
	} {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO evaluation_side_specs (
				id, pair_spec_id, sample_id, side, schema_version,
				canonical_spec, side_spec_hash
			) VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7)`, side.spec.ID,
			pair.ID, side.spec.SampleID, side.spec.Side, service.RequestManifestSchemaVersion,
			side.bytes, side.hash); err != nil {
			return fmt.Errorf("insert %s side spec: %w", side.spec.Side, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_pair_bindings (
			id, pair_spec_id, baseline_side_spec_id, candidate_side_spec_id,
			pair_binding_hash
		) VALUES ($1, $2, $3, $4, $5)`, binding.ID, pair.ID, baseline.ID,
		candidate.ID, binding.BindingHash); err != nil {
		return fmt.Errorf("insert pair binding: %w", err)
	}
	if result != nil {
		*result = binding
		result.PairSpecHash = pairHash
	}
	return nil
}
