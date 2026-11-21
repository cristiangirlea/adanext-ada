package config

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/adanext-ada/provider"
	"github.com/cristiangirlea/adanext-ada/tool"
)

const basic = `"providers":[{"name":"local","kind":"openai","base_url":"http://localhost","model":"model"}],"chat_provider":"local"`

func TestLoadRefusesInvalidSettings(t *testing.T) {
	for _, tc := range []struct{ settings, want string }{
		{`{` + basic + `,"other":true}`, "unknown field"},
		{`{` + basic + `,"hrn":{"mode":"write"}}`, "hrn.mode"},
		{`{}`, "chat_provider"},
		{`{` + basic + `,"hrn":{"mode":"yolo"}}`, "hrn.mode must be readonly or auto"},
		{`{` + basic + `,"hrn":{"backend":"api"}}`, "allow_metered"},
		{`{` + basic + `,"workspace_roots":["."]}`, "hrn.backend is required"},
		{`{"providers":[{"name":"duplicate"},{"name":"duplicate"}],"chat_provider":"local"}`, "duplicate"},
		{`{` + basic + `,"mcp_servers":[{"name":"writes","command":"must not run","effect":"delegate"}]}`, "must declare effect read or write"},
		{`{` + basic + `,"budget":{"timeout":"tomorrow"}}`, "budget.timeout"},
		{`{` + basic + `,"budget":{"unexpected":1}}`, "unknown field"},
		{`{` + basic + `} {}`, "settings"},
	} {
		if _, err := Load(settingsFile(t, tc.settings)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Load(%q) = %v, want %q", tc.settings, err, tc.want)
		}
	}
}

func TestLoadAllowsExplicitMeteredHRN(t *testing.T) {
	s, err := Load(settingsFile(t, `{`+basic+`,"hrn":{"backend":"api"},"allow_metered":true}`))
	if err != nil || s.HRN.Backend != "api" {
		t.Fatalf("Load = %+v, %v", s.HRN, err)
	}
}

func TestBuildProviderRefusals(t *testing.T) {
	selected := provider.Config{Name: "chat", Kind: "openai", BaseURL: "http://localhost"}
	for _, tc := range []struct {
		configs []provider.Config
		want    string
	}{
		{[]provider.Config{selected, {Name: "duplicate"}, {Name: "duplicate"}}, "duplicate"},
		{[]provider.Config{{Name: "chat", Kind: "openai", BaseURL: "https://metered.invalid"}}, "allow_metered"},
	} {
		if _, err := Build(Settings{Providers: tc.configs, ChatProvider: "chat"}, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Build = %v, want %q", err, tc.want)
		}
	}
}

func TestBuildMCPStartupTimeout(t *testing.T) {
	s, err := Load(settingsFile(t, `{`+basic+`,"budget":{"timeout":"1ns"}}`))
	if err != nil {
		t.Fatal(err)
	}
	s.MCPServers = []tool.MCPConfig{{
		Name: "stalled", Command: os.Args[0], Args: []string{"-test.run=^TestStalledMCPHelper$"},
		Env: map[string]string{"ADA_TEST_STALLED_MCP": "1"}, Effect: tool.Read,
	}}
	start := time.Now()
	const startupTimeout = 250 * time.Millisecond
	_, err = build(s, nil, startupTimeout)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) < startupTimeout {
		t.Fatalf("Build = %v after %s", err, time.Since(start))
	}
}

