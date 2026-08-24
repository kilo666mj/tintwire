package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/kilo666mj/tintwire/internal/store"
)

const sessionCookieName = "tintwire_session"

type userContextKey struct{}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.controlLeaseValid(w, r) {
		return
	}
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	var request loginRequest
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || strings.TrimSpace(request.Username) == "" || request.Password == "" {
		http.Error(w, "username and password are required", http.StatusBadRequest)
		return
	}
	clientIP := loginClientIP(r)
	username := strings.ToLower(strings.TrimSpace(request.Username))
	loginKey := "login:user:" + clientIP + ":" + username
	addressKey := "login:address:" + clientIP
	if s.limiter.blocked(loginKey, 8) || s.limiter.blocked(addressKey, 40) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many failed sign-in attempts", http.StatusTooManyRequests)
		return
	}
	user, err := s.store.AuthenticateUser(r.Context(), request.Username, request.Password)
	if errors.Is(err, store.ErrInvalidCredentials) {
		s.limiter.recordFailure(loginKey)
		s.limiter.recordFailure(addressKey)
		http.Error(w, "invalid username or password", http.StatusUnauthorized)
		return
	}
	s.limiter.clear(loginKey)
	if err != nil {
		http.Error(w, "unable to create session", http.StatusInternalServerError)
		return
	}
	type sessionResult struct {
		token   string
		expires time.Time
	}
	result, err := s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		token, expires, err := data.CreateSession(r.Context(), user.ID, 30*24*time.Hour)
		return sessionResult{token: token, expires: expires}, err
	})
	if err != nil {
		http.Error(w, "unable to create session", http.StatusInternalServerError)
		return
	}
	session := result.(sessionResult)
	token, expires := session.token, session.expires
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: token, Path: "/", HttpOnly: true, Secure: s.secureCookies(r), SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: int((30 * 24 * time.Hour).Seconds())})
	writeJSON(w, http.StatusOK, map[string]string{"username": user.Username})
}

// loginClientIP accepts the reference proxy's dedicated client-address header
// only across the loopback hop. Direct LAN clients therefore cannot spoof a
// different rate-limit bucket with a generic forwarding header.
func loginClientIP(r *http.Request) string {
	clientIP := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		clientIP = host
	}
	peer := net.ParseIP(clientIP)
	if peer == nil || !peer.IsLoopback() {
		return clientIP
	}
	forwarded := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Tintwire-Client-IP")))
	if forwarded == nil {
		return clientIP
	}
	return forwarded.String()
}

func (s *Server) sessionStatus(w http.ResponseWriter, r *http.Request) {
	if !s.controlLeaseValid(w, r) {
		return
	}
	if !s.authRequired {
		writeJSON(w, http.StatusOK, map[string]any{"auth_required": false, "authenticated": true, "oidc_enabled": s.oidc != nil})
		return
	}
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"auth_required": true, "authenticated": false, "oidc_enabled": s.oidc != nil})
		return
	}
	user, err := s.store.UserForSession(r.Context(), cookie.Value)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"auth_required": true, "authenticated": false, "oidc_enabled": s.oidc != nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"auth_required": true, "authenticated": true, "user_id": user.ID, "username": user.Username, "is_admin": user.IsAdmin, "oidc_enabled": s.oidc != nil})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		if _, err := s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
			return nil, data.DeleteSession(r.Context(), cookie.Value)
		}); err != nil {
			http.Error(w, "unable to revoke session", http.StatusServiceUnavailable)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Path: "/", HttpOnly: true, Secure: s.secureCookies(r), SameSite: http.SameSiteStrictMode, MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) requireReader(next http.HandlerFunc) http.HandlerFunc {
	if !s.authRequired {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.controlLeaseValid(w, r) {
			return
		}
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		user, err := s.store.UserForSession(r.Context(), cookie.Value)
		if err != nil {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userContextKey{}, user)))
	}
}

func (s *Server) controlLeaseValid(w http.ResponseWriter, r *http.Request) bool {
	if s.consensus != nil {
		if !s.consensus.Healthy(30 * time.Second) {
			http.Error(w, "security control quorum is unavailable", http.StatusServiceUnavailable)
			return false
		}
		return true
	}
	valid, err := s.store.ControlLeaseValid(r.Context())
	if err != nil || !valid {
		http.Error(w, "security control lease is unavailable", http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (s *Server) requireControlAuthority(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.consensus != nil {
			if !s.consensus.IsLeader() {
				leaderID, _ := s.consensus.Leader()
				if leaderID != "" && leaderID != s.consensus.NodeID() {
					w.Header().Set("Tintwire-Control-Leader", leaderID)
					if s.controlProxyPort != "" {
						target := &url.URL{Scheme: "http", Host: net.JoinHostPort(leaderID, s.controlProxyPort)}
						proxy := httputil.NewSingleHostReverseProxy(target)
						proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
							slog.Warn("forward control request", "leader", leaderID, "error", err)
							http.Error(w, "control leader is unavailable", http.StatusServiceUnavailable)
						}
						proxy.ServeHTTP(w, r)
						return
					}
				}
				http.Error(w, "security control changes must use the control leader", http.StatusServiceUnavailable)
				return
			}
			next(w, r)
			return
		}
		if !s.store.CanMutateControlPlane() {
			http.Error(w, "security control changes must use the control authority", http.StatusServiceUnavailable)
			return
		}
		next(w, r)
	}
}

func (s *Server) mutateControl(ctx context.Context, mutation func(*store.Store) (any, error)) (any, error) {
	if s.consensus != nil {
		return s.consensus.Mutate(ctx, mutation)
	}
	return mutation(s.store)
}

func (s *Server) requireControlLease(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.controlLeaseValid(w, r) {
			next(w, r)
		}
	}
}
