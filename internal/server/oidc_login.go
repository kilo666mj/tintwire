package server

import (
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kilo666mj/oidcrp"
	"github.com/kilo666mj/tintwire/internal/store"
)

const oidcStateCookieName = "tintwire_oidc_state"
const oidcDesktopConfirmationCookieName = "tintwire_oidc_desktop_confirmation"

type oidcLoginService struct {
	*oidcrp.Service
}

type tintwireOIDCSessions struct {
	server *Server
}

type oidcSessionResult struct {
	token   string
	expires time.Time
}

func newOIDCLoginService(server *Server, issuer, clientID, redirectURL string, publicURL *url.URL) (*oidcLoginService, error) {
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
	redirect, err := url.Parse(redirectURL)
	if err != nil || redirect.Scheme != "https" || redirect.Host == "" || redirect.RawQuery != "" || redirect.Fragment != "" {
		return nil, errors.New("OIDC redirect URL must be an absolute HTTPS URL")
	}
	sessions := &tintwireOIDCSessions{server: server}
	service := oidcrp.New(oidcrp.Config{
		Issuer:                 issuer,
		ClientID:               clientID,
		RedirectURL:            redirectURL,
		StateCookieName:        oidcStateCookieName,
		LoginPath:              "/",
		LoginStartPath:         "/api/v1/auth/oidc/start",
		CallbackPath:           "/api/v1/auth/oidc/callback",
		SuccessPath:            "/",
		DesktopHandoffParam:    "desktop",
		DesktopSuccessPath:     "/api/v1/auth/desktop/confirm",
		ValidateDesktopHandoff: validDesktopCode,
	}, sessions)
	return &oidcLoginService{Service: service}, nil
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

func (s *Server) oidcStart(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		http.NotFound(w, r)
		return
	}
	s.oidc.LoginStart(w, r)
}

func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		http.NotFound(w, r)
		return
	}
	s.oidc.Callback(w, r)
}

func (o *tintwireOIDCSessions) Valid(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	_, err = o.server.store.UserForSession(r.Context(), cookie.Value)
	return err == nil
}

func oidcIdentityUsername(identity oidcrp.Identity) string {
	if local, _, found := strings.Cut(strings.TrimSpace(identity.Email), "@"); found && local != "" {
		return local
	}
	return identity.Subject
}

func (o *tintwireOIDCSessions) Issue(w http.ResponseWriter, r *http.Request, identity oidcrp.Identity) error {
	value, err := o.server.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		user, err := data.FindOrCreateOIDCUser(r.Context(), identity.Subject, oidcIdentityUsername(identity))
		if err != nil {
			return nil, err
		}
		token, expires, err := data.CreateSession(r.Context(), user.ID, 30*24*time.Hour)
		return oidcSessionResult{token: token, expires: expires}, err
	})
	if err != nil {
		return err
	}
	session := value.(oidcSessionResult)
	o.server.setSessionCookie(w, r, session.token, session.expires)
	return nil
}

func (o *tintwireOIDCSessions) Clear(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Path: "/", HttpOnly: true, Secure: o.server.secureCookies(r), SameSite: http.SameSiteStrictMode, MaxAge: -1})
}

