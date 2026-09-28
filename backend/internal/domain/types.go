package domain

import (
	"encoding/json"
	"time"
)

var (
	ErrNotFound     = Error("not found")
	ErrConflict     = Error("conflict")
	ErrUnauthorized = Error("unauthorized")
	ErrInvalid      = Error("invalid input")
	ErrBusy         = Error("runtime capacity is busy")
)

type Error string

func (e Error) Error() string { return string(e) }

type User struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"displayName"`
	CreatedAt   time.Time `json:"createdAt"`
}

type Provider struct {
	ID            string    `json:"id"`
	UserID        string    `json:"-"`
	Name          string    `json:"name"`
	Kind          string    `json:"kind"`
	BaseURL       string    `json:"baseUrl"`
	Model         string    `json:"model"`
	ContextWindow int       `json:"contextWindow"`
	HasAPIKey     bool      `json:"hasApiKey"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type Project struct {
	ID                  string    `json:"id"`
	UserID              string    `json:"-"`
	Name                string    `json:"name"`
	Instructions        string    `json:"instructions"`
	InstructionsEnabled bool      `json:"instructionsEnabled"`
	Workdir             string    `json:"workdir"`
	RemoteRepoURL       string    `json:"remoteRepoUrl"`
	RemoteBranch        string    `json:"remoteBranch"`
	CreatedAt           time.Time `json:"createdAt"`
	UpdatedAt           time.Time `json:"updatedAt"`
}

type Conversation struct {
	ID                    string            `json:"id"`
	UserID                string            `json:"-"`
	Title                 string            `json:"title"`
	ProviderID            string            `json:"providerId"`
	AgentGenerationID     string            `json:"agentGenerationId,omitempty"`
	AgentDefinitionDigest string            `json:"agentDefinitionDigest,omitempty"`
	ProjectID             string            `json:"projectId,omitempty"`
	PermissionProfile     PermissionProfile `json:"permissionProfile"`
	ParentConversationID  string            `json:"parentConversationId,omitempty"`
	BranchFromMessageID   string            `json:"branchFromMessageId,omitempty"`
	ExecutionPaused       bool              `json:"executionPaused"`
	CreatedAt             time.Time         `json:"createdAt"`
	UpdatedAt             time.Time         `json:"updatedAt"`
}

type Message struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversationId"`
	Role           string    `json:"role"`
	Content        string    `json:"content"`
	CreatedAt      time.Time `json:"createdAt"`
}

type ConversationDetail struct {
	Conversation
	Messages        []Message                    `json:"messages"`
	LifecycleEvents []ConversationLifecycleEvent `json:"lifecycleEvents"`
}

type ConversationLifecycleEvent struct {
	ID             string          `json:"id"`
	ConversationID string          `json:"conversationId"`
	Kind           string          `json:"kind"`
	Details        json.RawMessage `json:"details"`
	CreatedAt      time.Time       `json:"createdAt"`
}

type TraceEvent struct {
	ID             string          `json:"id"`
	ConversationID string          `json:"conversationId"`
	TurnID         string          `json:"turnId"`
	Sequence       int             `json:"sequence"`
	Kind           string          `json:"kind"`
	Details        json.RawMessage `json:"details"`
	CreatedAt      time.Time       `json:"createdAt"`
}

type AgentTurn struct {
	ID                    string            `json:"id"`
	ConversationID        string            `json:"conversationId"`
	UserID                string            `json:"-"`
	InputMessageID        string            `json:"inputMessageId"`
	RetryOfTurnID         string            `json:"retryOfTurnId,omitempty"`
	ResultMessageID       string            `json:"resultMessageId,omitempty"`
	ProviderID            string            `json:"providerId"`
	AgentGenerationID     string            `json:"agentGenerationId"`
	AgentDefinitionDigest string            `json:"agentDefinitionDigest"`
	PermissionProfile     PermissionProfile `json:"permissionProfile"`
	Status                string            `json:"status"`
	StopReason            string            `json:"stopReason,omitempty"`
	RecoveryClass         string            `json:"recoveryClass,omitempty"`
	ReconciliationNote    string            `json:"-"`
	CancelRequested       bool              `json:"cancelRequested"`
	LastSequence          int               `json:"lastSequence"`
	StartedAt             time.Time         `json:"startedAt"`
	UpdatedAt             time.Time         `json:"updatedAt"`
	CompletedAt           *time.Time        `json:"completedAt,omitempty"`
}

