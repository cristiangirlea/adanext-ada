package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/cristiangirlea/adanext-ada/approval"
	"github.com/cristiangirlea/adanext-ada/provider"
)

// InheritedEnvNames returns the system variables shared by stdio plugins.
func InheritedEnvNames() []string { return slices.Clone(mcpInheritedEnv) }

type Effect string

const (
	Read           Effect = "read"
	DelegateEffect Effect = "delegate"
	Write          Effect = "write"
	Send           Effect = "send"
	Account        Effect = "account"
)

type CallEffect interface {
	EffectFor(json.RawMessage) (Effect, error)
}

type Summarizer interface {
	Summary(json.RawMessage) (string, error)
}

type approvedSummaryKey struct{}

type Tool interface {
	Spec() provider.ToolSpec
	Effect() Effect
	Run(context.Context, json.RawMessage) (string, error)
}

type Registry struct {
	Recorder        *approval.Recorder
	ApprovalTimeout time.Duration
	tools           map[string]Tool
	closers         []io.Closer
}

func (r *Registry) Add(t Tool) error {
	if t == nil {
		return fmt.Errorf("tool is required")
	}
	name := t.Spec().Name
	if name == "" {
		return fmt.Errorf("tool name is required")
	}
	switch t.Effect() {
	case Read, DelegateEffect, Write, Send, Account:
	default:
		return fmt.Errorf("tool %q has an unclassified effect", name)
	}
	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("duplicate tool %q", name)
	}
	if r.tools == nil {
		r.tools = make(map[string]Tool)
	}
	r.tools[name] = t
	return nil
}

func (r *Registry) Specs() []provider.ToolSpec {
	if r == nil {
		return nil
	}
	specs := make([]provider.ToolSpec, 0, len(r.tools))
	for _, t := range r.tools {
		specs = append(specs, t.Spec())
	}
	slices.SortFunc(specs, func(a, b provider.ToolSpec) int { return strings.Compare(a.Name, b.Name) })
	return specs
}

func (r *Registry) Run(ctx context.Context, name string, input json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if r == nil || r.tools[name] == nil {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	t := r.tools[name]
	effect := t.Effect()
	if c, ok := t.(CallEffect); ok {
		var err error
		effect, err = c.EffectFor(input)
		if err != nil {
			return "", fmt.Errorf("classify %s: %w", name, err)
		}
	}
	switch effect {
	case Read, DelegateEffect:
		return t.Run(ctx, input)
	case Write, Send:
		return r.runWithApproval(ctx, t, name, effect, input)
	case Account:
		return "", fmt.Errorf("%s refused: Ada never touches money or accounts", name)
	default:
		return "", fmt.Errorf("%s refused: unclassified effect %q", name, effect)
	}
}

func canonicalArgs(input json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	if !json.Valid(input) {
		return nil, fmt.Errorf("approval arguments must be valid JSON")
	}
	var args any
	if err := decoder.Decode(&args); err != nil {
		return nil, fmt.Errorf("decode approval arguments: %w", err)
	}
	return json.Marshal(args)
}

func (r *Registry) runWithApproval(ctx context.Context, t Tool, name string, effect Effect, input json.RawMessage) (string, error) {
	// Execute the same canonical arguments that the surface displays, including large integers.
	input, err := canonicalArgs(input)
	if err != nil {
		return "", fmt.Errorf("encode arguments for %s: %w", name, err)
	}
	summary := name + " " + string(input)
	if s, ok := t.(Summarizer); ok {
		summary, err = s.Summary(input)
		if err != nil {
			return "", fmt.Errorf("summarize %s: %w", name, err)
		}
		if strings.TrimSpace(summary) == "" {
			return "", fmt.Errorf("%s refused: missing approval summary", name)
		}
	}
	req := approval.NewRequest(ctx, name, string(effect), summary, r.ApprovalTimeout)
	d, askErr := approval.Decide(ctx, req)
	if r.Recorder != nil {
		if err := r.Recorder.Record(req, d); err != nil {
			return "", fmt.Errorf("%s refused: %w", name, err)
		}
	}
	if !d.Approved {
		if askErr != nil && d.Reason == "denied" {
			// Surface diagnostics can contain the private request ID.
			return "", fmt.Errorf("%s refused: could not read a decision from the approval surface", name)
		}
		if d.Reason == "no approver" {
			return "", fmt.Errorf("%s refused: no way to ask for approval here", name)
		}
		return "", fmt.Errorf("%s refused: approval %s", name, d.Reason)
	}
	if r.Recorder == nil {
		return "", fmt.Errorf("%s refused: approval recorder is unavailable", name)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return t.Run(context.WithValue(ctx, approvedSummaryKey{}, summary), input)
}

func (r *Registry) Close() error {
	var err error
	for _, closer := range r.closers {
		err = errors.Join(err, closer.Close())
	}
	r.closers = nil
	return err
}

func decodeInput(data json.RawMessage, input any) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' || !json.Valid(data) {
		return fmt.Errorf("tool input must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(input); err != nil {
		return fmt.Errorf("decode tool input: %w", err)
	}
	return nil
}
