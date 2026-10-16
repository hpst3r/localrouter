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

// fetch performs one GET {base_url}/models for account. It returns the
// validated listing, or a non-empty error class.
func (m *Manager) fetch(ctx context.Context, account core.Account) (listing, string) {
	ctx, cancel := context.WithTimeout(ctx, m.opts.Timeout)
	defer cancel()
	cred, err := m.creds.Credential(ctx, account.ID)
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
	resp, err := m.opts.HTTPClient.Do(req)
	if err != nil {
		return listing{}, ErrClassTransport
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return listing{}, ErrClassStatus
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, m.opts.MaxBodyBytes+1))
	if err != nil {
		return listing{}, ErrClassTransport
	}
	if int64(len(body)) > m.opts.MaxBodyBytes {
		return listing{}, ErrClassTooLarge
	}
	l, err := parseListing(body, m.opts.MaxIDs)
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
// of valid UTF-8 with no whitespace or control characters. The raw bytes are
// checked too, because encoding/json silently replaces invalid UTF-8 (and
// unpaired surrogate escapes) with U+FFFD, which is therefore also rejected.
func validID(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' || !utf8.Valid(raw) {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil || s == "" || len(s) > maxIDBytes {
		return "", false
	}
	for _, r := range s {
		if r == utf8.RuneError || unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", false
		}
	}
	return s, true
}
