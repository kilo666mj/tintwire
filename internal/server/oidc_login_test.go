package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kilo666mj/oidcrp"
	"github.com/kilo666mj/tintwire/internal/store"
)

const testDesktopHandoff = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestOIDCUsesRelyingPartyService(t *testing.T) {
	publicURL, err := url.Parse("https://tintwire.example")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{publicURL: publicURL}
	service, err := newOIDCLoginService(server, "https://id.example", "tintwire", "", publicURL)
	if err != nil {
		t.Fatal(err)
	}
	if service == nil || !service.Enabled() {
		t.Fatal("oidcrp service is not enabled")
	}
	request := httptest.NewRequest(http.MethodGet, "https://tintwire.example/api/v1/auth/oidc/start?desktop=attacker", nil)
	recorder := httptest.NewRecorder()
	service.LoginStart(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid handoff status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}

func TestDesktopOIDCHandoffRequiresBrowserConfirmation(t *testing.T) {
	server, data := newOIDCTestServer(t)
	sessions := &tintwireOIDCSessions{server: server}
	callback := httptest.NewRequest(http.MethodGet, "https://tintwire.example/api/v1/auth/oidc/callback", nil)
	callback.Host = "tintwire.example"
	callbackResult := httptest.NewRecorder()
	if err := sessions.IssueDesktop(callbackResult, callback, oidcrp.Identity{Subject: "subject-1", Email: "alice@example.com"}, testDesktopHandoff); err != nil {
		t.Fatal(err)
	}
	confirmationCookie := responseCookie(t, callbackResult.Result(), oidcDesktopConfirmationCookieName)
	if !confirmationCookie.HttpOnly || confirmationCookie.SameSite != http.SameSiteLaxMode || !confirmationCookie.Secure {
		t.Fatalf("confirmation cookie is not browser-only: %#v", confirmationCookie)
	}

	pending := exchangeDesktopHandoff(server, testDesktopHandoff)
	if pending.Code != http.StatusUnauthorized {
		t.Fatalf("attacker-chosen handoff exchanged before approval: status=%d body=%q", pending.Code, pending.Body.String())
	}

	pageRequest := httptest.NewRequest(http.MethodGet, "https://tintwire.example/api/v1/auth/desktop/confirm", nil)
	pageRequest.AddCookie(confirmationCookie)
	page := httptest.NewRecorder()
	server.desktopConfirmation(page, pageRequest)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "0123-4567") || !strings.Contains(page.Body.String(), "Approve only if the codes match") {
		t.Fatalf("confirmation page status=%d body=%q", page.Code, page.Body.String())
	}

	crossOrigin := confirmationRequest(http.MethodPost, "https://evil.example", confirmationCookie)
	crossOriginResult := httptest.NewRecorder()
	server.approveDesktopConfirmation(crossOriginResult, crossOrigin)
	if crossOriginResult.Code != http.StatusForbidden {
		t.Fatalf("cross-origin approval status=%d, want %d", crossOriginResult.Code, http.StatusForbidden)
	}
	if result := exchangeDesktopHandoff(server, testDesktopHandoff); result.Code != http.StatusUnauthorized {
		t.Fatalf("cross-origin request approved handoff: status=%d", result.Code)
	}

	approve := confirmationRequest(http.MethodPost, "https://tintwire.example", confirmationCookie)
	approved := httptest.NewRecorder()
	server.approveDesktopConfirmation(approved, approve)
	if approved.Code != http.StatusOK || !strings.Contains(approved.Body.String(), "Sign-in approved") {
		t.Fatalf("approval status=%d body=%q", approved.Code, approved.Body.String())
	}

	exchanged := exchangeDesktopHandoff(server, testDesktopHandoff)
	if exchanged.Code != http.StatusNoContent {
		t.Fatalf("approved exchange status=%d body=%q", exchanged.Code, exchanged.Body.String())
	}
	sessionCookie := responseCookie(t, exchanged.Result(), sessionCookieName)
	user, err := data.UserForSession(t.Context(), sessionCookie.Value)
	if err != nil || user.Username != "alice" {
		t.Fatalf("desktop session user=%#v err=%v", user, err)
	}
	if replay := exchangeDesktopHandoff(server, testDesktopHandoff); replay.Code != http.StatusUnauthorized {
		t.Fatalf("replayed handoff status=%d, want %d", replay.Code, http.StatusUnauthorized)
	}
}

