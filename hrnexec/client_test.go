package hrnexec

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRunProtocol(t *testing.T) {
	for _, scenario := range []string{"ok", "rejected", "gap", "repeat", "eof", "bad_version", "cancel", "no_result_after_cancel", "no_exit_after_result", "fatal", "fatal_eof", "wrong_attempt", "early_event"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("ADA_TEST_HRN", scenario)
			timeout := 15 * time.Second
			if scenario == "no_exit_after_result" {
				timeout = time.Second
			}
			ctx, cancel := context.WithTimeout(t.Context(), timeout)
			defer cancel()
			var events []Event
			onEvent := func(e Event) {
				events = append(events, e)
				if scenario == "cancel" || scenario == "no_result_after_cancel" {
					cancel()
				}
			}
			// The delegate tool passes no callback, and must still learn the fatal reason.
			if scenario == "fatal_eof" {
				onEvent = nil
			}
			result, err := Run(ctx, Options{HRNPath: os.Args[0], Grace: time.Second}, Task{Prompt: "do this", Mode: "readonly"}, onEvent)
			switch scenario {
			case "ok", "cancel", "fatal":
				want := "ok"
				if scenario == "cancel" {
					want = "canceled"
				}
				if err != nil || result != (Result{RunID: "run123", Status: want, Final: "finished", Backend: "fake", Model: "model", InputTokens: 11, OutputTokens: 13}) {
					t.Fatalf("Run = %+v, %v", result, err)
				}
				if len(events) != 2 || events[0] != (Event{Kind: "progress", Tool: "fake-tool", Text: "progress update"}) || events[1].Kind != "error" || !strings.Contains(events[1].Text, "retrying") ||
					scenario == "fatal" && !strings.Contains(events[1].Text, "fatal") {
					t.Fatalf("events = %+v", events)
				}
			case "no_result_after_cancel", "no_exit_after_result":
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("expected context error, got %v", err)
				}
			default:
				want := map[string]string{"rejected": "denied", "gap": "seq", "repeat": "seq", "eof": "EOF", "bad_version": "version",
					"wrong_attempt": "invalid hrn accepted", "early_event": "unexpected hrn event",
					"fatal_eof": "fatal error (temporary: retrying)"}[scenario]
				if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "stderr tail marker") || strings.Contains(err.Error(), "stderr head marker") {
					t.Fatalf("Run = %+v, %v, want error containing %q", result, err, want)
				}
				if scenario == "rejected" && !strings.Contains(err.Error(), "request rejected") {
					t.Fatalf("rejection omitted its message: %v", err)
				}
			}
		})
	}
}

