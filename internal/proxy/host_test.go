package proxy

import (
	"io"
	"net/http"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

// MH8 (proxy half): the ledger row carries the authenticated client's Host.
func TestRecordHostFromClient(t *testing.T) {
	h := newHarness(t)
	h.upstream("a", core.ProviderOpenAICompat, okJSON)
	singleRoute(h, "a")
	h.start()

	for _, key := range []string{bgKey, clientKey} {
		resp := h.post("/v1/responses", key, respBody, nil)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("key %q: status %d", key, resp.StatusCode)
		}
	}
	hosts := map[string]string{}
	for _, r := range h.waitRows(2) {
		hosts[r.Client] = r.Host
	}
	if hosts["batch"] != "vm1" || hosts["alice"] != "" {
		t.Fatalf("hosts by client = %v", hosts)
	}
}
