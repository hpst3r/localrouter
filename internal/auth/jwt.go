package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// jwtClaims holds the JWT payload fields LocalRouter needs.
type jwtClaims struct {
	Exp  float64 `json:"exp"`
	Auth struct {
		ChatGPTAccountID string `json:"chatgpt_account_id"`
	} `json:"https://api.openai.com/auth"`
}

var errBadJWT = errors.New("auth: malformed JWT")

// parseJWT decodes the payload of a JWT WITHOUT verifying its signature.
//
// This is deliberate: tokens are obtained directly from the issuer over TLS
// and are only replayed back to OpenAI, which verifies them. LocalRouter
// reads claims only to schedule refreshes (exp) and to label the account
// (chatgpt_account_id); a forged token gains nothing because upstream would
// reject it. Errors never include token content.
func parseJWT(tok string) (jwtClaims, error) {
	var c jwtClaims
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return c, errBadJWT
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return c, errBadJWT
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return c, errBadJWT
	}
	return c, nil
}

// jwtExpiry returns the exp claim, or zero if absent/unparseable.
func jwtExpiry(tok string) time.Time {
	c, err := parseJWT(tok)
	if err != nil || c.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(c.Exp), 0)
}

// chatGPTAccountID extracts chatgpt_account_id from the id token first, then
// the access token. Returns "" if neither carries it.
func chatGPTAccountID(idToken, accessToken string) string {
	for _, t := range []string{idToken, accessToken} {
		if t == "" {
			continue
		}
		if c, err := parseJWT(t); err == nil && c.Auth.ChatGPTAccountID != "" {
			return c.Auth.ChatGPTAccountID
		}
	}
	return ""
}
