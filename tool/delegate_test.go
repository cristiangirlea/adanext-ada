package tool_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cristiangirlea/adanext-ada/hrnexec"
	"github.com/cristiangirlea/adanext-ada/loop"
	"github.com/cristiangirlea/adanext-ada/provider"
	"github.com/cristiangirlea/adanext-ada/tool"
)

type codingProvider struct {
	input json.RawMessage
}

func (codingProvider) Name() string { return "fake" }
func (p codingProvider) Complete(_ context.Context, req provider.Request) (provider.Response, error) {
	last := req.Messages[len(req.Messages)-1]
	if last.Content[0].Type == "tool_result" {
		return provider.Response{Content: []provider.Block{{Type: "text", Text: last.Content[0].Text}}}, nil
	}
	return provider.Response{Content: []provider.Block{{Type: "tool_use", Name: "delegate_to_hrn", ID: "coding", Input: p.input}}}, nil
}

func TestCodingDelegation(t *testing.T) {
	t.Setenv("ADA_TEST_DELEGATE", "1")
	root := t.TempDir()
	r := new(tool.Registry)
	if err := r.Add(tool.Delegate{Roots: []string{root}, Options: hrnexec.Options{HRNPath: os.Args[0]}, Mode: "readonly", Backend: "configured"}); err != nil {
		t.Fatal(err)
	}
	for _, override := range []string{"", "mode", "backend", "failed"} {
		t.Run("delegation "+override, func(t *testing.T) {
			input := map[string]string{"prompt": "change the code", "dir": root, "purpose": "implement"}
			switch override {
			case "mode", "backend":
				input[override] = "override"
			case "failed":
				t.Setenv("ADA_TEST_DELEGATE", "failed")
			}
			raw, _ := json.Marshal(input)
			l := loop.Loop{Provider: codingProvider{raw}, Tools: r}
			answer, history, err := l.Turn(t.Context(), nil, "Update this repo")
			if err != nil || len(history) != 4 {
				t.Fatalf("Turn = %q, %v, %v", answer, history, err)
			}
			result := history[2].Content[0]
			if override == "failed" {
				if !result.IsError || !strings.Contains(answer, "failed") || !strings.Contains(answer, "succeeded-run") || !strings.Contains(answer, "readonly code analysis") {
					t.Fatalf("failed delegation result = %+v", result)
				}
			} else if override != "" {
				if !result.IsError || !strings.Contains(answer, "unknown field") {
					t.Fatalf("override result = %+v", result)
				}
			} else {
				if result.IsError {
					t.Fatalf("delegation failed: %s", answer)
				}
				for _, want := range []string{"readonly code analysis", "succeeded-run", "configured", "ok"} {
					if !strings.Contains(answer, want) {
						t.Errorf("answer %q does not contain %q", answer, want)
					}
				}
			}
		})
	}
}

func TestMain(m *testing.M) {
	if os.Getenv("ADA_TEST_DELEGATE") != "" {
		runDelegateHelper()
	}
	os.Exit(m.Run())
}

func runDelegateHelper() {
	fmt.Println(`{"v":1,"seq":1,"type":"hello","protocol":"v1"}`)
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		os.Exit(2)
	}
	var start struct {
		AttemptID string `json:"attempt_id"`
		Task      map[string]string
	}
	if json.Unmarshal(scanner.Bytes(), &start) != nil || start.Task["mode"] != "readonly" ||
		start.Task["timeout"] != "30m0s" || start.Task["backend"] != "configured" ||
		start.Task["purpose"] != "implement" || start.Task["prompt"] != "change the code" || start.Task["dir"] == "" {
		fmt.Fprintln(os.Stderr, "unexpected delegation task")
		os.Exit(2)
	}
	fmt.Printf(`{"v":1,"seq":2,"type":"accepted","attempt_id":%q,"run_id":"succeeded-run"}`+"\n", start.AttemptID)
	status := "ok"
	if os.Getenv("ADA_TEST_DELEGATE") == "failed" {
		status = "failed"
	}
	fmt.Printf(`{"v":1,"seq":3,"type":"result","attempt_id":%q,"run_id":"succeeded-run","result":{"status":%q,"backend":"configured","final":"readonly code analysis"}}`+"\n", start.AttemptID, status)
	os.Exit(0)
}

func TestDelegateSchema(t *testing.T) {
	var schema struct {
		Properties map[string]struct{ Enum []string }
	}
	if err := json.Unmarshal((tool.Delegate{}).Spec().InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if _, ok := schema.Properties["backend"]; ok {
		t.Error("schema includes model-controlled backend")
	}
	if got := schema.Properties["purpose"].Enum; !slices.Equal(got, []string{"review", "debug", "implement", "refactor", "test", "research", "plan", "ops"}) {
		t.Errorf("purpose enum = %v", got)
	}
}

func TestDelegateRefusals(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	for _, tc := range []struct{ name, dir, mode, purpose, want string }{
		{"auto", root, "auto", "review", "readonly"},
		{"unset mode", root, "", "review", "readonly"},
		{"invalid purpose", root, "readonly", "coding", "purpose"},
		{"empty purpose", root, "readonly", "", "purpose"},
		{"outside", outside, "readonly", "review", "outside the configured roots"},
		{"symlink", filepath.Join(root, "link"), "readonly", "review", "outside the configured roots"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "symlink" {
				if err := os.Symlink(outside, tc.dir); err != nil {
					t.Skip(err)
				}
			}
			d := tool.Delegate{Roots: []string{root}, Mode: tc.mode, Backend: "configured", Options: hrnexec.Options{HRNPath: filepath.Join(root, "must-not-run")}}
			input, _ := json.Marshal(map[string]string{"prompt": "review", "dir": tc.dir, "purpose": tc.purpose})
			if _, err := d.Run(t.Context(), input); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Run = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestDelegateRequiresConfiguredBackend(t *testing.T) {
	root := t.TempDir()
	d := tool.Delegate{Roots: []string{root}, Mode: "readonly", Options: hrnexec.Options{HRNPath: filepath.Join(root, "must-not-run")}}
	input, _ := json.Marshal(map[string]string{"prompt": "review", "dir": root, "purpose": "review"})
	if _, err := d.Run(t.Context(), input); err == nil || !strings.Contains(err.Error(), "hrn.backend is required") {
		t.Fatalf("Run = %v", err)
	}
}
