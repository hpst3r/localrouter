// Package config loads and validates the LocalRouter YAML configuration.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/hpst3r/localrouter/internal/core"
)

type Config struct {
	Listen           string `yaml:"listen"`
	AllowNonLoopback bool   `yaml:"allow_non_loopback"`
	// AllowedHosts are extra Host-header names/IPs accepted when
	// allow_non_loopback is true (loopback names are always accepted).
	AllowedHosts []string `yaml:"allowed_hosts"`
	TLSCertFile  string   `yaml:"tls_cert_file"`
	TLSKeyFile   string   `yaml:"tls_key_file"`
	// HostName labels usage that happens on this machine: proxied requests
	// from clients without a host, and the local claude_logs collector.
	// Default: the short OS hostname, lowercased.
	HostName    string           `yaml:"host_name"`
	DataDir     string           `yaml:"data_dir"`
	PricingFile string           `yaml:"pricing_file"`
	Quota       QuotaConfig      `yaml:"quota"`
	Policy      PolicyConfig     `yaml:"policy"`
	Control     ControlConfig    `yaml:"control"`
	Limits      LimitsConfig     `yaml:"limits"`
	Timeouts    TimeoutsConfig   `yaml:"timeouts"`
	Clients     []ClientConfig   `yaml:"clients"`
	Accounts    []AccountConfig  `yaml:"accounts"`
	Routes      []RouteConfig    `yaml:"routes"`
	ClaudeLogs  ClaudeLogsConfig `yaml:"claude_logs"`
	HermesLogs  HermesLogsConfig `yaml:"hermes_logs"`
}

// ClaudeLogsConfig controls ingestion of Claude Code transcript token usage.
type ClaudeLogsConfig struct {
	Enabled      bool     `yaml:"enabled"`
	Dir          string   `yaml:"dir"`           // default ~/.claude/projects
	Account      string   `yaml:"account"`       // claude account id rows are attributed to
	ScanInterval Duration `yaml:"scan_interval"` // default 1m
	// Client names the ledger client for locally collected Claude Code usage
	// (default "claude-code"); set it to a registered client to attribute it.
	Client string `yaml:"client"`
}

