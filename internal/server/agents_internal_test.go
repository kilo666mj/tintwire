package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/kilo666mj/tintwire/internal/store"
)

func fixedOAuthSubjectVerifier(token, subject string, resultErr error) func(context.Context, string) (string, error) {
	return func(_ context.Context, presented string) (string, error) {
		if presented != token {
			return "", errors.New("invalid token")
		}
		return subject, resultErr
	}
}

func TestRequireAgentDelegatesOnlyFromTrustedSwitchboardSubject(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "delegation.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	owner, err := db.CreateUser(t.Context(), "owner", "secure owner password", true)
	if err != nil {
		t.Fatal(err)
	}
	delegatedAgent, _, err := db.CreateAgent(t.Context(), store.CreateAgentInput{Name: "delegated", OwnerUserID: owner.ID, OAuthSubject: "subject-alice"})
	if err != nil {
		t.Fatal(err)
	}
	directAgent, _, err := db.CreateAgent(t.Context(), store.CreateAgentInput{Name: "direct-oauth", OwnerUserID: owner.ID, OAuthSubject: "ordinary-oauth-subject"})
	if err != nil {
		t.Fatal(err)
	}
	_, staticToken, err := db.CreateAgent(t.Context(), store.CreateAgentInput{Name: "static-agent", OwnerUserID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{store: db, verifyOAuthSubject: fixedOAuthSubjectVerifier("signed-service-token", "client-switchboard", nil), switchboardOAuthSubject: "client-switchboard"}
	handler := server.requireAgent(func(w http.ResponseWriter, r *http.Request) {
		agent, _ := r.Context().Value(agentContextKey{}).(store.Agent)
		_, _ = w.Write([]byte(agent.Name))
	})

	request := func(token, delegated string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		if delegated != "" {
			req.Header.Set(switchboardOAuthSubjectHeader, delegated)
		}
		response := httptest.NewRecorder()
		handler(response, req)
		return response
	}

	response := request("signed-service-token", "subject-alice")
	if response.Code != http.StatusOK || response.Body.String() != delegatedAgent.Name {
		t.Fatalf("trusted delegation = %d %q", response.Code, response.Body.String())
	}
	response = request("signed-service-token", "unknown-subject")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unknown delegated subject status = %d", response.Code)
	}
	response = request(staticToken, "subject-alice")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("static token delegation status = %d", response.Code)
	}

	server.verifyOAuthSubject = fixedOAuthSubjectVerifier("ordinary-user-token", "ordinary-oauth-subject", nil)
	response = request("ordinary-user-token", "")
	if response.Code != http.StatusOK || response.Body.String() != directAgent.Name {
		t.Fatalf("direct OAuth authentication = %d %q", response.Code, response.Body.String())
	}
	response = request("ordinary-user-token", "subject-alice")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("untrusted OAuth delegation status = %d", response.Code)
	}
	server.verifyOAuthSubject = fixedOAuthSubjectVerifier("invalid-token", "", errors.New("invalid"))
	response = request("invalid-token", "subject-alice")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("invalid bearer delegation status = %d", response.Code)
	}
}

func TestRequestedSwitchboardOAuthSubjectRejectsAmbiguity(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request.Header.Add(switchboardOAuthSubjectHeader, "subject-a")
	request.Header.Add(switchboardOAuthSubjectHeader, "subject-b")
	if _, _, err := requestedSwitchboardOAuthSubject(request); err == nil {
		t.Fatal("multiple delegated subjects were accepted")
	}
}
