package memory

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type Pairing struct {
	Used    bool      `json:"used,omitempty"`
	Channel string    `json:"channel"`
	Chat    string    `json:"chat"`
	Sender  string    `json:"sender"`
	Name    string    `json:"name"`
	Expires time.Time `json:"expires"`
}

// Pair and unpair run in another process while serve owns the session writer lock.
func (s *Store) withIdentities(change func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var file *os.File
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for {
		file, err = s.lockFile("identities.lock")
		if err == nil {
			break
		}
		if !errors.Is(err, errDirectoryBusy) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer releaseLock(file)
	return change()
}
func (s *Store) Unlink(channel, sender string) error {
	if !ValidIdentity(channel) || !ValidIdentity(sender) || channel == "cli" {
		return fmt.Errorf("invalid channel identity")
	}
	return s.withIdentities(func() error {
		codes, err := s.pairings(time.Now())
		if err != nil {
			return err
		}
		for code, p := range codes {
			if !p.Used && p.Channel == channel && p.Sender == sender {
				p.Used = true
				codes[code] = p
				if err := s.writeJSON("pairing.json", codes); err != nil {
					return err
				}
				break
			}
		}
		return s.setLink(channel, sender, "")
	})
}
func (s *Store) pairings(now time.Time) (map[string]Pairing, error) {
	codes := map[string]Pairing{}
	if err := readJSON(s.root, "pairing.json", &codes); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if codes == nil {
		return nil, fmt.Errorf("invalid pairing.json: expected an object")
	}
	for code, p := range codes {
		if !now.Before(p.Expires) {
			delete(codes, code)
		}
	}
	return codes, nil
}
func (s *Store) OfferPairing(p Pairing) (string, error) {
	return s.offerPairing(p, time.Now().UTC())
}
func (s *Store) offerPairing(p Pairing, now time.Time) (string, error) {
	if p.Channel == "cli" || !validOrigin(p.Channel, p.Chat, p.Sender) {
		return "", fmt.Errorf("invalid pairing origin")
	}
	var code string
	err := s.withIdentities(func() error {
		codes, err := s.pairings(now)
		if err != nil {
			return err
		}
		for _, existing := range codes {
			if existing.Channel == p.Channel && existing.Sender == p.Sender {
				return nil
			}
		}
		const alphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
		var raw [6]byte
		for {
			if _, err := rand.Read(raw[:]); err != nil {
				return fmt.Errorf("generate pairing code: %w", err)
			}
			for i, b := range raw {
				raw[i] = alphabet[int(b)%len(alphabet)]
			}
			code = string(raw[:])
			if _, used := codes[code]; !used {
				break
			}
		}
		p.Expires = now.Add(10 * time.Minute)
		codes[code] = p
		return s.writeJSON("pairing.json", codes)
	})
	return code, err
}
func (s *Store) Pair(code, owner string) (Pairing, error) {
	var p Pairing
	if !ValidIdentity(owner) {
		return p, fmt.Errorf("invalid owner")
	}
	code = strings.ToUpper(code)
	err := s.withIdentities(func() error {
		codes, err := s.pairings(time.Now())
		if err != nil {
			return err
		}
		var ok bool
		p, ok = codes[code]
		if !ok || p.Used || p.Channel == "cli" || !validOrigin(p.Channel, p.Chat, p.Sender) {
			return fmt.Errorf("unknown or expired pairing code")
		}
		p.Used = true
		codes[code] = p
		// Consume before linking so a failed update can never leave a reusable grant.
		if err := s.writeJSON("pairing.json", codes); err != nil {
			return err
		}
		path, err := allowPath(p.Channel)
		if err != nil {
			return err
		}
		managed, err := s.allowLinks(path)
		if err != nil {
			return err
		}
		if i := slices.Index(managed, p.Sender); i >= 0 {
			if err := s.writeJSON(path, slices.Delete(managed, i, i+1)); err != nil {
				return err
			}
		}
		return s.setLink(p.Channel, p.Sender, owner)
	})
	return p, err
}

func allowPath(channel string) (string, error) {
	if !filepath.IsLocal(channel) || filepath.Base(channel) != channel || channel == "." || channel == "cli" {
		return "", fmt.Errorf("invalid allow list channel")
	}
	return filepath.Join("channels", channel, "allow.json"), nil
}

func (s *Store) allowLinks(path string) ([]string, error) {
	var senders []string
	err := readJSON(s.root, path, &senders)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return senders, err
}

// SyncAllow records ownership before granting access and revokes access before
// dropping the ownership record. Either step can be retried after a crash.
func (s *Store) SyncAllow(channel string, allow []string, owner string) error {
	path, err := allowPath(channel)
	if err != nil {
		return err
	}
	if !ValidIdentity(owner) {
		return fmt.Errorf("invalid owner")
	}
	return s.withIdentities(func() error {
		managed, err := s.allowLinks(path)
		if err != nil {
			return err
		}
		links, err := s.links()
		if err != nil {
			return err
		}
		prefix := channel + "\x00"
		wanted := make(map[string]bool, len(allow))
		for _, sender := range allow {
			if !ValidIdentity(sender) {
				return fmt.Errorf("invalid allowed sender")
			}
			wanted[sender] = true
		}
		owned := make(map[string]bool, len(managed))
		before := len(links)
		for _, sender := range managed {
			owned[sender] = true
			if !wanted[sender] {
				delete(links, prefix+sender)
			}
		}
		if len(links) < before {
			if err := s.writeJSON("identities.json", links); err != nil {
				return err
			}
		}
		next := make([]string, 0, len(allow))
		changed := false
		for _, sender := range allow {
			key := prefix + sender
			if wanted[sender] && (links[key] == "" || owned[sender]) {
				next = append(next, sender)
				if links[key] != owner {
					links[key] = owner
					changed = true
				}
			}
			delete(wanted, sender)
		}
		if !slices.Equal(managed, next) {
			if err = s.root.MkdirAll(filepath.Dir(path), 0700); err != nil {
				return err
			}
			if err = s.writeJSON(path, next); err != nil {
				return err
			}
		}
		if changed {
			return s.writeJSON("identities.json", links)
		}
		return nil
	})
}
