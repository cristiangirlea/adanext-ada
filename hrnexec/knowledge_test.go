package hrnexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func init() {
	if len(os.Args) != 4 || os.Args[1] != "knowledge" || os.Args[2] != "search" || os.Getenv("ADA_TEST_KNOWLEDGE") == "" {
		return
	}
	info, err := os.Stat(os.Args[3])
	if err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		os.Exit(4)
	}
	data, err := os.ReadFile(os.Args[3])
	var fields map[string]string
	if err != nil || json.Unmarshal(data, &fields) != nil || len(fields) != 6 || fields["tenant_id"] != "t" || fields["project_id"] != "p" || fields["backend"] != "b" || fields["model"] != "m" || fields["revision"] != "r" || fields["query"] != "q" || os.Getenv("ADA_TEST_EXCLUDE") != "" {
		fmt.Fprint(os.Stderr, "invalid helper request")
		os.Exit(2)
	}
	if err := os.WriteFile(os.Getenv("ADA_REQUEST_PATH"), []byte(os.Args[3]), 0600); err != nil {
		os.Exit(3)
	}
	packet := `{"version_id":"v","audit_id":"a","entries":[{"key":"k","content":"c","source":"s","excerpt":true}]}`
	switch os.Getenv("ADA_TEST_KNOWLEDGE") {
	case "wrapped":
		fmt.Print(`{"packet":` + packet + "}")
	case "direct":
		fmt.Print(packet)
	case "wait":
		time.Sleep(time.Minute)
	case "unrelated":
		fmt.Print(`{"status":"failed"}`)
	case "malformed":
		fmt.Print(`{"packet":null}`)
	case "large":
		fmt.Print(strings.Repeat(" ", 1<<20) + packet)
	case "fail":
		fmt.Fprint(os.Stderr, "failure "+os.Getenv("HRN_TENANT_TOKEN")+" "+os.Getenv("HRN_SERVER_URL")+" "+strings.Repeat("x", 700))
		os.Exit(1)
	}
	os.Exit(0)
}

func TestCancelRunningKnowledge(t *testing.T) {
	stamp := filepath.Join(t.TempDir(), "path")
	t.Setenv("ADA_REQUEST_PATH", stamp)
	t.Setenv("ADA_TEST_KNOWLEDGE", "wait")
	t.Setenv("ADA_TEST_EXCLUDE", "")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := SearchKnowledge(ctx, Options{HRNPath: os.Args[0]}, KnowledgeRequest{TenantID: "t", ProjectID: "p", Backend: "b", Model: "m", Revision: "r", Query: "q"})
		done <- err
	}()
	for {
		if data, err := os.ReadFile(stamp); err == nil && len(data) > 0 {
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if _, err := os.Stat(string(data)); !os.IsNotExist(err) {
				t.Fatal("request file survived cancellation", err)
			}
			return
		}
		select {
		case err := <-done:
			t.Fatalf("helper exited before ready: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestKnowledgeSubprocess(t *testing.T) {
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	stamp := t.TempDir() + "/path"
	t.Setenv("ADA_REQUEST_PATH", stamp)
	t.Setenv("ADA_TEST_EXCLUDE", "excluded")
	t.Setenv("HRN_TENANT_TOKEN", "synthetic-value")
	t.Setenv("HRN_SERVER_URL", "synthetic-url")
	for _, shape := range []string{"direct", "wrapped", "fail", "large", "malformed", "unrelated"} {
		t.Setenv("ADA_TEST_KNOWLEDGE", shape)
		p, err := SearchKnowledge(t.Context(), Options{HRNPath: path, WithoutEnv: []string{"ADA_TEST_EXCLUDE"}}, KnowledgeRequest{TenantID: "t", ProjectID: "p", Backend: "b", Model: "m", Revision: "r", Query: "q"})
		switch shape {
		case "fail":
			if err == nil || !strings.Contains(err.Error(), "failure") || strings.Contains(err.Error(), "synthetic") || strings.Count(err.Error(), "x") > 512 {
				t.Fatal(err)
			}
		case "large", "malformed", "unrelated":
			if err == nil {
				t.Fatal("accepted invalid output", shape)
			}
		default:
			if err != nil || p.VersionID != "v" || p.AuditID != "a" || len(p.Entries) != 1 || !p.Entries[0].Excerpt {
				t.Fatal(p, err)
			}
		}
		data, err := os.ReadFile(stamp)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(string(data)); !os.IsNotExist(err) {
			t.Fatal("request file survived", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := SearchKnowledge(ctx, Options{}, KnowledgeRequest{}); err == nil {
		t.Fatal("canceled request")
	}
}
