package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyFilesResolutionAndKeyPaths(t *testing.T) {
	c, err := Load(write(t, `
clients:
  - {name: a, class: interactive, key_files: [ka1.key, ka2.key]}
  - {name: b, class: interactive, key_file: kb.key}
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Clients[0].KeyFiles) != 2 {
		t.Fatalf("key_files not parsed: %+v", c.Clients[0])
	}
	for _, p := range c.Clients[0].KeyFiles {
		if !filepath.IsAbs(p) {
			t.Fatalf("key_files entry not resolved absolute: %s", p)
		}
	}
	// key_file retains its original semantics and single-path resolution.
	if !filepath.IsAbs(c.Clients[1].KeyFile) {
		t.Fatalf("key_file not resolved absolute: %s", c.Clients[1].KeyFile)
	}

	// KeyPaths: legacy key_file -> one path; key_files -> all paths.
	ap := c.Clients[0].KeyPaths()
	bp := c.Clients[1].KeyPaths()
	if len(ap) != 2 || ap[0] != c.Clients[0].KeyFiles[0] || ap[1] != c.Clients[0].KeyFiles[1] {
		t.Fatalf("KeyPaths(key_files)=%v", ap)
	}
	if len(bp) != 1 || bp[0] != c.Clients[1].KeyFile {
		t.Fatalf("KeyPaths(key_file)=%v", bp)
	}

	// KeyPaths returns a copy: callers must not be able to mutate config.
	ap[0] = "/mutated"
	if c.Clients[0].KeyFiles[0] == "/mutated" {
		t.Fatal("KeyPaths aliased the config slice")
	}
}

func TestClientKeyExclusivity(t *testing.T) {
	both := `
clients:
  - {name: a, class: interactive, key_file: k, key_files: [k2]}
`
	neither := `
clients:
  - {name: a, class: interactive}
`
	for name, body := range map[string]string{"both set": both, "neither set": neither} {
		_, err := Load(write(t, body))
		if err == nil {
			t.Errorf("%s: expected error", name)
			continue
		}
		if !strings.Contains(err.Error(), "key_file") {
			t.Errorf("%s: error should mention key_file/key_files: %v", name, err)
		}
	}
}

func TestClientKeyFilesValidation(t *testing.T) {
	_, err := Load(write(t, `
clients:
  - {name: a, class: interactive, key_files: [k1, ""]}
  - {name: b, class: interactive, key_files: [k1, k1]}
  - {name: c, class: interactive, key_files: [same, same]}
`))
	if err == nil {
		t.Fatal("expected key_files validation errors")
	}
	for _, want := range []string{"empty", "duplicate"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}
