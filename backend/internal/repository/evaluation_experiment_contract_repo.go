package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

var ErrIncompleteExperimentBinding = errors.New("evaluation experiment binding is incomplete")

func requireCompletePairBindings(expected, actual int) error {
	if expected < 1 || actual != expected {
		return fmt.Errorf("%w: expected %d pair bindings, found %d", ErrIncompleteExperimentBinding, expected, actual)
	}
	return nil
}

const radarRouteProfileVersion = "radar-route-profile-v1"

type evaluationExperimentPersistResult struct {
	Bindings []service.PairBindingRef
	Pairs    int
}

type evaluationExperimentCase struct {
	evaluationCaseForRun
	promptSHA256 string
	toolSHA256   string
	manifest     service.CanonicalRequestManifest
	manifestID   uuid.UUID
}

type evaluationExperimentSample struct {
	id             uuid.UUID
	modelRoute     string
	modelConfig    []byte
	modelConfigSHA string
	side           string
	sideSpecID     uuid.UUID
}

func persistEvaluationExperimentContracts(
	ctx context.Context,
	tx *sql.Tx,
	runID uuid.UUID,
	runCreatedAt time.Time,
	cases []evaluationCaseForRun,
	matrix []evaluationMatrixEntry,
) (evaluationExperimentPersistResult, error) {
	if runID == uuid.Nil || len(cases) == 0 || len(matrix) == 0 {
		return evaluationExperimentPersistResult{}, ErrIncompleteExperimentBinding
	}
	result := evaluationExperimentPersistResult{}
	manifestCache := make(map[string]struct {
		id       uuid.UUID
		manifest service.CanonicalRequestManifest
	})
	for _, evaluationCase := range cases {
		requestSemantics, err := service.DeriveSingleRequestSemantics(evaluationCase.promptSpec)
		if err != nil {
			return result, fmt.Errorf("derive evaluation case %s request semantics: %w", evaluationCase.id, err)
		}
		promptSHA := requestSemantics.PromptHash
		toolSHA := requestSemantics.ToolSchemaHash
		cacheKey := promptSHA + "\x00" + toolSHA
		cached, ok := manifestCache[cacheKey]
		if !ok {
			semantics, semanticsErr := service.CanonicalizeRequestSemantics(requestSemantics)
			if semanticsErr != nil {
				return result, fmt.Errorf("freeze request semantics for case %s: %w", evaluationCase.id, semanticsErr)
			}
			manifest, manifestErr := service.CanonicalizeRequestManifest(service.RequestManifest{
				SchemaVersion:   service.RequestManifestSchemaV1,
				InteractionType: "single",
				OrdinalPolicy:   "exact",
				MinRequests:     1,
				MaxRequests:     1,
				RequestSlots: []service.RequestSlot{{
					SlotID:                         "request-0",
					OrdinalMin:                     0,
					OrdinalMax:                     0,
					Phase:                          "primary",
					Required:                       true,
					SemanticsMode:                  "exact",
					ExpectedRequestSemanticsSHA256: semantics.SHA256,
					ToolSchemaSHA256:               toolSHA,
					AllowedToolSetSHA256:           requestSemantics.ProvidedToolSetHash,
					MaxOccurrences:                 1,
				}},
			})
			if manifestErr != nil {
				return result, fmt.Errorf("freeze request manifest for case %s: %w", evaluationCase.id, manifestErr)
			}
			manifestID, insertErr := insertEvaluationRequestManifest(ctx, tx, manifest)
			if insertErr != nil {
				return result, insertErr
			}
			cached = struct {
				id       uuid.UUID
				manifest service.CanonicalRequestManifest
			}{id: manifestID, manifest: manifest}
			manifestCache[cacheKey] = cached
		}

		for matrixIndex, matrixEntry := range matrix {
			for sampleIndex := 0; sampleIndex < evaluationCase.sampleCount; sampleIndex++ {
				pairID := uuid.New()
				pair := service.PairSpec{
					DatasetVersionID:              evaluationCase.datasetVersionID,
					CaseID:                        evaluationCase.id,
					SampleIndex:                   sampleIndex,
					RepeatIndex:                   matrixIndex,
					PromptSHA256:                  promptSHA,
					ToolSchemaSHA256:              toolSHA,
					ExpectedRequestManifestID:     cached.id,
					ExpectedRequestManifestSHA256: cached.manifest.SHA256,
					GraderID:                      evaluationCase.graderID,
					GraderVersion:                 evaluationCase.graderVersion,
					SamplingPolicy:                "fixed",
					RandomSeed:                    "run:" + runID.String(),
					Region:                        "default",
					Protocol:                      "responses-v1",
					TimeBlock:                     runCreatedAt.UTC().Truncate(time.Minute).Format(time.RFC3339),
					InterleaveOrder:               "baseline-first",
					RetryPolicy:                   "lease-retry-v1",
					AllowedTreatmentFields: []string{
						"model_config_sha256", "expected_model_alias", "expected_resolved_model", "provider_parameters_sha256",
					},
				}
				pairContract, err := service.CanonicalizePairSpec(pair)
				if err != nil {
					return result, fmt.Errorf("freeze pair spec %s: %w", pairID, err)
				}
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO evaluation_pair_specs (
						id, run_id, case_id, sample_index, repeat_index,
						request_manifest_id, request_manifest_sha256, schema_version,
						canonical_spec, pair_spec_hash
					) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10)`,
					pairID, runID, pair.CaseID, pair.SampleIndex, pair.RepeatIndex,
					pair.ExpectedRequestManifestID, pair.ExpectedRequestManifestSHA256, service.PairSpecSchemaV1,
					pairContract.Bytes, pairContract.SHA256); err != nil {
					return result, fmt.Errorf("insert evaluation pair spec: %w", err)
				}

				baseline, err := makeEvaluationSideSpec("baseline", matrixEntry)
				if err != nil {
					return result, err
				}
				candidate, err := makeEvaluationSideSpec("candidate", matrixEntry)
				if err != nil {
					return result, err
				}
				binding, err := service.BindEvaluationPair(pair, baseline.spec, candidate.spec)
				if err != nil {
					return result, fmt.Errorf("bind evaluation pair %s: %w", pairID, err)
				}
				if err := insertEvaluationSideSpec(ctx, tx, pairID, baseline); err != nil {
					return result, err
				}
				if err := insertEvaluationSideSpec(ctx, tx, pairID, candidate); err != nil {
					return result, err
				}
				bindingID := uuid.New()
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO evaluation_pair_bindings (
						id, pair_spec_id, baseline_side_spec_id, candidate_side_spec_id, pair_binding_hash
					) VALUES ($1, $2, $3, $4, $5)`, bindingID, pairID,
					baseline.id, candidate.id, binding.BindingHash); err != nil {
					return result, fmt.Errorf("insert evaluation pair binding: %w", err)
				}

				baselineSample := evaluationExperimentSample{
					id: baseline.sampleID, modelRoute: "baseline:" + matrixEntry.route,
					modelConfig: matrixEntry.baselineConfig, modelConfigSHA: matrixEntry.baselineConfigSHA256,
					side: "baseline", sideSpecID: baseline.id,
				}
				candidateSample := evaluationExperimentSample{
					id: candidate.sampleID, modelRoute: "candidate:" + matrixEntry.route,
					modelConfig: matrixEntry.candidateConfig, modelConfigSHA: matrixEntry.candidateConfigSHA256,
					side: "candidate", sideSpecID: candidate.id,
				}
				for _, sample := range []evaluationExperimentSample{baselineSample, candidateSample} {
					if err := insertEvaluationSampleAndAssignment(ctx, tx, sample.id, runID, evaluationCase, sample.modelRoute, sample.modelConfig, sample.modelConfigSHA, sampleIndex); err != nil {
						return result, err
					}
				}
				result.Bindings = append(result.Bindings, service.PairBindingRef{
					PairSpecID: pairID, PairSpecHash: binding.PairSpecHash,
					BaselineSideID: baseline.id, CandidateSideID: candidate.id,
					BindingHash: binding.BindingHash,
				})
				result.Pairs++
			}
		}
	}
	if err := requireCompletePairBindings(result.Pairs, len(result.Bindings)); err != nil {
		return result, err
	}
	return result, nil
}

