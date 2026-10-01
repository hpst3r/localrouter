// Package config loads and validates the LocalRouter YAML configuration.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/hpst3r/localrouter/internal/core"
)

type Config struct {
	Listen           string          `yaml:"listen"`
	AllowNonLoopback bool            `yaml:"allow_non_loopback"`
	DataDir          string          `yaml:"data_dir"`
	PricingFile      string          `yaml:"pricing_file"`
	Quota            QuotaConfig     `yaml:"quota"`
	Policy           PolicyConfig    `yaml:"policy"`
	Control          ControlConfig   `yaml:"control"`
	Clients          []ClientConfig  `yaml:"clients"`
	Accounts         []AccountConfig `yaml:"accounts"`
	Routes           []RouteConfig   `yaml:"routes"`
}

type QuotaConfig struct {
	PollInterval Duration `yaml:"poll_interval"`
}

type PolicyConfig struct {
	StaleAfter       Duration `yaml:"stale_after"`
	SafetyMargin     float64  `yaml:"safety_margin"`
	InflightEstimate float64  `yaml:"inflight_estimate"`
	MaxFailovers     int      `yaml:"max_failovers"`
}

type ControlConfig struct {
	RequireAuth bool `yaml:"require_auth"`
}

type ClientConfig struct {
	Name    string `yaml:"name"`
	Class   string `yaml:"class"`
	KeyFile string `yaml:"key_file"`
}

type AccountConfig struct {
	ID         string             `yaml:"id"`
	Provider   string             `yaml:"provider"`
	BaseURL    string             `yaml:"base_url"`
	APIKeyFile string             `yaml:"api_key_file"`
	APIKeyEnv  string             `yaml:"api_key_env"`
	Reserve    map[string]float64 `yaml:"reserve"`
	CostBasis  string             `yaml:"cost_basis"`
}

type RouteConfig struct {
	Name          string   `yaml:"name"`
	Models        []string `yaml:"models"`
	UpstreamModel string   `yaml:"upstream_model"`
	Interactive   []string `yaml:"interactive"`
	Background    []string `yaml:"background"`
}

// Duration parses Go duration strings in YAML.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", n.Value, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

// Load reads, defaults, and validates a config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.applyDefaults(filepath.Dir(path))
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults(baseDir string) {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8787"
	}
	if c.DataDir == "" {
		if d, err := os.UserConfigDir(); err == nil {
			c.DataDir = filepath.Join(d, "localrouter")
		} else {
			c.DataDir = baseDir
		}
	}
	c.DataDir = expand(c.DataDir, baseDir)
	if c.PricingFile == "" {
		c.PricingFile = filepath.Join(c.DataDir, "pricing.yaml")
	}
	c.PricingFile = expand(c.PricingFile, baseDir)
	if c.Quota.PollInterval == 0 {
		c.Quota.PollInterval = Duration(5 * time.Minute)
	}
	if c.Policy.StaleAfter == 0 {
		c.Policy.StaleAfter = Duration(10 * time.Minute)
	}
	if c.Policy.InflightEstimate == 0 {
		c.Policy.InflightEstimate = 0.01
	}
	if c.Policy.MaxFailovers == 0 {
		c.Policy.MaxFailovers = 2
	}
	for i := range c.Clients {
		c.Clients[i].KeyFile = expand(c.Clients[i].KeyFile, baseDir)
	}
	for i := range c.Accounts {
		a := &c.Accounts[i]
		a.APIKeyFile = expand(a.APIKeyFile, baseDir)
		if a.BaseURL == "" {
			switch a.Provider {
			case core.ProviderCodex:
				a.BaseURL = "https://chatgpt.com/backend-api/codex"
			case core.ProviderOllama:
				a.BaseURL = "https://ollama.com/v1"
			}
		}
		a.BaseURL = strings.TrimRight(a.BaseURL, "/")
		if a.CostBasis == "" {
			if a.Provider == core.ProviderCodex || a.Provider == core.ProviderOllama {
				a.CostBasis = "api_equivalent"
			} else {
				a.CostBasis = "metered"
			}
		}
	}
}

func expand(p, baseDir string) string {
	if p == "" {
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(h, p[2:])
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(baseDir, p)
	}
	return p
}

