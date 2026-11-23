package schedule

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/adanext-ada/approval"
	"github.com/cristiangirlea/adanext-ada/loop"
	"github.com/cristiangirlea/adanext-ada/memory"
	"github.com/cristiangirlea/adanext-ada/provider"
	"github.com/cristiangirlea/adanext-ada/tool"
)

type fakeModel func(context.Context, provider.Request) (provider.Response, error)

func (fakeModel) Name() string { return "fake" }
func (f fakeModel) Complete(ctx context.Context, r provider.Request) (provider.Response, error) {
	return f(ctx, r)
}

func fixture(t *testing.T) (*Runner, *time.Time) {
	t.Helper()
	dir := t.TempDir()
	m, err := memory.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	unlock, err := m.LockSessions()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.Local)
	r := &Runner{Store: Store{Memory: m}, Owner: "owner", History: 40, Now: func() time.Time { return now }}
	return r, &now
}

func TestMissedDisabledIsolatedAndDelivered(t *testing.T) {
	r, now := fixture(t)
	ctx := context.Background()
	session, err := r.Store.Memory.Open("owner", "cli", "local", "owner", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	r.Loop = &loop.Loop{Provider: fakeModel(func(ctx context.Context, req provider.Request) (provider.Response, error) {
		user, _ := memory.UserFrom(ctx)
		if user != "owner" {
			t.Fatal(user)
		}
		calls++
		return provider.Response{Content: []provider.Block{{Type: "text", Text: "report"}}}, nil
	})}
	target := Target{"one", "room"}
	var sends int
	r.Deliver = func(ctx context.Context, to Target, text string) error {
		if to != target || text != "report" {
			t.Fatalf("%v %q", to, text)
		}
		sends++
		return nil
	}
	job, err := r.Store.Add(Job{Spec: "every 1m", Kind: "prompt", Prompt: "summarize", Target: target}, *now)
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := r.Store.Add(Job{Spec: "every 1m", Kind: "prompt", Prompt: "skip"}, *now)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Store.SetEnabled(disabled.ID, false); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(5 * 24 * time.Hour)
	for range 2 {
		if err = r.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := r.Store.Runs(10)
	if err != nil || len(runs) != 1 || runs[0].Job != job.ID || !runs[0].Delivered || sends != 1 || calls != 1 {
		t.Fatalf("%+v, %v, calls %d sends %d", runs, err, calls, sends)
	}
	jobs, err := r.Store.List()
	if err != nil || !jobs[0].LastRun.Equal(*now) {
		t.Fatal(jobs, err)
	}
	resumed, err := r.Store.Memory.Open("owner", "cli", "local", "owner", time.Hour)
	if err != nil || session.ID != resumed.ID {
		t.Fatal(resumed, err)
	}
}

func TestPromptRefusalSurvivesModelAnswer(t *testing.T) {
	r, now := fixture(t)
	reg := &tool.Registry{}
	if err := reg.Add(PromptTool{Store: r.Store}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	r.Loop = &loop.Loop{Tools: reg, Provider: fakeModel(func(ctx context.Context, req provider.Request) (provider.Response, error) {
		calls++
		return provider.Response{Content: []provider.Block{{Type: "tool_use", Name: "schedule_prompt_job", ID: "x", Input: []byte(`{"name":"x","spec":"every 2m","prompt":"x"}`)}}}, nil
	})}
	j, err := r.Store.Add(Job{Spec: "every 1m", Kind: "prompt", Prompt: "write"}, *now)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan approval.Request, 1)
	run, err := r.RunLocal(approval.WithApprover(context.Background(), approving{seen}), j.ID)
	if len(seen) != 0 {
		t.Fatal("scheduled run inherited an approver")
	}
	if err != nil || calls != 1 || !strings.Contains(run.Output, "needs the user") {
		t.Fatal(calls, run, err)
	}
	jobs, err := r.Store.List()
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	r.Loop.Provider = fakeModel(func(context.Context, provider.Request) (provider.Response, error) {
		return provider.Response{Content: []provider.Block{{Type: "tool_use", ID: "bad", Name: "schedule_prompt_job", Input: []byte(`{"kind":"script"}`)}}}, nil
	})
	run, err = r.RunLocal(context.Background(), j.ID)
	if err != nil || !strings.Contains(run.Output, "needs the user") {
		t.Fatal(run, err)
	}
}

type approving struct{ seen chan approval.Request }

func (a approving) Ask(_ context.Context, r approval.Request) (approval.Decision, error) {
	a.seen <- r
	return approval.Decision{Approved: true}, nil
}
func TestPromptToolApprovalAndScope(t *testing.T) {
	r, _ := fixture(t)
	reg := tool.Registry{Recorder: approval.Log(t.TempDir())}
	if err := reg.Add(PromptTool{Store: r.Store}); err != nil {
		t.Fatal(err)
	}
	seen := make(chan approval.Request, 4)
	cli := approval.WithApprover(memory.WithUser(context.Background(), "owner"), approving{seen})
	if _, err := reg.Run(cli, "schedule_prompt_job", []byte(`{"name":"a","spec":"every 1m","prompt":"x","to":"one:room"}`)); err == nil || !strings.Contains(err.Error(), "originating chat") {
		t.Fatalf("CLI delivery target: %v", err)
	}
	ctx := WithOrigin(cli, Target{"one", "room"})
	for _, input := range []string{`{"name":"a","spec":"every 1m","prompt":"x","argv":["bad"]}`, `{"name":"a","spec":"every 1m","prompt":"x","kind":"script"}`, `{"name":"a","spec":"every 1m","prompt":"x","to":"two:room"}`} {
		if _, err := reg.Run(ctx, "schedule_prompt_job", []byte(input)); err == nil {
			t.Fatal(input)
		}
	}
	if _, err := reg.Run(ctx, "schedule_prompt_job", []byte(`{"name":"a","spec":"every 1m","prompt":"hello","to":"one:room"}`)); err != nil {
		t.Fatal(err)
	}
	req := <-seen
	if len(seen) != 0 || req.Effect != "write" || !strings.Contains(req.Summary, "hello") || !strings.Contains(req.Summary, "one:room") || !strings.Contains(req.Summary, "every 1m") {
		t.Fatal(req)
	}
	jobs, err := r.Store.List()
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "prompt" {
		t.Fatal(jobs, err)
	}
	for i := 1; i <= 3; i++ {
		want := jobs[0].Created.Local().Add(time.Duration(i) * time.Minute).Format(time.RFC3339Nano)
		if !strings.Contains(req.Summary, want) {
			t.Fatalf("approval missing scheduled time %s: %s", want, req.Summary)
		}
	}
}

func TestScriptHelper(t *testing.T) {
	for i, arg := range os.Args {
		if arg != "--schedule-helper" {
			continue
		}
		switch os.Args[i+1] {
		case "output":
			wd, _ := os.Getwd()
			fmt.Print(strings.Repeat("x", 70000))
			fmt.Printf(" %s %q %s", filepath.Base(wd), os.Args[i+2], os.Getenv("ADA_TEST_PRIVATE"))
			if _, ok := os.LookupEnv("PATH"); ok {
				fmt.Print("unexpected PATH")
			}
			os.Exit(3)
		case "parent":
			cmd := exec.Command(os.Args[0], "-test.run=TestScriptHelper", "--", "--schedule-helper", "child", os.Args[i+2])
			if err := cmd.Start(); err != nil {
				os.Exit(2)
			}
			cmd.Wait()
			os.Exit(0)
		case "background":
			cmd := exec.Command(os.Args[0], "-test.run=TestScriptHelper", "--", "--schedule-helper", "sleep")
			cmd.Stdout = os.Stdout
			if err := cmd.Start(); err != nil {
				os.Exit(2)
			}
			fmt.Print("parent finished")
			os.Exit(0)
		case "sleep":
			time.Sleep(time.Minute)
			os.Exit(0)
		case "child":
			os.WriteFile(os.Args[i+2]+".ready", []byte("ready"), 0600)
			time.Sleep(2 * time.Second)
			os.WriteFile(os.Args[i+2], []byte("escaped"), 0600)
			os.Exit(0)
		}
	}
}

func TestScriptTailExitTimeoutAndModes(t *testing.T) {
	r, now := fixture(t)
	t.Setenv("ADA_TEST_PRIVATE", "filtered")
	r.WithoutEnv = []string{"PATH"}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	add := func(timeout time.Duration, args ...string) Job {
		j, err := r.Store.Add(Job{Kind: "script", Spec: "every 1m", Argv: append([]string{exe, "-test.run=TestScriptHelper", "--", "--schedule-helper"}, args...), Budget: Budget{Timeout: timeout}}, *now)
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	j := add(time.Minute, "output", "a & b")
	run, err := r.RunLocal(context.Background(), j.ID)
	if err != nil || len(run.Output) > 65536 || !strings.HasSuffix(run.Output, `work "a & b" `) || !strings.Contains(run.Status, "3") {
		t.Fatal(run.Status, len(run.Output), err)
	}
	marker := filepath.Join(t.TempDir(), "child")
	j = add(time.Minute, "background")
	run, err = r.RunLocal(context.Background(), j.ID)
	if err != nil || run.Status != "exit 0" || run.Output != "parent finished" {
		t.Fatalf("background child holding stdout: %+v, %v", run, err)
	}
	j = add(time.Second, "parent", marker)
	run, err = r.RunLocal(context.Background(), j.ID)
	if err != nil || run.Status != "timeout" {
		t.Fatal(run.Status, err)
	}
	if _, err = os.Stat(marker + ".ready"); err != nil {
		t.Fatal("descendant did not start", err)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("descendant survived", err)
	}
	if runtime.GOOS != "windows" {
		err = r.Store.Memory.WithScheduleFiles(func(root *os.Root) error {
			for _, name := range []string{"jobs.json", "runs.jsonl"} {
				info, err := root.Stat(name)
				if err != nil {
					return err
				}
				if info.Mode().Perm() != 0600 {
					t.Fatal(name, info.Mode())
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
