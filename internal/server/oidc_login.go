package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/kilo666mj/tintwire/internal/store"
	"golang.org/x/oauth2"
)

const oidcStateCookieName = "tintwire_oidc_state"

const desktopOIDCStatePrefix = "desktop_"
const desktopOIDCPollStatePrefix = "desktop_poll_"

type oidcLoginService struct {
	issuer, clientID, redirectURL string
	httpClient                    *http.Client
	mu                            sync.Mutex
	provider                      *oidc.Provider
}

func newOIDCLoginService(issuer, clientID, redirectURL string, publicURL *url.URL) (*oidcLoginService, error) {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return nil, nil
	}
	issuer = strings.TrimSuffix(strings.TrimSpace(issuer), "/")
	if issuer == "" {
		return nil, errors.New("interactive OIDC requires an OAuth issuer")
	}
	if redirectURL == "" && publicURL != nil {
		redirectURL = strings.TrimSuffix(publicURL.String(), "/") + "/api/v1/auth/oidc/callback"
	}
	u, err := url.Parse(redirectURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("OIDC redirect URL must be an absolute HTTPS URL")
	}
	return &oidcLoginService{issuer: issuer, clientID: clientID, redirectURL: redirectURL, httpClient: &http.Client{Timeout: 15 * time.Second}}, nil
}

func (o *oidcLoginService) config(ctx context.Context) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.provider == nil {
		provider, err := oidc.NewProvider(oidc.ClientContext(ctx, o.httpClient), o.issuer)
		if err != nil {
			return nil, nil, err
		}
		o.provider = provider
	}
	return &oauth2.Config{ClientID: o.clientID, Endpoint: o.provider.Endpoint(), RedirectURL: o.redirectURL, Scopes: []string{oidc.ScopeOpenID, "profile", "email"}}, o.provider.Verifier(&oidc.Config{ClientID: o.clientID}), nil
}

func randomOIDCValue() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func validDesktopCode(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}

func desktopPollCode(state string) (string, bool) {
	value := strings.TrimPrefix(state, desktopOIDCPollStatePrefix)
	if value == state {
		return "", false
	}
	code, _, found := strings.Cut(value, "_")
	return code, found && validDesktopCode(code)
}

func (s *Server) oidcStart(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		http.NotFound(w, r)
		return
	}
	if !s.controlLeaseValid(w, r) {
		return
	}
	state, err := randomOIDCValue()
	if err != nil {
		http.Error(w, "unable to start sign-in", 500)
		return
	}
	desktop := r.URL.Query().Get("desktop")
	if desktop == "1" {
		state = desktopOIDCStatePrefix + state
	} else if desktop != "" {
		if !validDesktopCode(desktop) {
			http.Error(w, "valid desktop sign-in code is required", http.StatusBadRequest)
			return
		}
		state = desktopOIDCPollStatePrefix + desktop + "_" + state
	}
	nonce, err := randomOIDCValue()
	if err != nil {
		http.Error(w, "unable to start sign-in", 500)
		return
	}
	verifier := oauth2.GenerateVerifier()
	if _, err = s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		return nil, data.CreateOIDCLoginState(r.Context(), state, verifier, nonce, 10*time.Minute)
	}); err != nil {
		http.Error(w, "unable to start sign-in", http.StatusServiceUnavailable)
		return
	}
	config, _, err := s.oidc.config(r.Context())
	if err != nil {
		http.Error(w, "identity provider unavailable", http.StatusBadGateway)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: oidcStateCookieName, Value: state, Path: "/api/v1/auth/oidc/callback", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: 600})
	http.Redirect(w, r, config.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce)), http.StatusFound)
}