type ApprovalRequest struct {
	ID                string            `json:"id"`
	ConversationID    string            `json:"conversationId"`
	TurnID            string            `json:"turnId"`
	ToolCallID        string            `json:"toolCallId"`
	ToolName          string            `json:"toolName"`
	Source            string            `json:"source"`
	PluginID          string            `json:"pluginId,omitempty"`
	ReleaseID         string            `json:"releaseId,omitempty"`
	Effect            string            `json:"effect"`
	PermissionProfile PermissionProfile `json:"permissionProfile"`
	Resource          string            `json:"resource,omitempty"`
	Impact            string            `json:"impact,omitempty"`
	Reason            string            `json:"reason"`
	Arguments         string            `json:"arguments"`
	Status            string            `json:"status"`
	CreatedAt         time.Time         `json:"createdAt"`
	ExpiresAt         time.Time         `json:"expiresAt"`
}

type ToolUsageMetric struct {
	ToolName                 string    `json:"toolName"`
	ProjectID                string    `json:"projectId,omitempty"`
	PluginID                 string    `json:"pluginId,omitempty"`
	ReleaseID                string    `json:"releaseId,omitempty"`
	Calls                    int64     `json:"calls"`
	Completed                int64     `json:"completed"`
	Failures                 int64     `json:"failures"`
	PermissionAllows         int64     `json:"permissionAllows"`
	PermissionDenials        int64     `json:"permissionDenials"`
	PermissionAsks           int64     `json:"permissionAsks"`
	ApprovalRequests         int64     `json:"approvalRequests"`
	ApprovalsPending         int64     `json:"approvalsPending"`
	ApprovalsGranted         int64     `json:"approvalsGranted"`
	ApprovalsDenied          int64     `json:"approvalsDenied"`
	ApprovalsExpired         int64     `json:"approvalsExpired"`
	ApprovalsCancelled       int64     `json:"approvalsCancelled"`
	PluginBuilds             int64     `json:"pluginBuilds,omitempty"`
	PluginBuildFailures      int64     `json:"pluginBuildFailures,omitempty"`
	PluginActivations        int64     `json:"pluginActivations,omitempty"`
	PluginDeactivations      int64     `json:"pluginDeactivations,omitempty"`
	PluginRollbacks          int64     `json:"pluginRollbacks,omitempty"`
	PluginPermissionRequests int64     `json:"pluginPermissionRequests,omitempty"`
	PluginPermissionsGranted int64     `json:"pluginPermissionsGranted,omitempty"`
	PluginRuntimeFailures    int64     `json:"pluginRuntimeFailures,omitempty"`
	PluginSourceChanges      int64     `json:"pluginSourceChanges,omitempty"`
	PluginUnusableMarks      int64     `json:"pluginUnusableMarks,omitempty"`
	PluginBundleCleanups     int64     `json:"pluginBundleCleanups,omitempty"`
	DurationMillis           int64     `json:"durationMillis"`
	ContextCompactions       int64     `json:"contextCompactions,omitempty"`
	OriginalContextChars     int64     `json:"originalContextChars,omitempty"`
	CompactedContextChars    int64     `json:"compactedContextChars,omitempty"`
	LastUsedAt               time.Time `json:"lastUsedAt,omitempty"`
}

