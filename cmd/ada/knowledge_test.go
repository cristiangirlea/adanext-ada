package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/adanext-ada/approval"
	"github.com/cristiangirlea/adanext-ada/config"
	"github.com/cristiangirlea/adanext-ada/learn"
	"github.com/cristiangirlea/adanext-ada/memory"
)

const firstRun = "20261123-120000-abcdef"
const secondRun = "20261123-120001-012345"
const reviewRun = "20261123-130000-112233"

func learned(run, slug string) string { return "learned/" + run + "/" + slug }

func init() {
	if os.Getenv("ADA_KNOWLEDGE_HELPER") == "" || len(os.Args) < 3 {
		return
	}
	if os.Args[1] == "exec" {
		fmt.Println(`{"v":1,"seq":1,"type":"hello","protocol":"v1"}`)
		var start struct {
			Attempt string `json:"attempt_id"`
		}
		if json.NewDecoder(os.Stdin).Decode(&start) != nil {
			os.Exit(2)
		}
		fmt.Printf(`{"v":1,"seq":2,"type":"accepted","attempt_id":%q,"run_id":%q}`+"\n", start.Attempt, firstRun)
		fmt.Printf(`{"v":1,"seq":3,"type":"result","attempt_id":%q,"run_id":%q,"result":{"status":%q,"final":"%s"}}`+"\n", start.Attempt, firstRun, os.Getenv("ADA_RUN_STATUS"), secondRun)
		os.Exit(0)
	}
	if os.Args[1] != "knowledge" {
		return
	}
	if err := fakeKnowledge(os.Args[2:]); err != nil {
		fmt.Fprint(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func fakeKnowledge(args []string) error {
	path := args[1]
	if args[0] == "propose" {
		if len(args) < 9 || args[1] != "-request" || args[3] != "-b" || args[4] != "codex" || args[len(args)-2] != "-o" {
			return fmt.Errorf("unexpected propose arguments %v", args)
		}
		path = args[2]
	} else if args[0] != "inspect" || len(args) != 2 {
		return fmt.Errorf("unexpected knowledge command")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		return fmt.Errorf("request permissions")
	}
	if os.Getenv("HRN_OPERATOR_TOKEN") != "" || os.Getenv("ADA_TEST_PROVIDER_KEY") != "" || os.Getenv("HRN_SERVER_URL") != "fake-url" || os.Getenv("HRN_TENANT_TOKEN") != "fake-tenant" {
		return fmt.Errorf("incorrect environment")
	}
	if err := os.WriteFile(os.Getenv("ADA_REQUEST_STAMP"), []byte(path), 0600); err != nil {
		return err
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	for field, want := range map[string]string{"tenant_id": "t", "project_id": "p", "backend": "scope", "model": "m", "revision": "r"} {
		if obj[field] != want {
			return fmt.Errorf("incorrect %s", field)
		}
	}
	mode := os.Getenv("ADA_KNOWLEDGE_HELPER")
	if args[0] == "inspect" {
		if len(obj) != 6 || obj["version_id"] != "v1" {
			return fmt.Errorf("incorrect inspect request")
		}
		delete(obj, "version_id")
		obj["id"] = "v1"
		if mode != "unsigned" {
			obj["admission"] = map[string]string{"verdict": mode}
		}
		obj["entries"] = []map[string]string{{"key": learned(firstRun, "a")}, {"key": learned(secondRun, "b")}}
		if mode == "wrong-id" || mode == "wrong-project" {
			obj["admission"] = map[string]string{"verdict": "admitted"}
			if mode == "wrong-id" {
				obj["id"] = "v2"
			} else {
				obj["project_id"] = "other"
			}
		}
		if mode == "missing" || mode == "missing-last" {
			obj["admission"] = map[string]string{"verdict": "admitted"}
			key := learned(secondRun, "b")
			if mode == "missing-last" {
				key = learned(firstRun, "a")
			}
			obj["entries"] = []map[string]string{{"key": key}}
		}
		return json.NewEncoder(os.Stdout).Encode(obj)
	}
	if len(obj) != 9 || obj["origin"] != "" || obj["reviewed_by"] != "" || obj["review_evidence"] != "" {
		return fmt.Errorf("incorrect base request")
	}
	if entries, ok := obj["entries"].([]any); !ok || len(entries) != 0 {
		return fmt.Errorf("base entries")
	}
	runs := strings.Fields(os.Getenv("ADA_EXPECT_RUNS"))
	var want []string
	for _, id := range runs {
		want = append(want, "-run", id)
	}
	want = append(want, "-json")
	if !slices.Equal(want, args[5:len(args)-2]) {
		return fmt.Errorf("unexpected runs %v", args[5:len(args)-2])
	}
	output := args[len(args)-1]
	summary := map[string]any{
		"reviewed": runs, "skipped": []any{}, "deferred": []any{}, "failed": []any{},
		"draft": "", "proposed": 0, "kept": 0,
	}
	reason := func(id string) map[string]string { return map[string]string{"run": id, "reason": "test reason"} }
	failure := func(id string) map[string]string {
		return map[string]string{"run": id, "review_run": reviewRun, "error": "test failure"}
	}
	exit := 0
	switch mode {
	case "wait":
		time.Sleep(30 * time.Second)
		return fmt.Errorf("review timeout did not fire")
	case "fail":
		return fmt.Errorf("review failed %s%s", os.Getenv("HRN_TENANT_TOKEN"), strings.Repeat("x", 900))
	case "raw-report":
		fmt.Print(os.Getenv("ADA_FAKE_REPORT"))
		return nil
	case "overflow":
		fmt.Print(strings.Repeat(" ", 1<<20))
		fallthrough
	case "none":
		return json.NewEncoder(os.Stdout).Encode(summary)
	case "deferred":
		summary["reviewed"], summary["deferred"] = []string{}, []any{reason(runs[0])}
		return json.NewEncoder(os.Stdout).Encode(summary)
	case "failed-reviews":
		var failures []map[string]string
		for _, run := range runs {
			failures = append(failures, failure(run))
		}
		summary["reviewed"], summary["failed"] = []string{}, failures
		if err := json.NewEncoder(os.Stdout).Encode(summary); err != nil {
			return err
		}
		os.Exit(3)
	case "mixed", "refused-mixed":
		summary["reviewed"] = runs[:1]
		summary["skipped"], summary["deferred"], summary["failed"] = []any{reason(runs[1])}, []any{reason(runs[2])}, []any{failure(runs[3])}
		runs, exit = runs[:1], 3
	case "wrong-scope":
		obj["revision"] = "different"
	case "signed":
		obj["origin"] = "reviewed_source"
	}
	entries := []map[string]string{{"key": "existing"}, {"key": learned("20261101-120000-abcdef", "active")}}
	if mode != "active-only" {
		entries = append(entries, map[string]string{"key": learned(runs[0], "a")}, map[string]string{"key": learned(runs[len(runs)-1], "b")})
	}
	obj["entries"] = entries
	data, err = json.Marshal(obj)
	if err != nil {
		return err
	}
	if mode == "malformed" || mode == "refused-mixed" {
		data = []byte("{")
	}
	if mode == "limit" || mode == "over-limit" {
		data = append(data, bytes.Repeat([]byte(" "), (8<<20)-len(data))...)
		if mode == "over-limit" {
			data = append(data, ' ')
		}
	}
	if mode == "nonregular" {
		err = os.Mkdir(output, 0700)
	} else {
		err = os.WriteFile(output, data, 0600)
	}
	if err != nil {
		return err
	}
	summary["draft"], summary["proposed"], summary["kept"] = output, 2, 2
	if mode == "wrong-draft-path" {
		summary["draft"] = output + ".other"
	}
	if mode == "unreported" {
		summary["draft"], summary["proposed"], summary["kept"] = "", 0, 0
	}
	if err := json.NewEncoder(os.Stdout).Encode(summary); err != nil {
		return err
	}
	if exit != 0 {
		os.Exit(exit)
	}
	return nil
}

type knowledgeCLI struct {
	dir, path, stamp string
	settings         map[string]any
}

func newKnowledgeCLI(t *testing.T) *knowledgeCLI {
	t.Helper()
	dir := t.TempDir()
	k := &knowledgeCLI{dir: dir, path: filepath.Join(dir, "config.json"), stamp: filepath.Join(dir, "request"), settings: map[string]any{
		"data_dir": dir, "owner": "maintainer", "chat_provider": "local",
		"providers": []map[string]string{{"kind": "openai", "name": "local", "model": "local", "base_url": "http://localhost", "api_key_env": "ADA_TEST_PROVIDER_KEY"}},
		"hrn":       map[string]string{"path": os.Args[0], "backend": "codex"},
		"knowledge": map[string]string{"tenant_id": "t", "project_id": "p", "backend": "scope", "model": "m", "revision": "r"},
	}}
	t.Setenv("ADA_KNOWLEDGE_HELPER", "draft")
	t.Setenv("ADA_EXPECT_RUNS", firstRun+" "+secondRun)
	t.Setenv("ADA_REQUEST_STAMP", k.stamp)
	t.Setenv("HRN_TENANT_TOKEN", "fake-tenant")
	t.Setenv("HRN_OPERATOR_TOKEN", "fake-operator")
	t.Setenv("HRN_SERVER_URL", "fake-url")
	t.Setenv("ADA_TEST_PROVIDER_KEY", "filtered")
	if err := os.Mkdir(filepath.Join(dir, "learning"), 0700); err != nil {
		t.Fatal(err)
	}
	return k
}

func (k *knowledgeCLI) journal(t *testing.T, ids ...string) {
	if err := os.MkdirAll(filepath.Join(k.dir, "learning"), 0700); err != nil {
		t.Fatal(err)
	}
	var lines strings.Builder
	for i, id := range ids {
		fmt.Fprintf(&lines, `{"run_id":%q,"user":"other","session":"abc","time":"2026-11-20T12:00:%02dZ"}`+"\n", id, i)
	}
	lines.WriteString(`{"run_id":`)
	if err := os.WriteFile(filepath.Join(k.dir, "learning", "delegations.jsonl"), []byte(lines.String()), 0600); err != nil {
		t.Fatal(err)
	}
}

func (k *knowledgeCLI) invoke(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	data, err := json.Marshal(k.settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(k.path, data, 0600); err != nil {
		t.Fatal(err)
	}
	var out, diag bytes.Buffer
	err = run(t.Context(), append([]string{"learn", args[0], "-config", k.path}, args[1:]...), strings.NewReader(""), &out, &diag)
	return out.String(), diag.String(), err
}

func TestKnowledgeReviewCLI(t *testing.T) {
	k := newKnowledgeCLI(t)
	k.journal(t, firstRun, secondRun)
	for _, mode := range []string{"fail", "malformed", "wrong-scope", "signed", "active-only", "nonregular", "draft"} {
		t.Setenv("ADA_KNOWLEDGE_HELPER", mode)
		out, _, err := k.invoke(t, "propose")
		if mode != "draft" {
			files, readErr := os.ReadDir(filepath.Join(k.dir, "learning", "knowledge"))
			if readErr != nil || len(files) != 0 {
				t.Fatal("invalid draft survived", files, readErr)
			}
			if err == nil || out != "" {
				t.Fatal(mode, out, err)
			}
			if mode == "fail" && (!strings.Contains(err.Error(), "review failed") || strings.Contains(err.Error(), "fake-tenant") || len(err.Error()) > 700) {
				t.Fatal(err)
			}
			continue
		}
		if err != nil || !strings.Contains(out, "2 knowledge entries from runs "+firstRun) || !strings.Contains(out, secondRun) {
			t.Fatal(out, err)
		}
	}
	path, err := os.ReadFile(k.stamp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(string(path)); !os.IsNotExist(err) {
		t.Fatal("temporary request survived", err)
	}
	m, err := memory.NewStore(k.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	store := learn.Store{Memory: m}
	items, err := store.List("maintainer", false)
	if err != nil || len(items) != 1 {
		t.Fatal(items, err)
	}
	p := items[0]
	draft := "learning/knowledge/" + p.ID + ".json"
	if p.Kind != "knowledge" || p.Outside || p.Draft != draft || !slices.Equal(p.SourceRuns, []string{firstRun, secondRun}) {
		t.Fatal(p)
	}
	if err := m.Link("echo", "sender", "maintainer"); err != nil {
		t.Fatal(err)
	}
	offer := learn.Offer{Channel: "echo", Chat: "private", Sender: "sender", DeliveryID: "id", Expires: time.Now().Add(time.Hour)}
	if sent, err := store.BindOffer("maintainer", p.ID, offer); err != nil || sent {
		t.Fatal("bound chat offer to project knowledge", sent, err)
	}
	if out, _, err := k.invoke(t, "list"); err != nil || !strings.Contains(out, draft) {
		t.Fatal(out, err)
	}
	if out, _, err := k.invoke(t, "propose"); out != "" || err != nil {
		t.Fatal(out, err)
	}
	if _, _, err := k.invoke(t, "keep", p.ID); err == nil || !strings.Contains(err.Error(), "hrn knowledge import") || !strings.Contains(err.Error(), "hrn knowledge admit") || !strings.Contains(err.Error(), "-version") {
		t.Fatal(err)
	}
	signed, err := os.ReadFile(filepath.Join(k.dir, draft))
	if err != nil {
		t.Fatal(err)
	}
	signed = bytes.Replace(signed, []byte(`"origin":""`), []byte(`"origin":"reviewed_source"`), 1)
	if err := os.WriteFile(filepath.Join(k.dir, draft), signed, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ mode, reason string }{
		{"unsigned", "not admitted"}, {"rejected", "rejected"},
		{"missing", learned(firstRun, "a")}, {"missing-last", learned(secondRun, "b")},
		{"wrong-id", "requested version and scope"}, {"wrong-project", "requested version and scope"},
		{"admitted", ""},
	} {
		t.Setenv("ADA_KNOWLEDGE_HELPER", tc.mode)
		_, _, err := k.invoke(t, "keep", p.ID, "-version", "v1")
		if tc.reason != "" && (err == nil || !strings.Contains(err.Error(), tc.reason)) || tc.reason == "" && err != nil {
			t.Fatal(tc, err)
		}
	}
	for _, action := range []string{"confirm", "restore"} {
		if _, _, err := k.invoke(t, action, p.ID); err == nil {
			t.Fatalf("%s accepted knowledge", action)
		}
	}
	if changed, err := store.Sweep("maintainer", time.Now().Add(365*24*time.Hour), time.Hour, time.Hour); err != nil || len(changed) != 0 {
		t.Fatal(changed, err)
	}
	if out, _, err := k.invoke(t, "rollback", p.ID); err != nil || !strings.Contains(out, "hrn knowledge rollback") {
		t.Fatal(out, err)
	}
	items, err = store.List("maintainer", true)
	if err != nil || len(items) != 1 || items[0].Status != "rolled_back" || items[0].Version != "v1" || len(m.Facts("maintainer")) != 0 {
		t.Fatal(items, err)
	}
}

func TestKnowledgeEmptyBatchAndReject(t *testing.T) {
	k := newKnowledgeCLI(t)
	k.settings["hrn"].(map[string]string)["timeout"] = "2h"
	var ids []string
	for i := range 11 {
		ids = append(ids, fmt.Sprintf("20261123-120000-%06x", i))
	}
	k.journal(t, ids...)
	t.Setenv("ADA_EXPECT_RUNS", strings.Join(ids[:10], " "))
	t.Setenv("ADA_KNOWLEDGE_HELPER", "none")
	if out, _, err := k.invoke(t, "propose"); err != nil || out != "" {
		t.Fatal(out, err)
	}
	t.Setenv("ADA_EXPECT_RUNS", ids[10])
	t.Setenv("ADA_KNOWLEDGE_HELPER", "draft")
	out, _, err := k.invoke(t, "propose")
	if err != nil || !strings.Contains(out, ids[10]) {
		t.Fatal(out, err)
	}
	id := strings.Fields(out)[0]
	if _, _, err := k.invoke(t, "reject", id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(k.dir, "learning", "knowledge", id+".json")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if out, _, err := k.invoke(t, "propose"); err != nil || out != "" {
		t.Fatal(out, err)
	}
}

func TestKnowledgeDisabled(t *testing.T) {
	k := newKnowledgeCLI(t)
	k.journal(t, firstRun)
	for _, backend := range []string{"api", "", "null", "absent-scope"} {
		k.settings["learning"] = map[string]string{"knowledge_backend": backend}
		if backend == "null" {
			k.settings["learning"] = map[string]any{"knowledge_backend": nil}
		}
		if backend == "absent-scope" {
			delete(k.settings, "knowledge")
			delete(k.settings, "learning")
		}
		out, diag, err := k.invoke(t, "propose")
		if err != nil || out != "" || !strings.Contains(diag, "knowledge") || strings.Count(diag, "\n") != 1 {
			t.Fatal(out, diag, err)
		}
		if _, err := os.Stat(k.stamp); !os.IsNotExist(err) {
			t.Fatal("disabled review launched hrn", err)
		}
	}
}

func TestOnlySuccessfulDelegateJournalsRun(t *testing.T) {
	k := newKnowledgeCLI(t)
	k.settings["workspace_roots"] = []string{k.dir}
	k.invoke(t, "list")
	settings, err := config.Load(k.path)
	if err != nil {
		t.Fatal(err)
	}
	app, err := config.Build(settings, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Tools.Close()
	defer app.Store.Close()
	ctx := approval.WithSession(memory.WithUser(t.Context(), "other"), "abc")
	raw, _ := json.Marshal(map[string]string{"prompt": "mention " + secondRun, "dir": k.dir})
	path := filepath.Join(k.dir, "learning", "delegations.jsonl")
	t.Setenv("ADA_RUN_STATUS", "failed")
	if _, err := app.Tools.Run(ctx, "delegate_to_hrn", raw); err == nil {
		t.Fatal("failed run succeeded")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("failed run journaled", err)
	}
	if err := os.WriteFile(path, []byte(`{"run_id":"torn`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADA_RUN_STATUS", "ok")
	normal, err := app.Tools.Run(ctx, "delegate_to_hrn", raw)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	var row struct {
		Run           string `json:"run_id"`
		User, Session string
		Time          time.Time
	}
	if err != nil || json.Unmarshal(data, &row) != nil || row.Run != firstRun || row.User != "other" || row.Session != "abc" || row.Time.IsZero() || !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatal(string(data), err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	var diag bytes.Buffer
	writer := log.Writer()
	log.SetOutput(&diag)
	t.Cleanup(func() { log.SetOutput(writer) })
	out, err := app.Tools.Run(ctx, "delegate_to_hrn", raw)
	note, ok := strings.CutPrefix(out, normal+"\n")
	if err != nil || !ok || strings.Contains(note, "\n") || !strings.Contains(note, "not recorded for knowledge review") || !strings.Contains(diag.String(), firstRun) {
		t.Fatalf("result %q, diagnostic %q, error %v", out, diag.String(), err)
	}
}

func newKnowledgeServe(t *testing.T) (*knowledgeCLI, *learningChat) {
	t.Helper()
	k := newKnowledgeCLI(t)
	c := newLearningChat(t, false)
	k.dir = c.dir
	k.settings["data_dir"] = c.dir
	c.settings.Owner = "maintainer"
	k.journal(t, firstRun, secondRun)
	c.settings.HRN.Path = os.Args[0]
	c.settings.HRN.Backend = "codex"
	t.Setenv("ADA_TEST_PROVIDER_KEY", "")
	c.settings.Knowledge = &config.KnowledgeSettings{TenantID: "t", ProjectID: "p", Backend: "scope", Model: "m", Revision: "r"}
	return k, c
}

func TestServeReviewsKnowledgeOnceForProject(t *testing.T) {
	_, c := newKnowledgeServe(t)
	lines := make(logLines, 32)
	c.startLearning(lines)
	store := learn.Store{Memory: c.app.Store}
	for until := time.Now().Add(4 * time.Second); time.Now().Before(until); {
		items, err := store.List(c.settings.Owner, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) > 0 {
			if len(items) != 1 || items[0].Kind != "knowledge" || items[0].Offer != nil {
				t.Fatal(items)
			}
			lines.wait(t, "pending proposal "+items[0].ID)
			// The same draft spans several sweeps.
			done := time.After(5 * c.settings.Learning.ProposeEvery)
			for {
				select {
				case line := <-lines:
					if strings.Contains(line, "pending proposal "+items[0].ID) {
						t.Fatalf("repeated diagnostic: %s", line)
					}
				case <-done:
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("serve did not propose knowledge")
}

func TestKnowledgeBackoffBeyondCapIsDue(t *testing.T) {
	k, c := newKnowledgeServe(t)
	state := `{"proposals":[],"reviewed":{},"knowledge_backoff":{"failures":1,"next":"9999-12-31T23:59:59Z"}}`
	if err := os.WriteFile(filepath.Join(k.dir, "learning", "maintainer.json"), []byte(state), 0600); err != nil {
		t.Fatal(err)
	}
	lines := make(logLines, 256)
	c.startLearning(lines)
	lines.wait(t, "pending proposal")
	if _, err := os.Stat(k.stamp); err != nil {
		t.Fatal("expected hrn to start", err)
	}
	if k.ownerState(t)["knowledge_backoff"] != nil {
		t.Fatal("successful sweep retained back-off")
	}
}

func TestKnowledgeManualRetryDuringSweepBackoff(t *testing.T) {
	k, c := newKnowledgeServe(t)
	t.Setenv("ADA_KNOWLEDGE_HELPER", "fail")
	_, diag, err := k.invoke(t, "propose")
	if err == nil {
		t.Fatal("expected outage")
	}
	k.checkBackoff(t, 1, diag)
	if err := os.Remove(k.stamp); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADA_KNOWLEDGE_HELPER", "draft")
	lines := make(logLines, 32)
	c.startLearning(lines)
	lines.wait(t, "next knowledge review attempt")
	if _, err := os.Stat(k.stamp); !os.IsNotExist(err) {
		t.Fatal("sweep started hrn during back-off", err)
	}
	out, diag, err := k.invoke(t, "propose")
	if err != nil || !strings.Contains(out, firstRun) || k.ownerState(t)["knowledge_backoff"] != nil {
		t.Fatal("expected manual retry to succeed and clear back-off", out, diag, err)
	}
}

func TestKnowledgeCallerCancellationLeavesRunsPending(t *testing.T) {
	k := newKnowledgeCLI(t)
	k.journal(t, firstRun, secondRun)
	if _, _, err := k.invoke(t, "list"); err != nil {
		t.Fatal(err)
	}
	state := `{"proposals":[],"reviewed":{}}`
	statePath := filepath.Join(k.dir, "learning", "maintainer.json")
	if err := os.WriteFile(statePath, []byte(state), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADA_KNOWLEDGE_HELPER", "wait")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- learnCommand(ctx, []string{"propose"}, k.path, io.Discard, io.Discard)
	}()
	for until := time.Now().Add(5 * time.Second); ; {
		if _, err := os.Stat(k.stamp); err == nil {
			break
		} else if !os.IsNotExist(err) || time.Now().After(until) {
			t.Fatal("fake hrn did not start", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("expected caller cancellation", err)
	}
	if data, err := os.ReadFile(statePath); err != nil || string(data) != state {
		t.Fatalf("cancellation changed owner state: %s, %v", data, err)
	}
	t.Setenv("ADA_KNOWLEDGE_HELPER", "draft")
	if out, _, err := k.invoke(t, "propose"); err != nil || !strings.Contains(out, firstRun+", "+secondRun) {
		t.Fatal("expected both runs on retry", out, err)
	}
}

func TestKnowledgeReviewTimeoutBacksOffWithExplicitBackend(t *testing.T) {
	k := newKnowledgeCLI(t)
	k.journal(t, firstRun)
	k.settings["allow_metered"] = true
	k.settings["hrn"] = map[string]string{"backend": "api", "path": os.Args[0], "timeout": "300ms"}
	if _, diag, err := k.invoke(t, "propose"); err != nil || !strings.Contains(diag, "disabled") {
		t.Fatal(diag, err)
	}
	k.settings["learning"] = map[string]string{"knowledge_backend": "codex"}
	t.Setenv("ADA_EXPECT_RUNS", firstRun)
	t.Setenv("ADA_KNOWLEDGE_HELPER", "wait")
	_, diag, err := k.invoke(t, "propose")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	k.checkBackoff(t, 1, diag)
	t.Setenv("ADA_KNOWLEDGE_HELPER", "draft")
	k.settings["hrn"].(map[string]string)["timeout"] = "30s"
	if out, _, err := k.invoke(t, "propose"); err != nil || !strings.Contains(out, firstRun) {
		t.Fatal(out, err)
	}
}

func TestKnowledgeReadsPieceTwoOwnerFile(t *testing.T) {
	k := newKnowledgeCLI(t)
	id := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	data := fmt.Sprintf(`{"reviewed":{"session":1},"proposals":[{"id":%q,"kind":"fact","text":"tea","session":"session","messages":[0],"outside":false,"status":"pending","created":"2026-01-01T01:00:00Z","decided":"0001-01-01T00:00:00Z","confirmed":"0001-01-01T00:00:00Z","fact_id":"","offer":{"channel":"echo","chat":"chat","sender":"s","delivery_id":"d","expires":"2026-12-01T00:00:00Z"}}]}`, id)
	if err := os.WriteFile(filepath.Join(k.dir, "learning", "maintainer.json"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := k.invoke(t, "keep", id, "-version", "v1"); err == nil || !strings.Contains(err.Error(), "-version requires a knowledge proposal") {
		t.Fatal(err)
	}
	k.journal(t, firstRun, secondRun)
	if _, _, err := k.invoke(t, "propose"); err != nil {
		t.Fatal(err)
	}
	if out, _, err := k.invoke(t, "list"); err != nil || !strings.Contains(out, id) || !strings.Contains(out, firstRun) {
		t.Fatal(out, err)
	}
}

func TestInvalidDelegationRefused(t *testing.T) {
	k := newKnowledgeCLI(t)
	m, err := memory.NewStore(k.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	store := learn.Store{Memory: m}
	for _, id := range []string{"", "20261123-120000-ABCDEF", firstRun + "!", "../" + firstRun} {
		if err := store.RecordDelegation(id, "other", "s"); err == nil {
			t.Fatal(id)
		}
	}
	if _, err := os.Stat(filepath.Join(k.dir, "learning", "delegations.jsonl")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestKnowledgeMixedOutcomesAndPendingDraft(t *testing.T) {
	k := newKnowledgeCLI(t)
	k.settings["hrn"].(map[string]string)["timeout"] = "40m"
	const deferred = "20261123-120002-123456"
	const failed = "20261123-120003-123456"
	ids := []string{firstRun, secondRun, deferred, failed}
	k.journal(t, ids...)
	t.Setenv("ADA_EXPECT_RUNS", strings.Join(ids, " "))
	t.Setenv("ADA_KNOWLEDGE_HELPER", "mixed")
	out, diag, err := k.invoke(t, "propose")
	if err != nil || !strings.Contains(out, "2 knowledge entries from runs "+firstRun) || strings.Contains(out, secondRun) ||
		!strings.Contains(diag, failed) || !strings.Contains(diag, reviewRun) {
		t.Fatal(out, diag, err)
	}
	id := strings.Fields(out)[0]
	if err := os.Remove(k.stamp); err != nil {
		t.Fatal(err)
	}
	if out, diag, err := k.invoke(t, "propose"); err != nil || out != "" || !strings.Contains(diag, "pending") || strings.Count(diag, "\n") != 1 {
		t.Fatal(out, diag, err)
	}
	if _, err := os.Stat(k.stamp); !os.IsNotExist(err) {
		t.Fatal("pending draft launched hrn", err)
	}
	if _, _, err := k.invoke(t, "reject", id); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADA_EXPECT_RUNS", deferred+" "+failed)
	t.Setenv("ADA_KNOWLEDGE_HELPER", "none")
	if out, _, err := k.invoke(t, "propose"); err != nil || out != "" {
		t.Fatal(out, err)
	}
}

func TestKnowledgeFailureLimit(t *testing.T) {
	k := newKnowledgeCLI(t)
	k.journal(t, firstRun)
	t.Setenv("ADA_EXPECT_RUNS", firstRun)
	for i, next := range []string{"failed-reviews", "failed-reviews", "deferred", "failed-reviews", "failed-reviews", "failed-reviews"} {
		t.Setenv("ADA_KNOWLEDGE_HELPER", next)
		if err := os.Remove(k.stamp); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		out, diag, err := k.invoke(t, "propose")
		if _, statErr := os.Stat(k.stamp); statErr != nil || out != "" || err != nil {
			t.Fatalf("attempt %d: %q, %q, %v, stamp %v", i, out, diag, err, statErr)
		}
		if next == "failed-reviews" && (!strings.Contains(diag, firstRun) || !strings.Contains(diag, reviewRun)) {
			t.Fatal(diag)
		}
		if i == 5 && (!strings.Contains(diag, firstRun) || !strings.Contains(diag, "3")) {
			t.Fatal("missing retirement diagnostic", diag)
		}
	}
	if err := os.Remove(k.stamp); err != nil {
		t.Fatal(err)
	}
	if out, _, err := k.invoke(t, "propose"); err != nil || out != "" {
		t.Fatal(out, err)
	}
	if _, err := os.Stat(k.stamp); !os.IsNotExist(err) {
		t.Fatal("retried after third consecutive failure", err)
	}
}

func TestKnowledgeFreshRunsBeforeRetries(t *testing.T) {
	k := newKnowledgeCLI(t)
	k.settings["hrn"].(map[string]string)["timeout"] = "100m"
	k.journal(t, firstRun)
	t.Setenv("ADA_EXPECT_RUNS", firstRun)
	t.Setenv("ADA_KNOWLEDGE_HELPER", "failed-reviews")
	if _, _, err := k.invoke(t, "propose"); err != nil {
		t.Fatal(err)
	}
	ids := []string{firstRun}
	for i := range 10 {
		ids = append(ids, fmt.Sprintf("20261123-120010-%06x", i))
	}
	k.journal(t, ids...)
	t.Setenv("ADA_EXPECT_RUNS", strings.Join(ids[1:], " "))
	t.Setenv("ADA_KNOWLEDGE_HELPER", "none")
	if _, _, err := k.invoke(t, "propose"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADA_EXPECT_RUNS", firstRun)
	if _, _, err := k.invoke(t, "propose"); err != nil {
		t.Fatal(err)
	}
}

func (k *knowledgeCLI) ownerState(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(k.dir, "learning", "maintainer.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func (k *knowledgeCLI) checkBackoff(t *testing.T, failures int, diag string) {
	t.Helper()
	var backoff struct {
		Failures int
		Next     time.Time
	}
	if err := json.Unmarshal(k.ownerState(t)["knowledge_backoff"], &backoff); err != nil {
		t.Fatal("missing back-off", err)
	}
	delay := min(time.Hour<<failures, 168*time.Hour)
	if backoff.Failures != failures || time.Until(backoff.Next) < delay-time.Minute || time.Until(backoff.Next) > delay {
		t.Fatalf("back-off %+v, want %d failures and delay %v", backoff, failures, delay)
	}
	next := backoff.Next.Format(time.RFC3339Nano)
	if !strings.Contains(diag, next) {
		t.Fatalf("missing next attempt %s in %q", next, diag)
	}
}

func TestKnowledgeOutageBackoff(t *testing.T) {
	k := newKnowledgeCLI(t)
	k.journal(t, firstRun)
	t.Setenv("ADA_EXPECT_RUNS", firstRun)
	t.Setenv("ADA_KNOWLEDGE_HELPER", "fail")
	for failures := 1; failures <= 9; failures++ {
		_, diag, err := k.invoke(t, "propose")
		if err == nil || !strings.Contains(err.Error(), "review failed") {
			t.Fatal(err)
		}
		k.checkBackoff(t, failures, diag)
	}
	for _, mode := range []string{"failed-reviews", "fail", "draft"} {
		t.Setenv("ADA_KNOWLEDGE_HELPER", mode)
		out, diag, err := k.invoke(t, "propose")
		if mode == "fail" {
			if err == nil {
				t.Fatal("expected outage")
			}
			k.checkBackoff(t, 1, diag)
		} else if err != nil || k.ownerState(t)["knowledge_backoff"] != nil || mode == "draft" && !strings.Contains(out, firstRun) {
			t.Fatal(mode, out, diag, err)
		}
	}
}

func TestKnowledgeLegacyBatchFailuresDoNotRetireRun(t *testing.T) {
	k := newKnowledgeCLI(t)
	path := filepath.Join(k.dir, "learning", "maintainer.json")
	data := fmt.Sprintf(`{"proposals":[],"reviewed":{},"run_failures":{%q:2}}`, firstRun)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	k.journal(t, firstRun)
	t.Setenv("ADA_EXPECT_RUNS", firstRun)
	t.Setenv("ADA_KNOWLEDGE_HELPER", "failed-reviews")
	for i := 1; i <= 3; i++ {
		_, diag, err := k.invoke(t, "propose")
		if err != nil || !strings.Contains(diag, firstRun) || strings.Contains(diag, "marked runs reviewed") != (i == 3) {
			t.Fatal(i, diag, err)
		}
	}
}

func TestKnowledgeInvalidBackoffRefused(t *testing.T) {
	k := newKnowledgeCLI(t)
	for _, raw := range []string{
		`{"failures":1}`, `{"failures":0,"next":"2026-11-24T01:00:00Z"}`,
		`{"failures":1,"next":"0001-01-01T00:00:00Z"}`, `{"failures":1,"next":null}`,
	} {
		data := fmt.Sprintf(`{"proposals":[],"reviewed":{},"knowledge_backoff":%s}`, raw)
		if err := os.WriteFile(filepath.Join(k.dir, "learning", "maintainer.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := k.invoke(t, "list"); err == nil {
			t.Fatal("accepted invalid back-off", raw)
		}
	}
}

func TestKnowledgeBatchFitsTimeout(t *testing.T) {
	k := newKnowledgeCLI(t)
	k.settings["hrn"].(map[string]string)["timeout"] = "20m"
	k.journal(t, firstRun, secondRun, reviewRun)
	t.Setenv("ADA_KNOWLEDGE_HELPER", "none")
	for _, want := range []string{firstRun + " " + secondRun, reviewRun} {
		t.Setenv("ADA_EXPECT_RUNS", want)
		if _, _, err := k.invoke(t, "propose"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestKnowledgeRefusedDraftOutcomes(t *testing.T) {
	k := newKnowledgeCLI(t)
	k.settings["hrn"].(map[string]string)["timeout"] = "40m"
	const deferred = "20261123-120002-123456"
	const failed = "20261123-120003-123456"
	k.journal(t, firstRun, secondRun, deferred, failed)
	t.Setenv("ADA_EXPECT_RUNS", strings.Join([]string{firstRun, secondRun, deferred, failed}, " "))
	t.Setenv("ADA_KNOWLEDGE_HELPER", "refused-mixed")
	out, diag, err := k.invoke(t, "propose")
	if out != "" || err == nil || !strings.Contains(err.Error(), "invalid knowledge draft") {
		t.Fatal(out, diag, err)
	}
	k.checkBackoff(t, 1, diag)
	files, err := os.ReadDir(filepath.Join(k.dir, "learning", "knowledge"))
	if err != nil || len(files) != 0 {
		t.Fatal(files, err)
	}
	t.Setenv("ADA_EXPECT_RUNS", strings.Join([]string{firstRun, deferred, failed}, " "))
	t.Setenv("ADA_KNOWLEDGE_HELPER", "failed-reviews")
	for i := range 2 {
		_, diag, err := k.invoke(t, "propose")
		if err != nil || (i == 1) != strings.Contains(diag, "consecutive failures: "+failed) {
			t.Fatal(diag, err)
		}
	}
	t.Setenv("ADA_EXPECT_RUNS", firstRun+" "+deferred)
	t.Setenv("ADA_KNOWLEDGE_HELPER", "draft")
	if out, _, err := k.invoke(t, "propose"); err != nil || !strings.Contains(out, firstRun) || !strings.Contains(out, deferred) {
		t.Fatal(out, err)
	}
}

func TestKnowledgeDraftFile(t *testing.T) {
	for _, tc := range []struct{ mode, refusal string }{
		{"limit", ""}, {"over-limit", "exceeds 8 MiB"}, {"unreported", "unreported knowledge draft"},
		{"wrong-draft-path", "knowledge proposal counts, draft or exit status disagree"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			k := newKnowledgeCLI(t)
			k.journal(t, firstRun, secondRun)
			t.Setenv("ADA_KNOWLEDGE_HELPER", tc.mode)
			out, diag, err := k.invoke(t, "propose")
			if tc.refusal == "" {
				if err != nil || !strings.Contains(out, firstRun) {
					t.Fatal(out, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.refusal) || out != "" {
				t.Fatal(out, err)
			}
			files, err := os.ReadDir(filepath.Join(k.dir, "learning", "knowledge"))
			if err != nil || len(files) != 0 {
				t.Fatal("refused draft survived", files, err)
			}
			k.checkBackoff(t, 1, diag)
		})
	}
}

func TestKnowledgeNothingEligible(t *testing.T) {
	k := newKnowledgeCLI(t)
	k.journal(t, firstRun, secondRun)
	t.Setenv("ADA_KNOWLEDGE_HELPER", "raw-report")
	t.Setenv("ADA_FAKE_REPORT", fmt.Sprintf(`{"draft":"","proposed":0,"kept":0,"reviewed":[],"skipped":[{"run":%q,"reason":"other repo"}],"deferred":[{"run":%q,"reason":"still running"}],"failed":[]}`, firstRun, secondRun))
	if out, _, err := k.invoke(t, "propose"); err != nil || out != "" {
		t.Fatal(out, err)
	}
	const fresh = "20261123-120003-123456"
	k.journal(t, firstRun, secondRun, fresh)
	t.Setenv("ADA_EXPECT_RUNS", fresh+" "+secondRun)
	t.Setenv("ADA_KNOWLEDGE_HELPER", "draft")
	if out, _, err := k.invoke(t, "propose"); err != nil || !strings.Contains(out, secondRun) {
		t.Fatal(out, err)
	}
}

func TestKnowledgeInvalidSummaryRetries(t *testing.T) {
	k := newKnowledgeCLI(t)
	k.journal(t, firstRun)
	t.Setenv("ADA_EXPECT_RUNS", firstRun)
	base := fmt.Sprintf(`{"draft":"","proposed":0,"kept":0,"reviewed":[%q],"skipped":[],"deferred":[],"failed":[]}`, firstRun)
	for _, raw := range []string{
		"{}", base + "{}", strings.Replace(base, `"kept":0`, `"kept":0,"kept":0`, 1),
		strings.Replace(base, `"draft":""`, `"draft":"","extra":0`, 1),
		strings.Replace(base, `"draft":""`, `"draft":"elsewhere"`, 1),
		strings.Replace(base, fmt.Sprintf("%q", firstRun), fmt.Sprintf("%q,%q", firstRun, secondRun), 1),
		strings.Replace(base, `"kept":0`, `"kept":null`, 1),
		strings.Replace(base, firstRun, secondRun, 1),
		strings.Replace(base, fmt.Sprintf("%q", firstRun), "", 1),
		strings.Replace(base, `"skipped":[]`, fmt.Sprintf(`"skipped":[{"run":%q,"reason":"x"}]`, firstRun), 1),
		fmt.Sprintf(`{"draft":"","proposed":0,"kept":0,"reviewed":[],"skipped":[],"deferred":[{"run":%q,"reason":"x","extra":1}],"failed":[]}`, firstRun),
		fmt.Sprintf(`{"draft":"","proposed":0,"kept":0,"reviewed":[],"skipped":[],"deferred":[],"failed":[{"run":%q,"review_run":"","error":"x"}]}`, firstRun),
	} {
		t.Setenv("ADA_KNOWLEDGE_HELPER", "raw-report")
		t.Setenv("ADA_FAKE_REPORT", raw)
		if out, _, err := k.invoke(t, "propose"); err == nil || out != "" {
			t.Fatalf("accepted %s: %q, %v", raw, out, err)
		}
	}
	t.Setenv("ADA_KNOWLEDGE_HELPER", "overflow")
	if _, _, err := k.invoke(t, "propose"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatal(err)
	}
	t.Setenv("ADA_KNOWLEDGE_HELPER", "draft")
	if out, _, err := k.invoke(t, "propose"); err != nil || !strings.Contains(out, firstRun) {
		t.Fatal(out, err)
	}
}

func (k *knowledgeCLI) pending(t *testing.T) string {
	t.Helper()
	k.journal(t, firstRun, secondRun)
	out, _, err := k.invoke(t, "propose")
	if err != nil || out == "" {
		t.Fatal(out, err)
	}
	return strings.Fields(out)[0]
}

func TestKnowledgeRejectRecovery(t *testing.T) {
	for _, removed := range []bool{false, true} {
		t.Run(fmt.Sprintf("draftRemoved=%t", removed), func(t *testing.T) {
			k := newKnowledgeCLI(t)
			id := k.pending(t)
			dir := filepath.Join(k.dir, "learning")
			draft := filepath.Join(dir, "knowledge", id+".json")
			journal := filepath.Join(dir, ".intent-maintainer.json")
			intent := fmt.Sprintf(`{"id":%q,"action":"reject","at":%q}`, id, time.Now().UTC().Format(time.RFC3339Nano))
			if err := os.WriteFile(journal, []byte(intent), 0600); err != nil {
				t.Fatal(err)
			}
			if removed {
				if err := os.Remove(draft); err != nil {
					t.Fatal(err)
				}
			}
			if out, _, err := k.invoke(t, "list", "-all"); err != nil || !strings.Contains(out, id+"\trejected") {
				t.Fatal(out, err)
			}
			for _, path := range []string{draft, journal} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal(path, err)
				}
			}
		})
	}
}

func TestKnowledgeInvalidOwnerFile(t *testing.T) {
	k := newKnowledgeCLI(t)
	id := k.pending(t)
	path := filepath.Join(k.dir, "learning", "maintainer.json")
	base, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		field string
		value any
	}{
		{"draft", "learning/elsewhere.json"}, {"source_runs", []string{reviewRun}}, {"version", "v1"},
	} {
		var state struct {
			Proposals    []map[string]any `json:"proposals"`
			Reviewed     map[string]int   `json:"reviewed"`
			ReviewedRuns map[string]bool  `json:"reviewed_runs"`
		}
		if err := json.Unmarshal(base, &state); err != nil {
			t.Fatal(err)
		}
		state.Proposals[0][tc.field] = tc.value
		data, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if out, _, err := k.invoke(t, "list"); err == nil || !strings.Contains(err.Error(), "read learning file") || strings.Contains(out, id) {
			t.Fatal(tc.field, out, err)
		}
	}
}

func TestKnowledgeDirectoryLinkRefused(t *testing.T) {
	k := newKnowledgeCLI(t)
	dir := filepath.Join(k.dir, "learning")
	if err := os.Mkdir(filepath.Join(dir, "other"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("other", filepath.Join(dir, "knowledge")); err != nil {
		t.Skip(err)
	}
	k.journal(t, firstRun, secondRun)
	if _, _, err := k.invoke(t, "propose"); err == nil {
		t.Fatal("accepted linked knowledge directory")
	}
	if _, err := os.Stat(k.stamp); !os.IsNotExist(err) {
		t.Fatal("launched hrn with linked output directory", err)
	}
}
