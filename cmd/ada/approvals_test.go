package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"testing/cryptotest"
	"time"

	"github.com/cristiangirlea/adanext-ada/approval"
)

func TestChatApprovalAndOwnerHistory(t *testing.T) {
	cryptotest.SetGlobalRandom(t, 17)
	root := t.TempDir()
	task, _ := json.Marshal(map[string]string{"prompt": "edit the notes", "dir": root})
	var calls atomic.Int32
	var requests bytes.Buffer
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		requests.Write(body)
		switch calls.Add(1) {
		case 1:
			fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call1","type":"function","function":{"name":"delegate_to_hrn","arguments":%q}}]}}]}`, task)
		case 2:
			if !bytes.Contains(body, []byte("delegate to hrn:")) {
				t.Errorf("missing launch error in model request: %s", body)
			}
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Skipped."}}]}`)
		case 3:
			if !bytes.Contains(body, []byte("next question")) {
				t.Errorf("missing next chat line: %s", body)
			}
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Next answer."}}]}`)
		default:
			t.Error("unexpected extra turn")
			http.Error(w, "extra turn", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	raw := fmt.Sprintf(`{"data_dir":%q,"owner":"Ada Owner","chat_provider":"local","providers":[{"name":"local","kind":"openai","base_url":%q,"model":"test"}],"workspace_roots":[%q],"hrn":{"mode":"auto","backend":"local","path":%q}}`, dir, server.URL, root, filepath.Join(dir, "must-not-run"))
	if err := os.WriteFile(settings, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	var output, diag bytes.Buffer
	in, w := io.Pipe()
	defer w.Close()
	prompts := make(promptSink, 8)
	done := make(chan error, 1)
	go func() {
		defer in.Close()
		done <- run(t.Context(), []string{"chat", "-config", settings}, in, &output, io.MultiWriter(&diag, prompts))
	}()
	if _, err := io.WriteString(w, "edit\n"); err != nil {
		t.Fatal(err)
	}
	<-prompts // The session announcement precedes the approval prompt.
	code := promptCode(t, <-prompts)
	if _, err := fmt.Fprintln(w, code+"\nnext question"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	server.Close()
	if calls.Load() != 3 || !strings.Contains(diag.String(), "Ada asks to write:") || output.String() != "Skipped.\nNext answer.\n" {
		t.Fatalf("chat output %q, stderr %q", output.String(), diag.String())
	}
	fields := strings.Fields(diag.String())
	records, err := approval.Log(dir).Latest("Ada Owner", []string{fields[1]}, 1)
	if err != nil || len(records) != 1 || records[0].Request.Session != fields[1] || records[0].Decision.By != "cli" || !records[0].Decision.Approved {
		t.Fatalf("records = %+v, %v", records, err)
	}
	audit, err := os.ReadFile(filepath.Join(dir, "approvals", fields[1]+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(requests.String(), code) || bytes.Contains(audit, []byte(code)) || strings.Contains(output.String(), code) {
		t.Fatal("confirmation code escaped stderr")
	}
	foreign := records[0]
	foreign.Request.User = "Someone else"
	foreign.Request.Summary = "foreign-summary-marker"
	foreign.Decision.At = time.Now().Add(time.Second)
	if err := approval.Log(dir).Record(foreign.Request, foreign.Decision); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := run(t.Context(), []string{"approvals", "-config", settings, "-n", "1"}, strings.NewReader(""), &output, &diag); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), records[0].Request.ID) || !strings.Contains(output.String(), "edit the notes") || !strings.Contains(output.String(), "approved") || strings.Contains(output.String(), "foreign-summary-marker") {
		t.Fatal(output.String())
	}
}

func cliRequest(summary string) approval.Request {
	now := time.Now()
	return approval.Request{Summary: summary, Effect: "write", Created: now, Expires: now.Add(10 * time.Minute)}
}

func promptCode(t *testing.T, prompt string) string {
	t.Helper()
	match := regexp.MustCompile(`^Ada asks to [^\n]+\nType ([ABCDEFGHJKLMNPQRSTUVWXYZ23456789]{4}) to approve, anything else to deny \(expires in [^)]+\):\n$`).FindStringSubmatch(prompt)
	if match == nil {
		t.Fatalf("unexpected prompt %q", prompt)
	}
	return match[1]
}

func TestCLIApprovalAnswers(t *testing.T) {
	for _, answer := range []string{"lowercase", "wrong code", "yes", "EOF", "partial"} {
		t.Run(answer, func(t *testing.T) {
			in, w := io.Pipe()
			defer w.Close()
			out := make(promptSink, 1)
			cli := newCLISurface(in, out)
			type result struct {
				d   approval.Decision
				err error
			}
			done := make(chan result, 1)
			go func() {
				d, err := cli.Ask(t.Context(), cliRequest("run"))
				done <- result{d, err}
			}()
			prompt := <-out
			code := promptCode(t, prompt)
			if !strings.HasPrefix(prompt, "Ada asks to write: run\n") || !strings.HasSuffix(prompt, "(expires in 10m):\n") {
				t.Fatalf("unexpected approval details: %q", prompt)
			}
			switch answer {
			case "lowercase":
				fmt.Fprintf(w, "  %s \r\n", strings.ToLower(code))
			case "wrong code":
				wrong := "A" + code[1:]
				if wrong == code {
					wrong = "B" + code[1:]
				}
				fmt.Fprintln(w, wrong)
			case "yes":
				fmt.Fprintln(w, "yes")
			case "partial":
				fmt.Fprint(w, code)
			}
			w.Close()
			if got := <-done; got.err != nil || got.d.Approved != (answer == "lowercase") || got.d.By != "cli" {
				t.Fatalf("Ask = %+v", got)
			}
		})
	}
}

func TestChatDeniesTypedAheadApproval(t *testing.T) {
	var diag bytes.Buffer
	cli := newCLISurface(strings.NewReader("msg\ny\n"), &diag)
	line, err := cli.read(t.Context(), nil)
	if err != nil || line.text != "msg" {
		t.Fatalf("first chat line = %+v, %v", line, err)
	}
	d, err := approval.Decide(approval.WithApprover(t.Context(), cli), cliRequest("edit"))
	if err != nil || d.Approved || d.Reason != "typed ahead" || !strings.Contains(diag.String(), "typed ahead") || strings.Contains(diag.String(), "Ada asks") {
		t.Fatalf("decision = %+v, %v; stderr %q", d, err, diag.String())
	}
	line, err = cli.read(t.Context(), nil)
	if err != nil || line.text != "y" {
		t.Fatalf("next chat line = %+v, %v", line, err)
	}
}

type promptSink chan string

func (s promptSink) Write(p []byte) (int, error) { s <- string(p); return len(p), nil }

func TestCLIApprovalSerializesReplies(t *testing.T) {
	cryptotest.SetGlobalRandom(t, 1)
	in, w := io.Pipe()
	defer w.Close()
	out := make(promptSink, 4)
	cli := newCLISurface(in, out)
	ask := func(ctx context.Context, name string) <-chan approval.Decision {
		ch := make(chan approval.Decision, 1)
		go func() { d, _ := cli.Ask(ctx, cliRequest(name)); ch <- d }()
		return ch
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	first := ask(ctx, "one")
	var code string
	select {
	case prompt := <-out:
		code = promptCode(t, prompt)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	second := ask(ctx, "two")
	select {
	case <-out:
		t.Fatal("second prompt before first reply")
	case <-time.After(15 * time.Millisecond):
	}
	fmt.Fprintln(w, code)
	if !(<-first).Approved {
		t.Fatal("first denied")
	}
	select {
	case prompt := <-out:
		if promptCode(t, prompt) == code {
			t.Fatal("second prompt reused the first code")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	fmt.Fprintln(w, code)
	if (<-second).Approved {
		t.Fatal("second approved")
	}
}
func TestExpiredCLIReplyCannotApproveNextRequest(t *testing.T) {
	in, w := io.Pipe()
	defer w.Close()
	out := make(promptSink, 4)
	cli := newCLISurface(in, out)
	r := cliRequest("first")
	r.Expires = time.Now().Add(50 * time.Millisecond)
	done := make(chan approval.Decision, 1)
	go func() { d, _ := cli.Ask(t.Context(), r); done <- d }()
	<-out
	select {
	case d := <-done:
		if d.Approved || d.Reason != "expired" {
			t.Fatal(d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("prompt did not expire")
	}
	select {
	case msg := <-out:
		if msg != "Approval expired; nothing was run.\n" {
			t.Fatal(msg)
		}
	default:
		t.Fatal("missing expiry notice")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	d, err := cli.Ask(ctx, cliRequest("second"))
	if err != nil || d.Approved || d.Reason != "typed ahead" {
		t.Fatalf("second = %+v, %v", d, err)
	}
	go func() { fmt.Fprintln(w, "y") }()
	line, err := cli.read(t.Context(), nil)
	if err != nil || line.text != "y" {
		t.Fatalf("pending chat line = %+v, %v", line, err)
	}
}

func TestChatResumesAfterApprovalExpiry(t *testing.T) {
	in, w := io.Pipe()
	defer w.Close()
	out := make(promptSink, 4)
	cli := newCLISurface(in, out)
	r := cliRequest("first")
	r.Expires = time.Now().Add(100 * time.Millisecond)
	done := make(chan approval.Decision, 1)
	go func() { d, _ := cli.Ask(t.Context(), r); done <- d }()
	code := promptCode(t, <-out)
	if d := <-done; d.Reason != "expired" {
		t.Fatal(d)
	}
	go func() { fmt.Fprintln(w, code+"\nnext question"); w.Close() }()
	line, err := cli.read(t.Context(), nil)
	if err != nil || line.text != "next question" {
		t.Fatalf("next chat line = %q, %v", line.text, err)
	}
}
