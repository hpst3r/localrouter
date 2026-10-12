package core

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDataScopeValidateExactlyOne(t *testing.T) {
	cases := []struct {
		name  string
		scope DataScope
		ok    bool
	}{
		{"neither", DataScope{}, false},
		{"both", DataScope{AllUsers: true, UserID: "u1"}, false},
		{"all users", DataScope{AllUsers: true}, true},
		{"one user", DataScope{UserID: "u1"}, true},
	}
	for _, c := range cases {
		err := c.scope.Validate()
		if c.ok && err != nil {
			t.Errorf("%s: Validate() = %v, want nil", c.name, err)
		}
		if !c.ok && !errors.Is(err, ErrInvalidScope) {
			t.Errorf("%s: Validate() = %v, want ErrInvalidScope", c.name, err)
		}
	}
}

func TestRequestRecordOwnerFieldsNotOnWire(t *testing.T) {
	b, err := json.Marshal(RequestRecord{UserID: "u_secretowner", KeyID: "k_secretkey"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "u_secretowner") || strings.Contains(string(b), "k_secretkey") {
		t.Fatalf("owner fields leaked into wire format: %s", b)
	}
	var r RequestRecord
	body := `{"UserID":"forged","user_id":"forged","KeyID":"forged","key_id":"forged"}`
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatal(err)
	}
	if r.UserID != "" || r.KeyID != "" {
		t.Fatalf("owner fields accepted from wire: UserID=%q KeyID=%q", r.UserID, r.KeyID)
	}
}

func TestAnalyticsQueryScopeNotOnWire(t *testing.T) {
	var q AnalyticsQuery
	if err := json.Unmarshal([]byte(`{"Scope":{"AllUsers":true}}`), &q); err != nil {
		t.Fatal(err)
	}
	if q.Scope != nil {
		t.Fatalf("Scope accepted from JSON: %+v", *q.Scope)
	}
}
