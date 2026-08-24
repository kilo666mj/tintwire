package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kilo666mj/tintwire/internal/store"
)

// dispatchSlashCommand sends the Mattermost-compatible slash-command form to the
// command's configured target, honoring the method, URL safety policy, and
// bounded response size. It returns the raw response body or a description of
// the delivery failure.
func (s *Server) dispatchSlashCommand(ctx context.Context, command store.SlashCommand, form url.Values) ([]byte, error) {
	var callback *http.Request
	var err error
	if command.Method == "GET" {
		parsed, _ := url.Parse(command.URL)
		query := parsed.Query()
		for key, values := range form {
			query[key] = values
		}
		parsed.RawQuery = query.Encode()
		callback, err = http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	} else {
		callback, err = http.NewRequestWithContext(ctx, http.MethodPost, command.URL, strings.NewReader(form.Encode()))
	}
	if err != nil {
		return nil, errors.New("command request could not be created")
	}
	if command.Method == "POST" {
		callback.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	response, err := actionHTTPClient(command.AllowPrivate).Do(callback)
	if err != nil {
		return nil, errors.New("command target could not be reached")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxActionResponse+1))
	if err != nil || len(body) > maxActionResponse {
		return nil, errors.New("command response was too large")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, errors.New("command target rejected the request")
	}
	return body, nil
}

type channelCommandRequest struct {
	Text string `json:"text"`
}

// executeChannelCommand runs an imported slash command against the selected
// channel's composer. The actor and channel are derived from authenticated state
// and the path; the client only supplies the `/trigger arguments` text. Commands
// are resolved team-scoped from the channel's compatibility alias, so no
// separate team/channel fields are required. Responses are recorded durably and
// rendered in the channel timeline with ephemeral/in-channel visibility.
func (s *Server) executeChannelCommand(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	actor, ok := r.Context().Value(userContextKey{}).(store.User)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	channelID := r.PathValue("id")
	readable, err := s.store.ChannelReadable(r.Context(), actor, channelID)
	if errors.Is(err, store.ErrNotificationNotFound) {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("channel command read", "error", err)
		http.Error(w, "unable to resolve channel", http.StatusInternalServerError)
		return
	}
	if !readable {
		http.Error(w, "channel access is required", http.StatusForbidden)
		return
	}
	var request channelCommandRequest
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid command", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(request.Text)
	if !strings.HasPrefix(text, "/") {
		http.Error(w, "text must begin with '/'", http.StatusBadRequest)
		return
	}
	rest := strings.TrimSpace(strings.TrimPrefix(text, "/"))
	trigger := rest
	commandText := ""
	if index := strings.IndexByte(rest, ' '); index >= 0 {
		trigger = rest[:index]
		commandText = strings.TrimSpace(rest[index+1:])
	}
	if trigger == "" {
		http.Error(w, "command name is required", http.StatusBadRequest)
		return
	}
	trigger = strings.ToLower(trigger)
	command, err := s.store.SlashCommandForChannel(r.Context(), channelID, trigger, actor)
	if errors.Is(err, store.ErrNotificationNotFound) {
		http.Error(w, "command not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "unable to resolve command", http.StatusInternalServerError)
		return
	}
	if err := validateActionTargetURL(command.URL, command.AllowPrivate); err != nil {
		http.Error(w, "command target is blocked", http.StatusBadGateway)
		return
	}
	operationKey := r.Header.Get("Idempotency-Key")
	if !operationKeyPattern.MatchString(operationKey) {
		http.Error(w, "a valid Idempotency-Key is required", http.StatusBadRequest)
		return
	}
	channelName, err := s.store.ChannelNameByID(r.Context(), channelID)
	if errors.Is(err, store.ErrNotificationNotFound) {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "unable to resolve channel", http.StatusInternalServerError)
		return
	}
	token, err := s.actions.decrypt(command.TokenCipher)
	if err != nil {
		http.Error(w, "command credentials are unavailable", http.StatusServiceUnavailable)
		return
	}
	capability, err := randomCapability()
	if err != nil {
		http.Error(w, "unable to create response URL", http.StatusInternalServerError)
		return
	}
	execution, fresh, err := s.store.CreateSlashCommandExecution(r.Context(), store.SlashCommandExecution{
		CommandID: command.ID, ChannelID: channelID, UserID: actor.ID, Text: commandText,
		ResponseTokenHash: capabilityHash(capability), RequestKey: operationKey,
		ExpiresAt: time.Now().UTC().Add(30 * time.Minute),
	})
	if errors.Is(err, store.ErrImportConflict) {
		http.Error(w, "idempotency key was used for another command", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "unable to record command", http.StatusInternalServerError)
		return
	}
	responses := func() []store.SlashCommandResponse {
		values, err := s.store.SlashCommandResponses(r.Context(), execution.ID, actor)
		if err != nil {
			return nil
		}
		return values
	}
	if !fresh {
		recorded := responses()
		if recorded == nil {
			http.Error(w, "unable to replay command result", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": execution.ID, "responses": recorded})
		return
	}
	responseOrigin := &url.URL{Scheme: "https", Host: r.Host}
	if s.publicURL != nil {
		responseOrigin.Scheme = s.publicURL.Scheme
		responseOrigin.Host = s.publicURL.Host
	} else if r.TLS == nil {
		responseOrigin.Scheme = "http"
	}
	responseOrigin.Path = "/hooks/commands/" + capability
	responseURL := responseOrigin.String()
	form := url.Values{
		"team_id":      {command.Team},
		"team_domain":  {command.Team},
		"channel_id":   {channelID},
		"channel_name": {channelName},
		"user_id":      {actor.ID},
		"user_name":    {actor.Username},
		"command":      {"/" + command.Trigger},
		"text":         {commandText},
		"token":        {token},
		"trigger_id":   {execution.ID},
		"response_url": {responseURL},
	}
	body, err := s.dispatchSlashCommand(r.Context(), command, form)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	values := normalizeSlashResponses(body)
	recorded := make([]store.SlashCommandResponse, 0, len(values))
	for _, value := range values {
		raw, _ := json.Marshal(value)
		item, err := s.store.AddSlashCommandResponse(r.Context(), execution.ID, value.ResponseType, value.Text, raw)
		if err != nil {
			http.Error(w, "unable to record command response", http.StatusInternalServerError)
			return
		}
		recorded = append(recorded, item)
		if s.push != nil {
			go s.push.deliverCommandResponse(item, channelName)
		}
	}
	s.publish(execution.ID)
	writeJSON(w, http.StatusOK, map[string]any{"id": execution.ID, "responses": recorded})
}
