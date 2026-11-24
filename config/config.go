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

	"github.com/cristiangirlea/adanext-ada/approval"
	"github.com/cristiangirlea/adanext-ada/channel"
	"github.com/cristiangirlea/adanext-ada/hrnexec"
	"github.com/cristiangirlea/adanext-ada/learn"
	"github.com/cristiangirlea/adanext-ada/loop"
	"github.com/cristiangirlea/adanext-ada/memory"
	"github.com/cristiangirlea/adanext-ada/provider"
	"github.com/cristiangirlea/adanext-ada/tool"
)

type KnowledgeSettings = hrnexec.KnowledgeScope

type Settings struct {
	Learning        LearningSettings   `json:"-"`
	Channels        []channel.Settings `json:"channels"`
	ApprovalTimeout time.Duration      `json:"-"`
	DataDir         string             `json:"data_dir"`
	Owner           string             `json:"owner"`
	SessionIdle     time.Duration      `json:"-"`
	HistoryMessages int                `json:"history_messages"`
	Knowledge       *KnowledgeSettings `json:"knowledge"`
	Providers       []provider.Config  `json:"providers"`
	ChatProvider    string             `json:"chat_provider"`
	AllowMetered    bool               `json:"allow_metered"`
	Budget          loop.Budget        `json:"-"`
	ReadRoots       []string           `json:"read_roots"`
	WorkspaceRoots  []string           `json:"workspace_roots"`
	HRN             HRNSettings        `json:"-"`
	MCPServers      []tool.MCPConfig   `json:"mcp_servers"`
}

type LearningSettings struct {
	Skills           bool
	KnowledgeBackend *string
	ProposeEvery     time.Duration
	OfferTTL         time.Duration
	StaleAfter       time.Duration
	ArchiveAfter     time.Duration
}

type HRNSettings struct {
	Skills  bool
	Home    string
	Path    string
	Mode    string
	Timeout time.Duration
	Backend string
}

