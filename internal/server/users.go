package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/kilo666mj/tintwire/internal/store"
)

func adminActor(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	actor, ok := r.Context().Value(userContextKey{}).(store.User)
	if !ok || !actor.IsAdmin {
		http.Error(w, "administrator access is required", http.StatusForbidden)
		return store.User{}, false
	}
	return actor, true
}

func (s *Server) listManagedUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := adminActor(w, r); !ok {
		return
	}
	users, err := s.store.ListManagedUsers(r.Context())
	if err != nil {
		http.Error(w, "unable to list users", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

func (s *Server) updateManagedUser(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	actor, ok := adminActor(w, r)
	if !ok {
		return
	}
	var request struct {
		IsAdmin  *bool  `json:"is_admin"`
		Disabled *bool  `json:"disabled"`
		Password string `json:"password"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid user update", http.StatusBadRequest)
		return
	}
	changes := 0
	if request.IsAdmin != nil {
		changes++
	}
	if request.Disabled != nil {
		changes++
	}
	if request.Password != "" {
		changes++
	}
	if changes != 1 {
		http.Error(w, "exactly one user update is required", http.StatusBadRequest)
		return
	}
	_, err := s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		if request.IsAdmin != nil {
			if err := data.SetManagedUserAdmin(r.Context(), actor.ID, r.PathValue("id"), *request.IsAdmin); err != nil {
				return nil, err
			}
		}
		if request.Disabled != nil {
			if err := data.SetManagedUserDisabled(r.Context(), actor.ID, r.PathValue("id"), *request.Disabled); err != nil {
				return nil, err
			}
		}
		if request.Password != "" {
			if err := data.ResetManagedUserPassword(r.Context(), actor.ID, r.PathValue("id"), request.Password); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	if err != nil {
		if errors.Is(err, store.ErrForbidden) || errors.Is(err, store.ErrInvalidCredentials) {
			http.Error(w, err.Error(), http.StatusForbidden)
		} else {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"updated": true})
}

func (s *Server) revokeManagedUserSessions(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	actor, ok := adminActor(w, r)
	if !ok {
		return
	}
	_, err := s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		return nil, data.RevokeManagedUserSessions(r.Context(), actor.ID, r.PathValue("id"))
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) updateManagedUserMembership(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	actor, ok := adminActor(w, r)
	if !ok {
		return
	}
	var request struct {
		Role string `json:"role"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid membership", http.StatusBadRequest)
		return
	}
	if request.Role != "" && request.Role != "viewer" && request.Role != "operator" && request.Role != "channel_admin" {
		http.Error(w, "invalid membership role", http.StatusBadRequest)
		return
	}
	_, err := s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		if request.Role == "" {
			return nil, data.RemoveChannelMember(r.Context(), actor.ID, r.PathValue("channel"), r.PathValue("id"))
		}
		return nil, data.SetChannelMemberByID(r.Context(), actor.ID, r.PathValue("channel"), r.PathValue("id"), request.Role)
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"role": request.Role})
}
