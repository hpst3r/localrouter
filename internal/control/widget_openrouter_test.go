package control

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// prepaidSnippet extracts the dashboard's pure prepaid-credit formatter so it
// can be exercised under node without a DOM.
func prepaidSnippet(t *testing.T) string {
	t.Helper()
	html := string(indexHTML)
	const begin, end = "// BEGIN prepaidLines", "// END prepaidLines"
	i, j := strings.Index(html, begin), strings.Index(html, end)
	if i < 0 || j < i {
		t.Fatal("dashboard has no prepaidLines block")
	}
	return html[i:j]
}

func TestWidgetPrepaidLines(t *testing.T) {
	src := prepaidSnippet(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; static check only")
	}
	accounts := `[
	  {"provider":"openrouter","credits":{"available":true,"balance_usd":-0.076117902,"total_credits_usd":770.8176,"total_usage_usd":770.893717902,"exhausted":true,"age_s":30,"stale":false,"error":null},
	   "key":{"available":true,"unlimited":true,"limit_usd":null,"limit_remaining_usd":null,"exhausted":false,"usage_usd":770.89,"usage_daily_usd":1.25,"usage_weekly_usd":7.5,"usage_monthly_usd":30.25,"byok_usage_usd":2,"error":null}},
	  {"provider":"openrouter","credits":{"available":false,"balance_usd":null,"exhausted":false,"error":"credits: management key required (http 403); balance unavailable"},
	   "key":{"available":true,"unlimited":false,"limit_usd":0,"limit_remaining_usd":0,"limit_reset":"daily","exhausted":true,"usage_usd":3,"error":null}},
	  {"provider":"openrouter","credits":{"available":true,"balance_usd":12.5,"total_credits_usd":20,"total_usage_usd":7.5,"exhausted":false},
	   "key":{"available":false,"error":"key: usage api: http 500"}},
	  {"provider":"codex"}
	]`
	script := src + "\nconst out = " + accounts + ".map((a) => prepaidLines(a));\nprocess.stdout.write(JSON.stringify(out));\n"
	cmd := exec.Command(node, "-e", script)
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, b)
	}
	var out [][]struct{ Cls, Text string }
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	join := func(ls []struct{ Cls, Text string }) string {
		var s []string
		for _, l := range ls {
			s = append(s, l.Cls+"|"+l.Text)
		}
		return strings.Join(s, "\n")
	}
	neg := join(out[0])
	for _, want := range []string{"-$0.08", "exhausted", "line-err", "unlimited", "$1.25 today"} {
		if !strings.Contains(neg, want) {
			t.Errorf("negative balance lines missing %q:\n%s", want, neg)
		}
	}
	if strings.Contains(neg, "$0.08 ") && !strings.Contains(neg, "-$0.08") {
		t.Error("sign dropped")
	}
	unavail := join(out[1])
	for _, want := range []string{"balance unavailable", "cap $0.00", "$0.00 left", "exhausted"} {
		if !strings.Contains(unavail, want) {
			t.Errorf("unavailable lines missing %q:\n%s", want, unavail)
		}
	}
	if strings.Contains(unavail, "balance $0") || strings.Contains(unavail, "unlimited") {
		t.Errorf("unavailable must not render as zero balance or unlimited:\n%s", unavail)
	}
	if pos := join(out[2]); !strings.Contains(pos, "$12.50") || strings.Contains(pos, "exhausted") || !strings.Contains(pos, "key usage unavailable") {
		t.Errorf("positive lines:\n%s", pos)
	}
	if len(out[3]) != 0 {
		t.Errorf("non-openrouter account got lines: %v", out[3])
	}
}
