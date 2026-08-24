package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/kilo666mj/tintwire/internal/store"
)

type createChannelMessageRequest struct {
	Text           string `json:"text"`
	ParentID       string `json:"parent_id"`
	IdempotencyKey string `json:"idempotency_key"`
}

// createChannelMessage stores a human-authored message in the selected channel.
// The channel and actor are derived from authenticated state; the client cannot
// post into an unreadable channel by supplying an arbitrary id. A leading slash
// is reserved for the package-02 command path and is rejected here so ordinary
// text cannot accidentally be treated as a command.
func (s *Server) createChannelMessage(w http.ResponseWriter, r *http.Request) {
	user, _ := s.inboxUser(r)
	channelID := r.PathValue("id")
	var request createChannelMessageRequest
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid message", http.StatusBadRequest)
		return
	}
	request.Text = strings.TrimSpace(request.Text)
	if strings.HasPrefix(request.Text, "/") {
		http.Error(w, "slash commands are dispatched by the command path", http.StatusBadRequest)
		return
	}
	if request.Text == "" || len(request.Text) > 4000 {
		http.Error(w, "message text is required and must be at most 4000 characters", http.StatusBadRequest)
		return
	}
	message, err := s.store.CreateChannelMessage(r.Context(), user, store.CreateMessageInput{
		ChannelID:      channelID,
		Text:           request.Text,
		ParentID:       strings.TrimSpace(request.ParentID),
		IdempotencyKey: strings.TrimSpace(request.IdempotencyKey),
	})
	if errors.Is(err, store.ErrForbidden) {
		http.Error(w, "channel access is required", http.StatusForbidden)
		return
	}
	if errors.Is(err, store.ErrNotificationNotFound) {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	if errors.Is(err, store.ErrMessageNotFound) {
		http.Error(w, "parent message not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("create channel message", "error", err)
		http.Error(w, "unable to create message", http.StatusInternalServerError)
		return
	}
	s.publishMessage(message.ID)
	if s.push != nil {
		go s.push.deliverMessage(message)
	}
	writeJSON(w, http.StatusCreated, message)
}

// getChannelMessage resolves a stable message deep link while preserving the
// channel's current visibility and membership boundary.
func (s *Server) getChannelMessage(w http.ResponseWriter, r *http.Request) {
	user, _ := s.inboxUser(r)
	message, err := s.store.ChannelMessageByID(r.Context(), user, r.PathValue("messageID"))
	if errors.Is(err, store.ErrForbidden) {
		http.Error(w, "channel access is required", http.StatusForbidden)
		return
	}
	if errors.Is(err, store.ErrMessageNotFound) {
		http.Error(w, "message not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("get channel message", "error", err)
		http.Error(w, "unable to get message", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, message)
}

// listChannelTimeline returns a stable, ordered page of the merged channel
// timeline (notification cards and human messages), enforcing read
// authorization and exposing a cursor for subsequent pages.
func (s *Server) listChannelTimeline(w http.ResponseWriter, r *http.Request) {
	user, _ := s.inboxUser(r)
	channelID := r.PathValue("id")
	limit := 100
	if value := r.URL.Query().Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 200 {
			http.Error(w, "limit must be between 1 and 200", http.StatusBadRequest)
			return
		}
		limit = parsed
	}
	var beforeAt int64
	var beforeID string
	if cursor := r.URL.Query().Get("before"); cursor != "" {
		var ok bool
		beforeAt, beforeID, ok = decodeNotificationCursor(cursor)
		if !ok {
			http.Error(w, "invalid timeline cursor", http.StatusBadRequest)
			return
		}
	}
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(search) > 200 {
		http.Error(w, "search must be at most 200 characters", http.StatusBadRequest)
		return
	}
	state := r.URL.Query().Get("state")
	if state != "" && state != "received" && state != "firing" && state != "acknowledged" && state != "resolved" && state != "dismissed" {
		http.Error(w, "unsupported state filter", http.StatusBadRequest)
		return
	}
	items, hasMore, err := s.store.ListChannelTimelineFiltered(r.Context(), user, channelID, search, state, limit, beforeAt, beforeID)
	if errors.Is(err, store.ErrForbidden) {
		http.Error(w, "channel access is required", http.StatusForbidden)
		return
	}
	if errors.Is(err, store.ErrNotificationNotFound) {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("list channel timeline", "error", err)
		http.Error(w, "unable to list timeline", http.StatusInternalServerError)
		return
	}
	enrichAndSanitizeCommandItems(items)
	nextCursor := ""
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		nextCursor = encodeNotificationCursor(last.CreatedAtMilli, last.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": nextCursor})
}

// listChannelThread returns the replies, in chronological order, attached to a
// thread root message in a channel the actor can read.
func (s *Server) listChannelThread(w http.ResponseWriter, r *http.Request) {
	user, _ := s.inboxUser(r)
	replies, err := s.store.ListChannelThread(r.Context(), user, r.PathValue("id"), r.PathValue("rootID"))
	if errors.Is(err, store.ErrForbidden) {
		http.Error(w, "channel access is required", http.StatusForbidden)
		return
	}
	if errors.Is(err, store.ErrMessageNotFound) {
		http.Error(w, "thread not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("list channel thread", "error", err)
		http.Error(w, "unable to list thread", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"replies": replies})
}

// enrichAndSanitizeCommandItems parses each command response payload into safe
// display fields (username/icon overrides, props, sanitized attachments) and
// clears the raw payload so no server-managed action context leaks to clients.
func enrichAndSanitizeCommandItems(items []store.TimelineItem) {
	for i := range items {
		if items[i].Kind != "command" || items[i].Command == nil {
			continue
		}
		item := items[i].Command
		if len(item.Payload) == 0 {
			item.Payload = nil
			continue
		}
		var decoded struct {
			Username    string          `json:"username"`
			IconURL     string          `json:"icon_url"`
			Props       json.RawMessage `json:"props"`
			Attachments json.RawMessage `json:"attachments"`
		}
		if json.Unmarshal(item.Payload, &decoded) == nil {
			if decoded.Username != "" {
				item.Username = decoded.Username
			}
			if decoded.IconURL != "" {
				item.AuthorIcon = decoded.IconURL
			}
			item.Props = decoded.Props
			item.Attachments = sanitizeCommandAttachments(decoded.Attachments)
		}
		item.Payload = nil
	}
}

// sanitizeCommandAttachments strips Mattermost integration callback secrets and
// native HTTP action context from command-response attachments while preserving
// safe links and content fields.
func sanitizeCommandAttachments(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var attachments []map[string]any
	if json.Unmarshal(raw, &attachments) != nil {
		return raw
	}
	changed := false
	for _, attachment := range attachments {
		actions, ok := attachment["actions"].([]any)
		if !ok {
			continue
		}
		for _, rawAction := range actions {
			action, ok := rawAction.(map[string]any)
			if !ok {
				continue
			}
			if _, ok := action["integration"]; ok {
				delete(action, "integration")
				delete(action, "context")
				delete(action, "context_cipher")
				action["executable"] = false
				changed = true
			}
			if t, _ := action["type"].(string); t == "http" {
				delete(action, "context")
				delete(action, "context_cipher")
				delete(action, "target")
				delete(action, "url")
				changed = true
			}
			if value, ok := action["url"].(string); ok && value != "" && !validHTTPURL(value) {
				delete(action, "url")
				changed = true
			}
		}
	}
	if !changed {
		return raw
	}
	if value, err := json.Marshal(attachments); err == nil {
		return value
	}
	return raw
}
