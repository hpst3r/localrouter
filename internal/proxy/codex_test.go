package proxy

import (
	"encoding/json"
	"testing"
)

func TestCodexBodyStripsRejectedFieldsAndForcesStoreFalse(t *testing.T) {
	in := []byte(`{"model":"m","input":[{"role":"user","content":"<b>&"}],"max_output_tokens":10,"metadata":{"a":1},"store":true,"stream":true}`)
	out := codexBody(in)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"max_output_tokens", "metadata"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s not stripped", k)
		}
	}
	if string(m["store"]) != "false" {
		t.Errorf("store = %s", m["store"])
	}
	if string(m["stream"]) != "true" || string(m["model"]) != `"m"` {
		t.Errorf("other fields altered: %s", out)
	}
	if string(m["input"]) != `[{"role":"user","content":"<b>&"}]` {
		t.Errorf("input altered or HTML-escaped: %s", m["input"])
	}
}

func TestCodexBodyUnchangedWhenAlreadyClean(t *testing.T) {
	in := []byte(`{"model":"m","store":false}`)
	if got := codexBody(in); string(got) != string(in) {
		t.Fatalf("clean body rewritten: %s", got)
	}
}

func TestCodexBodyNonObjectPassthrough(t *testing.T) {
	in := []byte(`[1,2]`)
	if got := codexBody(in); string(got) != string(in) {
		t.Fatalf("non-object altered: %s", got)
	}
}
