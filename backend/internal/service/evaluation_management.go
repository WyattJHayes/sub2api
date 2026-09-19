package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type RunControlResult struct {
	RunID             uuid.UUID   `json:"run_id"`
	FromStatus        string      `json:"from_status"`
	ToStatus          string      `json:"to_status"`
	PreviousEpoch     int64       `json:"previous_epoch"`
	CurrentEpoch      int64       `json:"current_epoch"`
	AffectedWorkCount int         `json:"affected_work_count"`
	ReplacementIDs    []uuid.UUID `json:"replacement_ids,omitempty"`
	EventID           uuid.UUID   `json:"event_id"`
}

type RunControlRepository interface {
	PauseRun(context.Context, uuid.UUID, string, int64, string) (*RunControlResult, error)
	ResumeRun(context.Context, uuid.UUID, string, int64, string) (*RunControlResult, error)
	CancelRun(context.Context, uuid.UUID, string, int64, string) (*RunControlResult, error)
	FenceRun(context.Context, uuid.UUID, string, int64, string) (*RunControlResult, error)
}

type CreateRadarCaseInput struct {
	CaseKey          string            `json:"case_key" binding:"required"`
	CapabilityDomain string            `json:"capability_domain" binding:"required"`
	Priority         string            `json:"priority" binding:"required"`
	Weight           decimal.Decimal   `json:"weight" binding:"required"`
	SampleCount      int               `json:"sample_count" binding:"required,min=1,max=10"`
	PromptSpec       json.RawMessage   `json:"prompt_spec" binding:"required"`
	ExpectedSpec     json.RawMessage   `json:"expected_spec" binding:"required"`
	ExecutionSpec    json.RawMessage   `json:"execution_spec" binding:"required"`
	GraderID         string            `json:"grader_id" binding:"required"`
	GraderVersion    string            `json:"grader_version" binding:"required"`
	Confidentiality  string            `json:"confidentiality" binding:"required"`
	EstimatedCost    decimal.Decimal   `json:"estimated_cost"`
	QualityDimension *QualityDimension `json:"quality_dimension,omitempty"`
	QualityProbeSpec *QualityProbeSpec `json:"quality_probe_spec,omitempty"`
}

type CreateRadarDatasetInput struct {
	DatasetKey string                 `json:"dataset_key" binding:"required"`
	Version    string                 `json:"version" binding:"required"`
	SourceType string                 `json:"source_type" binding:"required"`
	Cases      []CreateRadarCaseInput `json:"cases" binding:"required,min=1,dive"`
	CreatedBy  int64                  `json:"-"`
}

type RadarDatasetRecord struct {
	ID             uuid.UUID     `json:"id"`
	DatasetKey     string        `json:"dataset_key"`
	Version        string        `json:"version"`
	ManifestSHA256 string        `json:"manifest_sha256"`
	SourceType     string        `json:"source_type"`
	Status         DatasetStatus `json:"status"`
	CaseCount      int           `json:"case_count"`
	CreatedBy      int64         `json:"created_by"`
	PublishedAt    *time.Time    `json:"published_at,omitempty"`
	CreatedAt      time.Time     `json:"created_at"`
}

type CreateRadarPlanInput struct {
	Name             string          `json:"name" binding:"required"`
	DatasetVersionID uuid.UUID       `json:"dataset_version_id" binding:"required"`
	GatewayAPIKeyID  int64           `json:"gateway_api_key_id" binding:"required,gt=0"`
	TriggerType      string          `json:"trigger_type" binding:"required"`
	ModelMatrix      json.RawMessage `json:"model_matrix" binding:"required"`
	MaxRunCost       decimal.Decimal `json:"max_run_cost" binding:"required"`
	DailyCostLimit   decimal.Decimal `json:"daily_cost_limit" binding:"required"`
	MaxConcurrency   int             `json:"max_concurrency" binding:"required,min=1,max=1000"`
	CreatedBy        int64           `json:"-"`
}

type RadarPlanRecord struct {
	ID               uuid.UUID       `json:"id"`
	Name             string          `json:"name"`
	DatasetVersionID uuid.UUID       `json:"dataset_version_id"`
	GatewayAPIKeyID  int64           `json:"gateway_api_key_id"`
	TriggerType      string          `json:"trigger_type"`
	ModelMatrix      json.RawMessage `json:"model_matrix"`
	MaxRunCost       decimal.Decimal `json:"max_run_cost"`
	DailyCostLimit   decimal.Decimal `json:"daily_cost_limit"`
	MaxConcurrency   int             `json:"max_concurrency"`
	Enabled          bool            `json:"enabled"`
	CreatedBy        int64           `json:"created_by"`
	CreatedAt        time.Time       `json:"created_at"`
}