// HermesLogsConfig imports Hermes Agent's own per-session, per-model token
// accounting (state.db session_model_usage, read-only) so traffic Hermes
// sends directly to providers is counted. Rows whose billing_base_url points
// at this router are skipped (already in the ledger).
type HermesLogsConfig struct {
	Enabled bool   `yaml:"enabled"`
	Home    string `yaml:"home"` // default ~/.hermes (profiles/*/state.db included)
	// Accounts maps a Hermes billing_provider (e.g. "anthropic",
	// "openai-codex", "ollama-cloud") to a LocalRouter account id. Unmapped
	// providers are recorded with an empty account. Rows with an empty
	// billing_provider (auxiliary tasks) use model prefixes: claude-* ->
	// Accounts["anthropic"].
	Accounts     map[string]string `yaml:"accounts"`
	Client       string            `yaml:"client"`        // default "hermes"
	ScanInterval Duration          `yaml:"scan_interval"` // default 1m
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

// LimitsConfig bounds how many inference requests may be active at once.
// Zero (the default) means unlimited, which keeps the feature backwards
// compatible; negative values are rejected.
type LimitsConfig struct {
	// MaxConcurrent is the global cap on active inference requests. 0 = unlimited.
	MaxConcurrent int `yaml:"max_concurrent"`
	// MaxConcurrentPerClient caps a single authenticated client. Clients not
	// listed here are limited only by the global cap. 0 = unlimited.
	MaxConcurrentPerClient map[string]int `yaml:"max_concurrent_per_client"`
}

// TimeoutsConfig bounds inbound request handling. Every value must be
// positive (0 selects the default; negative is rejected). These are inbound
// read/idle deadlines only: there is deliberately no overall write or request
// timeout, which would truncate healthy long-lived SSE streams.
type TimeoutsConfig struct {
	// Header bounds reading request headers and the request line.
	Header Duration `yaml:"header"`
	// Body bounds reading a request body (inference, ingest and admit).
	Body Duration `yaml:"body"`
	// Idle is the keep-alive idle timeout between requests on a connection.
	Idle Duration `yaml:"idle"`
	// Shutdown is how long a graceful shutdown drains in-flight requests.
	Shutdown Duration `yaml:"shutdown"`
}

type ClientConfig struct {
	Name    string `yaml:"name"`
	Class   string `yaml:"class"`
	KeyFile string `yaml:"key_file"`
	// KeyFiles lists additional active key files so a client key can be
	// rotated without downtime (every listed key authenticates as this
	// client). Exactly one of key_file and key_files must be set; key_file
	// keeps its original single-key meaning and default resolution.
	KeyFiles []string `yaml:"key_files"`
	// Host attributes this client's proxied requests to a machine.
	Host string `yaml:"host"`
	// Ingest lets this client's key push agent data to /control/v1/ingest.
	Ingest bool `yaml:"ingest"`
}

// KeyPaths returns every configured key file for the client, in order, as a
// copy the caller may keep (key_file first, then key_files). Callers build a
// client name -> paths map to load keys; the copy protects the loaded config
// from mutation.
func (c ClientConfig) KeyPaths() []string {
	if len(c.KeyFiles) == 0 {
		if c.KeyFile == "" {
			return nil
		}
		return []string{c.KeyFile}
	}
	out := make([]string, 0, len(c.KeyFiles)+1)
	if c.KeyFile != "" {
		out = append(out, c.KeyFile)
	}
	return append(out, c.KeyFiles...)
}

type AccountConfig struct {
	ID         string `yaml:"id"`
	Provider   string `yaml:"provider"`
	BaseURL    string `yaml:"base_url"`
	APIKeyFile string `yaml:"api_key_file"`
	APIKeyEnv  string `yaml:"api_key_env"`
	// ManagementKeyFile / ManagementKeyEnv (openrouter only) locate an
	// optional OpenRouter management key used solely for GET /credits (the
	// account balance). It is never attached to inference or GET /key. File
	// is preferred when both are set.
	ManagementKeyFile string `yaml:"management_key_file"`
	ManagementKeyEnv  string `yaml:"management_key_env"`
	// CredentialsFile is the Claude Code credentials file read (never
	// written) for provider claude. Default ~/.claude/.credentials.json.
	CredentialsFile string `yaml:"credentials_file"`
	// QuotaSource (claude only): "local" (default) or "agent".
	QuotaSource string             `yaml:"quota_source"`
	Reserve     map[string]float64 `yaml:"reserve"`
	CostBasis   string             `yaml:"cost_basis"`
}

type RouteConfig struct {
	Name          string   `yaml:"name"`
	Models        []string `yaml:"models"`
	UpstreamModel string   `yaml:"upstream_model"`
	Interactive   []string `yaml:"interactive"`
	Background    []string `yaml:"background"`
	// Upstreams, when non-empty, opts the route into capability routing. It
	// maps a candidate account id (from interactive ∪ background) to its
	// capability descriptor; a descriptor is required for every candidate and
	// unknown keys are rejected. Absent/empty preserves the legacy
	// unconstrained route.
	Upstreams map[string]UpstreamSpec `yaml:"upstreams"`
}

// UpstreamSpec is one candidate account's capability descriptor inside
// routes[].upstreams. It is a config mirror of core.UpstreamSpec.
type UpstreamSpec struct {
	// UpstreamModel overrides the route-wide upstream_model for this
	// candidate; "" inherits the route's upstream_model (which, when also
	// empty, forwards the client model unchanged).
	UpstreamModel string `yaml:"upstream_model"`
	// Protocols is required (non-empty) when the route opts in: an explicit
	// subset of {"chat", "responses"}. Codex accounts cannot serve "chat".
	Protocols []string `yaml:"protocols"`
	// InputModalities is required (non-empty) when the route opts in: an
	// explicit subset of {"text", "image"}. Declaring only "image" is a
	// vision-only candidate and does not imply "text".
	InputModalities []string `yaml:"input_modalities"`
	// Tools / JSONSchema / Stream are tri-state by convention: absent (false)
	// means the optional feature is unsupported for this candidate.
	Tools      bool `yaml:"tools"`
	JSONSchema bool `yaml:"json_schema"`
	Stream     bool `yaml:"stream"`
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
	c.applyDefaults(filepath.Dir(absOr(path)))
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
	if c.Timeouts.Header == 0 {
		c.Timeouts.Header = Duration(10 * time.Second)
	}
	if c.Timeouts.Body == 0 {
		c.Timeouts.Body = Duration(30 * time.Second)
	}
	if c.Timeouts.Idle == 0 {
		c.Timeouts.Idle = Duration(120 * time.Second)
	}
	if c.Timeouts.Shutdown == 0 {
		c.Timeouts.Shutdown = Duration(30 * time.Second)
	}
	for i := range c.Clients {
		c.Clients[i].KeyFile = expand(c.Clients[i].KeyFile, baseDir)
		for j := range c.Clients[i].KeyFiles {
			c.Clients[i].KeyFiles[j] = expand(c.Clients[i].KeyFiles[j], baseDir)
		}
	}
	if c.TLSCertFile != "" {
		c.TLSCertFile = expand(c.TLSCertFile, baseDir)
	}
	if c.HostName == "" {
		c.HostName = defaultHostName()
	}
	if c.TLSKeyFile != "" {
		c.TLSKeyFile = expand(c.TLSKeyFile, baseDir)
	}
	if c.ClaudeLogs.Dir == "" {
		c.ClaudeLogs.Dir = "~/.claude/projects"
	}
	c.ClaudeLogs.Dir = expand(c.ClaudeLogs.Dir, baseDir)
	if c.ClaudeLogs.ScanInterval == 0 {
		c.ClaudeLogs.ScanInterval = Duration(time.Minute)
	}
	if c.ClaudeLogs.Client == "" {
		c.ClaudeLogs.Client = "claude-code"
	}
	if c.HermesLogs.Home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			c.HermesLogs.Home = filepath.Join(h, ".hermes")
		}
	} else {
		c.HermesLogs.Home = expand(c.HermesLogs.Home, baseDir)
	}
	if c.HermesLogs.Client == "" {
		c.HermesLogs.Client = "hermes"
	}
	if c.HermesLogs.ScanInterval == 0 {
		c.HermesLogs.ScanInterval = Duration(time.Minute)
	}
	for i := range c.Accounts {
		a := &c.Accounts[i]
		a.APIKeyFile = expand(a.APIKeyFile, baseDir)
		a.ManagementKeyFile = expand(a.ManagementKeyFile, baseDir)
		if a.Provider == core.ProviderClaude {
			if a.CredentialsFile == "" {
				a.CredentialsFile = "~/.claude/.credentials.json"
			}
			a.CredentialsFile = expand(a.CredentialsFile, baseDir)
			if a.BaseURL == "" {
				a.BaseURL = "https://api.anthropic.com"
			}
			if a.QuotaSource == "" {
				a.QuotaSource = "local"
			}
		}
		if a.BaseURL == "" {
			switch a.Provider {
			case core.ProviderCodex:
				a.BaseURL = "https://chatgpt.com/backend-api/codex"
			case core.ProviderOllama:
				a.BaseURL = "https://ollama.com/v1"
			case core.ProviderOpenRouter:
				a.BaseURL = "https://openrouter.ai/api/v1"
			}
		}
		a.BaseURL = strings.TrimRight(a.BaseURL, "/")
		if a.CostBasis == "" {
			if a.Provider == core.ProviderCodex || a.Provider == core.ProviderOllama || a.Provider == core.ProviderClaude {
				a.CostBasis = "api_equivalent"
			} else {
				a.CostBasis = "metered"
			}
		}
	}
}

var hostNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// defaultHostName is the short OS hostname, lowercased ("" if unknown).
func defaultHostName() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return strings.ToLower(h)
}

func absOr(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
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
	if c.HostName != "" && !hostNameRE.MatchString(c.HostName) {
		errs = append(errs, fmt.Errorf("host_name %q must match [A-Za-z0-9._-]{1,64}", c.HostName))
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		errs = append(errs, errors.New("tls_cert_file and tls_key_file must be set together"))
	}
	if c.AllowNonLoopback && !c.Control.RequireAuth {
		errs = append(errs, errors.New("allow_non_loopback requires control.require_auth: true (the control API would otherwise be open to the network)"))
	}
	if len(c.AllowedHosts) > 0 && !c.AllowNonLoopback {
		errs = append(errs, errors.New("allowed_hosts requires allow_non_loopback: true"))
	}
	for _, h := range c.AllowedHosts {
		if h == "" || strings.ContainsAny(h, " /") {
			errs = append(errs, fmt.Errorf("allowed_hosts entry %q invalid", h))
		}
	}
	if c.Policy.SafetyMargin < 0 || c.Policy.SafetyMargin >= 1 {
		errs = append(errs, errors.New("policy.safety_margin must be in [0,1)"))
	}
	if c.Policy.InflightEstimate < 0 || c.Policy.InflightEstimate >= 1 {
		errs = append(errs, errors.New("policy.inflight_estimate must be in [0,1)"))
	}
	if c.Limits.MaxConcurrent < 0 {
		errs = append(errs, errors.New("limits.max_concurrent must not be negative (0 = unlimited)"))
	}
	for name, lim := range c.Limits.MaxConcurrentPerClient {
		if lim < 0 {
			errs = append(errs, fmt.Errorf("limits.max_concurrent_per_client[%s] must not be negative (0 = unlimited)", name))
		}
	}
	for _, to := range []struct {
		name string
		d    Duration
	}{
		{"header", c.Timeouts.Header},
		{"body", c.Timeouts.Body},
		{"idle", c.Timeouts.Idle},
		{"shutdown", c.Timeouts.Shutdown},
	} {
		if to.d < 0 {
			errs = append(errs, fmt.Errorf("timeouts.%s must not be negative", to.name))
		}
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
		nfiles := 0
		if cl.KeyFile != "" {
			nfiles++
		}
		if len(cl.KeyFiles) > 0 {
			nfiles++
		}
		if nfiles != 1 {
			errs = append(errs, fmt.Errorf("client %s: exactly one of key_file or key_files is required", cl.Name))
		}
		seenKF := map[string]bool{}
		for _, kf := range cl.KeyFiles {
			if kf == "" {
				errs = append(errs, fmt.Errorf("client %s: key_files entries must not be empty", cl.Name))
				continue
			}
			if seenKF[kf] {
				errs = append(errs, fmt.Errorf("client %s: duplicate key_files entry %s", cl.Name, kf))
			}
			seenKF[kf] = true
		}
		if strings.ContainsAny(cl.Host, " /\\") {
			errs = append(errs, fmt.Errorf("client %s: host %q invalid", cl.Name, cl.Host))
		}
	}
	for name := range c.Limits.MaxConcurrentPerClient {
		if !seenC[name] {
			errs = append(errs, fmt.Errorf("limits.max_concurrent_per_client names unknown client %q", name))
		}
	}
	accts := map[string]bool{}
	provider := map[string]string{}
	for _, a := range c.Accounts {
		provider[a.ID] = a.Provider
		if a.ID == "" || accts[a.ID] || strings.ContainsAny(a.ID, "/\\. ") {
			errs = append(errs, fmt.Errorf("account id %q empty, duplicate, or contains / \\ . or space", a.ID))
		}
		accts[a.ID] = true
		switch a.Provider {
		case core.ProviderCodex, core.ProviderClaude:
		case core.ProviderOllama, core.ProviderOpenAICompat, core.ProviderOpenRouter:
			if a.APIKeyFile == "" && a.APIKeyEnv == "" {
				errs = append(errs, fmt.Errorf("account %s: api_key_file or api_key_env required", a.ID))
			}
			if a.BaseURL == "" {
				errs = append(errs, fmt.Errorf("account %s: base_url required", a.ID))
			}
		default:
			errs = append(errs, fmt.Errorf("account %s: unknown provider %q", a.ID, a.Provider))
		}
		if a.Provider != core.ProviderOpenRouter && (a.ManagementKeyFile != "" || a.ManagementKeyEnv != "") {
			errs = append(errs, fmt.Errorf("account %s: management_key_file/management_key_env apply only to openrouter accounts", a.ID))
		}
		if a.Provider == core.ProviderOpenRouter && len(a.Reserve) > 0 {
			// Prepaid credit has no rolling 5h/weekly windows to reserve.
			errs = append(errs, fmt.Errorf("account %s: reserve does not apply to prepaid openrouter accounts (no 5h/weekly windows)", a.ID))
		}
		for k, v := range a.Reserve {
			if k != core.Window5h && k != core.WindowWeekly {
				errs = append(errs, fmt.Errorf("account %s: reserve window %q must be 5h or weekly", a.ID, k))
			}
			if v < 0 || v >= 1 {
				errs = append(errs, fmt.Errorf("account %s: reserve %s must be in [0,1)", a.ID, k))
			}
		}
		if a.Provider == core.ProviderClaude && a.QuotaSource != "local" && a.QuotaSource != "agent" {
			errs = append(errs, fmt.Errorf("account %s: quota_source must be local or agent", a.ID))
		}
		if a.Provider != core.ProviderClaude && a.QuotaSource != "" {
			errs = append(errs, fmt.Errorf("account %s: quota_source applies only to claude accounts", a.ID))
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
			} else if provider[id] == core.ProviderClaude {
				errs = append(errs, fmt.Errorf("route %s: account %q is a quota-only claude account and cannot serve inference", r.Name, id))
			}
		}
		errs = append(errs, validateUpstreams(r, provider)...)
	}
	if c.ClaudeLogs.Enabled {
		if c.ClaudeLogs.Account == "" {
			errs = append(errs, errors.New("claude_logs.account is required when claude_logs.enabled"))
		} else if provider[c.ClaudeLogs.Account] != core.ProviderClaude {
			errs = append(errs, fmt.Errorf("claude_logs.account %q must be a configured claude account", c.ClaudeLogs.Account))
		}
	}
	return errors.Join(errs...)
}