type fileSettings struct {
	Learning struct {
		Skills           bool            `json:"skills"`
		KnowledgeBackend json.RawMessage `json:"knowledge_backend"`
		ProposeEvery     json.RawMessage `json:"propose_every"`
		OfferTTL         string          `json:"offer_ttl"`
		StaleAfter       string          `json:"stale_after"`
		ArchiveAfter     string          `json:"archive_after"`
	} `json:"learning"`
	Approvals struct {
		Timeout string `json:"timeout"`
	} `json:"approvals"`
	SessionIdle string `json:"session_idle"`
	Settings
	Budget struct {
		MaxIterations int    `json:"max_iterations"`
		MaxTokens     int    `json:"max_tokens"`
		Timeout       string `json:"timeout"`
	} `json:"budget"`
	HRN struct {
		Skills  bool   `json:"skills"`
		Home    string `json:"home"`
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
	s.Learning.Skills = data.Learning.Skills
	// Only an omitted backend inherits the delegate setting.
	if raw := data.Learning.KnowledgeBackend; raw != nil {
		var backend string
		if err := json.Unmarshal(raw, &backend); err != nil {
			return Settings{}, fmt.Errorf("parse learning.knowledge_backend: %w", err)
		}
		s.Learning.KnowledgeBackend = &backend
	}
	every := "1h"
	if string(data.Learning.ProposeEvery) == "0" {
		every = "0"
	} else if data.Learning.ProposeEvery != nil {
		if err := json.Unmarshal(data.Learning.ProposeEvery, &every); err != nil {
			return Settings{}, fmt.Errorf("parse learning.propose_every: %w", err)
		}
		if every == "" {
			every = "1h"
		}
	}
	s.Learning.ProposeEvery, err = parseTimeout("learning.propose_every", every)
	if err != nil {
		return Settings{}, err
	}
	s.Learning.OfferTTL, err = parseTimeout("learning.offer_ttl", data.Learning.OfferTTL)
	if err != nil {
		return Settings{}, err
	}
	s.Learning.StaleAfter, err = parseTimeout("learning.stale_after", data.Learning.StaleAfter)
	if err != nil {
		return Settings{}, err
	}
	s.Learning.ArchiveAfter, err = parseTimeout("learning.archive_after", data.Learning.ArchiveAfter)
	if err != nil {
		return Settings{}, err
	}
	s.ApprovalTimeout, err = parseTimeout("approvals.timeout", data.Approvals.Timeout)
	if err != nil {
		return Settings{}, err
	}
	s.SessionIdle, err = parseTimeout("session_idle", data.SessionIdle)
	if err != nil {
		return Settings{}, err
	}
	s.Budget = loop.Budget{MaxIterations: data.Budget.MaxIterations, MaxTokens: data.Budget.MaxTokens, Timeout: turnTimeout}
	s.HRN = HRNSettings{Skills: data.HRN.Skills, Home: data.HRN.Home, Path: data.HRN.Path, Mode: data.HRN.Mode, Timeout: hrnTimeout, Backend: data.HRN.Backend}
	if err := s.validate(); err != nil {
		return Settings{}, err
	}
	if err := s.setDefaults(); err != nil {
		return Settings{}, err
	}
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
	if s.Owner != "" && !memory.ValidIdentity(s.Owner) {
		return fmt.Errorf("owner must be a valid user id")
	}
	if s.HistoryMessages < 0 || s.SessionIdle < 0 {
		return fmt.Errorf("history_messages and session_idle cannot be negative")
	}
	if k := s.Knowledge; k != nil {
		for _, v := range []string{k.TenantID, k.ProjectID, k.Backend, k.Model, k.Revision} {
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("knowledge requires tenant_id, project_id, backend, model and revision")
			}
		}
	}
	if s.ChatProvider == "" {
		return fmt.Errorf("chat_provider is required")
	}
	if s.HRN.Skills && s.HRN.Backend != "claude" {
		return fmt.Errorf("hrn.skills requires hrn.backend claude")
	}
	if s.HRN.Mode != "" && s.HRN.Mode != "readonly" && s.HRN.Mode != "auto" {
		return fmt.Errorf("hrn.mode must be readonly or auto")
	}
	if s.ApprovalTimeout < 0 {
		return fmt.Errorf("approvals.timeout cannot be negative")
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
	channelNames := map[string]bool{}
	for _, ch := range s.Channels {
		if err := ch.Validate(); err != nil {
			return err
		}
		if channelNames[ch.Name] {
			return fmt.Errorf("duplicate channel %q", ch.Name)
		}
		channelNames[ch.Name] = true
	}
	for _, server := range s.MCPServers {
		if err := server.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Settings) setDefaults() error {
	if s.Learning.OfferTTL == 0 {
		s.Learning.OfferTTL = 168 * time.Hour
	}
	if s.Learning.StaleAfter == 0 {
		s.Learning.StaleAfter = 2160 * time.Hour
	}
	if s.Learning.ArchiveAfter == 0 {
		s.Learning.ArchiveAfter = 720 * time.Hour
	}
	if s.ApprovalTimeout == 0 {
		s.ApprovalTimeout = approval.DefaultTimeout
	}
	if s.DataDir == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return fmt.Errorf("locate Ada data directory: %w", err)
		}
		s.DataDir = filepath.Join(dir, "adanext-ada", "data")
	}
	if s.Owner == "" {
		s.Owner = "owner"
	}
	if s.HistoryMessages == 0 {
		s.HistoryMessages = memory.DefaultHistory
	}
	if s.SessionIdle == 0 {
		s.SessionIdle = memory.DefaultIdle
	}
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
	return nil
}

func (s Settings) RunsDir(getenv func(string) string) string {
	if s.HRN.Home != "" {
		return filepath.Join(s.HRN.Home, "runs")
	}
	return memory.HRNRunsDir(getenv)
}

