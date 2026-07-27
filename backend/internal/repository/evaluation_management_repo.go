package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type radarCaseRow struct {
	input         service.CreateRadarCaseInput
	prompt        []byte
	expected      []byte
	execution     []byte
	contentSHA256 string
}

func canonicalRadarJSON(raw json.RawMessage) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func radarCaseRows(input service.CreateRadarDatasetInput) ([]radarCaseRow, string, error) {
	if strings.TrimSpace(input.DatasetKey) == "" || len(input.DatasetKey) > 100 ||
		strings.TrimSpace(input.Version) == "" || len(input.Version) > 100 || input.CreatedBy <= 0 {
		return nil, "", errors.New("invalid evaluation dataset identity")
	}
	if input.SourceType != "public" && input.SourceType != "synthetic" && input.SourceType != "imported" {
		return nil, "", errors.New("invalid evaluation dataset source type")
	}
	if len(input.Cases) == 0 {
		return nil, "", errors.New("evaluation dataset must contain at least one case")
	}
	rows := make([]radarCaseRow, 0, len(input.Cases))
	seen := make(map[string]struct{}, len(input.Cases))
	for _, item := range input.Cases {
		item.CaseKey = strings.TrimSpace(item.CaseKey)
		if item.CaseKey == "" || len(item.CaseKey) > 160 {
			return nil, "", errors.New("invalid evaluation case key")
		}
		if _, exists := seen[item.CaseKey]; exists {
			return nil, "", fmt.Errorf("duplicate evaluation case key %q", item.CaseKey)
		}
		seen[item.CaseKey] = struct{}{}
		if !service.CapabilityDomain(item.CapabilityDomain).Valid() ||
			!service.CasePriority(item.Priority).Valid() || item.Weight.LessThanOrEqual(decimalZero) ||
			item.SampleCount < 1 || item.SampleCount > 10 || item.EstimatedCost.IsNegative() {
			return nil, "", fmt.Errorf("invalid evaluation case %q", item.CaseKey)
		}
		if item.Confidentiality != "public" && item.Confidentiality != "synthetic" {
			return nil, "", errors.New("management API accepts public or synthetic cases only")
		}
		if strings.TrimSpace(item.GraderID) == "" || strings.TrimSpace(item.GraderVersion) == "" {
			return nil, "", errors.New("evaluation grader identity is required")
		}
		prompt, err := canonicalRadarJSON(item.PromptSpec)
		if err != nil {
			return nil, "", fmt.Errorf("canonicalize prompt for %q: %w", item.CaseKey, err)
		}
		expected, err := canonicalRadarJSON(item.ExpectedSpec)
		if err != nil {
			return nil, "", fmt.Errorf("canonicalize expected output for %q: %w", item.CaseKey, err)
		}
		execution, err := canonicalRadarJSON(item.ExecutionSpec)
		if err != nil {
			return nil, "", fmt.Errorf("canonicalize execution spec for %q: %w", item.CaseKey, err)
		}
		digest := sha256.New()
		for _, value := range [][]byte{
			[]byte(item.CaseKey), []byte(item.CapabilityDomain), []byte(item.Priority),
			[]byte(item.Weight.String()), []byte(fmt.Sprintf("%d", item.SampleCount)),
			prompt, expected, execution, []byte(item.GraderID), []byte(item.GraderVersion),
			[]byte(item.Confidentiality), []byte(item.EstimatedCost.String()),
		} {
			_, _ = digest.Write(value)
			_, _ = digest.Write([]byte{0})
		}
		rows = append(rows, radarCaseRow{input: item, prompt: prompt, expected: expected, execution: execution, contentSHA256: hex.EncodeToString(digest.Sum(nil))})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].input.CaseKey < rows[j].input.CaseKey })
	manifest := sha256.New()
	_, _ = manifest.Write([]byte(strings.TrimSpace(input.DatasetKey)))
	_, _ = manifest.Write([]byte{0})
	_, _ = manifest.Write([]byte(strings.TrimSpace(input.Version)))
	for _, row := range rows {
		_, _ = manifest.Write([]byte{0})
		_, _ = manifest.Write([]byte(row.contentSHA256))
	}
	return rows, hex.EncodeToString(manifest.Sum(nil)), nil
}

var decimalZero = decimal.Zero

