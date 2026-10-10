package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/hpst3r/localrouter/internal/agent"
	"github.com/hpst3r/localrouter/internal/routing"
)

// exitError carries a process exit code: 1 = admission denied (including a
// capability_unsupported refusal), 2 = usage error or control API
// unreachable/failed.
type exitError struct {
	code int
	msg  string
}

func (e exitError) Error() string { return e.msg }

// Code is the process exit code main should use.
func (e exitError) Code() int { return e.code }

// admitResult mirrors the POST /control/v1/admit response.
type admitResult struct {
	Decision  string `json:"decision"`
	AccountID string `json:"account_id"`
	Reason    string `json:"reason"`
}

// admitRequirementWire is the optional "requirements" object of an admit
// request: the client-side mirror of the server's capability profile. Its field
// names are the lowercase-underscore spellings the server accepts, notably
// json_schema.
type admitRequirementWire struct {
	Protocol   string   `json:"protocol,omitempty"`
	Modalities []string `json:"modalities,omitempty"`
	Tools      bool     `json:"tools,omitempty"`
	JSONSchema bool     `json:"json_schema,omitempty"`
	Stream     bool     `json:"stream,omitempty"`
}

// admitRequirements builds the capability profile from the flags, returning nil
// when none was given so a legacy admit request is unchanged. It checks only the
// vocabulary the flags draw from — the same values routing models — and never
// fills in a default protocol or modality: on a capability-constrained route the
// server refuses an incomplete profile with 400
// capability_requirements_required, and the CLI must not paper over that by
// guessing what the caller meant.
func admitRequirements(protocol, modalities string, tools, jsonSchema, stream bool) (*admitRequirementWire, error) {
	protocol = strings.TrimSpace(protocol)
	if protocol != "" && protocol != routing.ProtocolChat && protocol != routing.ProtocolResponses {
		return nil, exitError{2, fmt.Sprintf("admit: --protocol must be chat or responses, got %q", protocol)}
	}
	var mods []string
	for _, m := range strings.Split(modalities, ",") {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		switch m {
		case routing.ModalityText, routing.ModalityImage, routing.ModalityAudio:
			mods = append(mods, m)
		default:
			return nil, exitError{2, fmt.Sprintf("admit: --input-modalities must be text, image or audio, got %q", m)}
		}
	}
	if protocol == "" && len(mods) == 0 && !tools && !jsonSchema && !stream {
		return nil, nil
	}
	return &admitRequirementWire{
		Protocol:   protocol,
		Modalities: mods,
		Tools:      tools,
		JSONSchema: jsonSchema,
		Stream:     stream,
	}, nil
}

// cmdAdmit asks a running LocalRouter whether a request of --class would be
// admitted on --account (or the route for --model), without creating a
// lease. It returns nil on allow and exitError{1} on deny (including a
// capability_unsupported refusal), so scripts can use it as a gate; any other
// failure is exitError{2}.
func cmdAdmit(args []string) error {
	return runAdmit(args, os.Stdout, os.Stderr)
}

func runAdmit(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("admit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	class := fs.String("class", "background", "workload class: background or interactive")
	account := fs.String("account", "", "account ID to check (exactly one of --account/--model)")
	model := fs.String("model", "", "model name whose route to check")
	base := fs.String("url", "http://127.0.0.1:8787", "LocalRouter base URL")
	asJSON := fs.Bool("json", false, "print the decision as JSON")
	timeout := fs.Duration("timeout", 5*time.Second, "request timeout")
	keyFile := fs.String("key-file", "", "client key file (needed only when control.require_auth is on)")
	protocol := fs.String("protocol", "", "capability requirement: wire protocol the request uses (chat or responses)")
	modalities := fs.String("input-modalities", "", "capability requirement: comma-separated input modalities (text,image,audio)")
	requiresTools := fs.Bool("requires-tools", false, "capability requirement: the request sends a non-empty top-level tools array")
	requiresJSONSchema := fs.Bool("requires-json-schema", false, "capability requirement: the request asks for structured output (json_schema)")
	stream := fs.Bool("stream", false, "capability requirement: the request asks for a streaming response")
	if err := fs.Parse(args); err != nil {
		return exitError{2, "admit: " + err.Error()}
	}
	if fs.NArg() > 0 {
		return exitError{2, "admit: unexpected arguments: " + strings.Join(fs.Args(), " ")}
	}
	if (*account == "") == (*model == "") {
		return exitError{2, "admit: exactly one of --account or --model is required"}
	}
	reqs, err := admitRequirements(*protocol, *modalities, *requiresTools, *requiresJSONSchema, *stream)
	if err != nil {
		return err
	}

	// A map keeps the legacy request bodies byte for byte what they were (Go
	// sorts map keys); "requirements" is appended only when a capability flag
	// was actually given, so the old CLI invocation is unchanged.
	req := map[string]any{"class": *class}
	if *account != "" {
		req["account"] = *account
	} else {
		req["model"] = *model
	}
	if reqs != nil {
		req["requirements"] = *reqs
	}
	body, _ := json.Marshal(req)
	endpoint := strings.TrimRight(*base, "/") + "/control/v1/admit"
	// Never follow a redirect: net/http re-sends Authorization to the same
	// hostname on any port.
	client := &http.Client{Timeout: *timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	hreq, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return exitError{2, "admit: " + err.Error()}
	}
	hreq.Header.Set("Content-Type", "application/json")
	if *keyFile != "" {
		if agent.InsecureServerURL(*base) {
			fmt.Fprintln(stderr, "admit: warning: --url is plain http to a non-loopback host; the client key is sent unencrypted (use https or an encrypted overlay)")
		}
		k, err := os.ReadFile(*keyFile)
		if err != nil {
			return exitError{2, "admit: reading key file: " + err.Error()}
		}
		hreq.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(k)))
	}
	resp, err := client.Do(hreq)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // drop the "Post <url>:" prefix; report the cause
		}
		return exitError{2, fmt.Sprintf("admit: control API unreachable at %s: %v", *base, err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return exitError{2, "admit: reading response: " + err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		msg := resp.Status
		if resp.StatusCode >= 300 && resp.StatusCode <= 399 {
			msg += " (redirect not followed; check --url)"
		}
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			// Capability refusals carry a stable type (e.g.
			// capability_unsupported); surface it so a script can branch on it.
			// Legacy errors are untyped and keep their old message.
			if e.Error.Type != "" {
				msg = fmt.Sprintf("HTTP %d: %s: %s", resp.StatusCode, e.Error.Type, e.Error.Message)
			} else {
				msg = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, e.Error.Message)
			}
			// No account can serve the declared profile: that is a definite
			// deny, not a usage error or an outage.
			if e.Error.Type == "capability_unsupported" {
				return exitError{1, "admit: " + msg}
			}
		}
		return exitError{2, "admit: " + msg}
	}
	var res admitResult
	if err := json.Unmarshal(raw, &res); err != nil || (res.Decision != "allow" && res.Decision != "deny") {
		return exitError{2, "admit: unexpected response from control API"}
	}

	if *asJSON {
		_ = json.NewEncoder(stdout).Encode(res)
	} else {
		line := res.Decision
		if res.AccountID != "" {
			line += " " + res.AccountID
		}
		if res.Reason != "" {
			line += ": " + res.Reason
		}
		fmt.Fprintln(stdout, line)
	}
	if res.Decision == "deny" {
		return exitError{1, "admission denied"}
	}
	return nil
}
