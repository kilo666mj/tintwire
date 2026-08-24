package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/kilo666mj/tintwire/internal/server"
	"github.com/kilo666mj/tintwire/internal/store"
)

func TestAgentAdministration(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.BootstrapUser(context.Background(), "admin", "secure agent password"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateUser(context.Background(), "viewer", "secure viewer password", false); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewWithOptions(db, server.Options{AuthRequired: true})
	if err != nil {
		t.Fatal(err)
	}
	send := func(method, target, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, target, bytes.NewBufferString(body))
		request.Header.Set("Origin", "http://example.com")
		request.Host = "example.com"
		if cookie != nil {
			request.AddCookie(cookie)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	sessionFor := func(username, password string) *http.Cookie {
		t.Helper()
		recorder := send(http.MethodPost, "/api/v1/session", `{"username":"`+username+`","password":"`+password+`"}`, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("login status = %d, body = %q", recorder.Code, recorder.Body.String())
		}
		return recorder.Result().Cookies()[0]
	}

	admin := sessionFor("admin", "secure agent password")
	viewer := sessionFor("viewer", "secure viewer password")

	if forbidden := send(http.MethodPost, "/api/v1/agents", `{"name":"triage"}`, viewer); forbidden.Code != http.StatusForbidden {
		t.Fatalf("non-administrator create status = %d, body = %q", forbidden.Code, forbidden.Body.String())
	}
	if listForbidden := send(http.MethodGet, "/api/v1/agents", "", viewer); listForbidden.Code != http.StatusForbidden {
		t.Fatalf("non-administrator list status = %d", listForbidden.Code)
	}
	if invalid := send(http.MethodPost, "/api/v1/agents", `{"name":"Triage Bot"}`, admin); invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid name status = %d, body = %q", invalid.Code, invalid.Body.String())
	}

	created := send(http.MethodPost, "/api/v1/agents", `{"name":"triage","display_name":"Triage","description":"Investigates alerts"}`, admin)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %q", created.Code, created.Body.String())
	}
	var registration struct {
		Agent       store.Agent `json:"agent"`
		AccessToken string      `json:"access_token"`
	}
	if err := json.NewDecoder(created.Body).Decode(&registration); err != nil {
		t.Fatal(err)
	}
	if registration.Agent.Owner != "admin" || registration.Agent.Username != "agent-triage" || !registration.Agent.Enabled {
		t.Fatalf("agent = %+v", registration.Agent)
	}
	if len(registration.AccessToken) != 68 {
		t.Fatalf("access token length = %d", len(registration.AccessToken))
	}

	if duplicate := send(http.MethodPost, "/api/v1/agents", `{"name":"triage"}`, admin); duplicate.Code != http.StatusConflict {
		t.Fatalf("duplicate status = %d, body = %q", duplicate.Code, duplicate.Body.String())
	}

	listed := send(http.MethodGet, "/api/v1/agents", "", admin)
	var directory struct {
		Agents []store.Agent `json:"agents"`
	}
	if err := json.NewDecoder(listed.Body).Decode(&directory); err != nil {
		t.Fatal(err)
	}
	if len(directory.Agents) != 1 || directory.Agents[0].Name != "triage" {
		t.Fatalf("directory = %+v", directory.Agents)
	}
	if bytes.Contains(listed.Body.Bytes(), []byte(registration.AccessToken)) {
		t.Fatal("agent directory must not expose credentials")
	}

	runs := send(http.MethodGet, "/api/v1/agents/triage/runs", "", admin)
	if runs.Code != http.StatusOK {
		t.Fatalf("runs status = %d, body = %q", runs.Code, runs.Body.String())
	}
	if missing := send(http.MethodGet, "/api/v1/agents/absent/runs", "", admin); missing.Code != http.StatusNotFound {
		t.Fatalf("missing agent runs status = %d", missing.Code)
	}

	if revoked := send(http.MethodPost, "/api/v1/agents/triage/revoke", "", admin); revoked.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d, body = %q", revoked.Code, revoked.Body.String())
	}
	if _, _, err := db.AgentForToken(context.Background(), registration.AccessToken); err == nil {
		t.Fatal("revoked agent token still authenticates")
	}
}
