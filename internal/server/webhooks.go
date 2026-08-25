package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/kilo666mj/tintwire/internal/store"
)

type createWebhookRequest struct {
	ChannelID     string `json:"channel_id"`
	ChannelLocked *bool  `json:"channel_locked"`
}

type updateWebhookRequest struct {
	ChannelLocked *bool `json:"channel_locked"`
}

func (s *Server) listWebhooks(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	webhooks, err := s.store.ListWebhooks(r.Context())
	if err != nil {
		slog.Error("list webhooks", "error", err)
		http.Error(w, "unable to list webhooks", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": webhooks})
}

func (s *Server) createWebhook(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var request createWebhookRequest
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(&struct{}{}) != io.EOF || request.ChannelID == "" {
		http.Error(w, "invalid webhook", http.StatusBadRequest)
		return
	}
	channelLocked := false
	if request.ChannelLocked != nil {
		channelLocked = *request.ChannelLocked
	}
	value, err := s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		webhook, token, err := data.CreateWebhook(r.Context(), request.ChannelID, channelLocked)
		return struct {
			webhook store.Webhook
			token   string
		}{webhook, token}, err
	})
	if errors.Is(err, store.ErrForbidden) {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("create webhook", "error", err)
		http.Error(w, "unable to create webhook", http.StatusInternalServerError)
		return
	}
	created := value.(struct {
		webhook store.Webhook
		token   string
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"webhook": created.webhook,
		"token":   created.token,
		"path":    "/hooks/" + created.token,
	})
}

func (s *Server) updateWebhook(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var request updateWebhookRequest
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(&struct{}{}) != io.EOF || request.ChannelLocked == nil {
		http.Error(w, "invalid webhook update", http.StatusBadRequest)
		return
	}
	_, err := s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		return nil, data.SetWebhookChannelLocked(r.Context(), r.PathValue("id"), *request.ChannelLocked)
	})
	if errors.Is(err, store.ErrWebhookNotFound) {
		http.Error(w, "webhook not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("update webhook", "error", err)
		http.Error(w, "unable to update webhook", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) revokeWebhook(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	_, err := s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		return nil, data.RevokeWebhook(r.Context(), r.PathValue("id"))
	})
	if errors.Is(err, store.ErrWebhookNotFound) {
		http.Error(w, "webhook not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("revoke webhook", "error", err)
		http.Error(w, "unable to revoke webhook", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) duplicateWebhook(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	value, err := s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		webhook, token, err := data.DuplicateWebhook(r.Context(), r.PathValue("id"))
		return struct {
			webhook store.Webhook
			token   string
		}{webhook, token}, err
	})
	if errors.Is(err, store.ErrWebhookNotFound) {
		http.Error(w, "webhook not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("create additional webhook URL", "error", err)
		http.Error(w, "unable to create webhook URL", http.StatusInternalServerError)
		return
	}
	created := value.(struct {
		webhook store.Webhook
		token   string
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"webhook": created.webhook,
		"token":   created.token,
		"path":    "/hooks/" + created.token,
	})
}
