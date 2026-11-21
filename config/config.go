package config

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cristiangirlea/adanext-ada/hrnexec"
	"github.com/cristiangirlea/adanext-ada/loop"
	"github.com/cristiangirlea/adanext-ada/provider"
	"github.com/cristiangirlea/adanext-ada/tool"
)

type Settings struct {
	Providers      []provider.Config `json:"providers"`
	ChatProvider   string            `json:"chat_provider"`
	AllowMetered   bool              `json:"allow_metered"`
	Budget         loop.Budget       `json:"-"`
	ReadRoots      []string          `json:"read_roots"`
	WorkspaceRoots []string          `json:"workspace_roots"`
	HRN            HRNSettings       `json:"-"`
	MCPServers     []tool.MCPConfig  `json:"mcp_servers"`
}

type HRNSettings struct {
	Path    string
	Mode    string
	Timeout time.Duration
	Backend string
}

type fileSettings struct {
	Settings
	Budget struct {
		MaxIterations int    `json:"max_iterations"`
		MaxTokens     int    `json:"max_tokens"`
		Timeout       string `json:"timeout"`
	} `json:"budget"`
	HRN struct {
		Path    string `json:"path"`
		Mode    string `json:"mode"`
		Timeout string `json:"timeout"`
		Backend string `json:"backend"`
	} `json:"hrn"`
}

func Load(path string) (Settings, error) {
	if path == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return Settings{}, fmt.Errorf("locate Ada settings directory: %w", err)
		}
		path = filepath.Join(dir, "adanext-ada", "settings.json")
	}
	file, err := os.Open(path)
	if err != nil {
		return Settings{}, fmt.Errorf("open Ada settings: %w", err)
	}
	defer file.Close()
	dec := json.NewDecoder(file)
	dec.DisallowUnknownFields()
	var data fileSettings
	if err := dec.Decode(&data); err != nil {
		return Settings{}, fmt.Errorf("decode Ada settings: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return Settings{}, fmt.Errorf("Ada settings must contain one JSON object")
	}
	turnTimeout, err := parseTimeout("budget.timeout", data.Budget.Timeout)
	if err != nil {
		return Settings{}, err
	}
	hrnTimeout, err := parseTimeout("hrn.timeout", data.HRN.Timeout)
	if err != nil {
		return Settings{}, err
	}
	s := data.Settings
	s.Budget = loop.Budget{MaxIterations: data.Budget.MaxIterations, MaxTokens: data.Budget.MaxTokens, Timeout: turnTimeout}
	s.HRN = HRNSettings{Path: data.HRN.Path, Mode: data.HRN.Mode, Timeout: hrnTimeout, Backend: data.HRN.Backend}
	if err := s.validate(); err != nil {
		return Settings{}, err
	}
	s.setDefaults()
	return s, nil
}

func parseTimeout(name, raw string) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s cannot be negative", name)
	}
	return d, nil
}

func (s Settings) validate() error {
	if s.ChatProvider == "" {
		return fmt.Errorf("chat_provider is required")
	}
	if s.HRN.Mode != "" && s.HRN.Mode != "readonly" {
		return fmt.Errorf("hrn.mode must be readonly because writes wait for the approvals slice")
	}
	if len(s.WorkspaceRoots) > 0 && strings.TrimSpace(s.HRN.Backend) == "" {
		return fmt.Errorf("hrn.backend is required when workspace_roots is set")
	}
	if strings.EqualFold(strings.TrimSpace(s.HRN.Backend), "api") && !s.AllowMetered {
		return fmt.Errorf("hrn.backend api requires allow_metered")
	}
	names := make(map[string]struct{}, len(s.Providers))
	for _, cfg := range s.Providers {
		if _, exists := names[cfg.Name]; exists {
			return fmt.Errorf("duplicate provider name %q", cfg.Name)
		}
		names[cfg.Name] = struct{}{}
	}
	for _, server := range s.MCPServers {
		if err := server.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Settings) setDefaults() {
	s.Budget = s.Budget.WithDefaults()
	if s.HRN.Path == "" {
		s.HRN.Path = "hrn"
	}
	if s.HRN.Mode == "" {
		s.HRN.Mode = "readonly"
	}
	if s.HRN.Timeout <= 0 {
		s.HRN.Timeout = tool.DefaultDelegateTimeout
	}
}

func Build(s Settings, getenv func(string) string) (*loop.Loop, error) {
	return build(s, getenv, 30*time.Second)
}

func build(s Settings, getenv func(string) string, mcpStartupTimeout time.Duration) (*loop.Loop, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	s.setDefaults()
	var selected *provider.Config
	for _, cfg := range s.Providers {
		if cfg.Name != s.ChatProvider {
			continue
		}
		selected = &cfg
		break
	}
	if selected == nil {
		return nil, fmt.Errorf("chat_provider %q is not configured", s.ChatProvider)
	}
	model, err := provider.New(*selected, s.AllowMetered, getenv)
	if err != nil {
		return nil, err
	}

	registry := new(tool.Registry)
	if err := registry.Add(tool.WebFetch{}); err != nil {
		return nil, err
	}
	if len(s.ReadRoots) > 0 {
		if err := registry.Add(tool.ReadFile{Roots: s.ReadRoots}); err != nil {
			return nil, err
		}
	}
	if len(s.WorkspaceRoots) > 0 {
		var withoutEnv []string
		for _, cfg := range s.Providers {
			if cfg.APIKeyEnv != "" {
				withoutEnv = append(withoutEnv, cfg.APIKeyEnv)
			}
		}
		if err := registry.Add(tool.Delegate{
			Roots: s.WorkspaceRoots, Options: hrnexec.Options{HRNPath: s.HRN.Path, WithoutEnv: withoutEnv}, Mode: s.HRN.Mode,
			Timeout: s.HRN.Timeout, Backend: s.HRN.Backend,
		}); err != nil {
			return nil, err
		}
	}
	for _, server := range s.MCPServers {
		ctx, cancel := context.WithTimeout(context.Background(), mcpStartupTimeout)
		err := registry.AddMCP(ctx, server)
		cancel()
		if err != nil {
			_ = registry.Close()
			return nil, err
		}
	}
	return &loop.Loop{Provider: model, Tools: registry, Model: selected.Model, Budget: s.Budget}, nil
}
