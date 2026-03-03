package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration for the proxy application.
type Config struct {
	Server    ServerConfig     `yaml:"server"`
	Router    RouterConfig     `yaml:"router"`
	Providers []ProviderConfig `yaml:"providers"`
}

// ServerConfig controls HTTP server and upstream timeout behavior.
type ServerConfig struct {
	ListenAddr      string        `yaml:"listen_addr"`
	ReadTimeout     time.Duration `yaml:"read_timeout"`
	WriteTimeout    time.Duration `yaml:"write_timeout"`
	IdleTimeout     time.Duration `yaml:"idle_timeout"`
	UpstreamTimeout time.Duration `yaml:"upstream_timeout"`
	Auth            AuthConfig    `yaml:"auth"`
}

// AuthConfig controls client authentication for requests entering the proxy.
type AuthConfig struct {
	Enabled bool   `yaml:"enabled"`
	Header  string `yaml:"header"`
	Scheme  string `yaml:"scheme"`
	Token   string `yaml:"token"`
}

// RouterConfig controls route selection policy.
type RouterConfig struct {
	Strategy          string      `yaml:"strategy"`
	DefaultProvider   string      `yaml:"default_provider"`
	HeaderProviderKey string      `yaml:"header_provider_key"`
	RouteRules        []RouteRule `yaml:"route_rules"`
}

// RouteRule describes an optional routing rule.
type RouteRule struct {
	PathPrefix  string   `yaml:"path_prefix"`
	ModelPrefix string   `yaml:"model_prefix"`
	ModelRegex  string   `yaml:"model_regex"`
	Providers   []string `yaml:"providers"`
}

// ProviderConfig defines an upstream Claude-compatible provider.
type ProviderConfig struct {
	Name          string            `yaml:"name"`
	BaseURL       string            `yaml:"base_url"`
	PathPrefix    string            `yaml:"path_prefix"`
	Enabled       *bool             `yaml:"enabled"`
	Weight        int               `yaml:"weight"`
	Timeout       time.Duration     `yaml:"timeout"`
	APIKey        string            `yaml:"api_key"`
	AuthType      string            `yaml:"auth_type"`
	AuthHeader    string            `yaml:"auth_header"`
	StaticHeaders map[string]string `yaml:"static_headers"`
	ModelPrefixes []string          `yaml:"model_prefixes"`
	ModelRegex    string            `yaml:"model_regex"`
	ModelMapping  map[string]string `yaml:"model_mapping"`
}

// Load reads and validates YAML config from disk.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse yaml: %w", err)
	}

	applyDefaults(&cfg)
	expandEnv(&cfg)
	if err := validate(cfg); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Server.ListenAddr == "" {
		cfg.Server.ListenAddr = ":8080"
	}
	if cfg.Server.ReadTimeout == 0 {
		cfg.Server.ReadTimeout = 30 * time.Second
	}
	if cfg.Server.WriteTimeout == 0 {
		cfg.Server.WriteTimeout = 300 * time.Second
	}
	if cfg.Server.IdleTimeout == 0 {
		cfg.Server.IdleTimeout = 60 * time.Second
	}
	if cfg.Server.UpstreamTimeout == 0 {
		cfg.Server.UpstreamTimeout = 300 * time.Second
	}
	if cfg.Server.Auth.Header == "" {
		cfg.Server.Auth.Header = "Authorization"
	}
	if cfg.Server.Auth.Scheme == "" {
		cfg.Server.Auth.Scheme = "Bearer"
	}

	if cfg.Router.Strategy == "" {
		cfg.Router.Strategy = "round_robin"
	}
	if cfg.Router.HeaderProviderKey == "" {
		cfg.Router.HeaderProviderKey = "X-AI-Provider"
	}

	for i := range cfg.Providers {
		if cfg.Providers[i].Weight <= 0 {
			cfg.Providers[i].Weight = 1
		}
		if cfg.Providers[i].Timeout == 0 {
			cfg.Providers[i].Timeout = cfg.Server.UpstreamTimeout
		}
		if cfg.Providers[i].StaticHeaders == nil {
			cfg.Providers[i].StaticHeaders = map[string]string{}
		}
	}
}

func expandEnv(cfg *Config) {
	cfg.Server.Auth.Token = os.ExpandEnv(cfg.Server.Auth.Token)
	cfg.Server.Auth.Header = os.ExpandEnv(cfg.Server.Auth.Header)
	cfg.Server.Auth.Scheme = os.ExpandEnv(cfg.Server.Auth.Scheme)

	for i := range cfg.Providers {
		cfg.Providers[i].BaseURL = os.ExpandEnv(cfg.Providers[i].BaseURL)
		cfg.Providers[i].PathPrefix = os.ExpandEnv(cfg.Providers[i].PathPrefix)
		cfg.Providers[i].APIKey = os.ExpandEnv(cfg.Providers[i].APIKey)
		cfg.Providers[i].AuthHeader = os.ExpandEnv(cfg.Providers[i].AuthHeader)
		for k, v := range cfg.Providers[i].StaticHeaders {
			cfg.Providers[i].StaticHeaders[k] = os.ExpandEnv(v)
		}
	}
}

func validate(cfg Config) error {
	if cfg.Server.Auth.Enabled && strings.TrimSpace(cfg.Server.Auth.Token) == "" {
		return errors.New("server.auth.token cannot be empty when auth is enabled")
	}

	if len(cfg.Providers) == 0 {
		return errors.New("no providers configured")
	}

	strategy := strings.ToLower(cfg.Router.Strategy)
	if strategy != "round_robin" && strategy != "weighted_random" {
		return fmt.Errorf("unsupported router strategy %q", cfg.Router.Strategy)
	}

	nameSeen := map[string]struct{}{}
	enabledSeen := map[string]struct{}{}
	for _, p := range cfg.Providers {
		if p.Name == "" {
			return errors.New("provider name cannot be empty")
		}
		if _, ok := nameSeen[p.Name]; ok {
			return fmt.Errorf("duplicate provider name %q", p.Name)
		}
		nameSeen[p.Name] = struct{}{}

		if p.BaseURL == "" {
			return fmt.Errorf("provider %q base_url cannot be empty", p.Name)
		}
		parsed, err := url.Parse(p.BaseURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("provider %q invalid base_url %q", p.Name, p.BaseURL)
		}

		if p.EnabledOrDefault() {
			enabledSeen[p.Name] = struct{}{}
		}
	}

	if len(enabledSeen) == 0 {
		return errors.New("no enabled providers")
	}

	if cfg.Router.DefaultProvider != "" {
		if _, ok := enabledSeen[cfg.Router.DefaultProvider]; !ok {
			return fmt.Errorf("default_provider %q is not an enabled provider", cfg.Router.DefaultProvider)
		}
	}

	for _, rule := range cfg.Router.RouteRules {
		if len(rule.Providers) == 0 {
			return errors.New("route rule providers cannot be empty")
		}
		for _, providerName := range rule.Providers {
			if _, ok := nameSeen[providerName]; !ok {
				return fmt.Errorf("route rule provider %q does not exist", providerName)
			}
		}
	}

	return nil
}

// EnabledOrDefault returns true when provider is enabled or the flag is omitted.
func (p ProviderConfig) EnabledOrDefault() bool {
	if p.Enabled == nil {
		return true
	}
	return *p.Enabled
}
