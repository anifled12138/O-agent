package agent

import "time"

type RuntimeHealth struct {
	ActiveRuns             int           `json:"activeRuns"`
	QueuedInputs           int           `json:"queuedInputs"`
	OutstandingEvaluations int           `json:"outstandingEvaluations"`
	MaxConcurrent          int           `json:"maxConcurrentRuns"`
	MaxTokensPerRun        int           `json:"maxTokensPerRun"`
	MaxModelCalls          int           `json:"maxModelCalls"`
	MaxRunDuration         time.Duration `json:"-"`
}

// RunLimits are host-owned ceilings. Agent generations may request stricter
// limits, but cannot widen these values.
type RunLimits struct {
	MaxConcurrentRuns int
	MaxTokensPerRun   int
	MaxModelCalls     int
	MaxRunDuration    time.Duration
}

func DefaultRunLimits() RunLimits {
	return RunLimits{
		MaxConcurrentRuns: 3,
		MaxTokensPerRun:   60000,
		MaxModelCalls:     64,
		MaxRunDuration:    30 * time.Minute,
	}
}

func (limits RunLimits) normalized() RunLimits {
	defaults := DefaultRunLimits()
	if limits.MaxConcurrentRuns < 1 {
		limits.MaxConcurrentRuns = defaults.MaxConcurrentRuns
	}
	if limits.MaxTokensPerRun < 1 {
		limits.MaxTokensPerRun = defaults.MaxTokensPerRun
	}
	if limits.MaxModelCalls < 1 {
		limits.MaxModelCalls = defaults.MaxModelCalls
	}
	if limits.MaxRunDuration <= 0 {
		limits.MaxRunDuration = defaults.MaxRunDuration
	}
	return limits
}
