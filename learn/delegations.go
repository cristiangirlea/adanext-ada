package learn

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"time"

	"github.com/cristiangirlea/adanext-ada/memory"
)

var runID = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[0-9a-f]{6}$`)

const delegationFile = "delegations.jsonl"

type delegation struct {
	RunID   string    `json:"run_id"`
	User    string    `json:"user"`
	Session string    `json:"session"`
	Time    time.Time `json:"time"`
}

func (d delegation) valid() bool {
	return runID.MatchString(d.RunID) && memory.ValidIdentity(d.User) && memory.ValidSessionID(d.Session) && !d.Time.IsZero()
}

func (s Store) RecordDelegation(id, user, session string) error {
	return s.Memory.WithLearningFiles(func(root *os.Root) error {
		d := delegation{id, user, session, time.Now().UTC()}
		if !d.valid() {
			return fmt.Errorf("invalid delegated run ID, user or session")
		}
		f, err := root.OpenFile(delegationFile, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := trimDelegationTail(f); err != nil {
			return err
		}
		// Windows requires a writable handle to truncate a torn tail before appending.
		if _, err := f.Seek(0, io.SeekEnd); err != nil {
			return err
		}
		if err := json.NewEncoder(f).Encode(d); err != nil {
			return err
		}
		return f.Sync()
	})
}

// A crash can leave an unterminated record that the next append would otherwise complete.
func trimDelegationTail(f *os.File) error {
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	var buf [4096]byte
	for pos := end; pos > 0; {
		n := min(pos, int64(len(buf)))
		pos -= n
		if _, err := f.ReadAt(buf[:n], pos); err != nil {
			return err
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			offset := pos + int64(i) + 1
			if offset == end {
				return nil
			}
			return f.Truncate(offset)
		}
	}
	if end > 0 {
		return f.Truncate(0)
	}
	return nil
}

func pendingDelegations(root *os.Root, reviewed map[string]bool, failures map[string]int, batchSize int) ([]string, error) {
	f, err := root.Open(delegationFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	reader := bufio.NewReader(f)
	// Keep at most one batch of retries while looking past them for fresh runs.
	var runs, retries []string
	for len(runs) < batchSize {
		line, err := reader.ReadBytes('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		var d delegation
		if _, err := object(line, "run_id", "user", "session", "time"); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(line, &d); err != nil || !d.valid() {
			return nil, fmt.Errorf("invalid delegation journal record")
		}
		if reviewed[d.RunID] || slices.Contains(runs, d.RunID) {
			continue
		}
		if _, tried := failures[d.RunID]; tried {
			if len(retries) < batchSize && !slices.Contains(retries, d.RunID) {
				retries = append(retries, d.RunID)
			}
		} else {
			runs = append(runs, d.RunID)
		}
	}
	return append(runs, retries[:min(len(retries), batchSize-len(runs))]...), nil
}
