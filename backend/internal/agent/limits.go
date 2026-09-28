package agent

type RuntimeHealth struct {
	ActiveRuns             int `json:"activeRuns"`
	QueuedInputs           int `json:"queuedInputs"`
	OutstandingEvaluations int `json:"outstandingEvaluations"`
	MaxModelCalls          int `json:"maxModelCalls"`
}

// RunLimits holds optional host-level run settings. A zero model-call budget
// means unlimited; positive values are a configurable per-run budget.
type RunLimits struct {
	MaxModelCalls int
}

func DefaultRunLimits() RunLimits {
	return RunLimits{MaxModelCalls: 300}
}

func (limits RunLimits) normalized() RunLimits {
	if limits.MaxModelCalls < 0 {
		limits.MaxModelCalls = DefaultRunLimits().MaxModelCalls
	}
	return limits
}
