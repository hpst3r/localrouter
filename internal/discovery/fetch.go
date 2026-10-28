package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hpst3r/localrouter/internal/core"
)

// Error classes recorded for a failed poll. They are the only failure detail
// the package keeps: never the URL, the body, or the credential.
const (
	ErrClassTransport  = "transport"
	ErrClassStatus     = "status"
	ErrClassTooLarge   = "too_large"
	ErrClassMalformed  = "malformed"
	ErrClassCredential = "credential"
)

// maxIDBytes bounds one discovered id.
const maxIDBytes = 256

// listing is the validated result of one successful poll.
type listing struct {
	ids     []string // sorted, unique
	dropped int      // invalid ids plus ids over the per-account cap
}

// Eligible reports whether accounts of provider serve an OpenAI-style
// GET {base_url}/models listing that discovery (and the backend model check)
// may poll: openai_compat and ollama. Codex, claude and openrouter are not.
func Eligible(provider string) bool {
	return provider == core.ProviderOpenAICompat || provider == core.ProviderOllama
}

// Fetch performs one GET {account.BaseURL}/models exactly as a discovery poll
// does (credential headers, bounds, redirects, id validation), using opts'
// HTTPClient, Timeout, MaxBodyBytes and MaxIDs (zero values select the
// defaults; a nil HTTPClient builds a fresh NewHTTPClient whose idle
// connections are closed before Fetch returns, so callers fetching repeatedly
// should pass their own). It returns the sorted, unique valid ids and the
// dropped count, or a non-empty error class. It never returns the URL, the
// body or the credential.
func Fetch(ctx context.Context, creds core.CredentialSource, account core.Account, opts Options) (ids []string, dropped int, errClass string) {
	if opts.HTTPClient == nil {
		opts.HTTPClient = NewHTTPClient()
		defer opts.HTTPClient.CloseIdleConnections()
	}
	l, class := fetchListing(ctx, creds, opts.withDefaults(), account)
	return l.ids, l.dropped, class
}

// fetch performs one GET {base_url}/models for account. It returns the
// validated listing, or a non-empty error class.
func (m *Manager) fetch(ctx context.Context, account core.Account) (listing, string) {
	return fetchListing(ctx, m.creds, m.opts, account)
}

// fetchListing is fetch with explicit dependencies; opts has its defaults
// applied.
func fetchListing(ctx context.Context, creds core.CredentialSource, opts Options, account core.Account) (listing, string) {
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	cred, err := creds.Credential(ctx, account.ID)
	if err != nil {
		return listing{}, ErrClassCredential
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(account.BaseURL, "/")+"/models", nil)
	if err != nil {
		return listing{}, ErrClassTransport
	}
	req.Header.Set("Accept", "application/json")
	// The credential headers are attached exactly as the proxy attaches them
	// to an inference request (internal/proxy send).
	for k, v := range cred.Headers {
		req.Header[k] = append([]string(nil), v...)
	}
	// An error here includes response headers over the transport's
	// MaxResponseHeaderBytes (64 KiB by default).
	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return listing{}, ErrClassTransport
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return listing{}, ErrClassStatus
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, opts.MaxBodyBytes+1))
	if err != nil {
		return listing{}, ErrClassTransport
	}
	if int64(len(body)) > opts.MaxBodyBytes {
		return listing{}, ErrClassTooLarge
	}
	l, err := parseListing(body, opts.MaxIDs)
	if err != nil {
		return listing{}, ErrClassMalformed
	}
	return l, ""
}

var errMalformed = errors.New("malformed model listing")

// parseListing reads an OpenAI-style {"data":[{"id":"..."},...]} object.
// Extra fields are ignored. An entry that is not an object or whose id is not
// a valid string is dropped (and counted); duplicates collapse; at most maxIDs
// unique ids are kept, the rest are dropped and counted.
func parseListing(body []byte, maxIDs int) (listing, error) {
	var doc struct {
		Data *[]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.Data == nil {
		return listing{}, errMalformed
	}
	seen := map[string]bool{}
	var l listing
	for _, raw := range *doc.Data {
		var item struct {
			ID json.RawMessage `json:"id"`
		}
		if json.Unmarshal(raw, &item) != nil {
			l.dropped++
			continue
		}
		id, ok := validID(item.ID)
		switch {
		case !ok:
			l.dropped++
		case seen[id]:
		case len(l.ids) >= maxIDs:
			// Over the cap: counted once per distinct id.
			seen[id] = true
			l.dropped++
		default:
			seen[id] = true
			l.ids = append(l.ids, id)
		}
	}
	slices.Sort(l.ids)
	return l, nil
}

// validID decodes a JSON string id and accepts it only if it is 1-256 bytes
// of valid UTF-8 whose every rune is graphic (unicode.IsGraphic) and not a
// space, which rules out control and format (Cf: zero-width, bidi, BOM)
// characters. The raw bytes are checked too, because encoding/json silently
// replaces invalid UTF-8 (and unpaired surrogate escapes) with U+FFFD, which
// is therefore also rejected.
func validID(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' || !utf8.Valid(raw) {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil || s == "" || len(s) > maxIDBytes {
		return "", false
	}
	for _, r := range s {
		if r == utf8.RuneError || !unicode.IsGraphic(r) || unicode.IsSpace(r) {
			return "", false
		}
	}
	return s, true
}