func TestRunKillsProcessTree(t *testing.T) {
	t.Setenv("ADA_TEST_HRN", "tree")
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Setenv("ADA_TEST_HRN_PID_FILE", pidFile)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := Run(ctx, Options{HRNPath: os.Args[0], Grace: 100 * time.Millisecond},
		Task{Prompt: "do this", Mode: "readonly"}, func(e Event) {
			if e.Kind == "progress" {
				cancel()
			}
		})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil || pid <= 0 {
		t.Fatalf("grandchild PID = %q", data)
	}
	deadline := time.Now().Add(5 * time.Second)
	for isProcessRunning(t, pid) {
		if time.Now().After(deadline) {
			if child, err := os.FindProcess(pid); err == nil {
				_ = child.Kill()
				_ = child.Release()
			}
			t.Fatalf("grandchild %d is still running", pid)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestRunFiltersEnvironment(t *testing.T) {
	t.Setenv("ADA_TEST_HRN", "filtered_env")
	t.Setenv("ADA_TEST_HRN_FIRST_KEY", "fake credential")
	t.Setenv("ADA_TEST_HRN_SECOND_KEY", "fake credential")
	t.Setenv("ADA_TEST_HRN_KEEP", "keep")
	opts := Options{HRNPath: os.Args[0], WithoutEnv: []string{"ADA_TEST_HRN_FIRST_KEY", "ADA_TEST_HRN_SECOND_KEY"}}
	result, err := Run(t.Context(), opts, Task{Prompt: "do this", Mode: "readonly"}, nil)
	if err != nil || result.Status != "ok" {
		t.Fatalf("Run = %+v, %v", result, err)
	}
}

func TestMain(m *testing.M) {
	if os.Getenv("ADA_TEST_HRN") == "grandchild" {
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	if scenario := os.Getenv("ADA_TEST_HRN"); scenario != "" {
		runHelperProcess(scenario)
	}
	os.Exit(m.Run())
}

func runHelperProcess(scenario string) {
	if !slices.Equal(os.Args[1:], []string{"exec", "--protocol", "v1"}) {
		fmt.Fprintln(os.Stderr, "bad hrn command")
		os.Exit(2)
	}
	fmt.Println(`{"v":1,"seq":1,"type":"hello","protocol":"v1","hrn":{"version":"0.0.0"},"extra":true}`)
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		os.Exit(2)
	}
	var start struct {
		Type      string `json:"type"`
		AttemptID string `json:"attempt_id"`
		Task      map[string]string
	}
	if json.Unmarshal(scanner.Bytes(), &start) != nil || start.Type != "start" || !strings.HasPrefix(start.AttemptID, "ada-") ||
		len(start.Task) != 2 || start.Task["mode"] != "readonly" || start.Task["prompt"] != "do this" {
		fmt.Fprintln(os.Stderr, "bad start message")
		os.Exit(2)
	}
	switch scenario {
	case "rejected", "gap", "repeat", "bad_version", "eof", "wrong_attempt", "early_event", "fatal_eof":
		fmt.Fprint(os.Stderr, "stderr head marker"+strings.Repeat("x", 5000)+"stderr tail marker")
	}
	switch scenario {
	case "filtered_env":
		_, first := os.LookupEnv("ADA_TEST_HRN_FIRST_KEY")
		_, second := os.LookupEnv("ADA_TEST_HRN_SECOND_KEY")
		if first || second || os.Getenv("ADA_TEST_HRN_KEEP") != "keep" {
			fmt.Fprintln(os.Stderr, "unexpected hrn environment")
			os.Exit(2)
		}
	case "rejected":
		fmt.Printf(`{"v":1,"seq":2,"type":"rejected","attempt_id":%q,"error":{"code":"denied","message":"request rejected"}}`+"\n", start.AttemptID)
		os.Exit(0)
	case "gap":
		fmt.Println(`{"v":1,"seq":3,"type":"accepted"}`)
		os.Exit(0)
	case "repeat":
		fmt.Println(`{"v":1,"seq":1,"type":"accepted"}`)
		os.Exit(0)
	case "bad_version":
		fmt.Println(`{"v":2,"seq":2,"type":"accepted"}`)
		os.Exit(0)
	case "eof":
		os.Exit(0)
	case "wrong_attempt":
		fmt.Println(`{"v":1,"seq":2,"type":"accepted","attempt_id":"different","run_id":"run123"}`)
		os.Exit(0)
	case "early_event":
		fmt.Printf(`{"v":1,"seq":2,"type":"event","attempt_id":%q,"run_id":"run123"}`+"\n", start.AttemptID)
		os.Exit(0)
	case "tree":
		child := exec.Command(os.Args[0])
		child.Env = append(child.Environ(), "ADA_TEST_HRN=grandchild")
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if err := os.WriteFile(os.Getenv("ADA_TEST_HRN_PID_FILE"), []byte(strconv.Itoa(child.Process.Pid)), 0600); err != nil {
			_ = child.Process.Kill()
			os.Exit(2)
		}
	}

	fmt.Printf(`{"v":1,"seq":2,"type":"accepted","attempt_id":%q,"run_id":"run123"}`+"\n", start.AttemptID)
	fmt.Printf(`{"v":1,"seq":3,"type":"event","attempt_id":%q,"run_id":"run123","event":{"kind":"progress","tool":"fake-tool","text":"progress update"}}`+"\n", start.AttemptID)
	fmt.Printf(`{"v":1,"seq":4,"type":"error","error":{"code":"temporary","message":"retrying"},"fatal":%t}`+"\n", scenario == "fatal" || scenario == "fatal_eof")
	if scenario == "fatal_eof" {
		os.Exit(0)
	}
	status := "ok"
	if scenario == "cancel" || scenario == "no_result_after_cancel" || scenario == "tree" {
		var cancel map[string]string
		if !scanner.Scan() || json.Unmarshal(scanner.Bytes(), &cancel) != nil || cancel["type"] != "cancel" || cancel["attempt_id"] != start.AttemptID {
			os.Exit(2)
		}
		status = "canceled"
		if scenario != "cancel" {
			time.Sleep(30 * time.Second)
		}
	}
	fmt.Printf(`{"v":1,"seq":5,"type":"result","attempt_id":%q,"run_id":"run123","result":{"backend":"fake","model":"model","status":%q,"final":"finished","input_tokens":11,"output_tokens":13}}`+"\n", start.AttemptID, status)
	if scenario == "no_exit_after_result" {
		time.Sleep(30 * time.Second)
	}
	if scenario == "cancel" {
		os.Exit(1)
	}
	os.Exit(0)
}
