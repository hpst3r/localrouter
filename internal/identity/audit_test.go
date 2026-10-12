package identity

import (
	"context"
	"strings"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

func TestAuditEventsAreIDOnlyAndPaged(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	l := f.login("sub-AUDITSUBJECT", core.RoleUser)
	l.Email, l.DisplayName = "AUDITMAIL@example.test", "AUDITNAME"
	u, err := f.s.ResolveLogin(ctx, l)
	if err != nil {
		t.Fatal(err)
	}
	k, err := f.s.CreateKey(ctx, u.ID, "AUDITKEYNAME", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.RevokeKey(ctx, Actor{Kind: ActorUser, UserID: u.ID}, u.ID, k.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DisableUser(ctx, Actor{Kind: ActorCLI}, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeleteUser(ctx, Actor{Kind: ActorCLI}, u.ID); err != nil {
		t.Fatal(err)
	}
	unknown := f.login("sub-AUDITSUBJECT-2", core.RoleUser)
	unknown.Provision = false
	f.s.ResolveLogin(ctx, unknown)

	events, err := f.s.AuditEvents(ctx, 0, MaxListLimit)
	if err != nil {
		t.Fatalf("AuditEvents = %v", err)
	}
	var actions []string
	for i, e := range events {
		actions = append(actions, e.Action+":"+e.Outcome)
		if i > 0 && e.Seq <= events[i-1].Seq {
			t.Fatal("events not in seq order")
		}
		if !e.At.Equal(f.clock.Now()) {
			t.Errorf("event %d at %v", e.Seq, e.At)
		}
		for _, v := range []string{e.Action, string(e.ActorKind), e.ActorUserID, e.TargetUserID, e.TargetKeyID, e.Outcome, e.Reason} {
			for _, pii := range []string{"AUDIT", "example.test", k.Token} {
				if strings.Contains(v, pii) {
					t.Errorf("audit event %d contains %q", e.Seq, v)
				}
			}
		}
	}
	want := []string{
		"user.provisioned:ok", "user.login:ok", "key.created:ok", "key.revoked:ok",
		"user.disabled:ok", "user.deleted:ok", "user.login:denied",
	}
	if strings.Join(actions, ",") != strings.Join(want, ",") {
		t.Fatalf("audit actions = %v, want %v", actions, want)
	}
	if events[3].TargetKeyID != k.ID || events[3].ActorUserID != u.ID || events[6].Reason != "not_provisioned" {
		t.Fatalf("audit details = %+v / %+v", events[3], events[6])
	}

	page, err := f.s.AuditEvents(ctx, events[1].Seq, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].Seq != events[2].Seq || page[1].Seq != events[3].Seq {
		t.Fatalf("page = %+v", page)
	}
}