// Capability-routing vocabulary. Kept unexported: the values are part of the
// wire/config contract, but proxy/control match them as plain strings so the
// frozen core package gains no new exported identifiers.
const (
	protocolChat      = "chat"
	protocolResponses = "responses"
	modalityText      = "text"
	modalityImage     = "image"
)

// validateUpstreams enforces the capability-routing contract for one route. A
// nil/empty Upstreams map means the route is unconstrained (legacy permissive)
// and is skipped entirely. When the route opts in, every candidate in
// interactive ∪ background MUST have a descriptor and no other keys may
// appear; protocols must be an explicit non-empty subset of {chat, responses}
// with no duplicates (and codex accounts may not advertise chat);
// input_modalities must be an explicit non-empty subset of {text, image} with
// no duplicates (declaring image alone is a vision-only candidate and does not
// imply text). The boolean feature flags are tri-state by absence: false means
// unsupported, so they need no further validation. Deliberately NOT enforced:
// requiring an image-capable candidate on a text-only route — capability
// declarations only narrow a candidate's applicability, they never make a new
// demand on the route.
func validateUpstreams(r RouteConfig, provider map[string]string) []error {
	if len(r.Upstreams) == 0 {
		return nil
	}
	var errs []error
	cands := map[string]bool{}
	for _, id := range r.Interactive {
		cands[id] = true
	}
	for _, id := range r.Background {
		cands[id] = true
	}
	for id := range r.Upstreams {
		if !cands[id] {
			errs = append(errs, fmt.Errorf("route %s: upstreams names unknown candidate account %q", r.Name, id))
		}
	}
	for id := range cands {
		if _, ok := r.Upstreams[id]; !ok {
			errs = append(errs, fmt.Errorf("route %s: upstreams is missing a descriptor for candidate %q", r.Name, id))
		}
	}
	for id, spec := range r.Upstreams {
		if len(spec.Protocols) == 0 {
			errs = append(errs, fmt.Errorf("route %s: upstreams[%s]: protocols must be a non-empty subset of chat,responses", r.Name, id))
		} else {
			seen := map[string]bool{}
			for _, p := range spec.Protocols {
				p = strings.TrimSpace(p)
				if p != protocolChat && p != protocolResponses {
					errs = append(errs, fmt.Errorf("route %s: upstreams[%s]: invalid protocol %q (want chat or responses)", r.Name, id, p))
					continue
				}
				if seen[p] {
					errs = append(errs, fmt.Errorf("route %s: upstreams[%s]: duplicate protocol %q", r.Name, id, p))
					continue
				}
				seen[p] = true
				if p == protocolChat && provider[id] == core.ProviderCodex {
					errs = append(errs, fmt.Errorf("route %s: upstreams[%s]: codex accounts cannot serve chat", r.Name, id))
				}
			}
		}
		if len(spec.InputModalities) == 0 {
			errs = append(errs, fmt.Errorf("route %s: upstreams[%s]: input_modalities must be a non-empty subset of text,image", r.Name, id))
			continue
		}
		seen := map[string]bool{}
		for _, m := range spec.InputModalities {
			m = strings.TrimSpace(m)
			if m != modalityText && m != modalityImage {
				errs = append(errs, fmt.Errorf("route %s: upstreams[%s]: invalid input modality %q (want text or image)", r.Name, id, m))
				continue
			}
			if seen[m] {
				errs = append(errs, fmt.Errorf("route %s: upstreams[%s]: duplicate input modality %q", r.Name, id, m))
				continue
			}
			seen[m] = true
		}
	}
	return errs
}