type evaluationSidePersist struct {
	id       uuid.UUID
	sampleID uuid.UUID
	spec     service.SideSpec
	contract service.CanonicalContract
}

func makeEvaluationSideSpec(side string, entry evaluationMatrixEntry) (evaluationSidePersist, error) {
	config, configSHA := entry.configForSide(side)
	alias, resolved := modelConfigIdentity(config)
	spec := service.SideSpec{
		Side: side, ModelRoute: side + ":" + entry.route,
		ModelConfigSHA256: configSHA, ExpectedModelAlias: alias,
		ExpectedResolvedModel: resolved, RouteProfileVersion: radarRouteProfileVersion,
		ProviderParametersSHA256: configSHA,
	}
	contract, err := service.CanonicalizeSideSpec(spec)
	if err != nil {
		return evaluationSidePersist{}, fmt.Errorf("freeze %s side spec: %w", side, err)
	}
	return evaluationSidePersist{id: uuid.New(), sampleID: uuid.New(), spec: spec, contract: contract}, nil
}

func insertEvaluationRequestManifest(ctx context.Context, tx *sql.Tx, manifest service.CanonicalRequestManifest) (uuid.UUID, error) {
	manifestID := uuid.New()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_request_manifests (
			id, schema_version, interaction_type, canonical_manifest_bytes, manifest_sha256
		) VALUES ($1, $2, 'single', $3, $4)
		ON CONFLICT (manifest_sha256) DO NOTHING`, manifestID, service.RequestManifestSchemaV1, manifest.Bytes, manifest.SHA256); err != nil {
		return uuid.Nil, fmt.Errorf("insert evaluation request manifest: %w", err)
	}
	var existingID uuid.UUID
	if err := tx.QueryRowContext(ctx, `SELECT id FROM evaluation_request_manifests WHERE manifest_sha256 = $1`, manifest.SHA256).Scan(&existingID); err != nil {
		return uuid.Nil, fmt.Errorf("load evaluation request manifest: %w", err)
	}
	return existingID, nil
}

func insertEvaluationSideSpec(ctx context.Context, tx *sql.Tx, pairID uuid.UUID, side evaluationSidePersist) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_side_specs (
			id, pair_spec_id, sample_id, side, schema_version, canonical_spec, side_spec_hash
		) VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7)`,
		side.id, pairID, side.sampleID, side.spec.Side, service.SideSpecSchemaV1,
		side.contract.Bytes, side.contract.SHA256); err != nil {
		return fmt.Errorf("insert %s evaluation side spec: %w", side.spec.Side, err)
	}
	return nil
}

func modelConfigIdentity(config []byte) (string, string) {
	var value map[string]any
	if json.Unmarshal(config, &value) != nil {
		return "unknown", "unknown"
	}
	alias := ""
	for _, key := range []string{"model_alias", "route", "model_route", "model", "id"} {
		if item, ok := value[key].(string); ok && strings.TrimSpace(item) != "" {
			alias = strings.TrimSpace(item)
			break
		}
	}
	if alias == "" {
		alias = "unknown"
	}
	resolved := alias
	if item, ok := value["resolved_model"].(string); ok && strings.TrimSpace(item) != "" {
		resolved = strings.TrimSpace(item)
	}
	return alias, resolved
}
