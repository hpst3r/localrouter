package control

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/hpst3r/localrouter/internal/identity"
)

const defaultAdminPage = 50

// usersDoc is the GET /ui/v1/admin/users response. NextCursor is the id to
// pass as ?after= for the next page; absent on the last page.
type usersDoc struct {
	Users      []userDoc `json:"users"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

// auditEventDoc is one audit row: opaque ids and fixed codes only.
type auditEventDoc struct {
	Seq          int64     `json:"seq"`
	At           time.Time `json:"at"`
	Action       string    `json:"action"`
	ActorKind    string    `json:"actor_kind"`
	ActorUserID  string    `json:"actor_user_id"`
	TargetUserID string    `json:"target_user_id"`
	TargetKeyID  string    `json:"target_key_id"`
	Outcome      string    `json:"outcome"`
	Reason       string    `json:"reason"`
}

// auditDoc is the GET /ui/v1/admin/audit response. NextCursor is the seq to
// pass as ?after= for the next page; absent on the last page.
type auditDoc struct {
	Events     []auditEventDoc `json:"events"`
	NextCursor *int64          `json:"next_cursor,omitempty"`
}

// pageLimit parses ?limit= (default defaultAdminPage, 1..MaxListLimit).
func pageLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return defaultAdminPage, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > identity.MaxListLimit {
		writeErrorCode(w, http.StatusBadRequest, "limit must be an integer between 1 and "+strconv.Itoa(identity.MaxListLimit), errTypeInvalid)
		return 0, false
	}
	return n, true
}

// adminUsers serves GET /ui/v1/admin/users?after=&limit=.
func (s *Server) adminUsers(w http.ResponseWriter, r *http.Request) {
	limit, ok := pageLimit(w, r)
	if !ok {
		return
	}
	after := r.URL.Query().Get("after")
	if after != "" && !idParamRE.MatchString(after) {
		writeErrorCode(w, http.StatusBadRequest, "after must be a user id", errTypeInvalid)
		return
	}
	users, err := s.deps.Identity.ListUsers(r.Context(), after, limit)
	if err != nil {
		identityFailed(w, err)
		return
	}
	doc := usersDoc{Users: make([]userDoc, 0, len(users))}
	for _, u := range users {
		doc.Users = append(doc.Users, newUserDoc(u))
	}
	if len(users) == limit {
		doc.NextCursor = users[len(users)-1].ID
	}
	writeJSON(w, http.StatusOK, doc)
}

// adminUserAction serves POST /ui/v1/admin/users/{id}/{disable,enable,delete}.
// An admin can never disable or delete their own account here (409); the
// local CLI is the break-glass path. Unknown users are 404.
func (s *Server) adminUserAction(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, _ := authFrom(r.Context())
		id := r.PathValue("id")
		if !idParamRE.MatchString(id) {
			writeErrorCode(w, http.StatusNotFound, "not found", errTypeNotFound)
			return
		}
		if !requireNoBody(w, r) {
			return
		}
		if id == a.principal.UserID && op != "enable" {
			writeErrorCode(w, http.StatusConflict, "an admin cannot "+op+" their own account", errTypeSelfAction)
			return
		}
		actor := identity.Actor{Kind: identity.ActorAdmin, UserID: a.principal.UserID}
		var err error
		switch op {
		case "disable":
			err = s.deps.Identity.DisableUser(r.Context(), actor, id)
		case "enable":
			err = s.deps.Identity.EnableUser(r.Context(), actor, id)
		case "delete":
			err = s.deps.Identity.DeleteUser(r.Context(), actor, id)
		}
		if err != nil {
			adminFailed(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	}
}

// adminFailed maps admin-path store errors. ErrInvalid on an admin action
// means the acting session is no longer an active admin (a concurrent role
// change or disable): forbidden, not a client error.
func adminFailed(w http.ResponseWriter, err error) {
	if errors.Is(err, identity.ErrInvalid) {
		writeErrorCode(w, http.StatusForbidden, msgForbidden, errTypeForbidden)
		return
	}
	identityFailed(w, err)
}

// adminUserKeys serves GET /ui/v1/admin/users/{id}/keys (metadata only).
func (s *Server) adminUserKeys(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idParamRE.MatchString(id) {
		writeErrorCode(w, http.StatusNotFound, "not found", errTypeNotFound)
		return
	}
	if _, err := s.deps.Identity.User(r.Context(), id); err != nil {
		identityFailed(w, err)
		return
	}
	s.listKeys(w, r, id)
}

// adminRevokeKey serves DELETE /ui/v1/admin/users/{uid}/keys/{kid}. A key
// not owned by uid is 404.
func (s *Server) adminRevokeKey(w http.ResponseWriter, r *http.Request) {
	a, _ := authFrom(r.Context())
	uid, kid := r.PathValue("uid"), r.PathValue("kid")
	if !idParamRE.MatchString(uid) || !idParamRE.MatchString(kid) {
		writeErrorCode(w, http.StatusNotFound, "not found", errTypeNotFound)
		return
	}
	actor := identity.Actor{Kind: identity.ActorAdmin, UserID: a.principal.UserID}
	if err := s.deps.Identity.RevokeKey(r.Context(), actor, uid, kid); err != nil {
		adminFailed(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// adminAudit serves GET /ui/v1/admin/audit?after=<seq>&limit=.
func (s *Server) adminAudit(w http.ResponseWriter, r *http.Request) {
	limit, ok := pageLimit(w, r)
	if !ok {
		return
	}
	var after int64
	if v := r.URL.Query().Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeErrorCode(w, http.StatusBadRequest, "after must be a non-negative sequence number", errTypeInvalid)
			return
		}
		after = n
	}
	events, err := s.deps.Identity.AuditEvents(r.Context(), after, limit)
	if err != nil {
		identityFailed(w, err)
		return
	}
	doc := auditDoc{Events: make([]auditEventDoc, 0, len(events))}
	for _, e := range events {
		doc.Events = append(doc.Events, auditEventDoc{Seq: e.Seq, At: e.At.UTC(), Action: e.Action,
			ActorKind: string(e.ActorKind), ActorUserID: e.ActorUserID, TargetUserID: e.TargetUserID,
			TargetKeyID: e.TargetKeyID, Outcome: e.Outcome, Reason: e.Reason})
	}
	if len(events) == limit {
		next := events[len(events)-1].Seq
		doc.NextCursor = &next
	}
	writeJSON(w, http.StatusOK, doc)
}
