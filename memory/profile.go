package memory

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type Fact struct {
	ID     string    `json:"id"`
	Text   string    `json:"text"`
	Source string    `json:"source"`
	Added  time.Time `json:"added"`
}

type profile struct {
	Facts []Fact `json:"facts"`
}

var plainProfileID = regexp.MustCompile(`^[a-z0-9_-]+$`)
var deviceName = regexp.MustCompile(`^(con|prn|aux|nul|com[1-9]|lpt[1-9])$`)

func profilePath(user string) (string, error) {
	name, err := UserFilename(user)
	return filepath.Join("profiles", name), err
}

// UserFilename keeps per-user files distinct even on case-insensitive filesystems.
func UserFilename(user string) (string, error) {
	if !ValidIdentity(user) {
		return "", fmt.Errorf("invalid profile user")
	}
	// Encode names that could escape the directory or alias another identity on disk.
	if !plainProfileID.MatchString(user) || deviceName.MatchString(user) {
		user = "~" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte(user))
	}
	return user + ".json", nil
}

func (s *Store) profile(path string) (profile, error) {
	var p profile
	err := readJSON(s.root, path, &p)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return profile{}, err
	}
	if err := p.validate(); err != nil {
		return profile{}, fmt.Errorf("read profile: %w", err)
	}
	return p, nil
}

func (p profile) validate() error {
	if p.Facts == nil {
		return fmt.Errorf("profile requires a facts array")
	}
	if len(p.Facts) > 40 {
		return fmt.Errorf("profile limit is 40 facts")
	}
	total := 0
	for _, f := range p.Facts {
		if err := ValidateFactText(f.Text); err != nil {
			return err
		}
		total += len(f.Text)
	}
	if total > 4000 {
		return fmt.Errorf("profile text exceeds 4000 bytes")
	}
	return nil
}

func ValidateFactText(text string) error {
	if strings.TrimSpace(text) == "" || !utf8.ValidString(text) || strings.IndexFunc(text, unicode.IsControl) >= 0 || strings.ContainsAny(text, "\u2028\u2029") {
		return fmt.Errorf("fact must contain nonempty single-line text without control characters")
	}
	if len(text) > 300 {
		return fmt.Errorf("fact exceeds 300 bytes")
	}
	return nil
}

func (s *Store) AddFact(user, text, source string) (Fact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := profilePath(user)
	if err != nil {
		return Fact{}, err
	}
	file, err := s.lockFile("profile.lock")
	if err != nil {
		return Fact{}, err
	}
	defer releaseLock(file)
	p, err := s.profile(path)
	if err != nil {
		return Fact{}, err
	}
	f := Fact{rand.Text(), text, source, time.Now().UTC()}
	p.Facts = append(p.Facts, f)
	if err := p.validate(); err != nil {
		return Fact{}, err
	}
	if err := s.writeJSON(path, p); err != nil {
		return Fact{}, err
	}
	return f, nil
}

func (s *Store) RemoveFact(user, id string) error {
	return s.removeFact(user, id, "")
}

// RemoveSourcedFact makes rollback idempotent without removing another source's fact.
func (s *Store) RemoveSourcedFact(user, id, source string) error {
	if source == "" {
		return fmt.Errorf("expected a fact source")
	}
	return s.removeFact(user, id, source)
}

func (s *Store) removeFact(user, id, source string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := profilePath(user)
	if err != nil {
		return err
	}
	file, err := s.lockFile("profile.lock")
	if err != nil {
		return err
	}
	defer releaseLock(file)
	p, err := s.profile(path)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(p.Facts, func(f Fact) bool { return f.ID == id })
	if i < 0 {
		if source != "" {
			return nil
		}
		return fmt.Errorf("unknown fact id %q", id)
	}
	if source != "" && p.Facts[i].Source != source {
		return fmt.Errorf("fact source does not match proposal")
	}
	p.Facts = slices.Delete(p.Facts, i, i+1)
	return s.writeJSON(path, p)
}
func (s *Store) ReadFacts(user string) ([]Fact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := profilePath(user)
	if err != nil {
		return nil, err
	}
	p, err := s.profile(path)
	return p.Facts, err
}
func (s *Store) Facts(user string) []Fact { facts, _ := s.ReadFacts(user); return facts }
func (s *Store) Render(user string) string {
	facts := s.Facts(user)
	if len(facts) == 0 {
		return ""
	}
	var text strings.Builder
	text.WriteString("Facts about the user (kept by the user):\n")
	for _, f := range facts {
		fmt.Fprintf(&text, "- %s\n", f.Text)
	}
	return text.String()
}
