package provider

import (
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

type Config struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	BaseURL   string `json:"base_url"`
	Model     string `json:"model"`
	APIKeyEnv string `json:"api_key_env"`
}

var localNetworks = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("fc00::/7"),
}

func isLocal(base string) bool {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(base, "#") {
		return false
	}
	if strings.EqualFold(u.Hostname(), "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil || ip.Zone() != "" {
		return false
	}
	for _, block := range localNetworks {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

func New(cfg Config, allowMetered bool, getenv func(string) string) (Provider, error) {
	var path string
	switch cfg.Kind {
	case "anthropic":
		path = "/v1/messages"
	case "openai":
		path = "/v1/chat/completions"
	default:
		return nil, fmt.Errorf("provider %q has unknown kind %q", cfg.Name, cfg.Kind)
	}
	local := isLocal(cfg.BaseURL)
	if !local && !allowMetered {
		return nil, fmt.Errorf("provider %q requires allow_metered for its endpoint", cfg.Name)
	}
	base, err := url.Parse(cfg.BaseURL)
	if err != nil || base.Hostname() == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, fmt.Errorf("provider %q requires a valid HTTP or HTTPS base_url", cfg.Name)
	}

	var key string
	if cfg.APIKeyEnv != "" && getenv != nil {
		key = getenv(cfg.APIKeyEnv)
	}
	if !local && key == "" {
		if cfg.APIKeyEnv == "" {
			return nil, fmt.Errorf("provider %q requires api_key_env for its metered endpoint", cfg.Name)
		}
		return nil, fmt.Errorf("provider %q requires a key in %s", cfg.Name, cfg.APIKeyEnv)
	}

	client := newHTTPClient(base.JoinPath(path).String(), key, local)
	switch cfg.Kind {
	case "anthropic":
		return &anthropicProvider{name: cfg.Name, model: cfg.Model, http: client}, nil
	default:
		return &openAIProvider{name: cfg.Name, model: cfg.Model, http: client}, nil
	}
}