func TestBuildFiltersEveryProviderKeyFromHRN(t *testing.T) {
	t.Setenv("ADA_TEST_CONFIG_HRN", "1")
	home := t.TempDir()
	t.Setenv("ADA_TEST_EXPECT_HOME", home)
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("ADA_TEST_CONFIG_SELECTED_KEY", "fixture")
	t.Setenv("ADA_TEST_CONFIG_UNUSED_KEY", "fixture")
	root := t.TempDir()
	l, err := Build(Settings{
		DataDir:      t.TempDir(),
		ChatProvider: "local",
		Providers: []provider.Config{
			{Name: "local", Kind: "openai", BaseURL: "http://localhost", APIKeyEnv: "ADA_TEST_CONFIG_SELECTED_KEY"},
			{Name: "unused", APIKeyEnv: "ADA_TEST_CONFIG_UNUSED_KEY"},
		},
		Knowledge:      &KnowledgeSettings{TenantID: "t", ProjectID: "p", Backend: "b", Model: "m", Revision: "r"},
		WorkspaceRoots: []string{root},
		HRN:            HRNSettings{Home: home, Path: os.Args[0], Backend: "configured"},
	}, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Tools.Close(); l.Store.Close() })
	input, _ := json.Marshal(map[string]string{"prompt": "review", "dir": root})
	if got, err := l.Tools.Run(t.Context(), "delegate_to_hrn", input); err != nil || !strings.Contains(got, "filtered") {
		t.Fatalf("delegate = %q, %v", got, err)
	}
	if got, err := l.Tools.Run(t.Context(), "search_knowledge", []byte(`{"query":"test"}`)); err != nil || got != "empty" {
		t.Fatal(got, err)
	}
}

func TestMain(m *testing.M) {
	if os.Getenv("ADA_TEST_CONFIG_HRN") == "1" {
		if os.Getenv("HRN_HOME") != os.Getenv("ADA_TEST_EXPECT_HOME") {
			fmt.Fprintln(os.Stderr, "wrong hrn home")
			os.Exit(2)
		}
		if len(os.Args) == 4 && os.Args[1] == "knowledge" && os.Args[2] == "search" {
			fmt.Println(`{"empty_reason":"empty"}`)
			os.Exit(0)
		}
		_, selected := os.LookupEnv("ADA_TEST_CONFIG_SELECTED_KEY")
		_, unused := os.LookupEnv("ADA_TEST_CONFIG_UNUSED_KEY")
		if selected || unused {
			fmt.Fprintln(os.Stderr, "provider variable reached hrn")
			os.Exit(2)
		}
		fmt.Println(`{"v":1,"seq":1,"type":"hello","protocol":"v1"}`)
		var start struct {
			AttemptID string `json:"attempt_id"`
		}
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() || json.Unmarshal(scanner.Bytes(), &start) != nil || start.AttemptID == "" {
			os.Exit(2)
		}
		fmt.Printf(`{"v":1,"seq":2,"type":"accepted","attempt_id":%q,"run_id":"env-check"}`+"\n", start.AttemptID)
		fmt.Printf(`{"v":1,"seq":3,"type":"result","attempt_id":%q,"run_id":"env-check","result":{"status":"ok","final":"filtered"}}`+"\n", start.AttemptID)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestStalledMCPHelper(t *testing.T) {
	if os.Getenv("ADA_TEST_STALLED_MCP") != "" {
		var b [1]byte
		_, _ = os.Stdin.Read(b[:])
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
}

func TestLoadDefaultsAndBuild(t *testing.T) {
	settings, err := Load(settingsFile(t, `{`+basic+`}`))
	if err != nil {
		t.Fatal(err)
	}
	if settings.AllowMetered || settings.HRN.Mode != "readonly" || settings.HRN.Timeout != 30*time.Minute || settings.HRN.Path != "hrn" ||
		settings.Budget.MaxIterations != 25 || settings.Budget.MaxTokens != 200000 || settings.Budget.Timeout != time.Hour {
		t.Fatalf("unexpected settings: %+v", settings)
	}
	settings.DataDir = t.TempDir()
	l, err := Build(settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Tools.Close(); l.Store.Close() })
	if l.Provider.Name() != "local" || l.Model != "model" {
		t.Fatalf("loop = %+v", l)
	}
	specs := l.Tools.Specs()
	if len(specs) != 2 || specs[0].Name != (tool.WebFetch{}).Spec().Name || specs[1].Name != "search_memory" {
		t.Fatalf("unexpected tools: %+v", specs)
	}
}

func settingsFile(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
