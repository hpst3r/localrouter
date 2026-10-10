package core

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCheckPrivateFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "k")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivateFile("key file", p); err != nil {
		t.Fatalf("0600: %v", err)
	}
	if runtime.GOOS != "windows" {
		for _, m := range []os.FileMode{0o640, 0o604, 0o644, 0o660} {
			if err := os.Chmod(p, m); err != nil {
				t.Fatal(err)
			}
			if err := CheckPrivateFile("key file", p); err == nil {
				t.Fatalf("mode %#o accepted", m)
			}
		}
	}
	if err := CheckPrivateFile("key file", dir); err == nil {
		t.Fatal("directory accepted")
	}
	if err := CheckPrivateFile("key file", filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestCheckNotWritableByOthers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	p := filepath.Join(t.TempDir(), "c")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckNotWritableByOthers("config", p); err != nil {
		t.Fatalf("0644: %v", err)
	}
	for _, m := range []os.FileMode{0o664, 0o646} {
		_ = os.Chmod(p, m)
		if err := CheckNotWritableByOthers("config", p); err == nil {
			t.Fatalf("mode %#o accepted", m)
		}
	}
}

func TestTruncateLabel(t *testing.T) {
	long := strings.Repeat("é", 100) // 200 bytes
	got := TruncateLabel(long)
	if len(got) > MaxLabelBytes || !utf8.ValidString(got) {
		t.Fatalf("len %d valid %v", len(got), utf8.ValidString(got))
	}
	if got := TruncateLabel("a\x00b\nc\xff"); got != "a\uFFFDb\uFFFDc\uFFFD" {
		t.Fatalf("sanitize: %q", got)
	}
	if got := TruncateLabel("plain-task"); got != "plain-task" {
		t.Fatalf("unchanged: %q", got)
	}
	if got := TruncateError(strings.Repeat("x", 1000)); len(got) != MaxErrorBytes {
		t.Fatalf("error len %d", len(got))
	}
}
