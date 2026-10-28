package control

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestWidgetSelfContained guards the embedded dashboard against external
// loads and HTML injection sinks; the CSP would block the former, the latter
// would let ledger values (hosts, models, tasks) inject markup.
func TestWidgetSelfContained(t *testing.T) {
	html := string(indexHTML)
	if len(html) == 0 {
		t.Fatal("embedded widget is empty")
	}
	lower := strings.ToLower(html)

	// The SVG namespace URI is an identifier, not a load.
	scrubbed := strings.ReplaceAll(lower, `"http:" + "//www.w3.org/2000/svg"`, "")
	if m := regexp.MustCompile(`https?:|//[a-z0-9.-]+\.[a-z]{2,}/`).FindString(scrubbed); m != "" {
		t.Errorf("widget references an external URL: %q", m)
	}
	for _, bad := range []string{
		"<script src", "src=", "<link", "<iframe", "<img", "@import", "srcdoc",
		"innerhtml", "outerhtml", "insertadjacenthtml", "document.write", "eval(", "new function",
	} {
		if strings.Contains(lower, bad) {
			t.Errorf("widget contains forbidden construct %q", bad)
		}
	}
	// No CSS url() loads (JS `new URL(` is fine).
	style := lower[strings.Index(lower, "<style>"):strings.Index(lower, "</style>")]
	if strings.Contains(style, "url(") {
		t.Error("widget CSS contains url()")
	}
	// Every <script> must be inline.
	if n := strings.Count(lower, "<script"); n != 1 {
		t.Errorf("want exactly one inline <script>, got %d", n)
	}
	// Key behaviour carried over from the previous widget.
	for _, want := range []string{"localrouter.controlKey", "/control/v1/status", "/control/v1/analytics",
		"/control/v1/analytics/performance", "demo"} {
		if !strings.Contains(html, want) {
			t.Errorf("widget missing %q", want)
		}
	}
}

// TestWidgetBreakdownOmitted: the table notes keys the server left out of
// breakdown, and demo mode caps breakdown like the server.
func TestWidgetBreakdownOmitted(t *testing.T) {
	html := string(indexHTML)
	for _, want := range []string{`id="breakdownmore"`, "doc.breakdown_omitted", "more.textContent",
		"breakdown_omitted: Math.max(0, ranked.length - MAX_BREAKDOWN)"} {
		if !strings.Contains(html, want) {
			t.Errorf("widget missing %q", want)
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; static check only")
	}
	i, j := strings.Index(html, "<script>"), strings.Index(html, "</script>")
	path := filepath.Join(t.TempDir(), "widget.js")
	if err := os.WriteFile(path, []byte(html[i+len("<script>"):j]), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--check", path).CombinedOutput(); err != nil {
		t.Fatalf("widget script does not parse: %v\n%s", err, out)
	}
}

// TestWidgetFmtMs: durations round before the unit is chosen (999.6 ms is
// "1 s", never "1000 ms").
func TestWidgetFmtMs(t *testing.T) {
	html := string(indexHTML)
	fn := regexp.MustCompile(`(?s)\n  function fmtMs\(v\) \{.*?\n  \}\n`).FindString(html)
	if fn == "" {
		t.Fatal("fmtMs not found in widget")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := fn + `
const cases = [[999.4, "999 ms"], [999.5, "1 s"], [999.6, "1 s"], [0, "0 ms"], [1234, "1.23 s"], [9996, "10 s"], [130000, "2.2 min"]];
const bad = cases.filter(([v, want]) => fmtMs(v) !== want).map(([v, want]) => v + ": got " + fmtMs(v) + ", want " + want);
if (bad.length) { console.log(bad.join("\n")); process.exit(1); }
`
	path := filepath.Join(t.TempDir(), "fmtms.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path).CombinedOutput(); err != nil {
		t.Fatalf("fmtMs: %v\n%s", err, out)
	}
}
