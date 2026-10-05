package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Credential sources.
const (
	SourceAuto     = "auto"
	SourceFile     = "file"
	SourceKeychain = "keychain"
)

// Defaults for Config.
const (
	DefaultPushInterval    = time.Minute
	DefaultQuotaInterval   = 5 * time.Minute
	DefaultKeychainService = "Claude Code-credentials"
)

// hostRe is the allowed agent host name (same rule as the ingest endpoint).
var hostRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Config is a validated agent configuration with defaults applied and all
// paths absolute.
type Config struct {
	Server            string // base URL of the central server (http or https)
	Host              string
	KeyFile           string
	Account           string // claude account id on the server
	ClaudeProjectsDir string
	Credentials       CredentialsConfig
	PushInterval      time.Duration
	QuotaInterval     time.Duration // 0 disables quota pushes
	StateDir          string
}

// CredentialsConfig selects how the Claude Code token is read (read-only).
type CredentialsConfig struct {
	Source          string // auto | file | keychain
	File            string
	KeychainService string
	// KeychainAccount defaults to $USER (Claude Code's own convention).
	KeychainAccount string
}

type fileConfig struct {
	Server            string    `yaml:"server"`
	Host              string    `yaml:"host"`
	KeyFile           string    `yaml:"key_file"`
	Account           string    `yaml:"account"`
	ClaudeProjectsDir string    `yaml:"claude_projects_dir"`
	Credentials       fileCreds `yaml:"credentials"`
	PushInterval      *duration `yaml:"push_interval"`
	QuotaInterval     *duration `yaml:"quota_interval"`
	StateDir          string    `yaml:"state_dir"`
}

type fileCreds struct {
	Source          string `yaml:"source"`
	File            string `yaml:"file"`
	KeychainService string `yaml:"keychain_service"`
	KeychainAccount string `yaml:"keychain_account"`
}

type duration time.Duration

func (d *duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", n.Value, err)
	}
	*d = duration(v)
	return nil
}

// DefaultConfigPath is ~/.config/localrouter/agent.yaml (os.UserConfigDir).
func DefaultConfigPath() string {
	d, err := os.UserConfigDir()
	if err != nil {
		return "agent.yaml"
	}
	return filepath.Join(d, "localrouter", "agent.yaml")
}

// LoadConfig reads, defaults, and validates an agent config file. Relative
// paths are resolved against the config file's directory; "~/" expands to
// the home directory.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	home, _ := os.UserHomeDir()
	base := filepath.Dir(absOr(path))
	return parseConfig(b, base, home, runtime.GOOS)
}

func parseConfig(b []byte, baseDir, home, goos string) (*Config, error) {
	var fc fileConfig
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&fc); err != nil {
		return nil, fmt.Errorf("parse agent config: %w", err)
	}
	exp := func(p, def string) string {
		if p == "" {
			p = def
		}
		if p == "" {
			return ""
		}
		if p == "~" {
			p = home
		} else if strings.HasPrefix(p, "~/") {
			p = filepath.Join(home, p[2:])
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(baseDir, p)
		}
		return filepath.Clean(p)
	}
	defState := "~/.local/state/localrouter-agent"
	if goos == "darwin" {
		defState = "~/Library/Application Support/localrouter-agent"
	}
	c := &Config{
		Server:            strings.TrimRight(fc.Server, "/"),
		Host:              fc.Host,
		KeyFile:           exp(fc.KeyFile, ""),
		Account:           fc.Account,
		ClaudeProjectsDir: exp(fc.ClaudeProjectsDir, "~/.claude/projects"),
		Credentials: CredentialsConfig{
			Source:          fc.Credentials.Source,
			File:            exp(fc.Credentials.File, "~/.claude/.credentials.json"),
			KeychainService: fc.Credentials.KeychainService,
			KeychainAccount: fc.Credentials.KeychainAccount,
		},
		PushInterval:  DefaultPushInterval,
		QuotaInterval: DefaultQuotaInterval,
		StateDir:      exp(fc.StateDir, defState),
	}
	if c.Credentials.Source == "" {
		c.Credentials.Source = SourceAuto
	}
	if c.Credentials.KeychainService == "" {
		c.Credentials.KeychainService = DefaultKeychainService
	}
	if fc.PushInterval != nil {
		c.PushInterval = time.Duration(*fc.PushInterval)
	}
	if fc.QuotaInterval != nil {
		c.QuotaInterval = time.Duration(*fc.QuotaInterval)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Validate checks the configuration.
func (c *Config) Validate() error {
	var errs []error
	u, err := url.Parse(c.Server)
	switch {
	case c.Server == "":
		errs = append(errs, errors.New("server is required"))
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		errs = append(errs, errors.New("server must be an http:// or https:// URL"))
	case u.User != nil || u.RawQuery != "" || u.Fragment != "":
		errs = append(errs, errors.New("server must not contain credentials, query, or fragment"))
	}
	if !hostRe.MatchString(c.Host) {
		errs = append(errs, fmt.Errorf("host %q invalid: must match [A-Za-z0-9._-]{1,64}", c.Host))
	}
	if c.KeyFile == "" {
		errs = append(errs, errors.New("key_file is required"))
	}
	if c.Account == "" {
		errs = append(errs, errors.New("account is required"))
	}
	switch c.Credentials.Source {
	case SourceAuto, SourceFile, SourceKeychain:
	default:
		errs = append(errs, fmt.Errorf("credentials.source %q invalid: auto, file, or keychain", c.Credentials.Source))
	}
	if c.PushInterval < time.Second {
		errs = append(errs, errors.New("push_interval must be at least 1s"))
	}
	if c.QuotaInterval != 0 && c.QuotaInterval < 30*time.Second {
		errs = append(errs, errors.New("quota_interval must be 0 (disabled) or at least 30s"))
	}
	return errors.Join(errs...)
}

// ReadKey reads a client key file, trimming whitespace. The key is never
// included in errors.
func ReadKey(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("key_file: %w", stripPath(err))
	}
	k := strings.TrimSpace(string(b))
	if k == "" || strings.ContainsAny(k, " \t\r\n") {
		return "", errors.New("key_file: must contain exactly one key")
	}
	return k, nil
}

// stripPath drops the file path from an *fs.PathError so logs stay path-free.
func stripPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

func absOr(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}
