package provider

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
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

func decodeOpenAIResponsesStream(reader io.Reader, emit func(StreamEvent) error) (Completion, error) {
	var final Completion
	err := readSSE(reader, func(frame sseFrame) error {
		var event struct {
			Type        string          `json:"type"`
			Delta       string          `json:"delta"`
			OutputIndex int             `json:"output_index"`
			Item        json.RawMessage `json:"item"`
			Response    json.RawMessage `json:"response"`
		}
		if err := json.Unmarshal(frame.Data, &event); err != nil {
			return err
		}
		switch event.Type {
		case "response.created", "response.in_progress":
			if event.Type == "response.created" {
				return emit(StreamEvent{Type: StreamStarted})
			}
		case "response.output_text.delta":
			return emit(StreamEvent{Type: StreamContentDelta, Delta: event.Delta})
		case "response.output_item.added":
			var item struct{ Type, CallID, Name string }
			if json.Unmarshal(event.Item, &item) == nil && item.Type == "function_call" {
				return emit(StreamEvent{Type: StreamToolCallStarted, ToolCallIndex: event.OutputIndex, ToolCallID: item.CallID, ToolName: item.Name})
			}
		case "response.function_call_arguments.delta":
			return emit(StreamEvent{Type: StreamToolArgumentsDelta, ToolCallIndex: event.OutputIndex, Delta: event.Delta})
		case "response.completed":
			completion, err := (openAIResponsesAdapter{}).decodeCompletion(event.Response)
			if err != nil {
				return err
			}
			final = completion
			return emit(StreamEvent{Type: StreamCompleted, Completion: &final})
		case "response.failed", "response.incomplete":
			return fmt.Errorf("OpenAI response stream ended with %s", event.Type)
		}
		return nil
	})
	if err != nil {
		return Completion{}, err
	}
	if final.Content == "" && len(final.ToolCalls) == 0 {
		return Completion{}, fmt.Errorf("OpenAI response stream ended without response.completed")
	}
	return final, nil
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
					Content   string `json:"content"`
					ToolCalls []struct {
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
			if choice.Delta.Content != "" {
				completion.Content += choice.Delta.Content
				if err := emit(StreamEvent{Type: StreamContentDelta, Delta: choice.Delta.Content}); err != nil {
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

func decodeAnthropicStream(reader io.Reader, emit func(StreamEvent) error) (Completion, error) {
	completion := Completion{}
	tools := map[int]*ToolCall{}
	argumentDeltas := map[int]string{}
	err := readSSE(reader, func(frame sseFrame) error {
		var event struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message struct {
				Model string `json:"model"`
				Usage struct {
					InputTokens int `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			ContentBlock struct {
				Type  string          `json:"type"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(frame.Data, &event); err != nil {
			return err
		}
		switch event.Type {
		case "message_start":
			completion.Model = event.Message.Model
			completion.Usage.PromptTokens = event.Message.Usage.InputTokens
			return emit(StreamEvent{Type: StreamStarted})
		case "content_block_start":
			if event.ContentBlock.Type == "tool_use" {
				tools[event.Index] = &ToolCall{ID: event.ContentBlock.ID, Type: "function", Function: ToolFunction{Name: event.ContentBlock.Name, Arguments: string(event.ContentBlock.Input)}}
				return emit(StreamEvent{Type: StreamToolCallStarted, ToolCallIndex: event.Index, ToolCallID: event.ContentBlock.ID, ToolName: event.ContentBlock.Name})
			}
		case "content_block_delta":
			if event.Delta.Type == "text_delta" {
				completion.Content += event.Delta.Text
				return emit(StreamEvent{Type: StreamContentDelta, Delta: event.Delta.Text})
			}
			if event.Delta.Type == "input_json_delta" {
				argumentDeltas[event.Index] += event.Delta.PartialJSON
				return emit(StreamEvent{Type: StreamToolArgumentsDelta, ToolCallIndex: event.Index, Delta: event.Delta.PartialJSON})
			}
		case "message_delta":
			completion.Usage.CompletionTokens = event.Usage.OutputTokens
			completion.Usage.TotalTokens = completion.Usage.PromptTokens + completion.Usage.CompletionTokens
		case "error":
			return fmt.Errorf("Anthropic stream failed: %s", event.Error.Message)
		case "message_stop":
			indexes := make([]int, 0, len(tools))
			for index := range tools {
				indexes = append(indexes, index)
			}
			sort.Ints(indexes)
			for _, index := range indexes {
				if argumentDeltas[index] != "" {
					tools[index].Function.Arguments = argumentDeltas[index]
				}
				completion.ToolCalls = append(completion.ToolCalls, *tools[index])
			}
			final, err := normalizeCompletion(completion)
			if err != nil {
				return err
			}
			completion = final
			return emit(StreamEvent{Type: StreamCompleted, Completion: &completion})
		}
		return nil
	})
	if err != nil {
		return Completion{}, err
	}
	if completion.Content == "" && len(completion.ToolCalls) == 0 {
		return Completion{}, fmt.Errorf("Anthropic stream ended without message_stop")
	}
	return completion, nil
}
