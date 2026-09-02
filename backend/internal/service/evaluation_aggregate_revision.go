package service

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

var ErrAggregateRevisionInvalid = errors.New("invalid evaluation aggregate revision input")

var (
	ErrAggregatePairsIncomplete = errors.New("evaluation aggregate requires complete baseline candidate pairs")
	ErrAggregateInputMismatch   = errors.New("evaluation aggregate input does not match frozen job input")
)

type SnapshotRef struct {
	ID          uuid.UUID `json:"snapshot_id"`
	WindowStart time.Time `json:"window_start"`
}

type CellPairInput struct {
	CaseID                     uuid.UUID       `json:"case_id"`
	SampleIndex                int             `json:"sample_index"`
	PairSpecHash               string          `json:"pair_spec_hash"`
	BaselineSideSpecHash       string          `json:"baseline_side_spec_hash"`
	CandidateSideSpecHash      string          `json:"candidate_side_spec_hash"`
	PairBindingHash            string          `json:"pair_binding_hash"`
	GraderID                   string          `json:"grader_id"`
	GraderVersion              string          `json:"grader_version"`
	BaselineHeadVersion        int             `json:"baseline_head_version"`
	BaselineScore              ScoreRef        `json:"baseline_score_ref"`
	BaselineSourceAssignment   uuid.UUID       `json:"baseline_source_assignment_id"`
	BaselineEvidenceSetHash    string          `json:"baseline_route_evidence_set_hash"`
	CandidateHeadVersion       int             `json:"candidate_head_version"`
	CandidateScore             ScoreRef        `json:"candidate_score_ref"`
	CandidateSourceAssignment  uuid.UUID       `json:"candidate_source_assignment_id"`
	CandidateEvidenceSetHash   string          `json:"candidate_route_evidence_set_hash"`
	CaseWeight                 decimal.Decimal `json:"case_weight"`
	BaselineSourceHeadEventID  uuid.UUID       `json:"-"`
	CandidateSourceHeadEventID uuid.UUID       `json:"-"`
	BaselineRevisionBatchID    uuid.UUID       `json:"-"`
	CandidateRevisionBatchID   uuid.UUID       `json:"-"`
}

type GlobalCellInput struct {
	CapabilityDomain    string      `json:"capability_domain"`
	CanonicalModelRoute string      `json:"model_route"`
	Snapshot            SnapshotRef `json:"snapshot_ref"`
	AggregateRevision   int64       `json:"aggregate_revision"`
	InputSetHash        string      `json:"input_set_hash"`
	AggregateHash       string      `json:"aggregate_hash"`
	RevisionBatchID     uuid.UUID   `json:"-"`
}

type CellAnalysisJobRequest struct {
	RunID            uuid.UUID
	CapabilityDomain string
	ModelRoute       string
	AnalysisVersion  string
}

type GlobalAnalysisJobRequest struct {
	RunID           uuid.UUID
	AnalysisVersion string
}

type AnalysisJobRevision struct {
	ID                  uuid.UUID     `json:"id"`
	RunID               uuid.UUID     `json:"run_id"`
	Scope               string        `json:"scope"`
	CapabilityDomain    string        `json:"capability_domain"`
	CanonicalModelRoute string        `json:"canonical_model_route"`
	AnalysisVersion     string        `json:"analysis_version"`
	InputSetHash        string        `json:"input_set_hash"`
	ScoreRefs           []ScoreRef    `json:"score_refs,omitempty"`
	SnapshotRefs        []SnapshotRef `json:"snapshot_refs,omitempty"`
	AggregateRevision   int64         `json:"aggregate_revision"`
	WorkOrigin          string        `json:"work_origin"`
	RevisionBatchID     uuid.UUID     `json:"revision_batch_id,omitempty"`
	CauseSetHash        string        `json:"cause_set_hash"`
}

type EvaluationAggregateRepository interface {
	EnsureCellAnalysisJob(ctx context.Context, request CellAnalysisJobRequest) (*AnalysisJobRevision, error)
	EnsureGlobalAnalysisJob(ctx context.Context, request GlobalAnalysisJobRequest) (*AnalysisJobRevision, error)
}

func CanonicalModelRoute(route string) string {
	if strings.HasPrefix(route, "baseline:") {
		return strings.TrimPrefix(route, "baseline:")
	}
	if strings.HasPrefix(route, "candidate:") {
		return strings.TrimPrefix(route, "candidate:")
	}
	return route
}

func CanonicalCellInputSetHash(inputs []CellPairInput) (string, error) {
	if len(inputs) == 0 {
		return "", ErrAggregateRevisionInvalid
	}
	canonical := append([]CellPairInput(nil), inputs...)
	sort.Slice(canonical, func(i, j int) bool {
		if canonical[i].CaseID != canonical[j].CaseID {
			return canonical[i].CaseID.String() < canonical[j].CaseID.String()
		}
		if canonical[i].SampleIndex != canonical[j].SampleIndex {
			return canonical[i].SampleIndex < canonical[j].SampleIndex
		}
		return canonical[i].GraderID < canonical[j].GraderID
	})
	return digestCanonicalAggregateInput(canonical)
}

func CanonicalGlobalInputSetHash(inputs []GlobalCellInput) (string, error) {
	if len(inputs) == 0 {
		return "", ErrAggregateRevisionInvalid
	}
	canonical := append([]GlobalCellInput(nil), inputs...)
	sort.Slice(canonical, func(i, j int) bool {
		if canonical[i].CapabilityDomain != canonical[j].CapabilityDomain {
			return canonical[i].CapabilityDomain < canonical[j].CapabilityDomain
		}
		return canonical[i].CanonicalModelRoute < canonical[j].CanonicalModelRoute
	})
	return digestCanonicalAggregateInput(canonical)
}

func digestCanonicalAggregateInput(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", ErrAggregateRevisionInvalid
	}
	return DigestCanonicalJSON(encoded)
}
