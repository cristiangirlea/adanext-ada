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

	"github.com/cristiangirlea/adanext-ada/provider"
)

type Effect string

const (
	Read           Effect = "read"
	DelegateEffect Effect = "delegate"
)

type Tool interface {
	Spec() provider.ToolSpec
	Effect() Effect
	Run(context.Context, json.RawMessage) (string, error)
}

type Registry struct {
	tools   map[string]Tool
	closers []io.Closer
}

func (r *Registry) Add(t Tool) error {
	if t == nil {
		return fmt.Errorf("tool is required")
	}
	name := t.Spec().Name
	if name == "" {
		return fmt.Errorf("tool name is required")
	}
	if effect := t.Effect(); effect != Read && effect != DelegateEffect {
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
	if r != nil {
		if t, exists := r.tools[name]; exists {
			return t.Run(ctx, input)
		}
	}
	return "", fmt.Errorf("unknown tool %q", name)
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