// CoreAccounts converts account config into core.Account values.
func (c *Config) CoreAccounts() []core.Account {
	out := make([]core.Account, 0, len(c.Accounts))
	for _, a := range c.Accounts {
		r := map[string]float64{}
		for k, v := range a.Reserve {
			r[k] = v
		}
		out = append(out, core.Account{ID: a.ID, Provider: a.Provider, BaseURL: a.BaseURL, Reserve: r, CostBasis: a.CostBasis, QuotaSource: a.QuotaSource})
	}
	return out
}

// CoreRoutes converts route config into core.Route values. A route with no
// background list falls back to its interactive list. The projection is a
// deep copy: every map and slice is freshly allocated (and the new capability
// descriptors have their whitespace trimmed), so a returned core.Route never
// aliases the loaded config and callers cannot mutate shared state.
func (c *Config) CoreRoutes() []core.Route {
	out := make([]core.Route, 0, len(c.Routes))
	for _, r := range c.Routes {
		bg := r.Background
		if len(bg) == 0 {
			bg = r.Interactive
		}
		var ups map[string]core.UpstreamSpec
		if len(r.Upstreams) > 0 {
			ups = make(map[string]core.UpstreamSpec, len(r.Upstreams))
			for id, s := range r.Upstreams {
				ups[id] = core.UpstreamSpec{
					UpstreamModel:   strings.TrimSpace(s.UpstreamModel),
					Protocols:       copyTrimmed(s.Protocols),
					InputModalities: copyTrimmed(s.InputModalities),
					Tools:           s.Tools,
					JSONSchema:      s.JSONSchema,
					Stream:          s.Stream,
				}
			}
		}
		out = append(out, core.Route{
			Name:          r.Name,
			Models:        append([]string(nil), r.Models...),
			UpstreamModel: r.UpstreamModel,
			Interactive:   append([]string(nil), r.Interactive...),
			Background:    append([]string(nil), bg...),
			Upstreams:     ups,
		})
	}
	return out
}

