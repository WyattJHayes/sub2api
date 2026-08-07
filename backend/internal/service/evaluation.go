package service

type DatasetStatus string

const (
	DatasetStatusDraft     DatasetStatus = "draft"
	DatasetStatusPublished DatasetStatus = "published"
	DatasetStatusRetired   DatasetStatus = "retired"
)

func (status DatasetStatus) Valid() bool {
	switch status {
	case DatasetStatusDraft, DatasetStatusPublished, DatasetStatusRetired:
		return true
	default:
		return false
	}
}

type RunStatus string

const (
	RunStatusPending      RunStatus = "pending"
	RunStatusRunning      RunStatus = "running"
	RunStatusPaused       RunStatus = "paused"
	RunStatusBudgetPaused RunStatus = "budget_paused"
	RunStatusCompleted    RunStatus = "completed"
	RunStatusFailed       RunStatus = "failed"
	RunStatusCancelled    RunStatus = "cancelled"
)

func (status RunStatus) Valid() bool {
	switch status {
	case RunStatusPending, RunStatusRunning, RunStatusPaused, RunStatusBudgetPaused,
		RunStatusCompleted, RunStatusFailed, RunStatusCancelled:
		return true
	default:
		return false
	}
}

type AssignmentStatus string

const (
	AssignmentStatusPending          AssignmentStatus = "pending"
	AssignmentStatusLeased           AssignmentStatus = "leased"
	AssignmentStatusRunning          AssignmentStatus = "running"
	AssignmentStatusEvidenceUploaded AssignmentStatus = "evidence_uploaded"
	AssignmentStatusGrading          AssignmentStatus = "grading"
	AssignmentStatusCompleted        AssignmentStatus = "completed"
	AssignmentStatusInfraFailed      AssignmentStatus = "infra_failed"
	AssignmentStatusUpstreamFailed   AssignmentStatus = "upstream_failed"
	AssignmentStatusInvalidEvidence  AssignmentStatus = "invalid_evidence"
	AssignmentStatusGradingFailed    AssignmentStatus = "grading_failed"
	AssignmentStatusCancelled        AssignmentStatus = "cancelled"
)

func (status AssignmentStatus) Valid() bool {
	switch status {
	case AssignmentStatusPending, AssignmentStatusLeased, AssignmentStatusRunning,
		AssignmentStatusEvidenceUploaded, AssignmentStatusGrading, AssignmentStatusCompleted,
		AssignmentStatusInfraFailed, AssignmentStatusUpstreamFailed, AssignmentStatusInvalidEvidence,
		AssignmentStatusGradingFailed, AssignmentStatusCancelled:
		return true
	default:
		return false
	}
}

type FailureClass string

const (
	FailureClassCapability      FailureClass = "capability"
	FailureClassProtocol        FailureClass = "protocol"
	FailureClassUpstream        FailureClass = "upstream"
	FailureClassInfrastructure  FailureClass = "infrastructure"
	FailureClassJudge           FailureClass = "judge"
	FailureClassInvalidEvidence FailureClass = "invalid_evidence"
	FailureClassSafety          FailureClass = "safety"
	FailureClassPerformance     FailureClass = "performance"
	FailureClassCost            FailureClass = "cost"
)

func (class FailureClass) Valid() bool {
	switch class {
	case FailureClassCapability, FailureClassProtocol, FailureClassUpstream,
		FailureClassInfrastructure, FailureClassJudge, FailureClassInvalidEvidence,
		FailureClassSafety, FailureClassPerformance, FailureClassCost:
		return true
	default:
		return false
	}
}

type CapabilityDomain string

const (
	CapabilityDomainCoding      CapabilityDomain = "coding"
	CapabilityDomainReasoning   CapabilityDomain = "reasoning"
	CapabilityDomainInstruction CapabilityDomain = "instruction"
	CapabilityDomainLongContext CapabilityDomain = "long_context"
	CapabilityDomainToolCall    CapabilityDomain = "tool_call"
	CapabilityDomainProtocol    CapabilityDomain = "protocol"
	CapabilityDomainSafety      CapabilityDomain = "safety"
	CapabilityDomainPerformance CapabilityDomain = "performance"
	CapabilityDomainCost        CapabilityDomain = "cost"
)

func (domain CapabilityDomain) Valid() bool {
	switch domain {
	case CapabilityDomainCoding, CapabilityDomainReasoning, CapabilityDomainInstruction,
		CapabilityDomainLongContext, CapabilityDomainToolCall, CapabilityDomainProtocol,
		CapabilityDomainSafety, CapabilityDomainPerformance, CapabilityDomainCost:
		return true
	default:
		return false
	}
}

type CasePriority string

const (
	CasePriorityP0 CasePriority = "P0"
	CasePriorityP1 CasePriority = "P1"
	CasePriorityP2 CasePriority = "P2"
)

func (priority CasePriority) Valid() bool {
	switch priority {
	case CasePriorityP0, CasePriorityP1, CasePriorityP2:
		return true
	default:
		return false
	}
}
