// Package approval asks the current surface for consent and records its decision.
package approval

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/cristiangirlea/adanext-ada/memory"
)

const DefaultTimeout = 10 * time.Minute

type Request struct {
	ID      string    `json:"id"`
	User    string    `json:"user"`
	Session string    `json:"session"`
	Tool    string    `json:"tool"`
	Effect  string    `json:"effect"`
	Summary string    `json:"summary"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
}

type Decision struct {
	Approved bool      `json:"approved"`
	By       string    `json:"by"`
	At       time.Time `json:"at"`
	Reason   string    `json:"reason"`
}

type Approver interface {
	Ask(context.Context, Request) (Decision, error)
}

type approverKey struct{}
type sessionKey struct{}

func WithApprover(ctx context.Context, a Approver) context.Context {
	return context.WithValue(ctx, approverKey{}, a)
}

func ApproverFrom(ctx context.Context) (Approver, bool) {
	a, ok := ctx.Value(approverKey{}).(Approver)
	return a, ok
}

func WithSession(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, sessionKey{}, id)
}

func NewRequest(ctx context.Context, tool, effect, summary string, timeout time.Duration) Request {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	now := time.Now().UTC()
	user, _ := memory.UserFrom(ctx)
	session, _ := ctx.Value(sessionKey{}).(string)
	return Request{
		ID: rand.Text()[:12], User: user, Session: session, Tool: tool,
		Effect: effect, Summary: summary, Created: now, Expires: now.Add(timeout),
	}
}

func Decide(ctx context.Context, r Request) (Decision, error) {
	a, ok := ApproverFrom(ctx)
	if !ok {
		return Decision{Reason: "no approver", At: time.Now().UTC()}, nil
	}
	ask, cancel := context.WithDeadline(ctx, r.Expires)
	defer cancel()
	type reply struct {
		d   Decision
		err error
	}
	result := make(chan reply, 1)
	go func() {
		d, err := a.Ask(ask, r)
		result <- reply{d, err}
	}()
	var answer reply
	select {
	case answer = <-result:
	case <-ask.Done():
	}
	d := answer.d
	// The deadline can win before the surface returns its decision.
	if named, ok := a.(interface{ SurfaceName() string }); ok && d.By == "" {
		d.By = named.SurfaceName()
	}
	now := time.Now().UTC()
	d.At = now
	switch {
	case !now.Before(r.Expires) || errors.Is(ask.Err(), context.DeadlineExceeded):
		d.Approved = false
		d.Reason = "expired"
	case ask.Err() == nil && answer.err == nil && !d.Approved && d.Reason == "typed ahead":
		// Keep this refusal distinguishable without forwarding arbitrary surface text.
		return d, nil
	case ask.Err() != nil || !d.Approved || answer.err != nil:
		d.Approved = false
		d.Reason = "denied"
	default:
		d.Reason = "approved"
	}
	return d, answer.err
}

type Record struct {
	Request  Request  `json:"request"`
	Decision Decision `json:"decision"`
}

type Recorder struct{ dir string }

var appendMu sync.Mutex

func Log(dataDir string) *Recorder { return &Recorder{dir: dataDir} }

func logPath(session string) (string, error) {
	if session == "" {
		session = "none"
	}
	if !memory.ValidSessionID(session) {
		return "", fmt.Errorf("invalid approval session id")
	}
	return filepath.Join("approvals", session+".jsonl"), nil
}

func (l *Recorder) Record(req Request, d Decision) error {
	appendMu.Lock()
	defer appendMu.Unlock()
	path, err := logPath(req.Session)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(l.dir, 0700); err != nil {
		return fmt.Errorf("create approval data directory: %w", err)
	}
	root, err := os.OpenRoot(l.dir)
	if err != nil {
		return fmt.Errorf("open approval data directory: %w", err)
	}
	defer root.Close()
	if err := root.MkdirAll("approvals", 0700); err != nil {
		return fmt.Errorf("create approvals directory: %w", err)
	}
	f, err := root.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open approval log: %w", err)
	}
	err = repairTail(root, path, f)
	if err == nil {
		err = json.NewEncoder(f).Encode(Record{req, d})
	}
	if err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return fmt.Errorf("append approval log: %w", err)
	}
	return nil
}

// An interrupted append can leave a partial line after the last complete decision.
func repairTail(root *os.Root, path string, f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	end := info.Size()
	var buf [4096]byte
	for pos := end; pos > 0; {
		size := min(pos, int64(len(buf)))
		pos -= size
		if _, err := f.ReadAt(buf[:size], pos); err != nil {
			return err
		}
		at := bytes.LastIndexByte(buf[:size], '\n')
		if at < 0 && pos > 0 {
			continue
		}
		offset := pos + int64(at) + 1
		if offset == end {
			return nil
		}
		// Windows append handles cannot truncate the incomplete tail.
		repair, err := root.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		return errors.Join(repair.Truncate(offset), repair.Close())
	}
	return nil
}

// Latest includes sessionless records belonging to the same user.
func (l *Recorder) Latest(user string, sessions []string, n int) ([]Record, error) {
	if !memory.ValidIdentity(user) || n < 1 {
		return nil, fmt.Errorf("approval history needs a valid user and positive limit")
	}
	root, err := os.OpenRoot(l.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open approval data directory: %w", err)
	}
	defer root.Close()
	var latest []Record
	for _, session := range append(slices.Clone(sessions), "") {
		path, err := logPath(session)
		if err != nil {
			return nil, err
		}
		f, err := root.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("open approval log: %w", err)
		}
		err = readRecords(f, func(r Record) {
			if r.Request.User != user || r.Request.Session != session {
				return
			}
			// Only retain the requested number, even for a long-lived session.
			i, _ := slices.BinarySearchFunc(latest, r, func(a, b Record) int { return b.Decision.At.Compare(a.Decision.At) })
			if i < n {
				latest = slices.Insert(latest, i, r)
				if len(latest) > n {
					latest = latest[:n]
				}
			}
		})
		if err = errors.Join(err, f.Close()); err != nil {
			return nil, fmt.Errorf("read approval log %s: %w", path, err)
		}
	}
	return latest, nil
}

func readRecords(f io.Reader, visit func(Record)) error {
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadBytes('\n')
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		var record Record
		if err := json.Unmarshal(line, &record); err != nil {
			return err
		}
		visit(record)
	}
}
