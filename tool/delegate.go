package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cristiangirlea/adanext-ada/approval"
	"github.com/cristiangirlea/adanext-ada/hrnexec"
	"github.com/cristiangirlea/adanext-ada/memory"
	"github.com/cristiangirlea/adanext-ada/provider"
)

const DefaultDelegateTimeout = 30 * time.Minute

var delegatePurposes = []string{"review", "debug", "implement", "refactor", "test", "research", "plan", "ops"}

type Delegate struct {
	RecordDelegation func(runID, user, session string) error
	Roots            []string
	Options          hrnexec.Options
	Mode             string
	Timeout          time.Duration
	Backend          string
}

func (Delegate) Spec() provider.ToolSpec {
	purposes, _ := json.Marshal(delegatePurposes)
	return provider.ToolSpec{
		Name:        "delegate_to_hrn",
		Description: "Use this tool for any request that changes code or needs a strong coding model.",
		InputSchema: json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"prompt":{"type":"string"},"dir":{"type":"string"},"purpose":{"type":"string","enum":%s}},"required":["prompt","dir"],"additionalProperties":false}`, purposes)),
	}
}

func (Delegate) Effect() Effect { return DelegateEffect }

func (t Delegate) EffectFor(json.RawMessage) (Effect, error) {
	switch t.Mode {
	case "readonly":
		return DelegateEffect, nil
	case "auto":
		return Write, nil
	default:
		return "", fmt.Errorf("hrn.mode must be readonly or auto")
	}
}

func (t Delegate) task(raw json.RawMessage) (hrnexec.Task, error) {
	var input struct {
		Prompt  string          `json:"prompt"`
		Dir     string          `json:"dir"`
		Purpose json.RawMessage `json:"purpose"`
	}
	if err := decodeInput(raw, &input); err != nil {
		return hrnexec.Task{}, err
	}
	if input.Prompt == "" {
		return hrnexec.Task{}, fmt.Errorf("delegation prompt is required")
	}
	if _, err := t.EffectFor(raw); err != nil {
		return hrnexec.Task{}, err
	}
	// hrn's own routing could pick a metered backend, so settings must name one.
	if strings.TrimSpace(t.Backend) == "" {
		return hrnexec.Task{}, fmt.Errorf("hrn.backend is required for delegation")
	}
	var purpose string
	if input.Purpose != nil {
		if err := json.Unmarshal(input.Purpose, &purpose); err != nil {
			return hrnexec.Task{}, fmt.Errorf("decode delegation purpose: %w", err)
		}
		if !slices.Contains(delegatePurposes, purpose) {
			return hrnexec.Task{}, fmt.Errorf("invalid delegation purpose %q", purpose)
		}
	}
	root, rel, err := withinRoots(input.Dir, t.Roots)
	if err != nil {
		return hrnexec.Task{}, fmt.Errorf("delegate workspace: %w", err)
	}
	dir := filepath.Join(root, rel)
	info, err := os.Stat(dir)
	if err != nil {
		return hrnexec.Task{}, fmt.Errorf("inspect delegate workspace: %w", err)
	}
	if !info.IsDir() {
		return hrnexec.Task{}, fmt.Errorf("delegate workspace must be a directory")
	}
	if t.Timeout <= 0 {
		t.Timeout = DefaultDelegateTimeout
	}
	return hrnexec.Task{
		Prompt: input.Prompt, Dir: dir, Backend: t.Backend, Mode: t.Mode,
		Timeout: t.Timeout.String(), Purpose: purpose,
	}, nil
}

func delegateSummary(task hrnexec.Task) string {
	return fmt.Sprintf("hrn exec backend=%q mode=%q dir=%q timeout=%s purpose=%q prompt=%q",
		task.Backend, task.Mode, task.Dir, task.Timeout, task.Purpose, task.Prompt)
}

func (t Delegate) Summary(raw json.RawMessage) (string, error) {
	task, err := t.task(raw)
	if err != nil {
		return "", err
	}
	return delegateSummary(task), nil
}

func (t Delegate) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	task, err := t.task(raw)
	if err != nil {
		return "", err
	}
	// Resolving the workspace again binds the launch to the directory shown before the wait.
	if t.Mode == "auto" && ctx.Value(approvedSummaryKey{}) != delegateSummary(task) {
		return "", fmt.Errorf("hrn auto mode requires approval for this exact task")
	}
	result, err := hrnexec.Run(ctx, t.Options, task, nil)
	if err != nil {
		return "", fmt.Errorf("delegate to hrn: %w", err)
	}
	if result.Status != "ok" {
		return "", fmt.Errorf("hrn run %s ended with status %s: %s", result.RunID, result.Status, result.Final)
	}
	text := fmt.Sprintf("Status: %s\nRun ID: %s\nBackend: %s\n\n%s", result.Status, result.RunID, result.Backend, result.Final)
	if t.RecordDelegation != nil {
		user, _ := memory.UserFrom(ctx)
		if err := t.RecordDelegation(result.RunID, user, approval.SessionFrom(ctx)); err != nil {
			log.Printf("hrn run %s was not recorded for knowledge review: %v", result.RunID, err)
			text += "\nThis run was not recorded for knowledge review."
		}
	}
	return text, nil
}
