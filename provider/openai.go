package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type openAIProvider struct {
	name  string
	model string
	http  *httpClient
}

type openAIMessage struct {
	Role       string       `json:"role"`
	Content    *string      `json:"content,omitempty"`
	ToolCalls  []openAICall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type openAICall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

func (p *openAIProvider) Name() string { return p.name }

func (p *openAIProvider) Complete(ctx context.Context, req Request) (Response, error) {
	body := struct {
		Model     string          `json:"model"`
		MaxTokens int             `json:"max_tokens"`
		Messages  []openAIMessage `json:"messages"`
		Tools     []openAITool    `json:"tools,omitempty"`
	}{Model: req.Model, MaxTokens: req.MaxTokens, Messages: make([]openAIMessage, 0, len(req.Messages)+1)}
	if body.Model == "" {
		body.Model = p.model
	}
	if req.System != "" {
		body.Messages = append(body.Messages, openAIMessage{Role: "system", Content: &req.System})
	}
	for _, msg := range req.Messages {
		messages, err := openAIMessages(msg)
		if err != nil {
			return Response{}, err
		}
		body.Messages = append(body.Messages, messages...)
	}
	for _, spec := range req.Tools {
		wire := openAITool{Type: "function"}
		wire.Function.Name, wire.Function.Description, wire.Function.Parameters = spec.Name, spec.Description, spec.InputSchema
		body.Tools = append(body.Tools, wire)
	}

	var reply struct {
		Choices []struct {
			Message      openAIMessage `json:"message"`
			FinishReason string        `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	headers := http.Header{}
	if p.http.key != "" {
		headers.Set("Authorization", "Bearer "+p.http.key)
	}
	if err := p.http.post(ctx, body, &reply, headers); err != nil {
		return Response{}, err
	}
	if len(reply.Choices) == 0 {
		return Response{}, fmt.Errorf("OpenAI response contains no choices")
	}
	choice := reply.Choices[0]
	res := Response{StopReason: choice.FinishReason, Usage: Usage{reply.Usage.PromptTokens, reply.Usage.CompletionTokens}}
	switch res.StopReason {
	case "stop":
		res.StopReason = "end_turn"
	case "tool_calls":
		res.StopReason = "tool_use"
	case "length":
		res.StopReason = "max_tokens"
	}
	if choice.Message.Content != nil {
		res.Content = append(res.Content, Block{Type: "text", Text: *choice.Message.Content})
	}
	for _, call := range choice.Message.ToolCalls {
		input := json.RawMessage(call.Function.Arguments)
		if !json.Valid(input) {
			return Response{}, fmt.Errorf("OpenAI tool arguments are invalid JSON")
		}
		res.Content = append(res.Content, Block{Type: "tool_use", ID: call.ID, Name: call.Function.Name, Input: input})
	}
	return res, nil
}

func openAIMessages(msg Message) ([]openAIMessage, error) {
	var messages []openAIMessage
	var text []string
	wire := openAIMessage{Role: msg.Role}
	flush := func() {
		if len(text) == 0 && len(wire.ToolCalls) == 0 {
			return
		}
		if len(text) > 0 {
			content := strings.Join(text, "\n")
			wire.Content = &content
		}
		messages = append(messages, wire)
		wire = openAIMessage{Role: msg.Role}
		text = nil
	}
	for _, b := range msg.Content {
		switch b.Type {
		case "text":
			text = append(text, b.Text)
		case "tool_use":
			call := openAICall{ID: b.ID, Type: "function"}
			call.Function.Name, call.Function.Arguments = b.Name, string(b.Input)
			if call.Function.Arguments == "" {
				call.Function.Arguments = "{}"
			}
			wire.ToolCalls = append(wire.ToolCalls, call)
		case "tool_result":
			flush()
			messages = append(messages, openAIMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: &b.Text})
		default:
			return nil, fmt.Errorf("OpenAI request has an unknown content block")
		}
	}
	flush()
	if len(messages) == 0 {
		messages = append(messages, openAIMessage{Role: msg.Role, Content: new(string)})
	}
	return messages, nil
}
