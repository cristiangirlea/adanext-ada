package hrnexec

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cristiangirlea/adanext-ada/internal/proctree"
)

func setProcessAttributes(cmd *exec.Cmd) { proctree.Configure(cmd) }

func manageProcessTree(cmd *exec.Cmd) (kill, close func(), err error) { return proctree.Manage(cmd) }

const (
	lineLimit   = 5 << 20
	stderrLimit = 4 << 10
)

type Options struct {
	HRNPath    string
	HRNHome    string
	Grace      time.Duration
	WithoutEnv []string
}

type Task struct {
	Prompt  string `json:"prompt"`
	Dir     string `json:"dir,omitempty"`
	Backend string `json:"backend,omitempty"`
	Mode    string `json:"mode"`
	Timeout string `json:"timeout,omitempty"`
	Purpose string `json:"purpose,omitempty"`
}

type Event struct {
	Kind string `json:"kind"`
	Tool string `json:"tool"`
	Text string `json:"text"`
}

type Result struct {
	RunID        string `json:"-"`
	Status       string `json:"status"`
	Final        string `json:"final"`
	Backend      string `json:"backend"`
	Model        string `json:"model"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

type frame struct {
	Version   int     `json:"v"`
	Sequence  uint64  `json:"seq"`
	Type      string  `json:"type"`
	Protocol  string  `json:"protocol"`
	AttemptID string  `json:"attempt_id"`
	RunID     string  `json:"run_id"`
	Event     Event   `json:"event"`
	Result    *Result `json:"result"`
	Fatal     bool    `json:"fatal"`
	Error     struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type protocolReader struct {
	scanner *bufio.Scanner
	seq     uint64
}

func (r *protocolReader) next() (frame, error) {
	var line frame
	if !r.scanner.Scan() {
		if err := r.scanner.Err(); err != nil {
			return line, fmt.Errorf("read hrn protocol: %w", err)
		}
		return line, io.EOF
	}
	if err := json.Unmarshal(r.scanner.Bytes(), &line); err != nil {
		return line, fmt.Errorf("decode hrn protocol line: %w", err)
	}
	if line.Version != 1 {
		return line, fmt.Errorf("hrn protocol version %d, expected 1", line.Version)
	}
	if line.Sequence != r.seq+1 {
		return line, fmt.Errorf("hrn protocol seq %d, expected %d", line.Sequence, r.seq+1)
	}
	r.seq = line.Sequence
	return line, nil
}

type stderrTail []byte

func (tail *stderrTail) Write(p []byte) (int, error) {
	total := len(p)
	if total >= stderrLimit {
		*tail = append((*tail)[:0], p[total-stderrLimit:]...)
		return total, nil
	}
	drop := max(0, len(*tail)+total-stderrLimit)
	*tail = append((*tail)[:copy(*tail, (*tail)[drop:])], p...)
	return total, nil
}

type startMessage struct {
	Type      string `json:"type"`
	AttemptID string `json:"attempt_id"`
	Task      Task   `json:"task"`
}

type cancelMessage struct {
	Type      string `json:"type"`
	AttemptID string `json:"attempt_id"`
}

func Run(ctx context.Context, opts Options, task Task, onEvent func(Event)) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if task.Mode == "" {
		return Result{}, fmt.Errorf("hrn task mode is required")
	}

	if opts.Grace <= 0 {
		opts.Grace = 10 * time.Second
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return Result{}, fmt.Errorf("create hrn attempt ID: %w", err)
	}
	attemptID := "ada-" + hex.EncodeToString(random[:])

	cmd, err := hrnCommand(opts, "exec", "--protocol", "v1")
	if err != nil {
		return Result{}, err
	}
	var tail stderrTail
	cmd.Stderr = &tail
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Result{}, fmt.Errorf("open hrn stdin: %w", err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("open hrn stdout: %w", err)
	}
	defer stdout.Close()
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start hrn: %w", err)
	}
	killTree, closeTree, err := manageProcessTree(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return Result{}, withStderr(err, tail)
	}
	defer closeTree()

	finished := make(chan struct{})
	watcherDone := make(chan struct{})
	starting := make(chan struct{})
	startWritten := make(chan struct{})
	var forced atomic.Bool
	kill := sync.OnceFunc(func() {
		killTree()
		_ = stdout.Close()
	})
	go func() {
		defer close(watcherDone)
		watchCancellation(ctx, finished, starting, startWritten, opts.Grace, &forced, kill, func() {
			_ = json.NewEncoder(stdin).Encode(cancelMessage{Type: "cancel", AttemptID: attemptID})
		})
	}()
	waited := false
	defer func() {
		close(finished)
		if !waited {
			kill()
			_ = cmd.Wait()
		}
		<-watcherDone
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), lineLimit)
	reader := protocolReader{scanner: scanner}
	result, err := readRun(&reader, onEvent, attemptID, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		close(starting)
		defer close(startWritten)
		if err := json.NewEncoder(stdin).Encode(startMessage{Type: "start", AttemptID: attemptID, Task: task}); err != nil {
			return fmt.Errorf("write hrn start: %w", err)
		}
		return nil
	}, func() { _ = stdin.Close() })

	if err != nil {
		kill()
	}
	_ = stdin.Close()
	waitErr := cmd.Wait()
	waited = true
	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("hrn did not finish (%v): %w", err, ctx.Err())
		} else if errors.Is(err, io.EOF) {
			err = fmt.Errorf("hrn exited before result: %w", err)
		}
		return Result{}, withStderr(err, tail)
	}
	if forced.Load() && ctx.Err() != nil {
		return Result{}, withStderr(ctx.Err(), tail)
	}
	var exited *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exited) {
		return Result{}, withStderr(fmt.Errorf("wait for hrn: %w", waitErr), tail)
	}
	return result, nil
}

func hrnCommand(opts Options, args ...string) (*exec.Cmd, error) {
	// Go restores SYSTEMROOT in Windows child environments.
	if runtime.GOOS == "windows" && slices.ContainsFunc(opts.WithoutEnv, func(name string) bool {
		return strings.EqualFold(name, "SYSTEMROOT")
	}) {
		return nil, fmt.Errorf("cannot start hrn with SYSTEMROOT excluded from its Windows environment")
	}
	if opts.HRNPath == "" {
		opts.HRNPath = "hrn"
	}
	cmd := exec.Command(opts.HRNPath, args...)
	setProcessAttributes(cmd)
	if len(opts.WithoutEnv) > 0 {
		cmd.Env = slices.DeleteFunc(cmd.Environ(), func(entry string) bool {
			name, _, _ := strings.Cut(entry, "=")
			return name != "" && slices.ContainsFunc(opts.WithoutEnv, func(excluded string) bool {
				return name == excluded || runtime.GOOS == "windows" && strings.EqualFold(name, excluded)
			})
		})
	}
	if opts.HRNHome != "" {
		cmd.Env = append(cmd.Environ(), "HRN_HOME="+opts.HRNHome)
	}
	cmd.WaitDelay = time.Second
	return cmd, nil
}

func withStderr(err error, tail stderrTail) error {
	if len(tail) != 0 {
		return fmt.Errorf("%w (stderr: %s)", err, strings.TrimSpace(string(tail)))
	}
	return err
}

func watchCancellation(ctx context.Context, finished, starting, startWritten <-chan struct{}, grace time.Duration, forced *atomic.Bool, kill, sendCancel func()) {
	select {
	case <-finished:
		return
	case <-ctx.Done():
	}
	select {
	case <-starting:
	default:
		kill()
		return
	}
	// The grace timer must run even if the child stops reading its stdin.
	cancelDone := make(chan struct{})
	go func() {
		defer close(cancelDone)
		select {
		case <-finished:
		case <-startWritten:
			sendCancel()
		}
	}()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-finished:
	case <-timer.C:
		forced.Store(true)
		kill()
	}
	<-cancelDone
}

func readRun(r *protocolReader, onEvent func(Event), attemptID string, start func() error, closeInput func()) (Result, error) {
	hello, err := r.next()
	if err != nil {
		return Result{}, err
	}
	if hello.Type != "hello" || hello.Protocol != "v1" {
		return Result{}, fmt.Errorf("expected hrn hello with protocol v1")
	}
	if err := start(); err != nil {
		return Result{}, err
	}

	var runID, fatal string
	var result *Result
	for {
		line, err := r.next()
		if err == io.EOF && result != nil {
			return *result, nil
		}
		// A caller without an event callback would otherwise never learn why hrn stopped.
		if err == io.EOF && fatal != "" {
			return Result{}, fmt.Errorf("hrn reported a fatal error (%s) and ended without a result: %w", fatal, err)
		}
		if err != nil {
			return Result{}, err
		}
		if result != nil {
			return Result{}, fmt.Errorf("unexpected hrn message after result")
		}
		switch line.Type {
		case "rejected":
			return Result{}, fmt.Errorf("hrn rejected the task (%s): %s", line.Error.Code, line.Error.Message)
		case "accepted":
			if runID != "" || line.AttemptID != attemptID || line.RunID == "" {
				return Result{}, fmt.Errorf("invalid hrn accepted message")
			}
			runID = line.RunID
		case "event", "result":
			if runID == "" || line.RunID != runID || line.AttemptID != attemptID {
				return Result{}, fmt.Errorf("unexpected hrn %s identifiers", line.Type)
			}
			if line.Type == "event" {
				if onEvent != nil {
					onEvent(line.Event)
				}
				continue
			}
			if line.Result == nil || line.Result.Status == "" {
				return Result{}, fmt.Errorf("hrn result is missing its status")
			}
			result = line.Result
			result.RunID = runID
			closeInput()
		case "error":
			text := fmt.Sprintf("%s: %s", line.Error.Code, line.Error.Message)
			if line.Fatal {
				fatal = text
				text = "fatal: " + text
			}
			if onEvent != nil {
				onEvent(Event{Kind: "error", Text: text})
			}
		default:
			return Result{}, fmt.Errorf("unexpected hrn message type %q", line.Type)
		}
	}
}
