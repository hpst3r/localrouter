package control

// Capability-admission tests for POST /control/v1/admit.
//
// The admit endpoint is the dry-run twin of the inference path. These tests pin
// the parity the endpoint owes it: a route that opts into capability routing
// (core.Route.Upstreams non-empty) refuses a dry run that does not state its
// capability profile explicitly, narrows the operator's candidate list through
// routing.Filter (same order, removals only) before the policy is consulted,
// and reports the same capability_unsupported condition the proxy reports. A
// legacy route keeps its old behaviour exactly, requirements or not.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

// capAddRoute appends a capability-opted-in route to a fixture's route table.
// The fixture itself is built by newFixture (control_test.go); mutating deps is
// safe because the handler resolves routes per request.
func capAddRoute(f *fixture, rt core.Route) {
	f.srv.deps.Routes = append(f.srv.deps.Routes, rt)
}

// capError returns the decoded error envelope of a response, failing the test
// when the body is not an error object.
func capError(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	m := decode(t, rec)
	e, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("body is not an error object: %s", rec.Body)
	}
	return e
}

// capErrorType asserts an error response carries the given stable error type.
func capErrorType(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if got := capError(t, rec)["type"]; got != want {
		t.Fatalf("error.type = %v want %q: %s", got, want, rec.Body)
	}
}

// capDryRuns joins each recorded dry run's candidate list, one run per line, so
// a test can assert exactly what the policy was asked about (and in what order).
func capDryRuns(f *fixture) string {
	f.policy.mu.Lock()
	defer f.policy.mu.Unlock()
	var b strings.Builder
	for _, d := range f.policy.dryRuns {
		b.WriteString(string(d.class))
		b.WriteString(": ")
		b.WriteString(strings.Join(d.candidates, ","))
		b.WriteString("\n")
	}
	return b.String()
}

// TestAdmitLegacyRouteIgnoresRequirements pins the legacy contract: a route
// without descriptors has no capability boundary, so a profile cannot narrow
// anything and must leave the candidate list — and the response — untouched.
// The capability-neutral error envelope also keeps its original, type-less body.
func TestAdmitLegacyRouteIgnoresRequirements(t *testing.T) {
	f := newFixture(false)

	// No requirements at all.
	rec := f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"gpt-5"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	// A complete profile on the same legacy route: ignored, identical outcome.
	// (A chat profile would also drop the route's Codex account, as the proxy
	// does for /v1/chat/completions; see TestAdmitChatProfileDropsCodexAccounts.)
	profiled := `{"class":"interactive","model":"gpt-5","requirements":{"protocol":"responses","modalities":["text","image"],"tools":true,"json_schema":true,"stream":true}}`
	rec2 := f.do(t, "POST", "/control/v1/admit", profiled, "")
	if rec2.Code != http.StatusOK {
		t.Fatalf("profiled legacy admit: code %d: %s", rec2.Code, rec2.Body)
	}
	if rec.Body.String() != rec2.Body.String() {
		t.Errorf("legacy admit changed with requirements:\n got %s\nwant %s", rec2.Body, rec.Body)
	}
	if got := capDryRuns(f); got != "interactive: primary,secondary\ninteractive: primary,secondary\n" {
		t.Fatalf("legacy candidates not preserved:\n%s", got)
	}

	// The legacy error envelope is unchanged: message only, no "type".
	bad := capError(t, f.do(t, "POST", "/control/v1/admit", `{"class":"bulk","model":"gpt-5"}`, ""))
	if len(bad) != 1 || bad["message"] == "" {
		t.Errorf("legacy error envelope = %v, want message only", bad)
	}
}

