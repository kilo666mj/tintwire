package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/kilo666mj/tintwire/internal/store"
)

var slashTriggerPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

type slashCommandImportRequest struct {
	Commands []struct {
		Team                    string `json:"team"`
		Trigger                 string `json:"trigger"`
		DisplayName             string `json:"display_name"`
		Description             string `json:"description"`
		Creator                 string `json:"creator"`
		Method                  string `json:"method"`
		URL                     string `json:"url"`
		Token                   string `json:"token"`
		AllowPrivate            bool   `json:"allow_private"`
		Autocomplete            bool   `json:"autocomplete"`
		AutocompleteHint        string `json:"autocomplete_hint"`
		AutocompleteDescription string `json:"autocomplete_description"`
		Username                string `json:"username"`
		IconURL                 string `json:"icon_url"`
	} `json:"commands"`
}

type slashCommandResponse struct {
	Text         string            `json:"text"`
	ResponseType string            `json:"response_type"`
	Attachments  json.RawMessage   `json:"attachments,omitempty"`
	Extra        []json.RawMessage `json:"extra_responses,omitempty"`
	ChannelID    string            `json:"channel_id,omitempty"`
	Username     string            `json:"username,omitempty"`
	IconURL      string            `json:"icon_url,omitempty"`
	Props        json.RawMessage   `json:"props,omitempty"`
	GotoLocation string            `json:"goto_location,omitempty"`
}

func (s *Server) importSlashCommands(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	actor, ok := r.Context().Value(userContextKey{}).(store.User)
	if !ok || !actor.IsAdmin {
		http.Error(w, "installation administrator access is required", http.StatusForbidden)
		return
	}
	var request slashCommandImportRequest
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || len(request.Commands) == 0 || len(request.Commands) > 1000 {
		http.Error(w, "invalid slash command import", http.StatusBadRequest)
		return
	}
	for index, item := range request.Commands {
		item.Team = strings.TrimSpace(item.Team)
		item.Trigger = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(item.Trigger)), "/")
		item.Method = strings.ToUpper(strings.TrimSpace(item.Method))
		if item.Team == "" || !slashTriggerPattern.MatchString(item.Trigger) || (item.Method != "GET" && item.Method != "POST") || validateActionTargetURL(item.URL, item.AllowPrivate) != nil {
			http.Error(w, "invalid slash command definition", http.StatusBadRequest)
			return
		}
		if item.Method == "GET" && item.Token != "" {
			slog.Warn("importing legacy GET slash command; target access logs may retain its token", "team", item.Team, "trigger", item.Trigger)
		}
		request.Commands[index] = item
	}
	result, err := s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		if err := s.actions.ensureStoredKey(r.Context(), data); err != nil {
			return nil, err
		}
		commands := make([]store.SlashCommand, 0, len(request.Commands))
		for _, item := range request.Commands {
			ciphertext, err := s.actions.encryptForStore(r.Context(), data, item.Token)
			if err != nil {
				return nil, err
			}
			tokenHash := sha256.Sum256([]byte(item.Token))
			commands = append(commands, store.SlashCommand{Team: item.Team, Trigger: item.Trigger, DisplayName: item.DisplayName, Description: item.Description, Creator: item.Creator, Method: item.Method, URL: item.URL, TokenCipher: ciphertext, TokenHash: tokenHash[:], AllowPrivate: item.AllowPrivate, Autocomplete: item.Autocomplete, AutocompleteHint: item.AutocompleteHint, AutocompleteDescription: item.AutocompleteDescription, Username: item.Username, IconURL: item.IconURL})
		}
		created, existing, err := data.ImportSlashCommands(r.Context(), commands)
		return [2]int{created, existing}, err
	})
	if errors.Is(err, store.ErrImportConflict) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if errors.Is(err, errActionEncryptionKeyRequired) || errors.Is(err, errInvalidActionEncryptionKey) {
		http.Error(w, "unable to protect command token", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		http.Error(w, "unable to import slash commands", http.StatusInternalServerError)
		return
	}
	counts := result.([2]int)
	created, existing := counts[0], counts[1]
	writeJSON(w, http.StatusOK, map[string]int{"created": created, "existing": existing})
}

func (s *Server) listSlashCommands(w http.ResponseWriter, r *http.Request) {
	actor, _ := r.Context().Value(userContextKey{}).(store.User)
	values, err := s.store.SlashCommands(r.Context(), r.URL.Query().Get("team"), actor)
	if err != nil {
		http.Error(w, "unable to list slash commands", http.StatusInternalServerError)
		return
	}
	result := make([]map[string]any, 0, len(values))
	for _, v := range values {
		result = append(result, map[string]any{"trigger": "/" + v.Trigger, "display_name": v.DisplayName, "description": v.Description, "autocomplete": v.Autocomplete, "autocomplete_hint": v.AutocompleteHint, "autocomplete_description": v.AutocompleteDescription})
	}
	writeJSON(w, http.StatusOK, map[string]any{"commands": result})
}