func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		http.NotFound(w, r)
		return
	}
	if providerError := r.URL.Query().Get("error"); providerError != "" {
		http.Error(w, "Pocket ID sign-in was cancelled", http.StatusUnauthorized)
		return
	}
	state := r.URL.Query().Get("state")
	cookie, err := r.Cookie(oidcStateCookieName)
	if err != nil || state == "" || cookie.Value != state {
		http.Error(w, "invalid or expired sign-in state", http.StatusBadRequest)
		return
	}
	loginState, err := s.store.OIDCLoginState(r.Context(), state)
	if err != nil {
		http.Error(w, "invalid or expired sign-in state", http.StatusBadRequest)
		return
	}
	config, verifier, err := s.oidc.config(r.Context())
	if err != nil {
		http.Error(w, "identity provider unavailable", http.StatusBadGateway)
		return
	}
	token, err := config.Exchange(oidc.ClientContext(r.Context(), s.oidc.httpClient), r.URL.Query().Get("code"), oauth2.VerifierOption(loginState.Verifier))
	if err != nil {
		http.Error(w, "Pocket ID code exchange failed", http.StatusUnauthorized)
		return
	}
	rawID, ok := token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "Pocket ID did not return an ID token", http.StatusUnauthorized)
		return
	}
	idToken, err := verifier.Verify(oidc.ClientContext(r.Context(), s.oidc.httpClient), rawID)
	if err != nil {
		http.Error(w, "Pocket ID token verification failed", http.StatusUnauthorized)
		return
	}
	var claims struct {
		Nonce             string `json:"nonce"`
		PreferredUsername string `json:"preferred_username"`
		Name              string `json:"name"`
		Email             string `json:"email"`
	}
	if err := idToken.Claims(&claims); err != nil || claims.Nonce != loginState.Nonce || strings.TrimSpace(idToken.Subject) == "" {
		http.Error(w, "Pocket ID token claims are invalid", http.StatusUnauthorized)
		return
	}
	username := claims.PreferredUsername
	if username == "" {
		username = strings.Split(claims.Email, "@")[0]
	}
	if username == "" {
		username = claims.Name
	}
	type result struct {
		token   string
		expires time.Time
	}
	desktopCode, desktopPolling := desktopPollCode(state)
	desktop := desktopPolling || strings.HasPrefix(state, desktopOIDCStatePrefix)
	sessionLifetime := 30 * 24 * time.Hour
	if desktop {
		sessionLifetime = 2 * time.Minute
	}
	value, err := s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		if _, err := data.ConsumeOIDCLoginState(r.Context(), state); err != nil {
			return nil, err
		}
		user, err := data.FindOrCreateOIDCUser(r.Context(), idToken.Subject, username)
		if err != nil {
			return nil, err
		}
		if desktopPolling {
			expires, err := data.CreateSessionWithToken(r.Context(), user.ID, desktopCode, sessionLifetime)
			return result{desktopCode, expires}, err
		}
		token, expires, err := data.CreateSession(r.Context(), user.ID, sessionLifetime)
		return result{token, expires}, err
	})
	if err != nil {
		http.Error(w, "unable to create Tintwire session", http.StatusServiceUnavailable)
		return
	}
	session := value.(result)
	http.SetCookie(w, &http.Cookie{Name: oidcStateCookieName, Path: "/api/v1/auth/oidc/callback", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	if desktopPolling {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<!doctype html><html><head><meta name=\"viewport\" content=\"width=device-width\"><title>Tintwire sign-in complete</title></head><body><main><h1>Sign-in complete</h1><p>You can close this window and return to Tintwire.</p></main></body></html>")
		return
	}
	if desktop {
		http.Redirect(w, r, "tintwire://auth?code="+url.QueryEscape(session.token), http.StatusFound)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: session.token, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, Expires: session.expires, MaxAge: int((30 * 24 * time.Hour).Seconds())})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) desktopSession(w http.ResponseWriter, r *http.Request) {
	if !s.controlLeaseValid(w, r) {
		return
	}
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	var request struct {
		Code string `json:"code"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || !validDesktopCode(request.Code) {
		http.Error(w, "valid desktop sign-in code is required", http.StatusBadRequest)
		return
	}
	type result struct {
		token   string
		expires time.Time
	}
	value, err := s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		token, expires, err := data.RotateSession(r.Context(), request.Code, 30*24*time.Hour)
		return result{token, expires}, err
	})
	if errors.Is(err, store.ErrInvalidCredentials) {
		http.Error(w, "desktop sign-in code is invalid or expired", http.StatusUnauthorized)
		return
	}
	if err != nil {
		http.Error(w, "unable to create desktop session", http.StatusServiceUnavailable)
		return
	}
	session := value.(result)
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: session.token, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, Expires: session.expires, MaxAge: int((30 * 24 * time.Hour).Seconds())})
	w.WriteHeader(http.StatusNoContent)
}
