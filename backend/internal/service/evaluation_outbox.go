package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrEvaluationOutboxInvalid       = errors.New("invalid evaluation outbox input")
	ErrEvaluationOutboxBatchMismatch = errors.New("evaluation outbox batch does not belong to run")
	ErrEvaluationOutboxFenced        = errors.New("evaluation outbox lease fenced")
	ErrEvaluationOutboxNotFound      = errors.New("evaluation outbox event not found")
	ErrEvaluationOutboxDedupConflict = errors.New("evaluation outbox dedup identity conflict")
)

type EvaluationOutboxStatus string

const (
	EvaluationOutboxPending    EvaluationOutboxStatus = "pending"
	EvaluationOutboxLeased     EvaluationOutboxStatus = "leased"
	EvaluationOutboxCompleted  EvaluationOutboxStatus = "completed"
	EvaluationOutboxDeadLetter EvaluationOutboxStatus = "dead_letter"
)

type EvaluationOutboxCause struct {
	EventID           uuid.UUID `json:"event_id"`
	SourceHeadEventID uuid.UUID `json:"source_head_event_id,omitempty"`
}

type EnqueueEvaluationOutboxInput struct {
	EventType       string
	RunID           uuid.UUID
	ScopeKey        string
	AnalysisVersion string
	SourceType      string
	SourceID        string
	SourceHash      string
	Payload         json.RawMessage
	WorkOrigin      string
	RevisionBatchID uuid.UUID
	Causes          []EvaluationOutboxCause
}

type EvaluationOutboxEvent struct {
	ID              uuid.UUID              `json:"id"`
	Sequence        int64                  `json:"sequence"`
	EventType       string                 `json:"event_type"`
	DedupKey        string                 `json:"dedup_key"`
	CausationID     string                 `json:"causation_id"`
	CauseSetHash    string                 `json:"cause_set_hash"`
	WorkOrigin      string                 `json:"work_origin"`
	RevisionBatchID uuid.UUID              `json:"revision_batch_id,omitempty"`
	RunID           uuid.UUID              `json:"run_id"`
	SourceType      string                 `json:"source_type"`
	SourceID        string                 `json:"source_id"`
	SourceHash      string                 `json:"source_hash"`
	PayloadHash     string                 `json:"payload_hash"`
	Payload         json.RawMessage        `json:"payload"`
	Status          EvaluationOutboxStatus `json:"status"`
	Attempt         int                    `json:"attempt"`
	AvailableAt     time.Time              `json:"available_at"`
	LeaseToken      string                 `json:"lease_token,omitempty"`
	LeaseOwner      uuid.UUID              `json:"lease_owner,omitempty"`
	LeaseExpiresAt  time.Time              `json:"lease_expires_at,omitempty"`
	LeaseEpoch      int64                  `json:"lease_epoch"`
	LastErrorCode   string                 `json:"last_error_code,omitempty"`
	CreatedAt       time.Time              `json:"created_at"`
	UpdatedAt       time.Time              `json:"updated_at"`
}

type EvaluationOutboxRepository interface {
	Enqueue(context.Context, EnqueueEvaluationOutboxInput) (*EvaluationOutboxEvent, error)
	Claim(context.Context, uuid.UUID, []string, int, time.Duration) ([]EvaluationOutboxEvent, error)
	Heartbeat(context.Context, uuid.UUID, string, int64, time.Duration) error
	Complete(context.Context, uuid.UUID, string, int64) error
	DeadLetter(context.Context, uuid.UUID, string, int64, string) error
	ReplayDeadLetter(context.Context, uuid.UUID) (*EvaluationOutboxEvent, error)
}

func OutboxDedupKey(eventType string, runID uuid.UUID, scopeKey, analysisVersion, sourceHash string) (string, error) {
	eventType = strings.TrimSpace(eventType)
	scopeKey = strings.TrimSpace(scopeKey)
	analysisVersion = strings.TrimSpace(analysisVersion)
	if eventType == "" || runID == uuid.Nil || scopeKey == "" || analysisVersion == "" || !isLowerHexSHA256(sourceHash) {
		return "", ErrEvaluationOutboxInvalid
	}
	canonical, err := json.Marshal(struct {
		EventType       string `json:"event_type"`
		RunID           string `json:"run_id"`
		ScopeKey        string `json:"scope_key"`
		AnalysisVersion string `json:"analysis_version"`
		SourceHash      string `json:"source_hash"`
	}{eventType, runID.String(), scopeKey, analysisVersion, sourceHash})
	if err != nil {
		return "", ErrEvaluationOutboxInvalid
	}
	return DigestCanonicalJSON(canonical)
}

func CauseSetHash(causes []uuid.UUID) (string, error) {
	if len(causes) == 0 {
		return "", ErrEvaluationOutboxInvalid
	}
	unique := make(map[uuid.UUID]struct{}, len(causes))
	for _, cause := range causes {
		if cause == uuid.Nil {
			return "", ErrEvaluationOutboxInvalid
		}
		unique[cause] = struct{}{}
	}
	ordered := make([]uuid.UUID, 0, len(unique))
	for cause := range unique {
		ordered = append(ordered, cause)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return bytes.Compare(ordered[i][:], ordered[j][:]) < 0
	})
	hash := sha256.New()
	for _, cause := range ordered {
		_, _ = hash.Write(cause[:])
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
