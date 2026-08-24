package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

type oauthAgentVerifier struct {
	issuer, resource, scope string
	httpClient              *http.Client
	mu                      sync.Mutex
	verifier                *oidc.IDTokenVerifier
}

func newOAuthAgentVerifier(issuer, resource, scope string) (*oauthAgentVerifier, error) {
	issuer, resource, scope = strings.TrimSuffix(strings.TrimSpace(issuer), "/"), strings.TrimSpace(resource), strings.TrimSpace(scope)
	if issuer == "" {
		return nil, nil
	}
	issuerURL, issuerErr := url.Parse(issuer)
	resourceURL, resourceErr := url.Parse(resource)
	if issuerErr != nil || issuerURL.Scheme != "https" || issuerURL.Host == "" || issuerURL.RawQuery != "" || issuerURL.Fragment != "" {
		return nil, errors.New("OAuth issuer must be an HTTPS URL without query or fragment")
	}
	if resourceErr != nil || resourceURL.Scheme != "https" || resourceURL.Host == "" || resourceURL.Fragment != "" {
		return nil, errors.New("OAuth resource must be an absolute HTTPS URL without fragment")
	}
	if scope == "" || strings.ContainsAny(scope, " \t\r\n") {
		return nil, errors.New("OAuth scope must be one nonblank permission")
	}
	return &oauthAgentVerifier{issuer: issuer, resource: resource, scope: scope, httpClient: &http.Client{Timeout: 15 * time.Second}}, nil
}

func (v *oauthAgentVerifier) verify(ctx context.Context, rawToken string) (string, error) {
	if v == nil || strings.Count(rawToken, ".") != 2 {
		return "", errors.New("not an OAuth JWT")
	}
	verifier, err := v.tokenVerifier(ctx)
	if err != nil {
		return "", err
	}
	token, err := verifier.Verify(oidc.ClientContext(ctx, v.httpClient), rawToken)
	if err != nil {
		return "", err
	}
	if !containsString(token.Audience, v.resource) {
		return "", errors.New("OAuth token audience does not include this MCP resource")
	}
	var claims struct {
		Scope json.RawMessage `json:"scope"`
	}
	if err := token.Claims(&claims); err != nil {
		return "", err
	}
	if !scopeContains(claims.Scope, v.scope) {
		return "", errors.New("OAuth token lacks the required MCP permission")
	}
	if strings.TrimSpace(token.Subject) == "" {
		return "", errors.New("OAuth token has no subject")
	}
	return token.Subject, nil
}

func (v *oauthAgentVerifier) tokenVerifier(ctx context.Context) (*oidc.IDTokenVerifier, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.verifier != nil {
		return v.verifier, nil
	}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, v.httpClient), v.issuer)
	if err != nil {
		return nil, err
	}
	v.verifier = provider.Verifier(&oidc.Config{SkipClientIDCheck: true})
	return v.verifier, nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func scopeContains(raw json.RawMessage, wanted string) bool {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		for _, scope := range strings.Fields(text) {
			if scope == wanted {
				return true
			}
		}
	}
	var values []string
	if json.Unmarshal(raw, &values) == nil {
		return containsString(values, wanted)
	}
	return false
}