// TestAdmitConstrainedRouteRequiresRequirements covers the fail-closed rule: an
// opted-in route with no profile is a 400 capability_requirements_required, and
// the policy is never consulted (there is no honest candidate set to hand it).
func TestAdmitConstrainedRouteRequiresRequirements(t *testing.T) {
	f := newFixture(false)
	capAddRoute(f, core.Route{
		Name: "cap", Models: []string{"cap-5"},
		Interactive: []string{"primary", "secondary"}, Background: []string{"secondary"},
		Upstreams: map[string]core.UpstreamSpec{
			"primary":   {Protocols: []string{"chat"}, InputModalities: []string{"text"}},
			"secondary": {Protocols: []string{"chat"}, InputModalities: []string{"text"}},
		},
	})

	rec := f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"cap-5"}`, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code %d want 400: %s", rec.Code, rec.Body)
	}
	capErrorType(t, rec, "capability_requirements_required")
	if e := capError(t, rec); e["message"] == "" {
		t.Errorf("no message: %s", rec.Body)
	}
	if got := capDryRuns(f); got != "" {
		t.Fatalf("policy consulted for a profile-less constrained admit:\n%s", got)
	}

	// An under-specified profile is refused the same way, never widened by a
	// silent default protocol or modality.
	for _, body := range []string{
		`{"class":"interactive","model":"cap-5","requirements":{}}`,
		`{"class":"interactive","model":"cap-5","requirements":{"protocol":"chat"}}`,
		`{"class":"interactive","model":"cap-5","requirements":{"modalities":["text"]}}`,
		`{"class":"interactive","model":"cap-5","requirements":{"protocol":"chat","modalities":[]}}`,
		`{"class":"interactive","model":"cap-5","requirements":{"protocol":"  ","modalities":["text"]}}`,
		`{"class":"interactive","model":"cap-5","requirements":{"protocol":"chat","modalities":[""," "]}}`,
	} {
		rec := f.do(t, "POST", "/control/v1/admit", body, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code %d want 400: %s", body, rec.Code, rec.Body)
			continue
		}
		capErrorType(t, rec, "capability_requirements_required")
	}
	if got := capDryRuns(f); got != "" {
		t.Fatalf("incomplete profile reached policy:\n%s", got)
	}
}

// TestAdmitConstrainedRejectsUnmodelledVocabulary covers the vocabulary bound:
// only chat/responses and text/image/audio/file are accepted. An unknown protocol
// or modality can never be satisfied, so it is capability_unsupported; audio (and
// file) is a known modality that no descriptor may declare, so it fails closed
// rather than being downgraded to text.
func TestAdmitConstrainedRejectsUnmodelledVocabulary(t *testing.T) {
	f := newFixture(false)
	capAddRoute(f, core.Route{
		Name: "cap", Models: []string{"cap-5"},
		Interactive: []string{"primary"},
		Upstreams: map[string]core.UpstreamSpec{
			"primary": {Protocols: []string{"chat", "responses"}, InputModalities: []string{"text", "image"}},
		},
	})

	for _, body := range []string{
		`{"class":"interactive","model":"cap-5","requirements":{"protocol":"completion","modalities":["text"]}}`,
		`{"class":"interactive","model":"cap-5","requirements":{"protocol":"chat","modalities":["video"]}}`,
		`{"class":"interactive","model":"cap-5","requirements":{"protocol":"chat","modalities":["text","video"]}}`,
		`{"class":"interactive","model":"cap-5","requirements":{"protocol":"chat","modalities":["audio"]}}`,
	} {
		rec := f.do(t, "POST", "/control/v1/admit", body, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code %d want 400: %s", body, rec.Code, rec.Body)
			continue
		}
		capErrorType(t, rec, "capability_unsupported")
	}
	if got := capDryRuns(f); got != "" {
		t.Fatalf("unmodelled vocabulary reached policy:\n%s", got)
	}
}

// TestAdmitConstrainedFiltersInOrderThenDryRun is the parity core: Filter runs
// first, keeps the operator's order and only removes, and its result is exactly
// what the policy dry run sees. The route's own slices must not be mutated.
func TestAdmitConstrainedFiltersInOrderThenDryRun(t *testing.T) {
	f := newFixture(false)
	f.policy.decision = core.Decision{Allow: true, AccountID: "third", Reason: "admitted"}
	capAddRoute(f, core.Route{
		Name: "cap", Models: []string{"cap-5"},
		Interactive: []string{"first", "second", "third"}, Background: []string{"first", "second", "third"},
		Upstreams: map[string]core.UpstreamSpec{
			"first":  {Protocols: []string{"chat"}, InputModalities: []string{"text"}},
			"second": {Protocols: []string{"responses"}, InputModalities: []string{"text", "image"}},
			"third":  {Protocols: []string{"chat", "responses"}, InputModalities: []string{"text", "image"}},
		},
	})

	body := `{"class":"background","model":"cap-5","requirements":{"protocol":"chat","modalities":["text"]}}`
	m := decode(t, f.do(t, "POST", "/control/v1/admit", body, ""))
	if m["decision"] != "allow" || m["account_id"] != "third" {
		t.Errorf("admit = %v", m)
	}
	// first (chat/text) and third (chat, image+text folded) survive, in order.
	if got := capDryRuns(f); got != "background: first,third\n" {
		t.Fatalf("dry runs:\n%swant filtered, ordered candidates", got)
	}
	// The route's own candidate lists are untouched.
	rt := f.srv.deps.Routes[1]
	if strings.Join(rt.Interactive, ",") != "first,second,third" || strings.Join(rt.Background, ",") != "first,second,third" {
		t.Fatalf("route candidates mutated: %+v", rt)
	}
}

// TestAdmitConstrainedUnsupportedWhenNothingMatches covers the second capability
// error: when at least one candidate existed but the profile rules them all
// out, the request is capability_unsupported (not a policy denial) and the
// policy is never asked.
func TestAdmitConstrainedUnsupportedWhenNothingMatches(t *testing.T) {
	f := newFixture(false)
	capAddRoute(f, core.Route{
		Name: "cap", Models: []string{"cap-5"},
		Interactive: []string{"primary", "secondary"},
		Upstreams: map[string]core.UpstreamSpec{
			"primary":   {Protocols: []string{"responses"}, InputModalities: []string{"text"}},
			"secondary": {Protocols: []string{"responses"}, InputModalities: []string{"text", "image"}},
		},
	})

	rec := f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"cap-5","requirements":{"protocol":"chat","modalities":["text"]}}`, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code %d want 400: %s", rec.Code, rec.Body)
	}
	capErrorType(t, rec, "capability_unsupported")
	if got := capDryRuns(f); got != "" {
		t.Fatalf("policy consulted with no surviving candidate:\n%s", got)
	}
}

