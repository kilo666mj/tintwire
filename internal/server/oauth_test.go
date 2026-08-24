package server

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

func TestScopeContains(t *testing.T) {
	for _, raw := range []json.RawMessage{json.RawMessage(`"openid tintwire:mcp"`), json.RawMessage(`["openid","tintwire:mcp"]`)} {
		if !scopeContains(raw, "tintwire:mcp") {
			t.Fatalf("scope not found in %s", raw)
		}
	}
	if scopeContains(json.RawMessage(`"tintwire:mcp-admin"`), "tintwire:mcp") {
		t.Fatal("partial scope matched")
	}
}

func TestOAuthVerifierChecksIssuerAudienceScopeAndSubject(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyID := "test-key"
	var issuer string
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/jwks"})
		case "/jwks":
			_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: keyID, Algorithm: string(jose.RS256), Use: "sig"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	issuer = provider.URL
	verifier, err := newOAuthAgentVerifier(issuer, "https://tintwire.example/mcp", "tintwire:mcp")
	if err != nil {
		t.Fatal(err)
	}
	verifier.httpClient = provider.Client()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: keyID}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	claims := struct {
		jwt.Claims
		Scope string `json:"scope"`
	}{Claims: jwt.Claims{Issuer: issuer, Subject: "client-pocket-id", Audience: jwt.Audience{"https://tintwire.example/mcp"}, Expiry: jwt.NewNumericDate(time.Now().Add(time.Minute))}, Scope: "tintwire:mcp"}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	if subject, err := verifier.verify(t.Context(), raw); err != nil || subject != "client-pocket-id" {
		t.Fatalf("subject=%q err=%v", subject, err)
	}
	claims.Audience = jwt.Audience{"https://another.example/api"}
	wrongAudience, _ := jwt.Signed(signer).Claims(claims).Serialize()
	if _, err := verifier.verify(t.Context(), wrongAudience); err == nil {
		t.Fatal("wrong audience accepted")
	}
	claims.Audience = jwt.Audience{"https://tintwire.example/mcp"}
	claims.Scope = "openid"
	wrongScope, _ := jwt.Signed(signer).Claims(claims).Serialize()
	if _, err := verifier.verify(t.Context(), wrongScope); err == nil {
		t.Fatal("missing scope accepted")
	}
}

func TestOAuthVerifierConfiguration(t *testing.T) {
	if _, err := newOAuthAgentVerifier("http://id.example", "https://tintwire.example/mcp", "tintwire:mcp"); err == nil {
		t.Fatal("HTTP issuer accepted")
	}
	if _, err := newOAuthAgentVerifier("https://id.example", "https://tintwire.example/mcp", "bad scope"); err == nil {
		t.Fatal("space-separated required scope accepted")
	}
	if verifier, err := newOAuthAgentVerifier("https://id.example", "https://tintwire.example/mcp", "tintwire:mcp"); err != nil || verifier == nil {
		t.Fatalf("valid configuration: verifier=%v err=%v", verifier, err)
	}
}
