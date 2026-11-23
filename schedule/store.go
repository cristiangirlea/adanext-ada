package schedule

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/cristiangirlea/adanext-ada/memory"
)

type Target struct {
	Channel string `json:"channel,omitempty"`
	Chat    string `json:"chat,omitempty"`
}

func ParseTarget(s string) (Target, error) {
	if s == "" {
		return Target{}, nil
	}
	ch, chat, ok := strings.Cut(s, ":")
	if !ok || strings.TrimSpace(ch) == "" || strings.TrimSpace(chat) == "" || strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return Target{}, fmt.Errorf("target must be CHANNEL:CHAT")
	}
	return Target{ch, chat}, nil
}
func (t Target) String() string {
	if t == (Target{}) {
		return "local only"
	}
	return t.Channel + ":" + t.Chat
}

type Budget struct {
	MaxIterations int           `json:"max_iterations"`
	Timeout       time.Duration `json:"timeout"`
}
type Job struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Spec    string    `json:"spec"`
	Kind    string    `json:"kind"`
	Prompt  string    `json:"prompt,omitempty"`
	Argv    []string  `json:"argv,omitempty"`
	Target  Target    `json:"target"`
	Budget  Budget    `json:"budget"`
	Created time.Time `json:"created"`
	LastRun time.Time `json:"last_run"`
	Enabled bool      `json:"enabled"`
}
type Run struct {
	Job       string    `json:"job"`
	Started   time.Time `json:"started"`
	Finished  time.Time `json:"finished"`
	Status    string    `json:"status"`
	Output    string    `json:"output"`
	Delivered bool      `json:"delivered"`
}
type Store struct{ Memory *memory.Store }

func (j Job) validate() error {
	switch j.Kind {
	case "script":
		if len(j.Argv) == 0 || j.Argv[0] == "" || j.Prompt != "" {
			return fmt.Errorf("script requires argv and no prompt")
		}
		for _, arg := range j.Argv {
			if strings.ContainsRune(arg, 0) {
				return fmt.Errorf("script argument contains NUL")
			}
		}
	case "prompt":
		if strings.TrimSpace(j.Prompt) == "" || len(j.Argv) > 0 {
			return fmt.Errorf("prompt job requires text and no argv")
		}
	default:
		return fmt.Errorf("unknown job kind %q", j.Kind)
	}
	if j.Target != (Target{}) {
		target, err := ParseTarget(j.Target.String())
		if err != nil || target != j.Target {
			return fmt.Errorf("invalid delivery target")
		}
	}
	if j.Budget.Timeout <= 0 || j.Budget.MaxIterations <= 0 {
		return fmt.Errorf("job budget must be positive")
	}
	return nil
}