func randomCapability() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
func capabilityHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (s *Server) executeSlashCommand(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	actor, ok := r.Context().Value(userContextKey{}).(store.User)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	var request struct{ Team, Channel, Command, Text string }
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil {
		http.Error(w, "invalid command", http.StatusBadRequest)
		return
	}
	operationKey := r.Header.Get("Idempotency-Key")
	if !operationKeyPattern.MatchString(operationKey) {
		http.Error(w, "a valid Idempotency-Key is required", http.StatusBadRequest)
		return
	}
	trigger := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(request.Command)), "/")
	command, channelID, err := s.store.SlashCommandForActor(r.Context(), request.Team, trigger, request.Channel, actor)
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
	execution, fresh, err := s.store.CreateSlashCommandExecution(r.Context(), store.SlashCommandExecution{CommandID: command.ID, ChannelID: channelID, UserID: actor.ID, Text: request.Text, ResponseTokenHash: capabilityHash(capability), RequestKey: operationKey, ExpiresAt: time.Now().UTC().Add(30 * time.Minute)})
	if errors.Is(err, store.ErrImportConflict) {
		http.Error(w, "idempotency key was used for another command", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "unable to record command", http.StatusInternalServerError)
		return
	}
	if !fresh {
		responses, err := s.store.SlashCommandResponses(r.Context(), execution.ID, actor)
		if err != nil {
			http.Error(w, "unable to replay command result", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": execution.ID, "responses": responses})
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
	form := url.Values{"team_id": {command.Team}, "team_domain": {command.Team}, "channel_id": {channelID}, "channel_name": {request.Channel}, "user_id": {actor.ID}, "user_name": {actor.Username}, "command": {"/" + command.Trigger}, "text": {request.Text}, "token": {token}, "trigger_id": {execution.ID}, "response_url": {responseURL}}
	var callback *http.Request
	if command.Method == "GET" {
		parsed, _ := url.Parse(command.URL)
		query := parsed.Query()
		for key, values := range form {
			query[key] = values
		}
		parsed.RawQuery = query.Encode()
		callback, err = http.NewRequestWithContext(r.Context(), http.MethodGet, parsed.String(), nil)
	} else {
		callback, err = http.NewRequestWithContext(r.Context(), http.MethodPost, command.URL, strings.NewReader(form.Encode()))
	}
	if err != nil {
		http.Error(w, "command request could not be created", http.StatusBadGateway)
		return
	}
	if command.Method == "POST" {
		callback.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	response, err := actionHTTPClient(command.AllowPrivate).Do(callback)
	if err != nil {
		http.Error(w, "command target could not be reached", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxActionResponse+1))
	if err != nil || len(body) > maxActionResponse {
		http.Error(w, "command response was too large", http.StatusBadGateway)
		return
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		http.Error(w, "command target rejected the request", http.StatusBadGateway)
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
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": execution.ID, "responses": recorded})
}

func normalizeSlashResponses(body []byte) []slashCommandResponse {
	value := slashCommandResponse{ResponseType: "ephemeral"}
	if json.Unmarshal(body, &value) != nil {
		value.Text = strings.TrimSpace(string(body))
	}
	if value.ResponseType != "in_channel" {
		value.ResponseType = "ephemeral"
	}
	if value.GotoLocation != "" {
		parsed, err := url.Parse(value.GotoLocation)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			value.GotoLocation = ""
		}
	}
	result := []slashCommandResponse{value}
	for _, raw := range value.Extra {
		if len(result) >= 5 {
			break
		}
		var extra slashCommandResponse
		if json.Unmarshal(raw, &extra) == nil {
			if extra.ResponseType != "in_channel" {
				extra.ResponseType = "ephemeral"
			}
			result = append(result, extra)
		}
	}
	result[0].Extra = nil
	return result
}

func (s *Server) delayedSlashResponse(w http.ResponseWriter, r *http.Request) {
	execution, err := s.store.UseSlashResponseToken(r.Context(), capabilityHash(r.PathValue("token")))
	if errors.Is(err, store.ErrNotificationNotFound) {
		http.Error(w, "response URL not found", http.StatusNotFound)
		return
	}
	if errors.Is(err, store.ErrForbidden) {
		http.Error(w, "response URL expired or exhausted", http.StatusGone)
		return
	}
	if err != nil {
		http.Error(w, "unable to use response URL", http.StatusInternalServerError)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxActionResponse)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "invalid response", http.StatusBadRequest)
		return
	}
	values := normalizeSlashResponses(body)
	for _, value := range values {
		raw, _ := json.Marshal(value)
		item, err := s.store.AddSlashCommandResponse(r.Context(), execution.ID, value.ResponseType, value.Text, raw)
		if err != nil {
			http.Error(w, "unable to record response", http.StatusInternalServerError)
			return
		}
		if s.push != nil {
			if channelName, nameErr := s.store.ChannelNameByID(r.Context(), execution.ChannelID); nameErr == nil {
				go s.push.deliverCommandResponse(item, channelName)
			}
		}
	}
	s.publish(execution.ID)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) slashCommandResponses(w http.ResponseWriter, r *http.Request) {
	actor, _ := r.Context().Value(userContextKey{}).(store.User)
	values, err := s.store.SlashCommandResponses(r.Context(), r.PathValue("id"), actor)
	if err != nil {
		http.Error(w, "unable to list command responses", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"responses": values})
}
