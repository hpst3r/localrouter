package control

import (
	"math"
	"net/http"
	"regexp"
	"time"

	"github.com/hpst3r/localrouter/internal/budget"
	"github.com/hpst3r/localrouter/internal/identity"
)

// idParamRE bounds a user or key id taken from a path. Anything else is a 404
// without touching the store; the store remains the authority on existence
// and ownership.
var idParamRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// keyDoc is the frozen session-API key metadata object. It never carries a
// token or digest. RevokedAt is null while the key is not revoked.
type keyDoc struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at"`
}

func newKeyDoc(k identity.APIKey) keyDoc {
	d := keyDoc{ID: k.ID, Name: k.Name, CreatedAt: k.CreatedAt.UTC(), ExpiresAt: k.ExpiresAt.UTC()}
	if !k.RevokedAt.IsZero() {
		t := k.RevokedAt.UTC()
		d.RevokedAt = &t
	}
	return d
}

type keysDoc struct {
	Keys []keyDoc `json:"keys"`
}

// newKeyDocResp is the 201 body of POST /ui/v1/me/keys. Token is returned
// exactly once and is never logged.
type newKeyDocResp struct {
	Key   keyDoc `json:"key"`
	Token string `json:"token"`
}

type createKeyRequest struct {
	Name       string `json:"name"`
	TTLSeconds *int64 `json:"ttl_seconds"`
}

// listKeys renders userID's keys (metadata only).
func (s *Server) listKeys(w http.ResponseWriter, r *http.Request, userID string) {
	keys, err := s.deps.Identity.ListKeys(r.Context(), userID)
	if err != nil {
		identityFailed(w, err)
		return
	}
	doc := keysDoc{Keys: make([]keyDoc, 0, len(keys))}
	for _, k := range keys {
		doc.Keys = append(doc.Keys, newKeyDoc(k))
	}
	writeJSON(w, http.StatusOK, doc)
}

// meKeys serves GET /ui/v1/me/keys.
func (s *Server) meKeys(w http.ResponseWriter, r *http.Request) {
	a, _ := authFrom(r.Context())
	s.listKeys(w, r, a.principal.UserID)
}

// meCreateKey serves POST /ui/v1/me/keys {name, ttl_seconds?}. The store
// enforces the name grammar, the TTL ceiling, the per-user key cap and the
// login-freshness policy; the name is never echoed in an error.
func (s *Server) meCreateKey(w http.ResponseWriter, r *http.Request) {
	a, _ := authFrom(r.Context())
	var req createKeyRequest
	if !decodeSessionJSON(w, r, &req) {
		return
	}
	var ttl time.Duration
	if req.TTLSeconds != nil {
		n := *req.TTLSeconds
		if n <= 0 || n > int64(identity.MaxKeyTTL/time.Second) || n > math.MaxInt64/int64(time.Second) {
			writeErrorCode(w, http.StatusBadRequest, "ttl_seconds out of range", errTypeInvalid)
			return
		}
		ttl = time.Duration(n) * time.Second
	}
	k, err := s.deps.Identity.CreateKey(r.Context(), a.principal.UserID, req.Name, ttl)
	if err != nil {
		identityFailed(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, newKeyDocResp{Key: newKeyDoc(k.APIKey), Token: k.Token})
}

// meRevokeKey serves DELETE /ui/v1/me/keys/{id}. A key that does not exist
// or belongs to someone else is 404 (no existence oracle).
func (s *Server) meRevokeKey(w http.ResponseWriter, r *http.Request) {
	a, _ := authFrom(r.Context())
	id := r.PathValue("id")
	if !idParamRE.MatchString(id) {
		writeErrorCode(w, http.StatusNotFound, "not found", errTypeNotFound)
		return
	}
	actor := identity.Actor{Kind: identity.ActorUser, UserID: a.principal.UserID}
	if err := s.deps.Identity.RevokeKey(r.Context(), actor, a.principal.UserID, id); err != nil {
		identityFailed(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// meBudget serves GET /ui/v1/me/budget: the session user's own day and month
// user-scope instances. The key is always the session's user id; query
// parameters are ignored.
func (s *Server) meBudget(w http.ResponseWriter, r *http.Request) {
	a, _ := authFrom(r.Context())
	src := s.deps.Budgets
	if src == nil {
		writeJSON(w, http.StatusOK, budgetDoc{SchemaVersion: SchemaVersion, Enabled: false})
		return
	}
	user := a.principal.UserID
	limits, ok := effectiveLimits(src, user)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, budgetUnavailableMsg)
		return
	}
	s.writeBudgetDoc(w, r, src, budget.ScopeUser, user, []string{budget.PeriodDay, budget.PeriodMonth}, limits)
}