func (o *tintwireOIDCSessions) IssueDesktop(w http.ResponseWriter, r *http.Request, identity oidcrp.Identity, handoff string) error {
	confirmation, err := oidcrp.NewDesktopConfirmation(handoff)
	if err != nil {
		return err
	}
	if _, err := o.server.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		user, err := data.FindOrCreateOIDCUser(r.Context(), identity.Subject, oidcIdentityUsername(identity))
		if err != nil {
			return nil, err
		}
		return nil, data.CreateOIDCDesktopConfirmation(r.Context(), user.ID, handoff, confirmation.BrowserSecret, confirmation.VerificationCode, 10*time.Minute)
	}); err != nil {
		return err
	}
	// The confirmation page is reached by an identity-provider redirect, so the
	// browser attributes that navigation to the provider's site. A Strict cookie
	// would be withheld there and every confirmation would fail as expired. Lax
	// still withholds the cookie from cross-site approve and cancel POSTs, which
	// those handlers additionally guard with an origin check.
	http.SetCookie(w, &http.Cookie{
		Name:     oidcDesktopConfirmationCookieName,
		Value:    confirmation.BrowserSecret,
		Path:     "/api/v1/auth/desktop",
		HttpOnly: true,
		Secure:   o.server.secureCookies(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((10 * time.Minute).Seconds()),
	})
	return nil
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: token, Path: "/", HttpOnly: true, Secure: s.secureCookies(r), SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: int((30 * 24 * time.Hour).Seconds())})
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
	value, err := s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		token, expires, err := data.ExchangeOIDCDesktopHandoff(r.Context(), request.Code, 30*24*time.Hour)
		return oidcSessionResult{token: token, expires: expires}, err
	})
	if errors.Is(err, store.ErrOIDCDesktopCancelled) {
		http.Error(w, "desktop sign-in was cancelled", http.StatusGone)
		return
	}
	if errors.Is(err, store.ErrInvalidCredentials) {
		http.Error(w, "desktop sign-in is pending, invalid, or expired", http.StatusUnauthorized)
		return
	}
	if err != nil {
		http.Error(w, "unable to create desktop session", http.StatusServiceUnavailable)
		return
	}
	session := value.(oidcSessionResult)
	s.setSessionCookie(w, r, session.token, session.expires)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) desktopConfirmation(w http.ResponseWriter, r *http.Request) {
	if !s.controlLeaseValid(w, r) {
		return
	}
	secret, err := desktopConfirmationSecret(r)
	if err != nil {
		http.Error(w, "desktop sign-in confirmation is invalid or expired", http.StatusGone)
		return
	}
	confirmation, err := s.store.OIDCDesktopConfirmation(r.Context(), secret)
	if err != nil || confirmation.Status != "pending" {
		http.Error(w, "desktop sign-in confirmation is invalid or expired", http.StatusGone)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, `<!doctype html><html><head><meta name="viewport" content="width=device-width"><title>Confirm Tintwire sign-in</title><style>body{background:#0d131b;color:#edf2f7;font:16px system-ui;margin:0;padding:2rem}main{margin:10vh auto;max-width:34rem;background:#151d28;border:1px solid #344256;border-radius:14px;padding:2rem}.code{font:700 2rem ui-monospace,monospace;letter-spacing:.12em}.actions{display:flex;gap:1rem}button{border:1px solid #7191f0;border-radius:8px;background:#4169e1;color:white;font:inherit;font-weight:700;padding:.7rem 1rem}.cancel{background:transparent;border-color:#738096}</style></head><body><main><h1>Confirm desktop sign-in</h1><p>Compare this code with the one shown in the Tintwire desktop app:</p><p class="code">`+html.EscapeString(confirmation.VerificationCode)+`</p><p>Approve only if the codes match and you started this sign-in.</p><div class="actions"><form method="post" action="/api/v1/auth/desktop/confirm"><button type="submit">Approve sign-in</button></form><form method="post" action="/api/v1/auth/desktop/cancel"><button class="cancel" type="submit">Cancel</button></form></div></main></body></html>`)
}

func (s *Server) approveDesktopConfirmation(w http.ResponseWriter, r *http.Request) {
	s.finishDesktopConfirmation(w, r, true)
}

func (s *Server) cancelDesktopConfirmation(w http.ResponseWriter, r *http.Request) {
	s.finishDesktopConfirmation(w, r, false)
}

func (s *Server) finishDesktopConfirmation(w http.ResponseWriter, r *http.Request, approve bool) {
	if !s.controlLeaseValid(w, r) {
		return
	}
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	secret, err := desktopConfirmationSecret(r)
	if err != nil {
		http.Error(w, "desktop sign-in confirmation is invalid or expired", http.StatusGone)
		return
	}
	_, err = s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		if approve {
			return nil, data.ApproveOIDCDesktopConfirmation(r.Context(), secret)
		}
		return nil, data.CancelOIDCDesktopConfirmation(r.Context(), secret)
	})
	if errors.Is(err, store.ErrInvalidCredentials) {
		http.Error(w, "desktop sign-in confirmation is invalid or expired", http.StatusGone)
		return
	}
	if err != nil {
		http.Error(w, "unable to update desktop sign-in", http.StatusServiceUnavailable)
		return
	}
	clearDesktopConfirmationCookie(w, r, s)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	message := "Sign-in approved. You can close this window and return to Tintwire."
	if !approve {
		message = "Sign-in cancelled. You can close this window."
	}
	_, _ = io.WriteString(w, `<!doctype html><html><head><meta name="viewport" content="width=device-width"><title>Tintwire sign-in</title></head><body><main><h1>`+html.EscapeString(message)+`</h1></main></body></html>`)
}

func desktopConfirmationSecret(r *http.Request) (string, error) {
	cookie, err := r.Cookie(oidcDesktopConfirmationCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return "", store.ErrInvalidCredentials
	}
	return cookie.Value, nil
}

func clearDesktopConfirmationCookie(w http.ResponseWriter, r *http.Request, s *Server) {
	http.SetCookie(w, &http.Cookie{Name: oidcDesktopConfirmationCookieName, Path: "/api/v1/auth/desktop", HttpOnly: true, Secure: s.secureCookies(r), SameSite: http.SameSiteLaxMode, MaxAge: -1})
}
