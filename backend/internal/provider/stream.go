package provider

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strings"
)

type StreamEventType string

const (
	StreamStarted            StreamEventType = "response.started"
	StreamContentDelta       StreamEventType = "content.delta"
	StreamToolCallStarted    StreamEventType = "tool_call.started"
	StreamToolArgumentsDelta StreamEventType = "tool_call.arguments.delta"
	StreamCompleted          StreamEventType = "response.completed"
)

type StreamEvent struct {
	Type          StreamEventType `json:"type"`
	Delta         string          `json:"delta,omitempty"`
	ToolCallIndex int             `json:"toolCallIndex,omitempty"`
	ToolCallID    string          `json:"toolCallId,omitempty"`
	ToolName      string          `json:"toolName,omitempty"`
	Completion    *Completion     `json:"completion,omitempty"`
}

type sseFrame struct {
	Event string
	Data  []byte
}

func readSSE(reader io.Reader, consume func(sseFrame) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	frame := sseFrame{}
	flush := func() error {
		if len(frame.Data) == 0 {
			frame = sseFrame{}
			return nil
		}
		frame.Data = bytes.TrimSuffix(frame.Data, []byte("\n"))
		err := consume(frame)
		frame = sseFrame{}
		return err
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			frame.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") {
			frame.Data = append(frame.Data, strings.TrimSpace(strings.TrimPrefix(line, "data:"))...)
			frame.Data = append(frame.Data, '\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}

func decodeOpenAIChatStream(reader io.Reader, emit func(StreamEvent) error) (Completion, error) {
	completion := Completion{}
	tools := map[int]*ToolCall{}
	started := false
	err := readSSE(reader, func(frame sseFrame) error {
		if string(frame.Data) == "[DONE]" {
			return nil
		}
		var chunk struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
					ToolCalls        []struct {
						Index    int                              `json:"index"`
						ID       string                           `json:"id"`
						Function struct{ Name, Arguments string } `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(frame.Data, &chunk); err != nil {
			return err
		}
		if !started {
			started = true
			if err := emit(StreamEvent{Type: StreamStarted}); err != nil {
				return err
			}
		}
		completion.Model = chunk.Model
		if chunk.Usage.TotalTokens > 0 {
			completion.Usage = Usage{PromptTokens: chunk.Usage.PromptTokens, CompletionTokens: chunk.Usage.CompletionTokens, TotalTokens: chunk.Usage.TotalTokens}
		}
		for _, choice := range chunk.Choices {
			deltaText := choice.Delta.Content
			if deltaText == "" {
				if choice.Delta.ReasoningContent != "" {
					deltaText = choice.Delta.ReasoningContent
				} else if choice.Delta.Reasoning != "" {
					deltaText = choice.Delta.Reasoning
				}
			}
			if deltaText != "" {
				completion.Content += deltaText
				if err := emit(StreamEvent{Type: StreamContentDelta, Delta: deltaText}); err != nil {
					return err
				}
			}
			for _, delta := range choice.Delta.ToolCalls {
				call := tools[delta.Index]
				if call == nil {
					call = &ToolCall{ID: delta.ID, Type: "function"}
					tools[delta.Index] = call
					if err := emit(StreamEvent{Type: StreamToolCallStarted, ToolCallIndex: delta.Index, ToolCallID: delta.ID, ToolName: delta.Function.Name}); err != nil {
						return err
					}
				}
				if delta.ID != "" {
					call.ID = delta.ID
				}
				if delta.Function.Name != "" {
					call.Function.Name = delta.Function.Name
				}
				call.Function.Arguments += delta.Function.Arguments
				if delta.Function.Arguments != "" {
					if err := emit(StreamEvent{Type: StreamToolArgumentsDelta, ToolCallIndex: delta.Index, Delta: delta.Function.Arguments}); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return Completion{}, err
	}
	indexes := make([]int, 0, len(tools))
	for index := range tools {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		completion.ToolCalls = append(completion.ToolCalls, *tools[index])
	}
	final, err := normalizeCompletion(completion)
	if err != nil {
		return Completion{}, err
	}
	if err := emit(StreamEvent{Type: StreamCompleted, Completion: &final}); err != nil {
		return Completion{}, err
	}
	return final, nil
}