func TestDesktopOIDCHandoffCancelAndExpiry(t *testing.T) {
	server, data := newOIDCTestServer(t)
	sessions := &tintwireOIDCSessions{server: server}
	handoff := strings.Repeat("a", 64)
	callback := httptest.NewRequest(http.MethodGet, "https://tintwire.example/api/v1/auth/oidc/callback", nil)
	callbackResult := httptest.NewRecorder()
	if err := sessions.IssueDesktop(callbackResult, callback, oidcrp.Identity{Subject: "subject-2", Email: "bob@example.com"}, handoff); err != nil {
		t.Fatal(err)
	}
	confirmationCookie := responseCookie(t, callbackResult.Result(), oidcDesktopConfirmationCookieName)
	cancelled := httptest.NewRecorder()
	server.cancelDesktopConfirmation(cancelled, confirmationRequest(http.MethodPost, "https://tintwire.example", confirmationCookie))
	if cancelled.Code != http.StatusOK || !strings.Contains(cancelled.Body.String(), "Sign-in cancelled") {
		t.Fatalf("cancel status=%d body=%q", cancelled.Code, cancelled.Body.String())
	}
	if result := exchangeDesktopHandoff(server, handoff); result.Code != http.StatusGone {
		t.Fatalf("cancelled exchange status=%d, want %d", result.Code, http.StatusGone)
	}

	user, err := data.FindOrCreateOIDCUser(t.Context(), "subject-expired", "expired")
	if err != nil {
		t.Fatal(err)
	}
	expiredHandoff := strings.Repeat("b", 64)
	if err := data.CreateOIDCDesktopConfirmation(t.Context(), user.ID, expiredHandoff, "browser-secret", "BBBB-BBBB", time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if _, err := data.OIDCDesktopConfirmation(t.Context(), "browser-secret"); !errors.Is(err, store.ErrInvalidCredentials) {
		t.Fatalf("expired browser confirmation error=%v", err)
	}
	if result := exchangeDesktopHandoff(server, expiredHandoff); result.Code != http.StatusUnauthorized {
		t.Fatalf("expired exchange status=%d, want %d", result.Code, http.StatusUnauthorized)
	}
}

// The confirmation page is reached by a redirect from the identity provider, so
// browsers attribute that navigation to the provider's site and withhold a
// SameSite=Strict cookie. That made every confirmation fail as expired, which no
// handler-level test caught because synthetic requests carry the cookie anyway.
func TestDesktopConfirmationCookieSurvivesProviderRedirect(t *testing.T) {
	server, _ := newOIDCTestServer(t)
	sessions := &tintwireOIDCSessions{server: server}
	callback := httptest.NewRequest(http.MethodGet, "https://tintwire.example/api/v1/auth/oidc/callback", nil)
	callback.Host = "tintwire.example"
	callbackResult := httptest.NewRecorder()
	if err := sessions.IssueDesktop(callbackResult, callback, oidcrp.Identity{Subject: "subject-redirect", Email: "carol@example.com"}, testDesktopHandoff); err != nil {
		t.Fatal(err)
	}
	confirmationCookie := responseCookie(t, callbackResult.Result(), oidcDesktopConfirmationCookieName)
	if confirmationCookie.SameSite == http.SameSiteStrictMode {
		t.Fatal("confirmation cookie is SameSite=Strict, so the provider redirect to the confirmation page will not carry it")
	}
	if confirmationCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("confirmation cookie SameSite = %v, want Lax", confirmationCookie.SameSite)
	}
	if confirmationCookie.Path != "/api/v1/auth/desktop" {
		t.Fatalf("confirmation cookie path = %q, want /api/v1/auth/desktop", confirmationCookie.Path)
	}

	// Lax keeps the approval POST protected: a cross-site form submission never
	// carries the cookie, and the handler rejects the origin regardless.
	crossOrigin := httptest.NewRecorder()
	server.approveDesktopConfirmation(crossOrigin, confirmationRequest(http.MethodPost, "https://evil.example", confirmationCookie))
	if crossOrigin.Code != http.StatusForbidden {
		t.Fatalf("cross-origin approval status=%d, want %d", crossOrigin.Code, http.StatusForbidden)
	}

	// Clearing must use the same attributes, or the browser keeps the cookie.
	cleared := httptest.NewRecorder()
	clearDesktopConfirmationCookie(cleared, callback, server)
	clearedCookie := responseCookie(t, cleared.Result(), oidcDesktopConfirmationCookieName)
	if clearedCookie.SameSite != confirmationCookie.SameSite || clearedCookie.Path != confirmationCookie.Path {
		t.Fatalf("clearing cookie %#v does not match issued cookie %#v", clearedCookie, confirmationCookie)
	}
}

func newOIDCTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	data, err := store.Open(filepath.Join(t.TempDir(), "oidc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	publicURL, err := url.Parse("https://tintwire.example")
	if err != nil {
		t.Fatal(err)
	}
	return &Server{store: data, publicURL: publicURL, limiter: newToolLimiter(), subscribers: make(map[chan liveUpdate]struct{})}, data
}

func exchangeDesktopHandoff(server *Server, handoff string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "https://tintwire.example/api/v1/auth/desktop/session", strings.NewReader(`{"code":"`+handoff+`"}`))
	request.Host = "tintwire.example"
	request.Header.Set("Origin", "https://tintwire.example")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.desktopSession(recorder, request)
	return recorder
}

func confirmationRequest(method, origin string, cookie *http.Cookie) *http.Request {
	request := httptest.NewRequest(method, "https://tintwire.example/api/v1/auth/desktop/confirm", nil)
	request.Host = "tintwire.example"
	request.Header.Set("Origin", origin)
	request.AddCookie(cookie)
	return request
}

func responseCookie(t *testing.T, response *http.Response, name string) *http.Cookie {
	t.Helper()
	defer func() { _ = response.Body.Close() }()
	for _, cookie := range response.Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("response has no %s cookie", name)
	return nil
}