// Validate checks structural consistency.
func (c *Config) Validate() error {
	var errs []error
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		errs = append(errs, fmt.Errorf("listen: %w", err))
	} else if !c.AllowNonLoopback {
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			errs = append(errs, fmt.Errorf("listen %q is not loopback; set allow_non_loopback: true to override", c.Listen))
		}
	}
	if c.Policy.SafetyMargin < 0 || c.Policy.SafetyMargin >= 1 {
		errs = append(errs, errors.New("policy.safety_margin must be in [0,1)"))
	}
	if c.Policy.InflightEstimate < 0 || c.Policy.InflightEstimate >= 1 {
		errs = append(errs, errors.New("policy.inflight_estimate must be in [0,1)"))
	}
	if len(c.Clients) == 0 {
		errs = append(errs, errors.New("at least one client is required"))
	}
	seenC := map[string]bool{}
	for _, cl := range c.Clients {
		if cl.Name == "" || seenC[cl.Name] {
			errs = append(errs, fmt.Errorf("client name %q empty or duplicate", cl.Name))
		}
		seenC[cl.Name] = true
		if cl.Class != string(core.ClassInteractive) && cl.Class != string(core.ClassBackground) {
			errs = append(errs, fmt.Errorf("client %s: class must be interactive or background", cl.Name))
		}
		if cl.KeyFile == "" {
			errs = append(errs, fmt.Errorf("client %s: key_file required", cl.Name))
		}
	}
	accts := map[string]bool{}
	for _, a := range c.Accounts {
		if a.ID == "" || accts[a.ID] || strings.ContainsAny(a.ID, "/\\. ") {
			errs = append(errs, fmt.Errorf("account id %q empty, duplicate, or contains / \\ . or space", a.ID))
		}
		accts[a.ID] = true
		switch a.Provider {
		case core.ProviderCodex:
		case core.ProviderOllama, core.ProviderOpenAICompat:
			if a.APIKeyFile == "" && a.APIKeyEnv == "" {
				errs = append(errs, fmt.Errorf("account %s: api_key_file or api_key_env required", a.ID))
			}
			if a.BaseURL == "" {
				errs = append(errs, fmt.Errorf("account %s: base_url required", a.ID))
			}
		default:
			errs = append(errs, fmt.Errorf("account %s: unknown provider %q", a.ID, a.Provider))
		}
		for k, v := range a.Reserve {
			if k != core.Window5h && k != core.WindowWeekly {
				errs = append(errs, fmt.Errorf("account %s: reserve window %q must be 5h or weekly", a.ID, k))
			}
			if v < 0 || v >= 1 {
				errs = append(errs, fmt.Errorf("account %s: reserve %s must be in [0,1)", a.ID, k))
			}
		}
		if a.CostBasis != "api_equivalent" && a.CostBasis != "metered" {
			errs = append(errs, fmt.Errorf("account %s: cost_basis must be api_equivalent or metered", a.ID))
		}
	}
	models := map[string]string{}
	for _, r := range c.Routes {
		if r.Name == "" || len(r.Models) == 0 {
			errs = append(errs, fmt.Errorf("route %q: name and models required", r.Name))
		}
		for _, m := range r.Models {
			if prev, ok := models[m]; ok {
				errs = append(errs, fmt.Errorf("model %q in both routes %s and %s", m, prev, r.Name))
			}
			models[m] = r.Name
		}
		if len(r.Interactive) == 0 {
			errs = append(errs, fmt.Errorf("route %s: interactive account list required", r.Name))
		}
		for _, id := range append(append([]string{}, r.Interactive...), r.Background...) {
			if !accts[id] {
				errs = append(errs, fmt.Errorf("route %s: unknown account %q", r.Name, id))
			}
		}
	}
	return errors.Join(errs...)
}

// CoreAccounts converts account config into core.Account values.
func (c *Config) CoreAccounts() []core.Account {
	out := make([]core.Account, 0, len(c.Accounts))
	for _, a := range c.Accounts {
		r := map[string]float64{}
		for k, v := range a.Reserve {
			r[k] = v
		}
		out = append(out, core.Account{ID: a.ID, Provider: a.Provider, BaseURL: a.BaseURL, Reserve: r, CostBasis: a.CostBasis})
	}
	return out
}

// CoreRoutes converts route config into core.Route values. A route with no
// background list falls back to its interactive list.
func (c *Config) CoreRoutes() []core.Route {
	out := make([]core.Route, 0, len(c.Routes))
	for _, r := range c.Routes {
		bg := r.Background
		if len(bg) == 0 {
			bg = r.Interactive
		}
		out = append(out, core.Route{Name: r.Name, Models: r.Models, UpstreamModel: r.UpstreamModel, Interactive: r.Interactive, Background: bg})
	}
	return out
}
