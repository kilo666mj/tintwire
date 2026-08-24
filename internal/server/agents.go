package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/kilo666mj/tintwire/internal/store"
)

type agentContextKey struct{}

type createAgentRequest struct {
	Name         string `json:"name"`
	DisplayName  string `json:"display_name"`
	Description  string `json:"description"`
	OAuthSubject string `json:"oauth_subject"`
	IsAdmin      bool   `json:"is_admin"`
}

func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	var request createAgentRequest
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid agent", http.StatusBadRequest)
		return
	}
	result, err := s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		agent, token, err := data.CreateAgent(r.Context(), store.CreateAgentInput{
			Name: request.Name, DisplayName: request.DisplayName, Description: request.Description,
			OwnerUserID: actor.ID, IsAdmin: request.IsAdmin, OAuthSubject: request.OAuthSubject,
		})
		return struct {
			agent store.Agent
			token string
		}{agent, token}, err
	})
	if err != nil {
		if store.IsAlreadyExists(err) {
			http.Error(w, "agent already exists", http.StatusConflict)
			return
		}
		if strings.Contains(err.Error(), "agent name") || strings.Contains(err.Error(), "too long") {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		slog.Error("create agent", "error", err)
		http.Error(w, "unable to create agent", http.StatusInternalServerError)
		return
	}
	created := result.(struct {
		agent store.Agent
		token string
	})
	agent, token := created.agent, created.token
	agent.Owner = actor.Username
	writeJSON(w, http.StatusCreated, map[string]any{"agent": agent, "access_token": token})
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	agents, err := s.store.ListAgents(r.Context())
	if err != nil {
		slog.Error("list agents", "error", err)
		http.Error(w, "unable to list agents", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
}

func (s *Server) revokeAgent(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	_, err := s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		return nil, data.RevokeAgent(r.Context(), r.PathValue("name"))
	})
	if err != nil {
		if errors.Is(err, store.ErrAgentNotFound) {
			http.Error(w, "agent not found", http.StatusNotFound)
			return
		}
		slog.Error("revoke agent", "error", err)
		http.Error(w, "unable to revoke agent", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listAgentRuns(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	agent, err := s.store.AgentByName(r.Context(), r.PathValue("name"))
	if errors.Is(err, store.ErrAgentNotFound) {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("load agent", "error", err)
		http.Error(w, "unable to load agent", http.StatusInternalServerError)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	runs, err := s.store.ListAgentRuns(r.Context(), agent.ID, limit)
	if err != nil {
		slog.Error("list agent runs", "error", err)
		http.Error(w, "unable to list agent runs", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": agent, "runs": runs})
}

func (s *Server) agentRunEvents(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	events, err := s.store.ListAgentRunEvents(r.Context(), r.PathValue("id"))
	if err != nil {
		slog.Error("list agent run events", "error", err)
		http.Error(w, "unable to list run activity", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// requireAdmin resolves the authenticated administrator for browser-originated
// administration. Mutations keep the same origin and host requirements as every
// other browser mutation.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	if r.Method != http.MethodGet && !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return store.User{}, false
	}
	actor, ok := r.Context().Value(userContextKey{}).(store.User)
	if !ok || !actor.IsAdmin {
		http.Error(w, "installation administrator access is required", http.StatusForbidden)
		return store.User{}, false
	}
	return actor, true
}

// requireAgent authenticates an agent access token. Agent credentials are
// separate from reader sessions and channel publishing tokens, and browser
// cookies are never accepted here.
func (s *Server) requireAgent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.controlLeaseValid(w, r) {
			return
		}
		token, ok := bearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", s.agentAuthenticateChallenge(""))
			http.Error(w, "agent access token is required", http.StatusUnauthorized)
			return
		}
		agent, user, err := s.store.AgentForToken(r.Context(), token)
		if err != nil && s.oauth != nil {
			subject, verifyErr := s.oauth.verify(r.Context(), token)
			if verifyErr == nil {
				agent, user, err = s.store.AgentForOAuthSubject(r.Context(), subject)
			}
		}
		if err != nil {
			w.Header().Set("WWW-Authenticate", s.agentAuthenticateChallenge("invalid_token"))
			http.Error(w, "invalid agent access token", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), agentContextKey{}, agent)
		ctx = context.WithValue(ctx, userContextKey{}, user)
		next(w, r.WithContext(ctx))
	}
}

func (s *Server) agentAuthenticateChallenge(authenticationError string) string {
	challenge := `Bearer realm="tintwire-mcp"`
	if authenticationError != "" {
		challenge += `, error="` + authenticationError + `"`
	}
	if s.publicURL != nil {
		challenge += `, resource_metadata="` + strings.TrimSuffix(s.publicURL.String(), "/") + `/.well-known/oauth-protected-resource/mcp"`
	}
	return challenge
}
