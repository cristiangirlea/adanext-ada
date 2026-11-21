package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cristiangirlea/adanext-ada/hrnexec"
	"github.com/cristiangirlea/adanext-ada/provider"
)

const DefaultDelegateTimeout = 30 * time.Minute

var delegatePurposes = []string{"review", "debug", "implement", "refactor", "test", "research", "plan", "ops"}

type Delegate struct {
	Roots   []string
	Options hrnexec.Options
	Mode    string
	Timeout time.Duration
	Backend string
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

func (t Delegate) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	var input struct {
		Prompt  string          `json:"prompt"`
		Dir     string          `json:"dir"`
		Purpose json.RawMessage `json:"purpose"`
	}
	if err := decodeInput(raw, &input); err != nil {
		return "", err
	}
	if input.Prompt == "" {
		return "", fmt.Errorf("delegation prompt is required")
	}
	if t.Mode != "readonly" {
		return "", fmt.Errorf("configured hrn mode must be readonly because writes wait for the approvals slice")
	}
	// hrn's own routing could pick a metered backend, so settings must name one.
	if strings.TrimSpace(t.Backend) == "" {
		return "", fmt.Errorf("hrn.backend is required for delegation")
	}
	var purpose string
	if input.Purpose != nil {
		if err := json.Unmarshal(input.Purpose, &purpose); err != nil {
			return "", fmt.Errorf("decode delegation purpose: %w", err)
		}
		if !slices.Contains(delegatePurposes, purpose) {
			return "", fmt.Errorf("invalid delegation purpose %q", purpose)
		}
	}
	root, rel, err := withinRoots(input.Dir, t.Roots)
	if err != nil {
		return "", fmt.Errorf("delegate workspace: %w", err)
	}
	dir := filepath.Join(root, rel)
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("inspect delegate workspace: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("delegate workspace must be a directory")
	}
	if t.Timeout <= 0 {
		t.Timeout = DefaultDelegateTimeout
	}
	result, err := hrnexec.Run(ctx, t.Options, hrnexec.Task{
		Prompt: input.Prompt, Dir: dir, Backend: t.Backend, Mode: t.Mode,
		Timeout: t.Timeout.String(), Purpose: purpose,
	}, nil)
	if err != nil {
		return "", fmt.Errorf("delegate to hrn: %w", err)
	}
	if result.Status != "ok" {
		return "", fmt.Errorf("hrn run %s ended with status %s: %s", result.RunID, result.Status, result.Final)
	}
	return fmt.Sprintf("Status: %s\nRun ID: %s\nBackend: %s\n\n%s", result.Status, result.RunID, result.Backend, result.Final), nil
}
