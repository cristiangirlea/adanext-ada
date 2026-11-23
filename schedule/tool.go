package schedule

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cristiangirlea/adanext-ada/provider"
	"github.com/cristiangirlea/adanext-ada/tool"
)

type originKey struct{}

func WithOrigin(ctx context.Context, t Target) context.Context {
	return context.WithValue(ctx, originKey{}, t)
}

type PromptTool struct{ Store Store }

func (PromptTool) Effect() tool.Effect { return tool.Write }
func (PromptTool) Spec() provider.ToolSpec {
	return provider.ToolSpec{Name: "schedule_prompt_job", Description: "Schedule a recurring prompt locally or deliver its answer to this chat.", InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"spec":{"type":"string"},"prompt":{"type":"string"},"to":{"type":"string","description":"CHANNEL:CHAT for this conversation, or omit for local only"}},"required":["name","spec","prompt"],"additionalProperties":false}`)}
}
func promptInput(ctx context.Context, raw json.RawMessage) (Job, error) {
	var input struct{ Name, Spec, Prompt, To string }
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if !json.Valid(raw) {
		return Job{}, fmt.Errorf("invalid schedule tool arguments")
	}
	if err := d.Decode(&input); err != nil {
		return Job{}, fmt.Errorf("decode prompt job: %w", err)
	}
	if strings.TrimSpace(input.Name) == "" || strings.TrimSpace(input.Prompt) == "" {
		return Job{}, fmt.Errorf("name and prompt are required")
	}
	target, err := ParseTarget(input.To)
	if err != nil {
		return Job{}, err
	}
	origin, _ := ctx.Value(originKey{}).(Target)
	if target != (Target{}) && target != origin {
		return Job{}, fmt.Errorf("delivery is limited to the originating chat")
	}
	return Job{Kind: "prompt", Name: input.Name, Spec: input.Spec, Prompt: input.Prompt, Target: target}, nil
}
func (t PromptTool) SummaryFor(ctx context.Context, raw json.RawMessage) (string, error) {
	j, err := promptInput(ctx, raw)
	if err != nil {
		return "", err
	}
	var times []string
	at := tool.ApprovalTime(ctx)
	for range 3 {
		at, err = Next(j.Spec, at)
		if err != nil {
			return "", err
		}
		times = append(times, at.Format(time.RFC3339Nano))
	}
	return fmt.Sprintf("schedule prompt job %q, spec %q, next %s, prompt %q, target %q", j.Name, j.Spec, strings.Join(times, ", "), j.Prompt, j.Target), nil
}
func (t PromptTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	j, err := promptInput(ctx, raw)
	if err != nil {
		return "", err
	}
	j, err = t.Store.Add(j, tool.ApprovalTime(ctx))
	return j.ID, err
}
