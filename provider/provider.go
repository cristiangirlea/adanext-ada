package provider

import (
	"context"
	"encoding/json"
	"strings"
)

type Message struct {
	Role    string
	Content []Block
}

// Text joins the text blocks as they appear in a conversation answer.
func (m Message) Text() string {
	var parts []string
	for _, block := range m.Content {
		if block.Type == "text" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

type Block struct {
	Type      string
	Text      string
	ID        string
	Name      string
	Input     json.RawMessage
	ToolUseID string
	IsError   bool
}

type ToolSpec struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

type Request struct {
	Model     string
	System    string
	Messages  []Message
	Tools     []ToolSpec
	MaxTokens int
}

type Response struct {
	Content    []Block
	StopReason string
	Usage      Usage
}

type Usage struct {
	InputTokens  int
	OutputTokens int
}

// EstimateTokens accounts for content when the provider omits token usage.
func EstimateTokens(req Request, res Response) int {
	size := len(req.System)
	for _, msg := range req.Messages {
		for _, block := range msg.Content {
			switch block.Type {
			case "text", "tool_result":
				size += len(block.Text)
			case "tool_use":
				size += len(block.Input)
			}
		}
	}
	for _, block := range res.Content {
		switch block.Type {
		case "text":
			size += len(block.Text)
		case "tool_use":
			size += len(block.Input)
		}
	}
	return size / 4
}

type Provider interface {
	Complete(context.Context, Request) (Response, error)
	Name() string
}
