package weblogin

import (
	"context"
	"strings"
	"testing"
	"time"
)

type nopHooks struct{}

func (nopHooks) CompleteLogin(context.Context, VerifiedLogin, bool) (Session, error) {
	return Session{}, nil
}
func (nopHooks) LoginDenied(context.Context, string, string, string) error { return nil }
func (nopHooks) Logout(context.Context, string, string) error              { return nil }

func validConfig() Config {
	return Config{
		PublicBaseURL: "https://router.example.test",
		Issuer:        "https://idp.example.test/tenant",
		ClientID:      "client-1",
		ClientSecret:  func() (string, error) { return "s3cret", nil },
		Policy:        testPolicy(),
		Hooks:         nopHooks{},
	}
}

func TestNewAcceptsValidConfigAndAppliesDefaults(t *testing.T) {
	s, err := New(validConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := s.cfg
	if c.ClientAuth != ClientSecretBasic || strings.Join(c.Scopes, " ") != "openid profile" ||
		strings.Join(c.SigningAlgs, " ") != "RS256" || c.LoginTimeout != 10*time.Minute ||
		c.Outbound.Timeout != 10*time.Second || c.Outbound.MaxBodyBytes != 1<<20 ||
		c.MaxRedeemedLogins != 256 || c.MaxConcurrentExchanges != 4 ||
		c.DiscoveryRetry != 30*time.Second || c.Clock == nil || c.Logger == nil {
		t.Fatalf("defaults not applied: %+v", c)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	cases := map[string]func(*Config){
		"http public url":         func(c *Config) { c.PublicBaseURL = "http://router.example.test" },
		"public url path":         func(c *Config) { c.PublicBaseURL = "https://router.example.test/app" },
		"public url query":        func(c *Config) { c.PublicBaseURL = "https://router.example.test?x=1" },
		"public url fragment":     func(c *Config) { c.PublicBaseURL = "https://router.example.test#f" },
		"public url userinfo":     func(c *Config) { c.PublicBaseURL = "https://u:p@router.example.test" },
		"public url no host":      func(c *Config) { c.PublicBaseURL = "https://" },
		"http issuer":             func(c *Config) { c.Issuer = "http://idp.example.test/tenant" },
		"issuer query":            func(c *Config) { c.Issuer = "https://idp.example.test/?t=1" },
		"issuer userinfo":         func(c *Config) { c.Issuer = "https://u@idp.example.test/" },
		"entra organizations":     func(c *Config) { c.Issuer = "https://login.microsoftonline.com/organizations/v2.0" },
		"entra common":            func(c *Config) { c.Issuer = "https://login.microsoftonline.com/common/v2.0" },
		"entra template":          func(c *Config) { c.Issuer = "https://login.microsoftonline.com/{tenantid}/v2.0" },
		"empty client id":         func(c *Config) { c.ClientID = "" },
		"client id whitespace":    func(c *Config) { c.ClientID = "a b" },
		"no secret":               func(c *Config) { c.ClientSecret = nil },
		"auto client auth":        func(c *Config) { c.ClientAuth = "auto" },
		"no openid":               func(c *Config) { c.Scopes = []string{"profile"} },
		"offline_access":          func(c *Config) { c.Scopes = []string{"openid", "offline_access"} },
		"bad scope token":         func(c *Config) { c.Scopes = []string{"openid", "a\"b"} },
		"HS256":                   func(c *Config) { c.SigningAlgs = []string{"HS256"} },
		"none alg":                func(c *Config) { c.SigningAlgs = []string{"RS256", "none"} },
		"tenant not in issuer":    func(c *Config) { c.TenantID = "00000000-0000-0000-0000-000000000000" },
		"no hooks":                func(c *Config) { c.Hooks = nil },
		"values without claim":    func(c *Config) { c.Policy.Claim = "" },
		"reserved claim":          func(c *Config) { c.Policy.Claim = "email" },
		"bad claim name":          func(c *Config) { c.Policy.Claim = "ro les" },
		"no admin path":           func(c *Config) { c.Policy.AdminValues, c.Policy.AdminSubjects = nil, nil },
		"no user path":            func(c *Config) { c.Policy.UserValues, c.Policy.AdminValues = nil, nil },
		"empty value":             func(c *Config) { c.Policy.UserValues = []string{""} },
		"duplicate value":         func(c *Config) { c.Policy.UserValues = []string{"a", "a"} },
		"empty admin subject":     func(c *Config) { c.Policy.AdminSubjects = []string{""} },
		"login timeout too short": func(c *Config) { c.LoginTimeout = 30 * time.Second },
		"login timeout too long":  func(c *Config) { c.LoginTimeout = time.Hour },
		"http timeout too long":   func(c *Config) { c.Outbound.Timeout = 2 * time.Minute },
		"extra host with scheme":  func(c *Config) { c.Outbound.ExtraEndpointHosts = []string{"https://x.test"} },
		"extra host with port":    func(c *Config) { c.Outbound.ExtraEndpointHosts = []string{"x.test:443"} },
		"negative redeemed":       func(c *Config) { c.MaxRedeemedLogins = -1 },
		"redeemed too large":      func(c *Config) { c.MaxRedeemedLogins = 1<<16 + 1 },
		"negative exchanges":      func(c *Config) { c.MaxConcurrentExchanges = -1 },
	}
	for name, mutate := range cases {
		c := validConfig()
		mutate(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s: New accepted invalid config", name)
		} else if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%s: error leaks secret: %v", name, err)
		}
	}
}
