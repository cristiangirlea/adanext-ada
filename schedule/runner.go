package schedule

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cristiangirlea/adanext-ada/approval"
	"github.com/cristiangirlea/adanext-ada/internal/proctree"
	"github.com/cristiangirlea/adanext-ada/loop"
	"github.com/cristiangirlea/adanext-ada/memory"
	"github.com/cristiangirlea/adanext-ada/provider"
	"github.com/cristiangirlea/adanext-ada/tool"
)

type Runner struct {
	Store      Store
	Loop       *loop.Loop
	Owner      string
	History    int
	WithoutEnv []string
	Deliver    func(context.Context, Target, string) error
	Status     func(string)
	Now        func() time.Time
	Ticks      <-chan time.Time
	mu         sync.Mutex
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
func (r *Runner) log(line string) {
	if r.Status != nil {
		r.Status(line)
	}
}

// Serve consumes ticks synchronously so a slow job cannot overlap the next tick.
func (r *Runner) Serve(ctx context.Context) {
	ticks := r.Ticks
	if ticks == nil {
		timer := time.NewTicker(30 * time.Second)
		defer timer.Stop()
		ticks = timer.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			if err := r.Tick(ctx); err != nil && ctx.Err() == nil {
				r.log(err.Error())
			}
		}
	}
}
func (r *Runner) Tick(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	jobs, err := r.Store.List()
	if err != nil {
		return err
	}
	var errs []error
	for _, j := range jobs {
		if err := ctx.Err(); err != nil {
			return err
		}
		now := r.now()
		due, err := j.due(now)
		if err != nil {
			errs = append(errs, fmt.Errorf("job %s: %w", j.ID, err))
			continue
		}
		if !due {
			continue
		}
		job, due, err := r.Store.claim(j.ID, now, false)
		if errors.Is(err, errUnknownJob) {
			continue
		}
		if err == nil && due {
			_, err = r.execute(ctx, job, true)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("job %s: %w", j.ID, err))
		}
	}
	return errors.Join(errs...)
}
func (r *Runner) RunLocal(ctx context.Context, id string) (Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Run{}, err
	}
	j, _, err := r.Store.claim(id, r.now(), true)
	if err != nil {
		return Run{}, err
	}
	return r.execute(ctx, j, false)
}

const outputLimit = 64 << 10

func tail(s string) string {
	if len(s) > outputLimit {
		s = s[len(s)-outputLimit:]
		for len(s) > 0 && !utf8.RuneStart(s[0]) {
			s = s[1:]
		}
	}
	return strings.ToValidUTF8(s, "?")
}
func (r *Runner) execute(ctx context.Context, j Job, deliver bool) (Run, error) {
	run := Run{Job: j.ID, Started: j.LastRun, Status: "ok"}
	var err error
	if j.Kind == "prompt" {
		run.Output, err = r.prompt(ctx, j)
	} else {
		run.Output, run.Status, err = r.script(ctx, j)
	}
	if err != nil {
		run.Status = "error"
		run.Output = err.Error()
	}
	if deliver && j.Target != (Target{}) {
		message := run.Output
		if j.Kind == "script" {
			message = run.Status + "\n" + message
		}
		if r.Deliver == nil {
			err = fmt.Errorf("channel %q is not running", j.Target.Channel)
		} else {
			wait, cancel := context.WithTimeout(ctx, j.Budget.Timeout)
			err = r.Deliver(wait, j.Target, message)
			cancel()
		}
		run.Delivered = err == nil
		if err != nil {
			r.log(fmt.Sprintf("job %s delivery to %s: %v", j.ID, j.Target, err))
		}
	}
	run.Output = tail(run.Output)
	run.Finished = r.now().UTC()
	return run, r.Store.record(run)
}

func (r *Runner) prompt(ctx context.Context, j Job) (string, error) {
	if r.Loop == nil {
		return "", fmt.Errorf("scheduled prompt requires a conversation provider")
	}
	m := r.Store.Memory
	session, err := m.OpenScheduled(r.Owner, j.ID)
	if err != nil {
		return "", err
	}
	history, err := m.History(session.ID, r.History)
	if err != nil {
		return "", err
	}
	ctx = approval.WithApprover(approval.WithSession(memory.WithUser(ctx, r.Owner), session.ID), nil)
	turn := *r.Loop
	turn.MaxIterations, turn.Timeout, turn.StopOnApproval = j.Budget.MaxIterations, j.Budget.Timeout, true
	answer, next, err := turn.Turn(ctx, history, j.Prompt)
	if err != nil {
		return "", err
	}
	last := &next[len(next)-1]
	if last.Text() != answer {
		last.Content = []provider.Block{{Type: "text", Text: answer}}
	}
	if err = m.Append(session.ID, "schedule", j.ID, r.Owner, next[len(history):]...); err != nil {
		return "", err
	}
	return answer, nil
}

type outputTail []byte

func (t *outputTail) Write(p []byte) (int, error) {
	n := len(p)
	if n >= outputLimit {
		*t = append((*t)[:0], p[n-outputLimit:]...)
	} else {
		drop := max(0, len(*t)+n-outputLimit)
		*t = append((*t)[:copy(*t, (*t)[drop:])], p...)
	}
	return n, nil
}

func (r *Runner) script(ctx context.Context, j Job) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, j.Budget.Timeout)
	defer cancel()
	cmd := exec.Command(j.Argv[0], j.Argv[1:]...)
	cmd.WaitDelay = time.Second
	cmd.Env = []string{}
	for _, name := range tool.InheritedEnvNames() {
		excluded := slices.ContainsFunc(r.WithoutEnv, func(key string) bool { return strings.EqualFold(key, name) })
		if excluded {
			if runtime.GOOS == "windows" && strings.EqualFold(name, "SYSTEMROOT") {
				return "", "", fmt.Errorf("cannot exclude required Windows system environment")
			}
			continue
		}
		if v, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+v)
		}
	}
	err := r.Store.Memory.WithScheduleFiles(func(root *os.Root) error {
		if err := root.MkdirAll("work", 0700); err != nil {
			return err
		}
		var err error
		cmd.Dir, err = filepath.Abs(filepath.Join(root.Name(), "work"))
		return err
	})
	if err != nil {
		return "", "", fmt.Errorf("prepare script directory: %w", err)
	}
	var out outputTail
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	kill, release, err := proctree.Start(cmd)
	if err != nil {
		return "", "", fmt.Errorf("start script: %w", err)
	}
	defer release()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
		kill()
	case <-ctx.Done():
		kill()
		<-done
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return tail(string(out)), "timeout", nil
	}
	if ctx.Err() != nil {
		return tail(string(out)), "canceled", nil
	}
	var exited *exec.ExitError
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) && !errors.As(err, &exited) {
		return "", "", fmt.Errorf("wait for script: %w", err)
	}
	return tail(string(out)), fmt.Sprintf("exit %d", cmd.ProcessState.ExitCode()), nil
}