// TestAdmitConstrainedEmptyCandidateListMirrorsProxy pins the boundary the
// proxy draws: it refuses only when it had a candidate to lose, so a class with
// no candidates at all still reaches the policy (which reports the denial).
func TestAdmitConstrainedEmptyCandidateListMirrorsProxy(t *testing.T) {
	f := newFixture(false)
	capAddRoute(f, core.Route{
		Name: "cap", Models: []string{"cap-5"},
		Interactive: []string{"primary"}, Background: nil,
		Upstreams: map[string]core.UpstreamSpec{
			"primary": {Protocols: []string{"chat"}, InputModalities: []string{"text"}},
		},
	})

	rec := f.do(t, "POST", "/control/v1/admit", `{"class":"background","model":"cap-5","requirements":{"protocol":"chat","modalities":["text"]}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d want 200: %s", rec.Code, rec.Body)
	}
	if got := capDryRuns(f); got != "background: \n" {
		t.Fatalf("dry runs:\n%swant one empty candidate list", got)
	}
}

// TestAdmitConstrainedOptionalFeatureFlags proves the tri-state features reach
// routing.Requirements under their lowercase-underscore wire names — most
// importantly json_schema, which would be silently dropped if the decoder used
// the Go field name — and that they narrow the candidate set.
func TestAdmitConstrainedOptionalFeatureFlags(t *testing.T) {
	f := newFixture(false)
	capAddRoute(f, core.Route{
		Name: "cap", Models: []string{"cap-5"},
		Interactive: []string{"full", "plain"},
		Upstreams: map[string]core.UpstreamSpec{
			"full":  {Protocols: []string{"chat"}, InputModalities: []string{"text"}, Tools: true, JSONSchema: true, Stream: true},
			"plain": {Protocols: []string{"chat"}, InputModalities: []string{"text"}},
		},
	})

	base := `{"class":"interactive","model":"cap-5","requirements":{"protocol":"chat","modalities":["text"]`
	cases := []struct {
		extra string
		want  string
	}{
		{"", "full,plain"},
		{`,"tools":true`, "full"},
		{`,"json_schema":true`, "full"},
		{`,"stream":true`, "full"},
		{`,"tools":true,"json_schema":true,"stream":true`, "full"},
		// An unknown sibling key is tolerated (the legacy decoder is lenient)
		// and changes nothing.
		{`,"bogus":true`, "full,plain"},
	}
	for _, c := range cases {
		f.policy.dryRuns = nil
		rec := f.do(t, "POST", "/control/v1/admit", base+c.extra+"}}", "")
		if rec.Code != http.StatusOK {
			t.Errorf("%s: code %d: %s", c.extra, rec.Code, rec.Body)
			continue
		}
		if got := strings.TrimSuffix(strings.TrimPrefix(capDryRuns(f), "interactive: "), "\n"); got != c.want {
			t.Errorf("%s: candidates = %q want %q", c.extra, got, c.want)
		}
	}
}

// TestAdmitConstrainedModalitiesExplicit pins the parity normalization: the
// profile is exactly the modalities the caller declares — deduplicated, with
// blanks ignored — and text is NOT folded in implicitly. An image-only profile
// asks for image alone, exactly as routing.Infer reports an image-only body, so
// a vision-only candidate is a valid answer rather than being excluded by a
// phantom text requirement.
func TestAdmitConstrainedModalitiesExplicit(t *testing.T) {
	f := newFixture(false)
	capAddRoute(f, core.Route{
		Name: "cap", Models: []string{"cap-5"},
		Interactive: []string{"both", "vision-only"},
		Upstreams: map[string]core.UpstreamSpec{
			"both":        {Protocols: []string{"chat"}, InputModalities: []string{"text", "image"}},
			"vision-only": {Protocols: []string{"chat"}, InputModalities: []string{"image"}},
		},
	})

	// Declaring only image asks for image alone: the vision-only candidate is
	// NOT excluded (no implicit text), so both survive, in operator order.
	rec := f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"cap-5","requirements":{"protocol":"chat","modalities":["image"]}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	if got := capDryRuns(f); got != "interactive: both,vision-only\n" {
		t.Fatalf("dry runs:\n%swant image alone (both,vision-only)", got)
	}

	// Duplicates and blanks are collapsed and never widen the profile: text plus
	// image still excludes the vision-only candidate, and the profile is deduped.
	f.policy.dryRuns = nil
	rec = f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"cap-5","requirements":{"protocol":"chat","modalities":["text","image","image"," "]}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	if got := capDryRuns(f); got != "interactive: both\n" {
		t.Fatalf("dry runs:\n%swant deduped profile (both)", got)
	}
}

// TestAdmitConstrainedImageOnlyAdmitsVisionOnly is the admit/proxy parity
// centerpiece: an image-only profile reaches a vision-only candidate while a
// text-only candidate is filtered out, so the dry run reports exactly the
// vision-only candidate and the request is admitted — matching the proxy's
// image-only routing instead of the old fold-text behaviour that would have
// refused it 400 capability_unsupported.
func TestAdmitConstrainedImageOnlyAdmitsVisionOnly(t *testing.T) {
	f := newFixture(false)
	f.policy.decision = core.Decision{Allow: true, AccountID: "visiononly", Reason: "admitted"}
	capAddRoute(f, core.Route{
		Name: "cap", Models: []string{"cap-5"},
		Interactive: []string{"visiononly", "textonly"},
		Upstreams: map[string]core.UpstreamSpec{
			"visiononly": {Protocols: []string{"chat"}, InputModalities: []string{"image"}},
			"textonly":   {Protocols: []string{"chat"}}, // no modalities: text only
		},
	})

	rec := f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"cap-5","requirements":{"protocol":"chat","modalities":["image"]}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	m := decode(t, rec)
	if m["decision"] != "allow" || m["account_id"] != "visiononly" {
		t.Errorf("admit = %v", m)
	}
	if got := capDryRuns(f); got != "interactive: visiononly\n" {
		t.Fatalf("dry runs:\n%swant the vision-only candidate admitted, text-only filtered", got)
	}
}

// TestAdmitConstrainedTextPlusImageExcludesVisionOnly pins the other half of the
// parity: a profile that really does carry text and image cannot be served by a
// vision-only candidate. text is explicit here, never implied, so the exclusion
// is a genuine modality mismatch and the both-capable candidate still serves.
func TestAdmitConstrainedTextPlusImageExcludesVisionOnly(t *testing.T) {
	f := newFixture(false)
	f.policy.decision = core.Decision{Allow: true, AccountID: "both", Reason: "admitted"}
	capAddRoute(f, core.Route{
		Name: "cap", Models: []string{"cap-5"},
		Interactive: []string{"both", "vision-only"},
		Upstreams: map[string]core.UpstreamSpec{
			"both":        {Protocols: []string{"chat"}, InputModalities: []string{"text", "image"}},
			"vision-only": {Protocols: []string{"chat"}, InputModalities: []string{"image"}},
		},
	})

	rec := f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"cap-5","requirements":{"protocol":"chat","modalities":["text","image"]}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	if got := capDryRuns(f); got != "interactive: both\n" {
		t.Fatalf("dry runs:\n%swant vision-only excluded by the text requirement", got)
	}
}

// TestAdmitConstrainedFileRejectedCapabilityUnsupported pins that file is a
// known input modality the router models, not unknown vocabulary: it is accepted
// by the profile decoder and then fails closed because no descriptor can declare
// it, so the refusal is 400 capability_unsupported from the filter stage ("no
// account supports..."), never the "unknown input modality" message an unknown
// value draws and never a silent text-only downgrade.
func TestAdmitConstrainedFileRejectedCapabilityUnsupported(t *testing.T) {
	f := newFixture(false)
	capAddRoute(f, core.Route{
		Name: "cap", Models: []string{"cap-5"},
		Interactive: []string{"primary"},
		Upstreams: map[string]core.UpstreamSpec{
			"primary": {Protocols: []string{"chat"}, InputModalities: []string{"text", "image"}},
		},
	})

	for _, body := range []string{
		`{"class":"interactive","model":"cap-5","requirements":{"protocol":"chat","modalities":["file"]}}`,
		`{"class":"interactive","model":"cap-5","requirements":{"protocol":"chat","modalities":["text","file"]}}`,
	} {
		rec := f.do(t, "POST", "/control/v1/admit", body, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code %d want 400: %s", body, rec.Code, rec.Body)
			continue
		}
		capErrorType(t, rec, "capability_unsupported")
		msg, _ := capError(t, rec)["message"].(string)
		if strings.Contains(msg, "unknown input modality") {
			t.Errorf("%s: file treated as unknown vocabulary, want known-but-unsupported: %q", body, msg)
		}
	}
	if got := capDryRuns(f); got != "" {
		t.Fatalf("unsupported file reached policy:\n%s", got)
	}
}

// TestAdmitAccountWithRequirementsIsAmbiguous pins the refusal: a single
// account has no route for a profile to filter, so asking for both is a 400
// rather than a silently ignored guarantee. Legacy account-only admits (no
// profile) are unchanged.
func TestAdmitAccountWithRequirementsIsAmbiguous(t *testing.T) {
	f := newFixture(false)
	f.policy.decision = core.Decision{Allow: true, AccountID: "stale", Reason: "admitted"}

	rec := f.do(t, "POST", "/control/v1/admit", `{"class":"background","account":"stale","requirements":{"protocol":"chat","modalities":["text"]}}`, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code %d want 400: %s", rec.Code, rec.Body)
	}
	if e := capError(t, rec); e["message"] == "" {
		t.Errorf("no message: %s", rec.Body)
	}
	if got := capDryRuns(f); got != "" {
		t.Fatalf("ambiguous admit reached policy:\n%s", got)
	}

	// The plain account dry run is unchanged.
	m := decode(t, f.do(t, "POST", "/control/v1/admit", `{"class":"background","account":"stale"}`, ""))
	if m["decision"] != "allow" || m["account_id"] != "stale" {
		t.Errorf("legacy account admit = %v", m)
	}
}

// TestAdmitRequirementsWireContract pins the exact wire spellings the control
// API accepts for a profile. Notably json_schema — the lowercase-underscore
// spelling shared with the config key — must be the one that carries the
// structured-output requirement; the Go field name JSONSchema would never be
// read and the requirement would vanish.
func TestAdmitRequirementsWireContract(t *testing.T) {
	f := newFixture(false)
	capAddRoute(f, core.Route{
		Name: "cap", Models: []string{"cap-5"},
		Interactive: []string{"full", "plain"},
		Upstreams: map[string]core.UpstreamSpec{
			"full":  {Protocols: []string{"chat"}, InputModalities: []string{"text"}, Tools: true, JSONSchema: true, Stream: true},
			"plain": {Protocols: []string{"chat"}, InputModalities: []string{"text"}},
		},
	})

	// The full profile, spelled exactly as documented, is accepted and lands in
	// the dry run as the narrowed candidate set.
	body := `{"class":"interactive","model":"cap-5","requirements":{"protocol":"chat","modalities":["text"],"tools":true,"json_schema":true,"stream":true}}`
	rec := f.do(t, "POST", "/control/v1/admit", body, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	if got := capDryRuns(f); got != "interactive: full\n" {
		t.Fatalf("dry runs:\n%swant the json_schema requirement to narrow to full", got)
	}

	// Dropping the optional features (still a valid profile) widens back to both:
	// it was json_schema, read from that exact key, that narrowed to full.
	f.policy.dryRuns = nil
	body = `{"class":"interactive","model":"cap-5","requirements":{"protocol":"chat","modalities":["text"]}}`
	if rec := f.do(t, "POST", "/control/v1/admit", body, ""); rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	if got := capDryRuns(f); got != "interactive: full,plain\n" {
		t.Fatalf("dry runs:\n%swant both candidates without json_schema", got)
	}
}

// TestAdmitChatProfileDropsCodexAccounts pins parity with the proxy, which
// removes Codex accounts from every /v1/chat/completions request before
// admission (legacy or constrained route alike) and refuses the request when
// only Codex accounts were configured. A chat dry run must not answer "allow"
// for an account the inference path would never use.
func TestAdmitChatProfileDropsCodexAccounts(t *testing.T) {
	f := newFixture(false) // primary is a Codex account
	capAddRoute(f, core.Route{
		Name: "cap", Models: []string{"cap-5"},
		Interactive: []string{"primary", "stale"},
		Upstreams: map[string]core.UpstreamSpec{
			"primary": {InputModalities: []string{"text"}},
			"stale":   {InputModalities: []string{"text"}},
		},
	})
	capAddRoute(f, core.Route{Name: "codex", Models: []string{"codex-only"}, Interactive: []string{"primary"}})

	for _, c := range []struct{ model, protocol, want string }{
		{"gpt-5", "chat", "interactive: secondary\n"},
		{"gpt-5", "responses", "interactive: primary,secondary\n"},
		{"cap-5", "chat", "interactive: stale\n"},
		{"cap-5", "responses", "interactive: primary,stale\n"},
	} {
		f.policy.dryRuns = nil
		body := `{"class":"interactive","model":"` + c.model + `","requirements":{"protocol":"` + c.protocol + `","modalities":["text"]}}`
		if rec := f.do(t, "POST", "/control/v1/admit", body, ""); rec.Code != http.StatusOK {
			t.Fatalf("%s/%s: code %d: %s", c.model, c.protocol, rec.Code, rec.Body)
		}
		if got := capDryRuns(f); got != c.want {
			t.Errorf("%s/%s: dry runs %q want %q", c.model, c.protocol, got, c.want)
		}
	}

	// Only Codex accounts on the route: a chat request can never be served.
	f.policy.dryRuns = nil
	rec := f.do(t, "POST", "/control/v1/admit", `{"class":"interactive","model":"codex-only","requirements":{"protocol":"chat","modalities":["text"]}}`, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("codex-only chat: code %d want 400: %s", rec.Code, rec.Body)
	}
	capErrorType(t, rec, "capability_unsupported")
	if got := capDryRuns(f); got != "" {
		t.Fatalf("codex-only chat reached policy:\n%s", got)
	}
}

// TestAdmitLegacyRouteValidatesRequirementsVocabulary pins that a declared
// profile is checked against the modelled vocabulary on a legacy route too: an
// unknown protocol or modality is capability_unsupported exactly as on a
// constrained route, never silently accepted. Completeness is not demanded
// there — a legacy route has no capability boundary to narrow against.
func TestAdmitLegacyRouteValidatesRequirementsVocabulary(t *testing.T) {
	f := newFixture(false)
	for _, body := range []string{
		`{"class":"interactive","model":"gpt-5","requirements":{"protocol":"foo","modalities":["text"]}}`,
		`{"class":"interactive","model":"gpt-5","requirements":{"protocol":"responses","modalities":["video"]}}`,
		`{"class":"interactive","model":"gpt-5","requirements":{"modalities":["video"]}}`,
	} {
		rec := f.do(t, "POST", "/control/v1/admit", body, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code %d want 400: %s", body, rec.Code, rec.Body)
			continue
		}
		capErrorType(t, rec, "capability_unsupported")
	}
	if got := capDryRuns(f); got != "" {
		t.Fatalf("invalid legacy profile reached policy:\n%s", got)
	}

	for _, body := range []string{
		`{"class":"interactive","model":"gpt-5","requirements":{}}`,
		`{"class":"interactive","model":"gpt-5","requirements":{"protocol":"responses"}}`,
		`{"class":"interactive","model":"gpt-5","requirements":{"modalities":["image"],"tools":true}}`,
	} {
		f.policy.dryRuns = nil
		if rec := f.do(t, "POST", "/control/v1/admit", body, ""); rec.Code != http.StatusOK {
			t.Errorf("%s: code %d want 200: %s", body, rec.Code, rec.Body)
		}
		if got := capDryRuns(f); got != "interactive: primary,secondary\n" {
			t.Errorf("%s: dry runs %q", body, got)
		}
	}
}

// Ingest bounds the capability-routing record fields like every other label:
// an agent's upstream_model / pricing_model (the ledger's pricing key) is
// rejected when oversized or carrying control characters, and accepted at the
// bound.
func TestCapIngestBoundsUpstreamAndPricingModel(t *testing.T) {
	long := strings.Repeat("a", core.MaxLabelBytes+1)
	for name, mut := range map[string]func(*core.RequestRecord){
		"upstream_model long":    func(r *core.RequestRecord) { r.UpstreamModel = long },
		"pricing_model long":     func(r *core.RequestRecord) { r.PricingModel = long },
		"upstream_model control": func(r *core.RequestRecord) { r.UpstreamModel = "a\nb" },
		"pricing_model control":  func(r *core.RequestRecord) { r.PricingModel = "a\x00" },
	} {
		f := newIngestFixture(t, true, true)
		r := goodRecord("r1")
		mut(&r)
		rec := f.post(t, ingestKey, core.IngestRequest{SchemaVersion: 1, Host: "vm1", Records: []core.RequestRecord{r}})
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), CodeInvalidRecord) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
		if len(f.rows()) != 0 {
			t.Errorf("%s: stored", name)
		}
	}

	f := newIngestFixture(t, true, true)
	r := goodRecord("r1")
	r.UpstreamModel, r.PricingModel = long[1:], long[1:]
	rec := f.post(t, ingestKey, core.IngestRequest{SchemaVersion: 1, Host: "vm1", Records: []core.RequestRecord{r}})
	if rec.Code != http.StatusOK || len(f.rows()) != 1 {
		t.Fatalf("at bound: %d %s", rec.Code, rec.Body)
	}
}