func (r *radarGovernanceRepository) CreateDataset(ctx context.Context, input service.CreateRadarDatasetInput) (*service.RadarDatasetRecord, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	rows, manifest, err := radarCaseRows(input)
	if err != nil {
		return nil, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin evaluation dataset creation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	record := &service.RadarDatasetRecord{ID: uuid.New()}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO evaluation_dataset_versions
			(id, dataset_key, version, manifest_sha256, source_type, status, created_by)
		VALUES ($1, $2, $3, $4, $5, 'draft', $6)
		RETURNING dataset_key, version, manifest_sha256, source_type, status, created_by, created_at`,
		record.ID, strings.TrimSpace(input.DatasetKey), strings.TrimSpace(input.Version), manifest,
		input.SourceType, input.CreatedBy).Scan(&record.DatasetKey, &record.Version,
		&record.ManifestSHA256, &record.SourceType, &record.Status, &record.CreatedBy, &record.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("create evaluation dataset: %w", err)
	}
	for _, row := range rows {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO evaluation_cases (
				id, dataset_version_id, case_key, capability_domain, priority, weight,
				sample_count, prompt_spec, expected_spec, execution_spec, grader_id,
				grader_version, content_sha256, confidentiality, estimated_cost
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9::jsonb, $10::jsonb,
				$11, $12, $13, $14, $15)`, uuid.New(), record.ID, row.input.CaseKey,
			row.input.CapabilityDomain, row.input.Priority, row.input.Weight, row.input.SampleCount,
			string(row.prompt), string(row.expected), string(row.execution), strings.TrimSpace(row.input.GraderID),
			strings.TrimSpace(row.input.GraderVersion), row.contentSHA256, row.input.Confidentiality,
			row.input.EstimatedCost)
		if err != nil {
			return nil, fmt.Errorf("create evaluation case %q: %w", row.input.CaseKey, err)
		}
	}
	record.CaseCount = len(rows)
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit evaluation dataset creation: %w", err)
	}
	return record, nil
}

func (r *radarGovernanceRepository) PublishDataset(ctx context.Context, datasetID uuid.UUID, actorID int64) (*service.RadarDatasetRecord, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	if datasetID == uuid.Nil || actorID <= 0 {
		return nil, errors.New("evaluation dataset and actor are required")
	}
	record := &service.RadarDatasetRecord{ID: datasetID}
	err := r.db.QueryRowContext(ctx, `
		UPDATE evaluation_dataset_versions d
		SET status = 'published', published_at = NOW(), updated_at = NOW()
		WHERE d.id = $1 AND d.status = 'draft'
		RETURNING d.dataset_key, d.version, d.manifest_sha256, d.source_type, d.status,
		          (SELECT COUNT(*) FROM evaluation_cases c WHERE c.dataset_version_id = d.id),
		          d.created_by, d.published_at, d.created_at`, datasetID).Scan(
		&record.DatasetKey, &record.Version, &record.ManifestSHA256, &record.SourceType,
		&record.Status, &record.CaseCount, &record.CreatedBy, &record.PublishedAt, &record.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("publish evaluation dataset: %w", err)
	}
	return record, nil
}

func (r *radarGovernanceRepository) CreatePlan(ctx context.Context, input service.CreateRadarPlanInput) (*service.RadarPlanRecord, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(input.Name) == "" || input.DatasetVersionID == uuid.Nil ||
		input.GatewayAPIKeyID <= 0 || input.TriggerType != "manual" || input.CreatedBy <= 0 ||
		input.MaxRunCost.LessThanOrEqual(decimalZero) || input.DailyCostLimit.LessThanOrEqual(decimalZero) ||
		input.MaxConcurrency < 1 || input.MaxConcurrency > 1000 {
		return nil, errors.New("invalid evaluation plan")
	}
	if _, err := evaluationMatrixEntries(input.ModelMatrix); err != nil {
		return nil, err
	}
	record := &service.RadarPlanRecord{ID: uuid.New()}
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO evaluation_plans (
			id, name, dataset_version_id, gateway_api_key_id, trigger_type, model_matrix,
			max_run_cost, daily_cost_limit, max_concurrency, created_by
		)
		SELECT $1, $2, d.id, k.id, $5, $6::jsonb, $7, $8, $9, $10
		FROM evaluation_dataset_versions d
		JOIN api_keys k ON k.id = $4
		JOIN users u ON u.id = k.user_id
		LEFT JOIN groups g ON g.id = k.group_id
		WHERE d.id = $3 AND d.status = 'published'
		  AND k.is_evaluation = TRUE AND k.status = 'active' AND k.deleted_at IS NULL
		  AND (k.expires_at IS NULL OR k.expires_at > NOW())
		  AND (k.quota = 0 OR k.quota_used < k.quota)
		  AND u.status = 'active' AND u.deleted_at IS NULL
		  AND (g.id IS NULL OR (g.status = 'active' AND g.deleted_at IS NULL))
		RETURNING id, name, dataset_version_id, gateway_api_key_id, trigger_type,
		          model_matrix, max_run_cost, daily_cost_limit, max_concurrency,
		          enabled, created_by, created_at`, record.ID, strings.TrimSpace(input.Name),
		input.DatasetVersionID, input.GatewayAPIKeyID, input.TriggerType, string(input.ModelMatrix),
		input.MaxRunCost, input.DailyCostLimit, input.MaxConcurrency, input.CreatedBy).Scan(
		&record.ID, &record.Name, &record.DatasetVersionID, &record.GatewayAPIKeyID,
		&record.TriggerType, &record.ModelMatrix, &record.MaxRunCost, &record.DailyCostLimit,
		&record.MaxConcurrency, &record.Enabled, &record.CreatedBy, &record.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("published dataset and usable dedicated evaluation API key are required")
	}
	if err != nil {
		return nil, fmt.Errorf("create evaluation plan: %w", err)
	}
	return record, nil
}

func (r *radarGovernanceRepository) CreateRunWithMatrix(ctx context.Context, input service.CreateRunInput) (*service.EvaluationRun, error) {
	return (&evaluationRepository{db: r.db}).CreateRunWithMatrix(ctx, input)
}
