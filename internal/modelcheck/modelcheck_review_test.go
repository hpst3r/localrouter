package modelcheck

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

// TestReviewMCR1TruncatedListingFalseMissing: a backend that lists more than
// discovery.DefaultMaxIDs ids, with the route's expected id beyond the cap, is
// reported "missing" (exit 1 under check -probe, a warn log at runtime) even
// though the backend does serve it. discovery.Fetch reports the dropped count,
// but fetchAll discards it, so a truncated listing is treated as complete.
func TestReviewMCR1TruncatedListingFalseMissing(t *testing.T) {
	if os.Getenv("LR_REVIEW") == "" {
		t.Skip("BUG MC-1: expected id dropped by the 1000-id cap is reported missing, not unchecked")
	}
	var b strings.Builder
	b.WriteString(`{"object":"list","data":[`)
	for i := range 1000 {
		fmt.Fprintf(&b, `{"id":"filler-%04d"},`, i)
	}
	b.WriteString(`{"id":"wanted-model"}]}`)
	body := b.String()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	accounts := map[string]core.Account{"big": {ID: "big", Provider: core.ProviderOpenAICompat, BaseURL: srv.URL}}
	routes := []core.Route{{Name: "r", Models: []string{"wanted-model"}, Interactive: []string{"big"}}}
	res := Probe(context.Background(), fakeCreds{}, accounts, routes, Options{})
	if len(res.Verdicts) != 1 {
		t.Fatalf("verdicts = %+v", res.Verdicts)
	}
	if v := res.Verdicts[0]; v.Status == StatusMissing {
		t.Fatalf("backend lists %q (beyond the id cap) but the verdict is %q; a truncated listing must not yield missing", v.Expected, v.Status)
	}
}