type InboxInput struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversationId"`
	Content        string    `json:"content"`
	Status         string    `json:"status"`
	TurnID         string    `json:"turnId,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
}

type ConversationCancelReceipt struct {
	ConversationID       string `json:"conversationId"`
	CancelledTurnID      string `json:"cancelledTurnId,omitempty"`
	CancelledTurnStatus  string `json:"cancelledTurnStatus,omitempty"`
	CancelledInboxCount  int64  `json:"cancelledInboxCount"`
	QueuedInboxRemaining int    `json:"queuedInboxRemaining"`
	ExecutionPaused      bool   `json:"executionPaused"`
}

type AgentTurnReconciliation struct {
	ID             string    `json:"id"`
	TurnID         string    `json:"turnId"`
	ConversationID string    `json:"conversationId"`
	Decision       string    `json:"decision"`
	Note           string    `json:"note"`
	CreatedAt      time.Time `json:"createdAt"`
}

type TurnReceipt struct {
	TurnID         string `json:"turnId"`
	ConversationID string `json:"conversationId"`
	InputMessageID string `json:"inputMessageId"`
	Status         string `json:"status"`
}

type AgentStep struct {
	ID           string     `json:"id"`
	TurnID       string     `json:"turnId"`
	Ordinal      int        `json:"ordinal"`
	Status       string     `json:"status"`
	AttemptCount int        `json:"attemptCount"`
	StartedAt    time.Time  `json:"startedAt"`
	CompletedAt  *time.Time `json:"completedAt,omitempty"`
}

// AgentSpec is the mutable part of an Agent Definition. The Seed Kernel owns
// execution and permissions; a generation may only select a strategy and its
// bounded parameters.
type AgentSpec struct {
	Strategy      string `json:"strategy"`
	SystemPrompt  string `json:"systemPrompt"`
	PlannerPrompt string `json:"plannerPrompt,omitempty"`
	MaxSteps      int    `json:"maxSteps"`
}

type AgentDefinition struct {
	Digest       string    `json:"digest"`
	UserID       string    `json:"-"`
	APIVersion   string    `json:"apiVersion"`
	Name         string    `json:"name"`
	Description  string    `json:"description"`
	ParentDigest string    `json:"parentDigest,omitempty"`
	Spec         AgentSpec `json:"spec"`
	CreatedAt    time.Time `json:"createdAt"`
}

type AgentGeneration struct {
	ID               string          `json:"id"`
	UserID           string          `json:"-"`
	Number           int             `json:"number"`
	Scope            string          `json:"scope"`
	ScopeKey         string          `json:"scopeKey,omitempty"`
	Status           string          `json:"status"`
	DefinitionDigest string          `json:"definitionDigest"`
	Definition       AgentDefinition `json:"definition"`
	Evidence         json.RawMessage `json:"evidence,omitempty"`
	CreatedAt        time.Time       `json:"createdAt"`
	UpdatedAt        time.Time       `json:"updatedAt"`
}

type FrontierChallenge struct {
	ID                   string          `json:"id"`
	UserID               string          `json:"-"`
	Title                string          `json:"title"`
	Objective            string          `json:"objective"`
	FailureEvidence      string          `json:"failureEvidence,omitempty"`
	SuccessCriteria      string          `json:"successCriteria"`
	GapHypotheses        json.RawMessage `json:"gapHypotheses,omitempty"`
	BaselineGenerationID string          `json:"baselineGenerationId"`
	Status               string          `json:"status"`
	CreatedAt            time.Time       `json:"createdAt"`
	UpdatedAt            time.Time       `json:"updatedAt"`
}

type EvalCase struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Prompt    string `json:"prompt"`
	Evaluator string `json:"evaluator"`
	Expected  string `json:"expected,omitempty"`
}

type RunMetrics struct {
	ModelCalls            int   `json:"modelCalls"`
	ToolCalls             int   `json:"toolCalls"`
	PromptTokens          int   `json:"promptTokens"`
	CompletionTokens      int   `json:"completionTokens"`
	TotalTokens           int   `json:"totalTokens"`
	DurationMillis        int64 `json:"durationMillis"`
	ReachedStepLimit      bool  `json:"reachedStepLimit"`
	ReachedTokenLimit     bool  `json:"reachedTokenLimit,omitempty"`
	ReachedModelCallLimit bool  `json:"reachedModelCallLimit,omitempty"`
}

type EvalTrial struct {
	ID           string     `json:"id"`
	ExperimentID string     `json:"experimentId"`
	CaseID       string     `json:"caseId"`
	Side         string     `json:"side"`
	Repetition   int        `json:"repetition"`
	Success      bool       `json:"success"`
	Status       string     `json:"status"`
	FailureClass string     `json:"failureClass,omitempty"`
	Response     string     `json:"response,omitempty"`
	Error        string     `json:"error,omitempty"`
	Metrics      RunMetrics `json:"metrics"`
	CreatedAt    time.Time  `json:"createdAt"`
}

type EvalSideSummary struct {
	Trials             int     `json:"trials"`
	Successes          int     `json:"successes"`
	SuccessRate        float64 `json:"successRate"`
	TotalTokens        int     `json:"totalTokens"`
	AverageTokens      float64 `json:"averageTokens"`
	AverageDurationMS  float64 `json:"averageDurationMillis"`
	ReachedLimits      int     `json:"reachedLimits"`
	NonPermissionError int     `json:"errors"`
}

type EvalReport struct {
	Baseline             EvalSideSummary `json:"baseline"`
	Candidate            EvalSideSummary `json:"candidate"`
	FrontierWins         int             `json:"frontierWins"`
	Regressions          int             `json:"regressions"`
	PairedCases          int             `json:"pairedCases"`
	EvidenceComplete     bool            `json:"evidenceComplete"`
	IncompleteTrials     int             `json:"incompleteTrials"`
	InfrastructureErrors int             `json:"infrastructureErrors"`
	Recommendation       string          `json:"recommendation"`
	RecommendationCause  string          `json:"recommendationCause"`
	CompletedAt          time.Time       `json:"completedAt"`
}

type EvalExperiment struct {
	ID                    string      `json:"id"`
	UserID                string      `json:"-"`
	ChallengeID           string      `json:"challengeId"`
	BaselineGenerationID  string      `json:"baselineGenerationId"`
	CandidateGenerationID string      `json:"candidateGenerationId"`
	ProviderID            string      `json:"providerId"`
	Status                string      `json:"status"`
	Cases                 []EvalCase  `json:"cases"`
	Repetitions           int         `json:"repetitions"`
	Report                *EvalReport `json:"report,omitempty"`
	LastError             string      `json:"lastError,omitempty"`
	Trials                []EvalTrial `json:"trials,omitempty"`
	CreatedAt             time.Time   `json:"createdAt"`
	UpdatedAt             time.Time   `json:"updatedAt"`
}
