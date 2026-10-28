package discovery

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// Amendment R2.3: Fetch with a nil Options.HTTPClient closes the idle
// connections of the client it built, so no keep-alive connection lingers.
func TestFetchNilClientClosesIdleConnections(t *testing.T) {
	closed := make(chan struct{}, 1)
	srv := httptest.NewUnstartedServer(listingBody("m1"))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	srv.Start()
	defer srv.Close()
	acct := core.Account{ID: "a", Provider: core.ProviderOpenAICompat, BaseURL: srv.URL + "/v1"}
	ids, dropped, class := Fetch(context.Background(), &fakeCreds{}, acct, Options{})
	if class != "" || dropped != 0 || !slices.Equal(ids, []string{"m1"}) {
		t.Fatalf("Fetch = %q, %d, %q", ids, dropped, class)
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("connection left open after Fetch with a nil HTTPClient")
	}
}
