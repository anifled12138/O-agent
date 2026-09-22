package provider

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	KindNewAPI            = "new-api"
	KindOneAPI            = "one-api"
	KindOpenAICompatible  = "openai-compatible"
	KindOpenAIResponses   = "openai-responses"
	KindAnthropicMessages = "anthropic-messages"
	KindDeepSeekChat      = "deepseek-chat"
	KindOllama            = "ollama"
	KindVLLM              = "vllm"
)

type SupportLevel string

const (
	SupportUnknown SupportLevel = "unknown"
	SupportNo      SupportLevel = "no"
	SupportYes     SupportLevel = "yes"
)

type ModelCapabilities struct {
	Tools             SupportLevel `json:"tools"`
	ParallelToolCalls SupportLevel `json:"parallelToolCalls"`
	Streaming         SupportLevel `json:"streaming"`
	Vision            SupportLevel `json:"vision"`
	StrictJSONSchema  SupportLevel `json:"strictJsonSchema"`
	ReasoningControl  SupportLevel `json:"reasoningControl"`
	PromptCaching     SupportLevel `json:"promptCaching"`
}

type KindDescriptor struct {
	Kind           string            `json:"kind"`
	Label          string            `json:"label"`
	Description    string            `json:"description"`
	DefaultBaseURL string            `json:"defaultBaseUrl"`
	Capabilities   ModelCapabilities `json:"capabilities"`
}

func Kinds() []KindDescriptor {
	return []KindDescriptor{
		{Kind: KindNewAPI, Label: "New-API / One-API", Description: "Universal open-source LLM router / gateway with unified OpenAI completions format.", DefaultBaseURL: "http://127.0.0.1:3000/v1", Capabilities: ModelCapabilities{Tools: SupportYes, ParallelToolCalls: SupportYes, Streaming: SupportYes, Vision: SupportYes, StrictJSONSchema: SupportYes, ReasoningControl: SupportYes, PromptCaching: SupportYes}},
		{Kind: KindOpenAIResponses, Label: "OpenAI Responses", Description: "OpenAI's Responses API with native function calls.", DefaultBaseURL: "https://api.openai.com/v1", Capabilities: ModelCapabilities{Tools: SupportYes, ParallelToolCalls: SupportYes, Streaming: SupportYes, Vision: SupportYes, StrictJSONSchema: SupportYes, ReasoningControl: SupportYes, PromptCaching: SupportYes}},
		{Kind: KindAnthropicMessages, Label: "Anthropic Messages", Description: "Anthropic Messages API with tool_use and tool_result blocks.", DefaultBaseURL: "https://api.anthropic.com/v1", Capabilities: ModelCapabilities{Tools: SupportYes, ParallelToolCalls: SupportYes, Streaming: SupportYes, Vision: SupportYes, StrictJSONSchema: SupportUnknown, ReasoningControl: SupportYes, PromptCaching: SupportYes}},
		{Kind: KindDeepSeekChat, Label: "DeepSeek Chat", Description: "DeepSeek's OpenAI-compatible chat API with conservative defaults.", DefaultBaseURL: "https://api.deepseek.com/v1", Capabilities: ModelCapabilities{Tools: SupportYes, ParallelToolCalls: SupportUnknown, Streaming: SupportYes, Vision: SupportNo, StrictJSONSchema: SupportUnknown, ReasoningControl: SupportUnknown, PromptCaching: SupportYes}},
		{Kind: KindOllama, Label: "Ollama", Description: "Local open-source models via Ollama (Llama, Qwen, DeepSeek).", DefaultBaseURL: "http://127.0.0.1:11434/v1", Capabilities: ModelCapabilities{Tools: SupportYes, ParallelToolCalls: SupportUnknown, Streaming: SupportYes, Vision: SupportYes, StrictJSONSchema: SupportUnknown, ReasoningControl: SupportUnknown, PromptCaching: SupportUnknown}},
		{Kind: KindVLLM, Label: "vLLM / LocalAI", Description: "High-throughput open-source LLM inference serving OpenAI-compatible endpoints.", DefaultBaseURL: "http://127.0.0.1:8000/v1", Capabilities: ModelCapabilities{Tools: SupportYes, ParallelToolCalls: SupportYes, Streaming: SupportYes, Vision: SupportUnknown, StrictJSONSchema: SupportUnknown, ReasoningControl: SupportUnknown, PromptCaching: SupportUnknown}},
		{Kind: KindOpenAICompatible, Label: "OpenAI-compatible", Description: "A custom endpoint implementing the Chat Completions wire format.", DefaultBaseURL: "http://127.0.0.1:8000/v1", Capabilities: ModelCapabilities{Tools: SupportUnknown, ParallelToolCalls: SupportUnknown, Streaming: SupportUnknown, Vision: SupportUnknown, StrictJSONSchema: SupportUnknown, ReasoningControl: SupportUnknown, PromptCaching: SupportUnknown}},
	}
}

func normalizeKind(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", KindOpenAICompatible, "openai-chat", "openai-chat-completions":
		return KindOpenAICompatible, true
	case KindNewAPI, "newapi", KindOneAPI, "oneapi":
		return KindNewAPI, true
	case KindOpenAIResponses, "responses":
		return KindOpenAIResponses, true
	case KindAnthropicMessages, "anthropic":
		return KindAnthropicMessages, true
	case KindDeepSeekChat, "deepseek":
		return KindDeepSeekChat, true
	case KindOllama:
		return KindOllama, true
	case KindVLLM, "localai":
		return KindVLLM, true
	default:
		return "", false
	}
}

type ErrorClass string

const (
	ErrorAuthentication  ErrorClass = "authentication"
	ErrorRateLimit       ErrorClass = "rate_limit"
	ErrorUnavailable     ErrorClass = "unavailable"
	ErrorInvalidRequest  ErrorClass = "invalid_request"
	ErrorNotFound        ErrorClass = "not_found"
	ErrorContextOverflow ErrorClass = "context_overflow"
	ErrorUnsupported     ErrorClass = "unsupported"
	ErrorProtocol        ErrorClass = "protocol"
	ErrorCancelled       ErrorClass = "cancelled"
)

type ProviderError struct {
	Class      ErrorClass
	StatusCode int
	RetryAfter time.Duration
	SafeDetail string
	Cause      error
}

func (e *ProviderError) Error() string {
	if e.SafeDetail != "" {
		return e.SafeDetail
	}
	return fmt.Sprintf("provider request failed (%s)", e.Class)
}

func (e *ProviderError) Unwrap() error { return e.Cause }

type CompatibilityWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// UnmarshalJSON accepts the standard JSON-string argument representation and
// the object form emitted by some nominally OpenAI-compatible providers.
func (f *ToolFunction) UnmarshalJSON(data []byte) error {
	f.normalizedObjectArguments = false
	var value struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	f.Name = value.Name
	if len(value.Arguments) == 0 || string(value.Arguments) == "null" {
		f.Arguments = "{}"
		return nil
	}
	if value.Arguments[0] == '"' {
		return json.Unmarshal(value.Arguments, &f.Arguments)
	}
	compact, err := json.Marshal(json.RawMessage(value.Arguments))
	if err != nil {
		return err
	}
	f.Arguments = string(compact)
	f.normalizedObjectArguments = true
	return nil
}