// ProviderKeyEnv names variables that child processes must never inherit.
func (s Settings) ProviderKeyEnv() []string {
	var names []string
	for _, cfg := range s.Providers {
		if cfg.APIKeyEnv != "" {
			names = append(names, cfg.APIKeyEnv)
		}
	}
	return names
}

func (s Settings) hrnOptions() hrnexec.Options {
	return hrnexec.Options{HRNPath: s.HRN.Path, HRNHome: s.HRN.Home, WithoutEnv: s.ProviderKeyEnv()}
}

func (s Settings) SkillReview() learn.SkillReview {
	return learn.SkillReview{Options: s.hrnOptions(), Timeout: s.HRN.Timeout}
}

func (s Settings) KnowledgeReview() learn.KnowledgeReview {
	backend := s.HRN.Backend
	if s.Learning.KnowledgeBackend != nil {
		backend = *s.Learning.KnowledgeBackend
	}
	opts := s.hrnOptions()
	opts.WithoutEnv = append(opts.WithoutEnv, "HRN_OPERATOR_TOKEN")
	k := learn.KnowledgeReview{Options: opts, Backend: backend, Timeout: s.HRN.Timeout}
	if s.Knowledge != nil {
		k.Scope = *s.Knowledge
	}
	return k
}

type App struct {
	*loop.Loop
	Store *memory.Store
}

func Build(s Settings, getenv func(string) string) (*App, error) {
	return build(s, getenv, 30*time.Second)
}

// BuildProvider applies chat's endpoint policy without starting any tools.
func (s Settings) BuildProvider(getenv func(string) string) (provider.Provider, string, error) {
	for _, cfg := range s.Providers {
		if cfg.Name == s.ChatProvider {
			model, err := provider.New(cfg, s.AllowMetered, getenv)
			return model, cfg.Model, err
		}
	}
	return nil, "", fmt.Errorf("chat_provider %q is not configured", s.ChatProvider)
}

func build(s Settings, getenv func(string) string, mcpStartupTimeout time.Duration) (*App, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	if err := s.setDefaults(); err != nil {
		return nil, err
	}
	model, modelName, err := s.BuildProvider(getenv)
	if err != nil {
		return nil, err
	}

	registry := &tool.Registry{Recorder: approval.Log(s.DataDir), ApprovalTimeout: s.ApprovalTimeout}
	if err := registry.Add(tool.WebFetch{}); err != nil {
		return nil, err
	}
	if len(s.ReadRoots) > 0 {
		if err := registry.Add(tool.ReadFile{Roots: s.ReadRoots}); err != nil {
			return nil, err
		}
	}
	opts := s.hrnOptions()
	if k := s.Knowledge; k != nil {
		req := hrnexec.KnowledgeRequest{TenantID: k.TenantID, ProjectID: k.ProjectID, Backend: k.Backend, Model: k.Model, Revision: k.Revision}
		if err := registry.Add(tool.SearchKnowledge{Options: opts, Request: req}); err != nil {
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
	store, err := memory.NewStore(s.DataDir)
	if err != nil {
		registry.Close()
		return nil, err
	}
	if len(s.WorkspaceRoots) > 0 {
		if err := registry.Add(tool.Delegate{
			RecordDelegation: (learn.Store{Memory: store}).RecordDelegation,
			Skills:           s.HRN.Skills, Roots: s.WorkspaceRoots, Options: opts, Mode: s.HRN.Mode,
			Timeout: s.HRN.Timeout, Backend: s.HRN.Backend,
		}); err != nil {
			store.Close()
			registry.Close()
			return nil, err
		}
	}
	if err := registry.Add(tool.SearchMemory{Store: store, HRNRuns: s.RunsDir(getenv)}); err != nil {
		store.Close()
		registry.Close()
		return nil, err
	}
	profile := func(ctx context.Context) string {
		user, ok := memory.UserFrom(ctx)
		if !ok {
			return ""
		}
		return store.Render(user)
	}
	return &App{Loop: &loop.Loop{Provider: model, Tools: registry, Model: modelName, Budget: s.Budget, Context: profile}, Store: store}, nil
}
