package userui

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestAppBehaviourUnderNode runs testdata/app.test.js (node:test, no
// third-party packages) against the embedded index.html and app.js. It is
// skipped when Node.js 18 or newer is not installed.
func TestAppBehaviourUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	ver, err := exec.Command(node, "--version").Output()
	if err != nil {
		t.Skip("node --version failed")
	}
	major, _ := strconv.Atoi(strings.SplitN(strings.TrimPrefix(strings.TrimSpace(string(ver)), "v"), ".", 2)[0])
	if major < 18 {
		t.Skipf("node %s is older than 18", strings.TrimSpace(string(ver)))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--test", "app.test.js")
	cmd.Dir = "testdata"
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node --test: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "# fail 0") {
		t.Fatalf("node --test reported failures:\n%s", out)
	}
}
