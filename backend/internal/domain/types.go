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
	ID        string    `json:"id"`
	UserID    string    `json:"-"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	BaseURL   string    `json:"baseUrl"`
	Model     string    `json:"model"`
	HasAPIKey bool      `json:"hasApiKey"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type Conversation struct {
	ID                    string    `json:"id"`
	UserID                string    `json:"-"`
	Title                 string    `json:"title"`
	ProviderID            string    `json:"providerId"`
	AgentGenerationID     string    `json:"agentGenerationId,omitempty"`
	AgentDefinitionDigest string    `json:"agentDefinitionDigest,omitempty"`
	CreatedAt             time.Time `json:"createdAt"`
	UpdatedAt             time.Time `json:"updatedAt"`
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
	Messages []Message `json:"messages"`
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
	ID                    string     `json:"id"`
	ConversationID        string     `json:"conversationId"`
	UserID                string     `json:"-"`
	InputMessageID        string     `json:"inputMessageId"`
	ResultMessageID       string     `json:"resultMessageId,omitempty"`
	ProviderID            string     `json:"providerId"`
	AgentGenerationID     string     `json:"agentGenerationId"`
	AgentDefinitionDigest string     `json:"agentDefinitionDigest"`
	Status                string     `json:"status"`
	StopReason            string     `json:"stopReason,omitempty"`
	RecoveryClass         string     `json:"recoveryClass,omitempty"`
	CancelRequested       bool       `json:"cancelRequested"`
	LastSequence          int        `json:"lastSequence"`
	StartedAt             time.Time  `json:"startedAt"`
	UpdatedAt             time.Time  `json:"updatedAt"`
	CompletedAt           *time.Time `json:"completedAt,omitempty"`
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
	ModelCalls       int   `json:"modelCalls"`
	ToolCalls        int   `json:"toolCalls"`
	PromptTokens     int   `json:"promptTokens"`
	CompletionTokens int   `json:"completionTokens"`
	TotalTokens      int   `json:"totalTokens"`
	DurationMillis   int64 `json:"durationMillis"`
	ReachedStepLimit bool  `json:"reachedStepLimit"`
}

type EvalTrial struct {
	ID           string     `json:"id"`
	ExperimentID string     `json:"experimentId"`
	CaseID       string     `json:"caseId"`
	Side         string     `json:"side"`
	Repetition   int        `json:"repetition"`
	Success      bool       `json:"success"`
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
	Baseline            EvalSideSummary `json:"baseline"`
	Candidate           EvalSideSummary `json:"candidate"`
	FrontierWins        int             `json:"frontierWins"`
	Regressions         int             `json:"regressions"`
	PairedCases         int             `json:"pairedCases"`
	Recommendation      string          `json:"recommendation"`
	RecommendationCause string          `json:"recommendationCause"`
	CompletedAt         time.Time       `json:"completedAt"`
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
