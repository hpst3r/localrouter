package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// secFixture writes a config whose every secret file is 0600, plus a data
// dir, and returns the config path and loaded config.
func secFixture(t *testing.T) (string, *Config) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits not enforced on windows")
	}
	dir := t.TempDir()
	for _, f := range []string{"c.key", "c2.key", "or.key", "or-mgmt.key", "tls.key", "tls.crt"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("secret-material-0123456789\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "config.yaml")
	body := `
data_dir: data
tls_cert_file: tls.crt
tls_key_file: tls.key
clients:
  - {name: c, class: interactive, key_files: [c.key, c2.key]}
accounts:
  - {id: or, provider: openrouter, base_url: "https://openrouter.ai/api/v1", api_key_file: or.key, management_key_file: or-mgmt.key}
  - {id: ol, provider: ollama, base_url: "https://ollama.com/v1", api_key_env: OLLAMA_KEY}
routes:
  - {name: r, models: [m], interactive: [or]}
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return p, c
}

// CRED-1: every secret file must be 0600-style; the config file and data_dir
// must not be group/world writable.
func TestSecCheckFiles(t *testing.T) {
	p, c := secFixture(t)
	dir := filepath.Dir(p)
	if err := c.CheckFiles(p); err != nil {
		t.Fatalf("private files rejected: %v", err)
	}
	// config.yaml 0644 and a 0755 data_dir are fine (not writable by others).
	if err := os.Chmod(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckFiles(p); err != nil {
		t.Fatalf("0755 data_dir rejected: %v", err)
	}

	for _, tc := range []struct {
		path string
		mode os.FileMode
		want string
	}{
		{"c.key", 0o644, "client c key file"},
		{"c2.key", 0o640, "client c key file"},
		{"or.key", 0o644, "account or api_key_file"},
		{"or-mgmt.key", 0o604, "account or management_key_file"},
		{"tls.key", 0o644, "tls_key_file"},
		{"config.yaml", 0o664, "config file"},
		{"data", 0o777, "data_dir"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			f := filepath.Join(dir, tc.path)
			fi, err := os.Stat(f)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(f, tc.mode); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(f, fi.Mode().Perm())
			err = c.CheckFiles(p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("mode %#o: want error mentioning %q, got %v", tc.mode, tc.want, err)
			}
			if strings.Contains(err.Error(), "secret-material") {
				t.Fatalf("error leaks file content: %v", err)
			}
		})
	}
}

func TestSecCheckFilesMissingSecret(t *testing.T) {
	p, c := secFixture(t)
	if err := os.Remove(filepath.Join(filepath.Dir(p), "or.key")); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckFiles(p); err == nil || !strings.Contains(err.Error(), "api_key_file") {
		t.Fatalf("missing api_key_file accepted: %v", err)
	}
}

// A data_dir that does not exist yet is created 0700 by serve; not an error.
func TestSecCheckFilesDataDirNotYetCreated(t *testing.T) {
	p, c := secFixture(t)
	c.DataDir = filepath.Join(filepath.Dir(p), "not-yet")
	if err := c.CheckFiles(p); err != nil {
		t.Fatalf("missing data_dir rejected: %v", err)
	}
}
