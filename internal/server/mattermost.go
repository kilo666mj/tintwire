package server

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/kilo666mj/tintwire/internal/store"
)

type mattermostPostRequest struct {
	ChannelID string          `json:"channel_id"`
	Message   string          `json:"message"`
	RootID    string          `json:"root_id"`
	Props     json.RawMessage `json:"props"`
}
type mattermostBotImportRequest struct {
	Token    string `json:"token"`
	Username string `json:"username"`
	Team     string `json:"team"`
	Channel  string `json:"channel"`
}

type mattermostBotGrantRequest struct {
	Token   string `json:"token"`
	Team    string `json:"team"`
	Channel string `json:"channel"`
}

func (s *Server) mattermostBot(r *http.Request) (store.MattermostBot, error) {
	token, ok := bearerToken(r)
	if !ok {
		return store.MattermostBot{}, store.ErrInvalidCredentials
	}
	return s.store.AuthenticateMattermostBot(r.Context(), token)
}
func mattermostUnauthorized(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnauthorized, map[string]string{"id": "api.context.session_expired.app_error", "message": "Invalid or expired session"})
}

func (s *Server) mattermostMe(w http.ResponseWriter, r *http.Request) {
	bot, err := s.mattermostBot(r)
	if err != nil {
		mattermostUnauthorized(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": bot.User.ID, "username": bot.User.Username, "roles": "system_user"})
}

func (s *Server) mattermostUserByUsername(w http.ResponseWriter, r *http.Request) {
	bot, err := s.mattermostBot(r)
	if err != nil {
		mattermostUnauthorized(w)
		return
	}
	if r.PathValue("username") != bot.User.Username {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": bot.User.ID, "username": bot.User.Username, "roles": "system_user"})
}

func (s *Server) mattermostChannelByName(w http.ResponseWriter, r *http.Request) {
	bot, err := s.mattermostBot(r)
	if err != nil {
		mattermostUnauthorized(w)
		return
	}
	if r.PathValue("team") != bot.TeamName {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	channelID, channelName, err := s.store.ResolveBotChannel(r.Context(), bot, r.PathValue("channel"))
	if errors.Is(err, store.ErrForbidden) || errors.Is(err, store.ErrNotificationNotFound) {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "unable to resolve channel", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": channelID, "team_id": mattermostTeamID(bot.TeamName), "name": channelName, "display_name": channelName, "type": "O"})
}

func mattermostTeamID(name string) string { return "team_" + shortDigest(name) }
func shortDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum[:12])
}

func (s *Server) mattermostCreatePost(w http.ResponseWriter, r *http.Request) {
	bot, err := s.mattermostBot(r)
	if err != nil {
		mattermostUnauthorized(w)
		return
	}
	token, _ := bearerToken(r)
	if !s.allowIngestion(w, token) {
		return
	}
	var request mattermostPostRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid post", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(request.Message) == "" && len(request.Props) == 0 {
		http.Error(w, "empty post", http.StatusBadRequest)
		return
	}
	channelID := request.ChannelID
	if channelID == "" {
		channelID = bot.ChannelID
	}
	resolvedChannelID, channelName, err := s.store.ResolveBotChannel(r.Context(), bot, channelID)
	if errors.Is(err, store.ErrForbidden) {
		http.Error(w, "channel not authorized", http.StatusForbidden)
		return
	}
	if errors.Is(err, store.ErrNotificationNotFound) {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "unable to resolve channel", http.StatusInternalServerError)
		return
	}
	if len(request.Props) == 0 {
		request.Props = json.RawMessage(`{}`)
	}
	protectedProps, protectedAttachments, err := s.protectMattermostActionProps(r.Context(), request.Props)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	request.Props = protectedProps
	notificationID := ""
	if request.RootID == "" {
		var props struct {
			Attachments json.RawMessage `json:"attachments"`
		}
		_ = json.Unmarshal(request.Props, &props)
		if len(protectedAttachments) > 0 {
			props.Attachments = protectedAttachments
		}
		raw, _ := json.Marshal(map[string]any{"text": request.Message, "username": bot.User.Username, "props": json.RawMessage(request.Props), "attachments": json.RawMessage(props.Attachments)})
		notification, err := s.store.CreateBotNotification(r.Context(), bot, resolvedChannelID, channelName, store.IncomingNotification{Text: request.Message, Username: bot.User.Username, Attachments: props.Attachments, RawPayload: raw, State: "received"})
		if err != nil {
			http.Error(w, "unable to create post", http.StatusInternalServerError)
			return
		}
		notificationID = notification.ID
	}
	post, err := s.store.RecordMattermostPost(r.Context(), bot, resolvedChannelID, request.Message, request.RootID, request.Props, notificationID)
	if errors.Is(err, store.ErrNotificationNotFound) {
		http.Error(w, "root post not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "unable to create post", http.StatusInternalServerError)
		return
	}
	s.publish(post.ID)
	writeJSON(w, http.StatusCreated, post)
}

func (s *Server) mattermostListPosts(w http.ResponseWriter, r *http.Request) {
	bot, err := s.mattermostBot(r)
	if err != nil {
		mattermostUnauthorized(w)
		return
	}
	channelID, _, err := s.store.ResolveBotChannel(r.Context(), bot, r.PathValue("channel_id"))
	if errors.Is(err, store.ErrForbidden) || errors.Is(err, store.ErrNotificationNotFound) {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "unable to resolve channel", http.StatusInternalServerError)
		return
	}
	since := int64(0)
	if value := r.URL.Query().Get("since"); value != "" {
		since, err = strconv.ParseInt(value, 10, 64)
		if err != nil || since < 0 {
			http.Error(w, "invalid since", http.StatusBadRequest)
			return
		}
	}
	posts, err := s.store.ListMattermostPosts(r.Context(), channelID, since)
	if err != nil {
		http.Error(w, "unable to list posts", http.StatusInternalServerError)
		return
	}
	order := make([]string, 0, len(posts))
	values := make(map[string]store.MattermostPost, len(posts))
	for _, post := range posts {
		order = append(order, post.ID)
		values[post.ID] = post
	}
	writeJSON(w, http.StatusOK, map[string]any{"order": order, "posts": values})
}

func (s *Server) mattermostReactions(w http.ResponseWriter, r *http.Request) {
	bot, err := s.mattermostBot(r)
	if err != nil {
		mattermostUnauthorized(w)
		return
	}
	channelID, err := s.store.MattermostPostChannel(r.Context(), r.PathValue("post_id"))
	if errors.Is(err, store.ErrNotificationNotFound) {
		http.Error(w, "post not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "unable to resolve post", http.StatusInternalServerError)
		return
	}
	ok, err := s.store.BotChannelAuthorized(r.Context(), bot, channelID)
	if err != nil {
		http.Error(w, "unable to authorize channel", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "channel not authorized", http.StatusForbidden)
		return
	}
	reactions, err := s.store.ListMattermostReactions(r.Context(), r.PathValue("post_id"), channelID)
	if errors.Is(err, store.ErrNotificationNotFound) {
		http.Error(w, "post not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "unable to list reactions", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, reactions)
}

func (s *Server) importMattermostBot(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	actor, ok := r.Context().Value(userContextKey{}).(store.User)
	if !ok || !actor.IsAdmin {
		http.Error(w, "installation administrator access is required", http.StatusForbidden)
		return
	}
	var request mattermostBotImportRequest
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(&struct{}{}) != io.EOF || request.Token == "" || request.Username == "" || request.Team == "" || !channelNamePattern.MatchString(request.Channel) {
		http.Error(w, "invalid bot mapping", http.StatusBadRequest)
		return
	}
	_, err := s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		return nil, data.ImportMattermostBot(r.Context(), request.Token, request.Username, request.Team, request.Channel)
	})
	if errors.Is(err, store.ErrImportConflict) {
		http.Error(w, "bot mapping conflicts with existing identities", http.StatusConflict)
		return
	} else if err != nil {
		http.Error(w, "unable to import bot", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// grantMattermostBotChannel adds an explicit bot-to-channel grant, letting one
// compatibility bot identity operate in multiple permitted mapped channels in
// its team. It also maps the channel to the team if it is not already aliased.
func (s *Server) grantMattermostBotChannel(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	actor, ok := r.Context().Value(userContextKey{}).(store.User)
	if !ok || !actor.IsAdmin {
		http.Error(w, "installation administrator access is required", http.StatusForbidden)
		return
	}
	var request mattermostBotGrantRequest
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(&struct{}{}) != io.EOF || request.Token == "" || request.Team == "" || !channelNamePattern.MatchString(request.Channel) {
		http.Error(w, "invalid bot grant", http.StatusBadRequest)
		return
	}
	_, err := s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		return nil, data.GrantMattermostBotChannel(r.Context(), request.Token, request.Team, request.Channel)
	})
	if errors.Is(err, store.ErrInvalidCredentials) {
		http.Error(w, "bot not found", http.StatusNotFound)
		return
	}
	if errors.Is(err, store.ErrForbidden) {
		http.Error(w, "cross-team grant rejected", http.StatusForbidden)
		return
	}
	if errors.Is(err, store.ErrNotificationNotFound) {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	if errors.Is(err, store.ErrImportConflict) {
		http.Error(w, "channel alias conflict", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "unable to grant bot channel", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
