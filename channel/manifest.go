// Package channel hosts Ada's stdio channel plugins.
package channel

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/cristiangirlea/adanext-ada/memory"
	"github.com/cristiangirlea/adanext-ada/tool"
)

type Settings struct {
	Name    string   `json:"name"`
	Path    string   `json:"path"`
	Enabled bool     `json:"enabled"`
	Allow   []string `json:"allow"`
	Groups  bool     `json:"groups"`
}

var channelName = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (s Settings) Validate() error {
	if !channelName.MatchString(s.Name) || !filepath.IsLocal(s.Name) || s.Name == "cli" {
		return fmt.Errorf("invalid or reserved channel name %q", s.Name)
	}
	if s.Path == "" {
		return fmt.Errorf("channel %s requires a binary path", s.Name)
	}
	for _, id := range s.Allow {
		if !memory.ValidIdentity(id) {
			return fmt.Errorf("channel %s: invalid allowed sender", s.Name)
		}
	}
	return nil
}

type Manifest struct {
	Name      string   `json:"name"`
	Label     string   `json:"label"`
	SecretEnv string   `json:"secret_env"`
	Hosts     []string `json:"hosts"`
}

func (s Settings) Manifest() (Manifest, error) {
	var m Manifest
	f, err := os.Open(filepath.Join(filepath.Dir(s.Path), "channel.json"))
	if err != nil {
		return m, fmt.Errorf("open channel.json: %w", err)
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return m, fmt.Errorf("decode channel.json: %w", err)
	}
	if d.Decode(new(any)) != io.EOF {
		return m, fmt.Errorf("channel.json must contain one object")
	}
	if m.Name != s.Name {
		return m, fmt.Errorf("channel.json names %q, expected %q", m.Name, s.Name)
	}
	if m.SecretEnv != "" && !envName.MatchString(m.SecretEnv) {
		return m, fmt.Errorf("invalid secret_env in channel.json")
	}
	return m, nil
}
func (m Manifest) environment(excluded []string) ([]string, error) {
	blocked := func(name string) bool {
		return slices.ContainsFunc(excluded, func(k string) bool { return k == name || runtime.GOOS == "windows" && strings.EqualFold(k, name) })
	}
	if m.SecretEnv != "" && blocked(m.SecretEnv) || runtime.GOOS == "windows" && blocked("SYSTEMROOT") {
		return nil, fmt.Errorf("channel environment conflicts with a provider key setting")
	}
	env := []string{}
	names := tool.InheritedEnvNames()
	if m.SecretEnv != "" {
		names = append(names, m.SecretEnv)
	}
	for _, name := range names {
		if !blocked(name) {
			if value, ok := os.LookupEnv(name); ok {
				env = append(env, name+"="+value)
			}
		}
	}
	return env, nil
}