// copyTrimmed returns a whitespace-trimmed copy of in, or nil when in is empty
// (an empty slice means "text only" for input_modalities). It never aliases
// the caller's slice.
func copyTrimmed(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = strings.TrimSpace(v)
	}
	return out
}

// CheckFiles verifies file permissions that Validate (no I/O) does not:
// every client key file, api_key_file, management_key_file and tls_key_file
// must be a regular file that is not group/world accessible
// (core.CheckPrivateFile); the config file at configPath and data_dir must
// not be group/world writable, since they decide which files are read as
// secrets and where tokens are stored. A data_dir that does not exist yet is
// accepted (serve creates it 0700). `localrouter check` and `localrouter
// serve` fail when this returns an error.
func (c *Config) CheckFiles(configPath string) error {
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	add(core.CheckNotWritableByOthers("config file", configPath))
	if _, err := os.Stat(c.DataDir); !errors.Is(err, os.ErrNotExist) {
		add(core.CheckNotWritableByOthers("data_dir", c.DataDir))
	}
	for _, cl := range c.Clients {
		for _, p := range cl.KeyPaths() {
			add(core.CheckPrivateFile("client "+cl.Name+" key file", p))
		}
	}
	for _, a := range c.Accounts {
		if a.APIKeyFile != "" {
			add(core.CheckPrivateFile("account "+a.ID+" api_key_file", a.APIKeyFile))
		}
		if a.ManagementKeyFile != "" {
			add(core.CheckPrivateFile("account "+a.ID+" management_key_file", a.ManagementKeyFile))
		}
	}
	if c.TLSKeyFile != "" {
		add(core.CheckPrivateFile("tls_key_file", c.TLSKeyFile))
	}
	return errors.Join(errs...)
}
