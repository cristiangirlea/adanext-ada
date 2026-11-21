package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

type anthropicProvider struct {
	name  string
	model string
	http  *httpClient
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      *string         `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   *bool           `json:"is_error,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

func (p *anthropicProvider) Name() string { return p.name }

func (p *anthropicProvider) Complete(ctx context.Context, req Request) (Response, error) {
	body := struct {
		Model     string             `json:"model"`
		System    string             `json:"system,omitempty"`
		MaxTokens int                `json:"max_tokens"`
		Messages  []anthropicMessage `json:"messages"`
		Tools     []anthropicTool    `json:"tools,omitempty"`
	}{
		Model: req.Model, System: req.System, MaxTokens: req.MaxTokens,
		Messages: make([]anthropicMessage, 0, len(req.Messages)),
	}
	if body.Model == "" {
		body.Model = p.model
	}
	for _, message := range req.Messages {
		wire := anthropicMessage{Role: message.Role, Content: make([]anthropicBlock, 0, len(message.Content))}
		for _, b := range message.Content {
			switch b.Type {
			case "text":
				wire.Content = append(wire.Content, anthropicBlock{Type: b.Type, Text: &b.Text})
			case "tool_use":
				input := json.RawMessage(bytes.TrimSpace(b.Input))
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				if input[0] != '{' {
					return Response{}, fmt.Errorf("Anthropic tool input must be a JSON object")
				}
				wire.Content = append(wire.Content, anthropicBlock{Type: b.Type, ID: b.ID, Name: b.Name, Input: input})
			case "tool_result":
				content, _ := json.Marshal(b.Text)
				wire.Content = append(wire.Content, anthropicBlock{Type: b.Type, ToolUseID: b.ToolUseID, Content: content, IsError: &b.IsError})
			default:
				return Response{}, fmt.Errorf("anthropic request has an unknown content block")
			}
		}
		body.Messages = append(body.Messages, wire)
	}
	for _, tool := range req.Tools {
		body.Tools = append(body.Tools, anthropicTool(tool))
	}

	var reply struct {
		Content    []json.RawMessage `json:"content"`
		StopReason string            `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	key := p.http.key
	if key == "" {
		key = "local"
	}
	headers := http.Header{}
	headers.Set("anthropic-version", "2023-06-01")
	headers.Set("x-api-key", key)
	if err := p.http.post(ctx, body, &reply, headers); err != nil {
		return Response{}, err
	}
	res := Response{
		StopReason: reply.StopReason,
		Usage:      Usage(reply.Usage),
	}
	for _, raw := range reply.Content {
		var kind struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &kind); err != nil {
			return Response{}, fmt.Errorf("decode Anthropic content block type")
		}
		switch kind.Type {
		case "text", "tool_use", "tool_result":
		default:
			continue
		}
		var b anthropicBlock
		if err := json.Unmarshal(raw, &b); err != nil {
			return Response{}, fmt.Errorf("decode Anthropic content block")
		}
		switch b.Type {
		case "text":
			res.Content = append(res.Content, Block{Type: b.Type, Text: stringValue(b.Text)})
		case "tool_use":
			res.Content = append(res.Content, Block{Type: b.Type, ID: b.ID, Name: b.Name, Input: b.Input})
		case "tool_result":
			var text string
			if len(b.Content) > 0 && json.Unmarshal(b.Content, &text) != nil {
				return Response{}, fmt.Errorf("decode Anthropic tool result content")
			}
			res.Content = append(res.Content, Block{Type: b.Type, Text: text, ToolUseID: b.ToolUseID, IsError: b.IsError != nil && *b.IsError})
		}
	}
	return res, nil
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
