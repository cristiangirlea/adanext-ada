package memory

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

type Hit struct {
	Kind    string
	Ref     string
	Time    time.Time
	Score   float64
	Excerpt string
}

func HRNRunsDir(getenv func(string) string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	if home := getenv("HRN_HOME"); home != "" {
		return filepath.Join(home, "runs")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".hrn", "runs")
}

var words = regexp.MustCompile(`[\pL\pN]+`)

func tokens(ctx context.Context, text string, visit func(string, int)) {
	// Scan incrementally because even a single tool result can contain many words.
	for offset := 0; offset < len(text) && ctx.Err() == nil; {
		loc := words.FindStringIndex(text[offset:])
		if loc == nil {
			break
		}
		start, end := offset+loc[0], offset+loc[1]
		word := text[start:end]
		if _, size := utf8.DecodeRuneInString(word); size < len(word) {
			visit(strings.ToLower(word), start)
		}
		offset = end
	}
}

func excerpt(text string, pos int) string {
	start := max(0, pos-100)
	for start < pos && !utf8.RuneStart(text[start]) {
		start++
	}
	end := min(start+300, len(text))
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	return strings.Clone(strings.Join(strings.Fields(text[start:end]), " "))
}

type candidate struct {
	hit Hit
	tf  map[string]int
}

func (s *Store) Search(user, query string, limit int, hrnRuns string) ([]Hit, error) {
	return s.SearchContext(context.Background(), user, query, limit, hrnRuns)
}

func (s *Store) SearchContext(ctx context.Context, user, query string, limit int, hrnRuns string) ([]Hit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !ValidIdentity(user) {
		return nil, fmt.Errorf("invalid search user")
	}
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("search query is empty")
	}
	df := map[string]int{}
	tokens(ctx, query, func(word string, _ int) { df[word] = 0 })
	if len(df) == 0 {
		return nil, ctx.Err()
	}
	var matches []candidate
	n := 0
	add := func(hit Hit, text string) {
		n++
		var tf map[string]int
		tokens(ctx, text, func(word string, pos int) {
			if _, ok := df[word]; !ok {
				return
			}
			if tf == nil {
				tf = map[string]int{}
				hit.Excerpt = excerpt(text, pos)
			}
			if tf[word] == 0 {
				df[word]++
			}
			tf[word]++
		})
		if tf != nil {
			matches = append(matches, candidate{hit, tf})
		}
	}
	s.mu.Lock()
	err := s.sessionDocuments(ctx, user, add)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := runDocuments(ctx, hrnRuns, add); err != nil {
		return nil, err
	}
	return rankHits(ctx, matches, df, n, limit)
}

func rankHits(ctx context.Context, matches []candidate, df map[string]int, n, limit int) ([]Hit, error) {
	if limit <= 0 {
		limit = 8
	}
	limit = min(limit, 20)
	hits := make([]Hit, 0, limit+1)
	for _, m := range matches {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// A fixed term order keeps equivalent scores identical after floating-point addition.
		for _, word := range slices.Sorted(maps.Keys(m.tf)) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			m.hit.Score += (1 + math.Log(float64(m.tf[word]))) * math.Log1p(float64(n)/float64(df[word]))
		}
		at := sort.Search(len(hits), func(i int) bool {
			h := hits[i]
			return cmp.Or(cmp.Compare(m.hit.Score, h.Score), m.hit.Time.Compare(h.Time), strings.Compare(h.Ref, m.hit.Ref)) > 0
		})
		if at < limit {
			hits = slices.Insert(hits, at, m.hit)
			hits = hits[:min(len(hits), limit)]
		}
	}
	return hits, ctx.Err()
}

func (s *Store) sessionDocuments(ctx context.Context, user string, add func(Hit, string)) error {
	dirs, err := fs.ReadDir(s.root.FS(), "sessions")
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}
	for _, d := range dirs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !d.IsDir() || !sessionID.MatchString(d.Name()) {
			continue
		}
		id := d.Name()
		session, err := s.get(id)
		if err != nil {
			return err
		}
		if session.User != user {
			continue
		}
		err = s.visit(session, func(i int, r record) bool {
			if ctx.Err() != nil {
				return false
			}
			var blocks []string
			for _, b := range r.Message.Content {
				if b.Type == "text" || b.Type == "tool_result" {
					blocks = append(blocks, b.Text)
				}
			}
			add(Hit{Kind: "session", Ref: fmt.Sprintf("session:%s#%d", id, i), Time: r.Time}, strings.Join(blocks, "\n"))
			return true
		})
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}

func runDocuments(ctx context.Context, dir string, add func(Hit, string)) error {
	if dir == "" {
		return nil
	}
	root, err := os.OpenRoot(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open hrn runs: %w", err)
	}
	defer root.Close()
	dirs, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return fmt.Errorf("list hrn runs: %w", err)
	}
	count := 0
	for _, d := range slices.Backward(dirs) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !d.IsDir() {
			continue
		}
		if count == 500 {
			break
		}
		count++
		var task struct{ Prompt string }
		var result struct{ Final, Status string }
		taskTime, taskOK := readRunFile(root, filepath.Join(d.Name(), "task.json"), &task)
		resultTime, resultOK := readRunFile(root, filepath.Join(d.Name(), "result.json"), &result)
		if !taskOK && !resultOK {
			continue
		}
		var parts []string
		if taskOK {
			parts = append(parts, task.Prompt)
		}
		if resultOK {
			parts = append(parts, result.Final, result.Status)
		}
		if resultTime.Before(taskTime) {
			resultTime = taskTime
		}
		add(Hit{Kind: "hrn_run", Ref: "hrn:" + d.Name(), Time: resultTime.UTC()}, strings.Join(parts, "\n"))
	}
	return nil
}

func readRunFile(root *os.Root, path string, value any) (time.Time, bool) {
	const limit = 1 << 20
	info, err := root.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return time.Time{}, false
	}
	f, err := root.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || len(data) > limit || json.Unmarshal(data, value) != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}
