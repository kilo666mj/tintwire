package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/kilo666mj/tintwire/internal/store"
)

func (s *Server) listSavedViews(w http.ResponseWriter, r *http.Request) {
	user, ok := s.inboxUser(r)
	if !ok {
		http.Error(w, "reader authentication is required", http.StatusConflict)
		return
	}
	views, err := s.store.ListSavedViews(r.Context(), user.ID)
	if err != nil {
		http.Error(w, "unable to list saved views", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"views": views})
}

func (s *Server) saveSavedView(w http.ResponseWriter, r *http.Request) {
	user, ok := s.inboxUser(r)
	if !ok {
		http.Error(w, "reader authentication is required", http.StatusConflict)
		return
	}
	var request savedViewRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, "invalid JSON payload", http.StatusBadRequest)
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" || len(request.Name) > 40 || len(request.Channels) == 0 || len(request.Channels) > 20 {
		http.Error(w, "name and 1 to 20 channels are required", http.StatusBadRequest)
		return
	}
	seen := map[string]bool{}
	channels := make([]string, 0, len(request.Channels))
	for _, channel := range request.Channels {
		channel = strings.TrimSpace(channel)
		if channel != "" && !seen[channel] {
			channels = append(channels, channel)
			seen[channel] = true
		}
	}
	if len(channels) == 0 {
		http.Error(w, "at least one channel is required", http.StatusBadRequest)
		return
	}
	if request.State != "" && request.State != "received" && request.State != "firing" && request.State != "acknowledged" && request.State != "resolved" && request.State != "dismissed" {
		http.Error(w, "unsupported state filter", http.StatusBadRequest)
		return
	}
	if request.Severity != "" && request.Severity != "info" && request.Severity != "warning" && request.Severity != "critical" && request.Severity != "success" {
		http.Error(w, "unsupported severity filter", http.StatusBadRequest)
		return
	}
	view, err := s.store.SaveSavedView(r.Context(), user.ID, store.SavedView{Name: request.Name, Channels: channels, Search: strings.TrimSpace(request.Search), State: request.State, Severity: request.Severity, Unread: request.Unread})
	if err != nil {
		http.Error(w, "unable to save view", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func (s *Server) deleteSavedView(w http.ResponseWriter, r *http.Request) {
	user, ok := s.inboxUser(r)
	if !ok {
		http.Error(w, "reader authentication is required", http.StatusConflict)
		return
	}
	if err := s.store.DeleteSavedView(r.Context(), user.ID, r.PathValue("id")); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "saved view not found", http.StatusNotFound)
			return
		}
		http.Error(w, "unable to delete saved view", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