func readJobs(root *os.Root) ([]Job, error) {
	data, err := root.ReadFile("jobs.json")
	if errors.Is(err, os.ErrNotExist) {
		return []Job{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read jobs: %w", err)
	}
	var jobs []Job
	if err = json.Unmarshal(data, &jobs); err != nil {
		return nil, fmt.Errorf("decode jobs: %w", err)
	}
	return jobs, nil
}
func (s Store) List() ([]Job, error) {
	var jobs []Job
	err := s.Memory.WithScheduleFiles(func(root *os.Root) error {
		var err error
		jobs, err = readJobs(root)
		return err
	})
	return jobs, err
}
func (s Store) Add(j Job, now time.Time) (Job, error) {
	if j.Budget.MaxIterations == 0 {
		j.Budget.MaxIterations = 8
	}
	if j.Budget.Timeout == 0 {
		j.Budget.Timeout = 10 * time.Minute
	}
	if err := j.validate(); err != nil {
		return Job{}, err
	}
	if _, err := Next(j.Spec, now); err != nil {
		return Job{}, err
	}
	j.ID, j.Created, j.Enabled = rand.Text()[:12], now.UTC(), true
	j.LastRun = time.Time{}
	if j.Name == "" {
		j.Name = j.ID
	}
	err := s.Memory.WithScheduleFiles(func(root *os.Root) error {
		jobs, err := readJobs(root)
		if err != nil {
			return err
		}
		return memory.WriteJSON(root, "jobs.json", append(jobs, j))
	})
	return j, err
}

var errUnknownJob = errors.New("unknown schedule job")

func (s Store) update(id string, change func(*Job) (save, remove bool, err error)) error {
	return s.Memory.WithScheduleFiles(func(root *os.Root) error {
		jobs, err := readJobs(root)
		if err != nil {
			return err
		}
		for i := range jobs {
			if jobs[i].ID == id {
				save, remove, err := change(&jobs[i])
				if err != nil || !save {
					return err
				}
				if remove {
					jobs = slices.Delete(jobs, i, i+1)
				}
				return memory.WriteJSON(root, "jobs.json", jobs)
			}
		}
		return fmt.Errorf("%w %q", errUnknownJob, id)
	})
}
func (s Store) Remove(id string) error {
	return s.update(id, func(*Job) (bool, bool, error) { return true, true, nil })
}
func (s Store) SetEnabled(id string, enabled bool) error {
	return s.update(id, func(j *Job) (bool, bool, error) {
		j.Enabled = enabled
		return true, false, nil
	})
}

func (j Job) due(now time.Time) (bool, error) {
	if !j.Enabled {
		return false, nil
	}
	from := j.LastRun
	if from.IsZero() {
		from = j.Created
	}
	next, err := Next(j.Spec, from.In(now.Location()))
	return !next.After(now), err
}

// claim advances only the selected job and rechecks edits made since the tick began.
func (s Store) claim(id string, now time.Time, manual bool) (Job, bool, error) {
	var claimed Job
	err := s.update(id, func(j *Job) (bool, bool, error) {
		if !manual {
			due, err := j.due(now)
			if err != nil || !due {
				return false, false, err
			}
		}
		if err := j.validate(); err != nil {
			return false, false, err
		}
		j.LastRun = now.UTC()
		claimed = *j
		return true, false, nil
	})
	return claimed, claimed.ID != "", err
}

func (s Store) record(run Run) error {
	return s.Memory.WithScheduleFiles(func(root *os.Root) error {
		f, err := root.OpenFile("runs.jsonl", os.O_RDWR|os.O_CREATE, 0600)
		if err != nil {
			return fmt.Errorf("open run history: %w", err)
		}
		defer f.Close()
		end, err := f.Seek(0, io.SeekEnd)
		if err != nil {
			return err
		}
		// Recover an interrupted last line while holding the same lock used by readers.
		var buf [4096]byte
		for pos := end; pos > 0; {
			size := min(pos, int64(len(buf)))
			pos -= size
			if _, err = f.ReadAt(buf[:size], pos); err != nil {
				return err
			}
			i := bytes.LastIndexByte(buf[:size], '\n')
			if i < 0 && pos > 0 {
				continue
			}
			good := pos + int64(i+1)
			if good < end {
				if err = f.Truncate(good); err != nil {
					return err
				}
				if _, err = f.Seek(good, io.SeekStart); err != nil {
					return err
				}
			}
			break
		}
		if err = json.NewEncoder(f).Encode(run); err != nil {
			return fmt.Errorf("append run: %w", err)
		}
		if err = f.Sync(); err != nil {
			return fmt.Errorf("sync run: %w", err)
		}
		return nil
	})
}

func (s Store) Runs(n int) ([]Run, error) {
	if n < 1 {
		return nil, fmt.Errorf("run count must be positive")
	}
	var recent []Run
	index := 0
	err := s.Memory.WithScheduleFiles(func(root *os.Root) error {
		f, err := root.Open("runs.jsonl")
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		defer f.Close()
		reader := bufio.NewReader(f)
		for {
			line, err := reader.ReadBytes('\n')
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			var r Run
			if err = json.Unmarshal(line, &r); err != nil {
				return fmt.Errorf("decode run: %w", err)
			}
			if len(recent) < n {
				recent = append(recent, r)
			} else {
				recent[index] = r
				index = (index + 1) % n
			}
		}
		return nil
	})
	slices.Reverse(recent[:index])
	slices.Reverse(recent[index:])
	return recent, err
}
