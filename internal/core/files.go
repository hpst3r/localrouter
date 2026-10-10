package core

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"unicode/utf8"
)

// CheckPrivateFile verifies that path names a regular file that is not
// group- or world-accessible (mode & 0o077 == 0), the rule applied to every
// secret LocalRouter reads: client keys, upstream API and management keys,
// the agent key and the TLS private key. Symlinks are followed (the target
// must satisfy the rule). On Windows the POSIX mode bits are synthesized by
// Go and carry no access information, so only existence and regularity are
// checked there. label names the file's role in errors (e.g. "api key
// file"); the path itself is included because the caller is the operator.
func CheckPrivateFile(label, path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s %s is not a regular file", label, path)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s %s has mode %#o; must not be group/world accessible (chmod 600)",
			label, path, fi.Mode().Perm())
	}
	return nil
}

// CheckNotWritableByOthers verifies that path is not group- or
// world-writable. It is used for configuration files, which select which
// files are read as secrets and where credentials are sent. No-op on Windows.
func CheckNotWritableByOthers(label, path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s %s has mode %#o; must not be group/world writable (chmod go-w)",
			label, path, fi.Mode().Perm())
	}
	return nil
}

// TruncateLabel bounds a free-form label to MaxLabelBytes without splitting a
// UTF-8 sequence, and replaces invalid UTF-8 and control characters with
// U+FFFD so stored labels are always valid, printable text.
func TruncateLabel(s string) string { return truncateUTF8(sanitizeText(s), MaxLabelBytes) }

// TruncateError bounds a stored error string to MaxErrorBytes the same way.
func TruncateError(s string) string { return truncateUTF8(sanitizeText(s), MaxErrorBytes) }

func sanitizeText(s string) string {
	if utf8.ValidString(s) && strings.IndexFunc(s, isControl) < 0 {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if r == utf8.RuneError || isControl(r) {
			r = utf8.RuneError
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) }

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
