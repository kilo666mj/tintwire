package server

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kilo666mj/tintwire/internal/store"
)

const maxWebhookBody = 1 << 20

var alertmanagerTitlePattern = regexp.MustCompile(`(?is)^\s*\[(FIRING|RESOLVED)(?::\d+)?\]\s+(.+?)\s*$`)
var channelNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

//go:embed web/*
var webFiles embed.FS

var webAssetVersion = func() string {
	digest := sha256.New()
	for _, name := range []string{"web/emoji.js", "web/markdown.js", "web/app.js", "web/sentinel.css", "web/sw.js"} {
		data, err := webFiles.ReadFile(name)
		if err == nil {
			_, _ = digest.Write(data)
		}
	}
	return hex.EncodeToString(digest.Sum(nil)[:8])
}()

type Server struct {
	store            *store.Store
	consensus        ControlConsensus
	controlProxyPort string
	push             *pushService
	subscribers      map[chan liveUpdate]struct{}
	subscribersMu    sync.RWMutex
	authRequired     bool
	actions          *actionService
	limiter          *toolLimiter
	publicURL        *url.URL
	oauth            *oauthAgentVerifier
	oidc             *oidcLoginService
	startedAt        time.Time
	requests         atomic.Uint64
	errors           atomic.Uint64
	unknownWebhooks  atomic.Uint64
}

type Options struct {
	VAPIDContact     string
	AuthRequired     bool
	ActionKey        string
	PublicURL        string
	OAuthIssuer      string
	OAuthResource    string
	OAuthScope       string
	OIDCClientID     string
	OIDCRedirectURL  string
	Consensus        ControlConsensus
	ControlProxyPort string
}

type liveUpdate struct {
	Kind string
	ID   string
}

// ControlConsensus serializes security and per-user control state through a
// majority-committed log. The callback always receives an isolated store; its
// changes become visible only after the proposal commits.
type ControlConsensus interface {
	NodeID() string
	IsLeader() bool
	Leader() (string, string)
	Healthy(time.Duration) bool
	VoterCount() (int, error)
	Mutate(context.Context, func(*store.Store) (any, error)) (any, error)
}

type incomingWebhook struct {
	Text        string          `json:"text"`
	Channel     string          `json:"channel"`
	Username    string          `json:"username"`
	IconURL     string          `json:"icon_url"`
	IconEmoji   string          `json:"icon_emoji"`
	Attachments json.RawMessage `json:"attachments"`
	Props       json.RawMessage `json:"props"`
	Blocks      json.RawMessage `json:"blocks"`
}

type nativeCard struct {
	Version  int          `json:"version"`
	Channel  string       `json:"channel,omitempty"`
	Title    string       `json:"title"`
	Summary  string       `json:"summary"`
	Severity string       `json:"severity"`
	Source   string       `json:"source"`
	Metrics  []cardMetric `json:"metrics,omitempty"`
	Fields   []cardField  `json:"fields,omitempty"`
	Badges   []cardBadge  `json:"badges,omitempty"`
	Images   []cardImage  `json:"images,omitempty"`
	Links    []cardLink   `json:"links,omitempty"`
	Rows     []cardRow    `json:"rows,omitempty"`
	Actions  []cardAction `json:"actions,omitempty"`
}

type cardMetric struct {
	Label string `json:"label"`
	Value any    `json:"value"`
}

type cardField struct {
	Label string `json:"label"`
	Value string `json:"value"`
}
type cardBadge struct {
	Label string `json:"label"`
	Tone  string `json:"tone,omitempty"`
}
type cardImage struct {
	URL string `json:"url"`
	Alt string `json:"alt"`
}
type cardLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type cardRow struct {
	Primary  string   `json:"primary"`
	Tags     []string `json:"tags,omitempty"`
	Emphasis string   `json:"emphasis,omitempty"`
}

type cardAction struct {
	Label         string          `json:"label"`
	Type          string          `json:"type"`
	URL           string          `json:"url"`
	Target        string          `json:"target"`
	Context       json.RawMessage `json:"context,omitempty"`
	ContextCipher string          `json:"context_cipher,omitempty"`
}

type simpleMessage struct {
	Text   string `json:"text"`
	Source string `json:"source"`
}

type createChannelRequest struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	AccentColor string `json:"accent_color"`
	Visibility  string `json:"visibility"`
}

type createUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	IsAdmin  bool   `json:"is_admin"`
}
type channelMemberRequest struct {
	Role string `json:"role"`
}

type channelNotificationPreferenceRequest struct {
	Level string `json:"level"`
}

type notificationInboxRequest struct {
	Action store.NotificationInboxAction `json:"action"`
}

type webhookImportRequest struct {
	DryRun   bool `json:"dry_run"`
	Webhooks []struct {
		ID            string `json:"id"`
		Channel       string `json:"channel"`
		ChannelLocked *bool  `json:"channel_locked"`
	} `json:"webhooks"`
}

type activityEvent struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	Title     string    `json:"title,omitempty"`
	Text      string    `json:"text,omitempty"`
	Color     string    `json:"color,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Actor     string    `json:"actor,omitempty"`
}

func New(store *store.Store) http.Handler {
	handler, err := NewWithOptions(store, Options{})
	if err != nil {
		slog.Error("initialize web push", "error", err)
	}
	return handler
}

func NewWithOptions(data *store.Store, options Options) (http.Handler, error) {
	push, pushErr := newPushService(data, options.VAPIDContact, options.AuthRequired)
	actions, actionErr := newActionService(data, options.ActionKey)
	if actionErr != nil {
		return nil, actionErr
	}
	var publicURL *url.URL
	if strings.TrimSpace(options.PublicURL) != "" {
		parsed, err := url.Parse(options.PublicURL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
			return nil, errors.New("public URL must be an HTTP(S) origin without path, query, fragment, or user information")
		}
		parsed.Path = ""
		publicURL = parsed
	}
	if options.OAuthResource == "" && publicURL != nil {
		options.OAuthResource = strings.TrimSuffix(publicURL.String(), "/") + "/mcp"
	}
	if options.OAuthScope == "" {
		options.OAuthScope = "tintwire:mcp"
	}
	oauthVerifier, err := newOAuthAgentVerifier(options.OAuthIssuer, options.OAuthResource, options.OAuthScope)
	if err != nil {
		return nil, err
	}
	oidcLogin, err := newOIDCLoginService(options.OAuthIssuer, options.OIDCClientID, options.OIDCRedirectURL, publicURL)
	if err != nil {
		return nil, err
	}
	if options.ControlProxyPort != "" {
		port, err := strconv.Atoi(options.ControlProxyPort)
		if err != nil || port < 1 || port > 65535 {
			return nil, errors.New("control proxy port must be a valid TCP port")
		}
	}
	s := &Server{store: data, consensus: options.Consensus, controlProxyPort: options.ControlProxyPort, push: push, actions: actions, limiter: newToolLimiter(), publicURL: publicURL, oauth: oauthVerifier, oidc: oidcLogin, startedAt: time.Now(), subscribers: make(map[chan liveUpdate]struct{}), authRequired: options.AuthRequired}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hooks/{id}", s.receiveWebhook)
	mux.HandleFunc("POST /api/v1/notifications", s.receiveNativeCard)
	mux.HandleFunc("POST /api/v1/messages", s.receiveSimpleMessage)
	mux.HandleFunc("POST /api/v1/session", s.requireControlAuthority(s.login))
	mux.HandleFunc("GET /api/v1/session", s.sessionStatus)
	mux.HandleFunc("DELETE /api/v1/session", s.requireControlAuthority(s.logout))
	mux.HandleFunc("GET /api/v1/auth/oidc/start", s.requireControlAuthority(s.oidcStart))
	mux.HandleFunc("GET /api/v1/auth/oidc/callback", s.requireControlAuthority(s.oidcCallback))
	mux.HandleFunc("POST /api/v1/auth/desktop/session", s.requireControlAuthority(s.desktopSession))
	mux.HandleFunc("GET /api/v1/notifications", s.requireReader(s.listNotifications))
	mux.HandleFunc("GET /api/v1/channels", s.requireReader(s.listChannels))
	mux.HandleFunc("POST /api/v1/channels", s.requireReader(s.requireControlAuthority(s.createChannel)))
	mux.HandleFunc("PUT /api/v1/channels/{id}/members/{username}", s.requireReader(s.requireControlAuthority(s.setChannelMember)))
	mux.HandleFunc("GET /api/v1/channels/{id}/notification-preference", s.requireReader(s.channelNotificationPreference))
	mux.HandleFunc("PUT /api/v1/channels/{id}/notification-preference", s.requireReader(s.requireControlAuthority(s.setChannelNotificationPreference)))
	mux.HandleFunc("POST /api/v1/users", s.requireReader(s.requireControlAuthority(s.createUser)))
	mux.HandleFunc("GET /api/v1/admin/users", s.requireReader(s.listManagedUsers))
	mux.HandleFunc("PUT /api/v1/admin/users/{id}", s.requireReader(s.requireControlAuthority(s.updateManagedUser)))
	mux.HandleFunc("DELETE /api/v1/admin/users/{id}/sessions", s.requireReader(s.requireControlAuthority(s.revokeManagedUserSessions)))
	mux.HandleFunc("PUT /api/v1/admin/users/{id}/memberships/{channel}", s.requireReader(s.requireControlAuthority(s.updateManagedUserMembership)))
	mux.HandleFunc("GET /api/v1/agents", s.requireReader(s.listAgents))
	mux.HandleFunc("POST /api/v1/agents", s.requireReader(s.requireControlAuthority(s.createAgent)))
	mux.HandleFunc("POST /api/v1/agents/{name}/revoke", s.requireReader(s.requireControlAuthority(s.revokeAgent)))
	mux.HandleFunc("GET /api/v1/agents/{name}/runs", s.requireReader(s.listAgentRuns))
	mux.HandleFunc("GET /api/v1/agents/runs/{id}/events", s.requireReader(s.agentRunEvents))
	mux.HandleFunc("GET /api/v1/webhooks", s.requireReader(s.listWebhooks))
	mux.HandleFunc("POST /api/v1/webhooks", s.requireReader(s.requireControlAuthority(s.createWebhook)))
	mux.HandleFunc("PUT /api/v1/webhooks/{id}", s.requireReader(s.requireControlAuthority(s.updateWebhook)))
	mux.HandleFunc("POST /api/v1/webhooks/{id}/revoke", s.requireReader(s.requireControlAuthority(s.revokeWebhook)))
	mux.HandleFunc("POST /mcp", s.requireAgent(s.mcpEndpoint))
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.oauthProtectedResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", s.oauthProtectedResourceMetadata)
	mux.HandleFunc("POST /api/v1/admin/import/webhooks", s.requireReader(s.requireControlAuthority(s.importWebhooks)))
	mux.HandleFunc("POST /api/v1/admin/import/mattermost-bot", s.requireReader(s.requireControlAuthority(s.importMattermostBot)))
	mux.HandleFunc("POST /api/v1/admin/mattermost-bots/grant", s.requireReader(s.requireControlAuthority(s.grantMattermostBotChannel)))
	mux.HandleFunc("POST /api/v1/admin/import/slash-commands", s.requireReader(s.requireControlAuthority(s.importSlashCommands)))
	mux.HandleFunc("GET /api/v1/commands", s.requireReader(s.listSlashCommands))
	mux.HandleFunc("POST /api/v1/commands", s.requireReader(s.executeSlashCommand))
	mux.HandleFunc("GET /api/v1/commands/{id}/responses", s.requireReader(s.slashCommandResponses))
	mux.HandleFunc("POST /hooks/commands/{token}", s.delayedSlashResponse)
	mux.HandleFunc("PUT /api/v1/action-targets/{name}", s.requireReader(s.requireControlAuthority(s.saveActionTarget)))
	mux.HandleFunc("DELETE /api/v1/action-targets/{name}", s.requireReader(s.requireControlAuthority(s.deleteActionTarget)))
	mux.HandleFunc("POST /api/v1/notifications/{id}/actions/{index}", s.requireReader(s.executeHTTPAction))
	mux.HandleFunc("POST /api/v1/notifications/{id}/mattermost-actions/{attachment}/{action}", s.requireReader(s.executeMattermostAction))
	mux.HandleFunc("POST /api/v1/channels/{id}/messages", s.requireReader(s.createChannelMessage))
	mux.HandleFunc("GET /api/v1/messages/{messageID}", s.requireReader(s.getChannelMessage))
	mux.HandleFunc("POST /api/v1/channels/{id}/commands", s.requireReader(s.executeChannelCommand))
	mux.HandleFunc("GET /api/v1/channels/{id}/timeline", s.requireReader(s.listChannelTimeline))
	mux.HandleFunc("GET /api/v1/channels/{id}/threads/{rootID}", s.requireReader(s.listChannelThread))
	mux.HandleFunc("POST /api/v1/notifications/read", s.requireReader(s.requireControlAuthority(s.markAllRead)))
	mux.HandleFunc("POST /api/v1/channels/{id}/read", s.requireReader(s.requireControlAuthority(s.markChannelRead)))
	mux.HandleFunc("POST /api/v1/notifications/{id}/inbox", s.requireReader(s.requireControlAuthority(s.updateNotificationInbox)))
	mux.HandleFunc("GET /api/v1/notifications/{id}/events", s.requireReader(s.notificationEvents))
	mux.HandleFunc("POST /api/v1/notifications/{id}/state", s.requireReader(s.setNotificationState))
	mux.HandleFunc("POST /api/v1/notifications/{id}/approval", s.requireReader(s.setMattermostApproval))
	mux.HandleFunc("GET /api/v1/events", s.requireReader(s.streamEvents))
	mux.HandleFunc("GET /api/v1/push/config", s.requireReader(s.pushConfig))
	mux.HandleFunc("POST /api/v1/push/subscriptions", s.requireReader(s.requireControlAuthority(s.savePushSubscription)))
	mux.HandleFunc("DELETE /api/v1/push/subscriptions", s.requireReader(s.requireControlAuthority(s.removePushSubscription)))
	mux.HandleFunc("GET /api/v4/users/me", s.requireControlLease(s.mattermostMe))
	mux.HandleFunc("GET /api/v4/users/username/{username}", s.requireControlLease(s.mattermostUserByUsername))
	mux.HandleFunc("GET /api/v4/teams/name/{team}/channels/name/{channel}", s.requireControlLease(s.mattermostChannelByName))
	mux.HandleFunc("GET /api/v4/channels/{channel_id}/posts", s.requireControlLease(s.mattermostListPosts))
	mux.HandleFunc("POST /api/v4/posts", s.requireControlLease(s.mattermostCreatePost))
	mux.HandleFunc("GET /api/v4/posts/{post_id}/reactions", s.requireControlLease(s.mattermostReactions))
	mux.HandleFunc("GET /healthz", health)
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("GET /assets/{name}", serveAsset)
	mux.HandleFunc("GET /manifest.webmanifest", serveManifest)
	mux.HandleFunc("GET /sw.js", serveServiceWorker)
	mux.HandleFunc("GET /", serveWeb)
	return securityHeaders(s.observe(mux)), pushErr
}

func (s *Server) receiveSimpleMessage(w http.ResponseWriter, r *http.Request) {
	token, ok := bearerToken(r)
	if !ok {
		http.Error(w, "bearer publishing token is required", http.StatusUnauthorized)
		return
	}
	if !s.allowIngestion(w, token) {
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (mediaType != "text/plain" && mediaType != "application/json") {
		http.Error(w, "content type must be text/plain or application/json", http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "message payload is too large", http.StatusRequestEntityTooLarge)
		return
	}
	message := simpleMessage{Source: "publisher"}
	if mediaType == "text/plain" {
		message.Text = string(body)
	} else {
		decoder := json.NewDecoder(strings.NewReader(string(body)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&message); err != nil {
			http.Error(w, "unable to parse message", http.StatusBadRequest)
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			http.Error(w, "message payload must contain one JSON object", http.StatusBadRequest)
			return
		}
	}
	message.Text = strings.TrimSpace(message.Text)
	message.Source = strings.TrimSpace(message.Source)
	if message.Text == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}
	if len(message.Text) > maxWebhookBody || len(message.Source) > 100 {
		http.Error(w, "message field is too long", http.StatusBadRequest)
		return
	}
	if message.Source == "" {
		message.Source = "publisher"
	}
	notification, err := s.store.CreateFromWebhook(r.Context(), token, store.IncomingNotification{
		Text: message.Text, Username: message.Source, RawPayload: append(json.RawMessage(nil), body...), State: "received",
	})
	if errors.Is(err, store.ErrWebhookNotFound) {
		http.Error(w, "publishing token not found", http.StatusUnauthorized)
		return
	}
	if err != nil {
		slog.Error("store simple message", "error", err)
		http.Error(w, "unable to store message", http.StatusInternalServerError)
		return
	}
	s.publish(notification.ID)
	if s.push != nil {
		go s.push.deliver(notification)
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": notification.ID})
}

func bearerToken(r *http.Request) (string, bool) {
	authorization := r.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
	return token, token != ""
}

func (s *Server) receiveNativeCard(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		http.Error(w, "content type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	token, ok := bearerToken(r)
	if !ok {
		http.Error(w, "bearer publishing token is required", http.StatusUnauthorized)
		return
	}
	if !s.allowIngestion(w, token) {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "card payload is too large", http.StatusRequestEntityTooLarge)
		return
	}
	var card nativeCard
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&card); err != nil {
		http.Error(w, "unable to parse native card", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "card payload must contain one JSON object", http.StatusBadRequest)
		return
	}
	if err := validateNativeCard(card); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	storedCard, err := s.protectNativeActionContexts(card)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	notification, err := s.store.CreateFromWebhook(r.Context(), token, store.IncomingNotification{
		Channel: card.Channel, Text: card.Summary, Username: card.Source, Card: storedCard,
		RawPayload: storedCard, State: "received",
	})
	if errors.Is(err, store.ErrWebhookNotFound) {
		http.Error(w, "publishing token not found", http.StatusUnauthorized)
		return
	}
	if errors.Is(err, store.ErrWebhookChannelDenied) {
		http.Error(w, "webhook channel override is not allowed", http.StatusForbidden)
		return
	}
	if err != nil {
		slog.Error("store native card", "error", err)
		http.Error(w, "unable to store card", http.StatusInternalServerError)
		return
	}
	s.publish(notification.ID)
	if s.push != nil {
		go s.push.deliver(notification)
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": notification.ID})
}

func validateNativeCard(card nativeCard) error {
	if card.Version != 1 {
		return errors.New("version must be 1")
	}
	if strings.TrimSpace(card.Title) == "" {
		return errors.New("title is required")
	}
	if len(card.Title) > 200 || len(card.Summary) > 500 {
		return errors.New("title or summary is too long")
	}
	if card.Severity != "" && card.Severity != "info" && card.Severity != "warning" && card.Severity != "critical" && card.Severity != "success" {
		return errors.New("unsupported severity")
	}
	if len(card.Metrics) > 12 || len(card.Fields) > 24 || len(card.Badges) > 16 || len(card.Images) > 4 || len(card.Links) > 12 || len(card.Rows) > 2000 || len(card.Actions) > 8 {
		return errors.New("card component limit exceeded")
	}
	for _, field := range card.Fields {
		if strings.TrimSpace(field.Label) == "" || len(field.Label) > 100 || len(field.Value) > 1000 {
			return errors.New("invalid card field")
		}
	}
	for _, badge := range card.Badges {
		if strings.TrimSpace(badge.Label) == "" || len(badge.Label) > 80 || !map[string]bool{"": true, "neutral": true, "info": true, "warning": true, "critical": true, "success": true}[badge.Tone] {
			return errors.New("invalid card badge")
		}
	}
	for _, image := range card.Images {
		if !validHTTPURL(image.URL) || strings.TrimSpace(image.Alt) == "" || len(image.Alt) > 200 {
			return errors.New("invalid card image")
		}
	}
	for _, link := range card.Links {
		if strings.TrimSpace(link.Label) == "" || len(link.Label) > 100 || !validHTTPURL(link.URL) {
			return errors.New("invalid card link")
		}
	}
	for _, metric := range card.Metrics {
		if strings.TrimSpace(metric.Label) == "" {
			return errors.New("metric label is required")
		}
		switch metric.Value.(type) {
		case string, float64:
		default:
			return errors.New("metric value must be a string or number")
		}
	}
	for _, row := range card.Rows {
		if strings.TrimSpace(row.Primary) == "" {
			return errors.New("row primary is required")
		}
		if row.Emphasis != "" && row.Emphasis != "strong" {
			return errors.New("unsupported row emphasis")
		}
		if len(row.Tags) > 16 {
			return errors.New("row tag limit exceeded")
		}
	}
	for _, action := range card.Actions {
		if strings.TrimSpace(action.Label) == "" {
			return errors.New("action label is required")
		}
		switch action.Type {
		case "link":
			request, err := http.NewRequest(http.MethodGet, action.URL, nil)
			if err != nil || request.URL.Host == "" || (request.URL.Scheme != "http" && request.URL.Scheme != "https") {
				return errors.New("action URL must use HTTP or HTTPS")
			}
			if action.Target != "" || len(action.Context) > 0 {
				return errors.New("link action contains unsupported fields")
			}
		case "http":
			if action.ContextCipher != "" {
				return errors.New("context_cipher is server-managed")
			}
			if !actionNamePattern.MatchString(action.Target) || action.URL != "" {
				return errors.New("HTTP action requires a registered target")
			}
			if len(action.Context) == 0 {
				action.Context = json.RawMessage(`{}`)
			}
			var contextValue map[string]any
			if len(action.Context) > 16<<10 || json.Unmarshal(action.Context, &contextValue) != nil {
				return errors.New("HTTP action context must be a small JSON object")
			}
		default:
			return errors.New("unsupported action type")
		}
	}
	return nil
}

func (s *Server) receiveWebhook(w http.ResponseWriter, r *http.Request) {
	if !s.allowIngestion(w, r.PathValue("id")) {
		return
	}
	contentType := r.Header.Get("Content-Type")
	mediaType := "application/json"
	if contentType != "" {
		var err error
		mediaType, _, err = mime.ParseMediaType(contentType)
		if err != nil || (mediaType != "application/json" && mediaType != "application/x-www-form-urlencoded" && mediaType != "multipart/form-data") {
			http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
			return
		}
	}

	body, err := readWebhookPayload(w, r, mediaType)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var payload incomingWebhook
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "unable to parse incoming data", http.StatusBadRequest)
		return
	}
	if len(payload.Blocks) > 0 && !validJSONArray(payload.Blocks) {
		http.Error(w, "blocks must be a JSON array", http.StatusBadRequest)
		return
	}
	blockText := normalizeSlackBlocks(payload.Blocks)
	if strings.TrimSpace(payload.Text) == "" && emptyJSONArray(payload.Attachments) && blockText == "" {
		http.Error(w, "text, attachments, or blocks is required", http.StatusBadRequest)
		return
	}
	if len(payload.Attachments) > 0 && !validJSONArray(payload.Attachments) {
		http.Error(w, "attachments must be a JSON array", http.StatusBadRequest)
		return
	}
	state, externalKey := alertmanagerLifecycle(payload.Attachments)
	text := payload.Text
	if blockText != "" {
		if strings.TrimSpace(text) != "" {
			text += "\n\n"
		}
		text += blockText
	}

	notification, err := s.store.CreateFromWebhook(r.Context(), r.PathValue("id"), store.IncomingNotification{
		Channel: payload.Channel, Text: text, Username: payload.Username, IconURL: payload.IconURL,
		Attachments: payload.Attachments, RawPayload: append(json.RawMessage(nil), body...),
		State: state, ExternalKey: externalKey,
	})
	if errors.Is(err, store.ErrWebhookNotFound) {
		s.unknownWebhooks.Add(1)
		http.Error(w, "webhook not found", http.StatusNotFound)
		return
	}
	if errors.Is(err, store.ErrWebhookChannelDenied) {
		http.Error(w, "webhook channel override is not allowed", http.StatusForbidden)
		return
	}
	if err != nil {
		slog.Error("store webhook notification", "error", err)
		http.Error(w, "unable to store incoming data", http.StatusInternalServerError)
		return
	}
	s.publish(notification.ID)
	if s.push != nil {
		go s.push.deliver(notification)
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

func (s *Server) allowIngestion(w http.ResponseWriter, token string) bool {
	if s.limiter == nil {
		return true
	}
	digest := sha256.Sum256([]byte(token))
	if s.limiter.allow("ingest:"+hex.EncodeToString(digest[:16]), 240) {
		return true
	}
	w.Header().Set("Retry-After", "60")
	http.Error(w, "publishing rate limit exceeded", http.StatusTooManyRequests)
	return false
}

func normalizeSlackBlocks(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var blocks []struct {
		Type string `json:"type"`
		Text struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"text"`
		Fields []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"fields"`
		Elements []struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			URL     string `json:"url"`
			AltText string `json:"alt_text"`
		} `json:"elements"`
		ImageURL string `json:"image_url"`
		AltText  string `json:"alt_text"`
		Title    struct {
			Text string `json:"text"`
		} `json:"title"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "header":
			if text := strings.TrimSpace(block.Text.Text); text != "" {
				parts = append(parts, "**"+text+"**")
			}
		case "section":
			section := strings.TrimSpace(block.Text.Text)
			for _, field := range block.Fields {
				if value := strings.TrimSpace(field.Text); value != "" {
					if section != "" {
						section += "\n"
					}
					section += value
				}
			}
			if section != "" {
				parts = append(parts, section)
			}
		case "context", "actions":
			values := make([]string, 0, len(block.Elements))
			for _, element := range block.Elements {
				label := strings.TrimSpace(element.Text)
				if label == "" {
					label = strings.TrimSpace(element.AltText)
				}
				if element.URL != "" && validHTTPURL(element.URL) && label != "" {
					values = append(values, "<"+element.URL+"|"+label+">")
				} else if label != "" {
					values = append(values, label)
				}
			}
			if len(values) > 0 {
				parts = append(parts, strings.Join(values, " · "))
			}
		case "image":
			label := strings.TrimSpace(block.Title.Text)
			if label == "" {
				label = strings.TrimSpace(block.AltText)
			}
			if validHTTPURL(block.ImageURL) {
				if label == "" {
					label = "Image"
				}
				parts = append(parts, "<"+block.ImageURL+"|"+label+">")
			} else if label != "" {
				parts = append(parts, label)
			}
		case "divider":
			parts = append(parts, "────────")
		default:
			blockType := strings.TrimSpace(block.Type)
			if blockType == "" {
				blockType = "unknown"
			}
			parts = append(parts, "[Unsupported Slack block: "+blockType+"]")
		}
	}
	return strings.Join(parts, "\n\n")
}

func validHTTPURL(value string) bool {
	request, err := http.NewRequest(http.MethodGet, value, nil)
	return err == nil && request.URL.Host != "" && (request.URL.Scheme == "http" || request.URL.Scheme == "https")
}

func readWebhookPayload(w http.ResponseWriter, r *http.Request, mediaType string) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBody)
	if mediaType == "application/json" {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, errors.New("webhook payload is too large")
		}
		return body, nil
	}
	if mediaType == "application/x-www-form-urlencoded" {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, errors.New("webhook payload is too large")
		}
		values, err := url.ParseQuery(string(body))
		if err != nil || values.Get("payload") == "" {
			return nil, errors.New("payload form field is required")
		}
		return []byte(values.Get("payload")), nil
	}
	if err := r.ParseMultipartForm(maxWebhookBody); err != nil {
		return nil, errors.New("unable to parse multipart webhook")
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	payload := r.FormValue("payload")
	if payload == "" {
		return nil, errors.New("payload form field is required")
	}
	return []byte(payload), nil
}

func alertmanagerLifecycle(raw json.RawMessage) (string, string) {
	var attachments []struct {
		Title string `json:"title"`
	}
	if json.Unmarshal(raw, &attachments) != nil || len(attachments) == 0 {
		return "received", ""
	}
	match := alertmanagerTitlePattern.FindStringSubmatch(attachments[0].Title)
	if len(match) != 3 {
		return "received", ""
	}
	identity := strings.Join(strings.Fields(match[2]), " ")
	if identity == "" {
		return "received", ""
	}
	digest := sha256.Sum256([]byte(identity))
	return strings.ToLower(match[1]), "alertmanager-slack:" + hex.EncodeToString(digest[:])
}

func (s *Server) listNotifications(w http.ResponseWriter, r *http.Request) {
	query := store.NotificationQuery{
		Search: r.URL.Query().Get("q"), Channel: r.URL.Query().Get("channel"),
		State: r.URL.Query().Get("state"), Severity: r.URL.Query().Get("severity"),
	}
	if truthy(r.URL.Query().Get("include_dismissed")) {
		query.ShowDismissed = true
	}
	if query.State == "dismissed" {
		query.DismissedOnly = true
		query.State = ""
	}
	user, hasInboxUser := s.inboxUser(r)
	if hasInboxUser {
		query.UserID = user.ID
		query.UserAdmin = user.IsAdmin
		query.UnreadOnly = truthy(r.URL.Query().Get("unread"))
	}
	limit := 100
	if value := r.URL.Query().Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 200 {
			http.Error(w, "limit must be between 1 and 200", http.StatusBadRequest)
			return
		}
		limit = parsed
	}
	if cursor := r.URL.Query().Get("before"); cursor != "" {
		beforeAt, beforeID, ok := decodeNotificationCursor(cursor)
		if !ok {
			http.Error(w, "invalid history cursor", http.StatusBadRequest)
			return
		}
		query.BeforeAt, query.BeforeID = beforeAt, beforeID
	}
	if query.State != "" && query.State != "received" && query.State != "firing" && query.State != "acknowledged" && query.State != "resolved" {
		http.Error(w, "unsupported state filter", http.StatusBadRequest)
		return
	}
	if query.Severity != "" && query.Severity != "info" && query.Severity != "warning" && query.Severity != "critical" && query.Severity != "success" {
		http.Error(w, "unsupported severity filter", http.StatusBadRequest)
		return
	}
	query.Limit = limit + 1
	notifications, err := s.store.QueryNotifications(r.Context(), query)
	if err != nil {
		slog.Error("list notifications", "error", err)
		http.Error(w, "unable to list notifications", http.StatusInternalServerError)
		return
	}
	notificationIDs := make([]string, len(notifications))
	for index := range notifications {
		notificationIDs[index] = notifications[index].ID
	}
	actionResults, err := s.store.LatestMattermostActionResults(r.Context(), notificationIDs)
	if err != nil {
		slog.Error("list notification action results", "error", err)
		http.Error(w, "unable to list notification actions", http.StatusInternalServerError)
		return
	}
	sanitizeNotificationCards(notifications)
	sanitizeMattermostActions(notifications, actionResults)
	unreadCount, err := s.store.UnreadCount(r.Context(), user)
	if err != nil {
		slog.Error("count unread notifications", "error", err)
		http.Error(w, "unable to count unread notifications", http.StatusInternalServerError)
		return
	}
	nextCursor := ""
	if len(notifications) > limit {
		notifications = notifications[:limit]
		last := notifications[len(notifications)-1]
		nextCursor = encodeNotificationCursor(last.CreatedAt.UnixMilli(), last.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"notifications": notifications, "next_cursor": nextCursor, "unread_count": unreadCount})
}

func sanitizeNotificationCards(notifications []store.Notification) {
	for index := range notifications {
		if len(notifications[index].Card) == 0 {
			continue
		}
		var card map[string]any
		if json.Unmarshal(notifications[index].Card, &card) != nil {
			continue
		}
		actions, ok := card["actions"].([]any)
		if !ok {
			continue
		}
		for _, raw := range actions {
			action, ok := raw.(map[string]any)
			if !ok || action["type"] != "http" {
				continue
			}
			delete(action, "context")
			delete(action, "context_cipher")
			delete(action, "target")
			delete(action, "url")
		}
		if sanitized, err := json.Marshal(card); err == nil {
			notifications[index].Card = sanitized
		}
	}
}

func sanitizeMattermostActions(notifications []store.Notification, results map[string]map[int]store.MattermostActionResult) {
	for index := range notifications {
		if len(notifications[index].Attachments) == 0 {
			continue
		}
		var attachments []map[string]any
		if json.Unmarshal(notifications[index].Attachments, &attachments) != nil {
			continue
		}
		changed := false
		for attachmentIndex, attachment := range attachments {
			actions, ok := attachment["actions"].([]any)
			if !ok {
				continue
			}
			result, hasResult := results[notifications[index].ID][attachmentIndex]
			for actionIndex, raw := range actions {
				action, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if _, ok := action["integration"]; ok {
					delete(action, "integration")
					action["executable"] = !hasResult || result.Status != "succeeded"
					if hasResult && result.ActionIndex == actionIndex {
						action["selected"] = true
					}
					changed = true
				}
			}
			if hasResult {
				attachment["action_result"] = result
				changed = true
			}
		}
		if changed {
			if value, err := json.Marshal(attachments); err == nil {
				notifications[index].Attachments = value
			}
		}
	}
}

func (s *Server) listChannels(w http.ResponseWriter, r *http.Request) {
	user, _ := s.inboxUser(r)
	channels, err := s.store.ListChannels(r.Context(), user)
	if err != nil {
		slog.Error("list channels", "error", err)
		http.Error(w, "unable to list channels", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": channels})
}

func (s *Server) channelNotificationPreference(w http.ResponseWriter, r *http.Request) {
	user, _ := s.inboxUser(r)
	level, err := s.store.ChannelNotificationPreference(r.Context(), user, r.PathValue("id"))
	if errors.Is(err, store.ErrForbidden) {
		http.Error(w, "channel access is required", http.StatusForbidden)
		return
	}
	if err != nil {
		http.Error(w, "unable to read notification preference", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"level": level})
}

func (s *Server) setChannelNotificationPreference(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	user, _ := s.inboxUser(r)
	var request channelNotificationPreferenceRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid notification preference", http.StatusBadRequest)
		return
	}
	_, err := s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		return nil, data.SetChannelNotificationPreference(r.Context(), user, r.PathValue("id"), request.Level)
	})
	if err != nil {
		if errors.Is(err, store.ErrForbidden) {
			http.Error(w, "channel access is required", http.StatusForbidden)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"level": request.Level})
}

func (s *Server) createChannel(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	user, ok := r.Context().Value(userContextKey{}).(store.User)
	if !ok || !user.IsAdmin {
		http.Error(w, "installation administrator access is required", http.StatusForbidden)
		return
	}
	var request createChannelRequest
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid channel", http.StatusBadRequest)
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	request.DisplayName = strings.TrimSpace(request.DisplayName)
	request.Description = strings.TrimSpace(request.Description)
	request.AccentColor = strings.TrimSpace(request.AccentColor)
	if !channelNamePattern.MatchString(request.Name) {
		http.Error(w, "channel name must be URL-safe lowercase text", http.StatusBadRequest)
		return
	}
	if request.DisplayName == "" {
		request.DisplayName = request.Name
	}
	if len(request.DisplayName) > 100 || len(request.Description) > 500 {
		http.Error(w, "channel metadata is too long", http.StatusBadRequest)
		return
	}
	if request.Visibility == "" {
		request.Visibility = "public"
	}
	if request.Visibility != "public" && request.Visibility != "private" {
		http.Error(w, "visibility must be public or private", http.StatusBadRequest)
		return
	}
	if request.AccentColor != "" && !regexp.MustCompile(`^#[0-9a-fA-F]{6}$`).MatchString(request.AccentColor) {
		http.Error(w, "accent color must be a six-digit hex color", http.StatusBadRequest)
		return
	}
	result, err := s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		channel, token, err := data.CreateChannel(r.Context(), store.CreateChannelInput{Name: request.Name, DisplayName: request.DisplayName, Description: request.Description, AccentColor: request.AccentColor, Visibility: request.Visibility})
		return struct {
			channel store.ChannelSummary
			token   string
		}{channel, token}, err
	})
	if err != nil {
		if store.IsAlreadyExists(err) {
			http.Error(w, "channel already exists", http.StatusConflict)
			return
		}
		slog.Error("create channel", "error", err)
		http.Error(w, "unable to create channel", http.StatusInternalServerError)
		return
	}
	created := result.(struct {
		channel store.ChannelSummary
		token   string
	})
	channel, token := created.channel, created.token
	writeJSON(w, http.StatusCreated, map[string]any{"channel": channel, "publishing_token": token})
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	actor, ok := r.Context().Value(userContextKey{}).(store.User)
	if !ok || !actor.IsAdmin {
		http.Error(w, "installation administrator access is required", http.StatusForbidden)
		return
	}
	var request createUserRequest
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid user", http.StatusBadRequest)
		return
	}
	result, err := s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		return data.CreateUser(r.Context(), request.Username, request.Password, request.IsAdmin)
	})
	if err != nil {
		if store.IsAlreadyExists(err) {
			http.Error(w, "user already exists", http.StatusConflict)
			return
		}
		if strings.Contains(err.Error(), "12 characters") {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		slog.Error("create user", "error", err)
		http.Error(w, "unable to create user", http.StatusInternalServerError)
		return
	}
	user := result.(store.User)
	writeJSON(w, http.StatusCreated, map[string]any{"user": map[string]any{"id": user.ID, "username": user.Username, "is_admin": user.IsAdmin}})
}

func (s *Server) setChannelMember(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	actor, ok := r.Context().Value(userContextKey{}).(store.User)
	if !ok || !actor.IsAdmin {
		http.Error(w, "installation administrator access is required", http.StatusForbidden)
		return
	}
	var request channelMemberRequest
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid membership", http.StatusBadRequest)
		return
	}
	if request.Role != "viewer" && request.Role != "operator" && request.Role != "channel_admin" {
		http.Error(w, "unsupported channel role", http.StatusBadRequest)
		return
	}
	_, err := s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		return nil, data.SetChannelMember(r.Context(), r.PathValue("id"), r.PathValue("username"), request.Role)
	})
	if err != nil {
		if errors.Is(err, store.ErrInvalidCredentials) {
			http.Error(w, "user not found", http.StatusNotFound)
			return
		}
		slog.Error("set channel membership", "error", err)
		http.Error(w, "unable to set channel membership", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) importWebhooks(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	actor, ok := r.Context().Value(userContextKey{}).(store.User)
	if !ok || !actor.IsAdmin {
		http.Error(w, "installation administrator access is required", http.StatusForbidden)
		return
	}
	var request webhookImportRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid webhook import", http.StatusBadRequest)
		return
	}
	if len(request.Webhooks) == 0 || len(request.Webhooks) > 1000 {
		http.Error(w, "webhook import must contain between 1 and 1000 entries", http.StatusBadRequest)
		return
	}
	imports := make([]store.WebhookImport, 0, len(request.Webhooks))
	for _, item := range request.Webhooks {
		item.ID = strings.TrimSpace(item.ID)
		item.Channel = strings.TrimSpace(item.Channel)
		if item.ID == "" || len(item.ID) > 256 || !channelNamePattern.MatchString(item.Channel) {
			http.Error(w, "each webhook requires a valid ID and channel", http.StatusBadRequest)
			return
		}
		channelLocked := false
		if item.ChannelLocked != nil {
			channelLocked = *item.ChannelLocked
		}
		imports = append(imports, store.WebhookImport{Token: item.ID, Channel: item.Channel, ChannelLocked: channelLocked})
	}
	rawResult, err := s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		return data.ImportWebhooks(r.Context(), imports, !request.DryRun)
	})
	if errors.Is(err, store.ErrImportConflict) {
		http.Error(w, "webhook import conflicts with existing mappings or channels", http.StatusConflict)
		return
	}
	if err != nil {
		slog.Error("import webhooks", "error", err)
		http.Error(w, "unable to import webhooks", http.StatusInternalServerError)
		return
	}
	result := rawResult.(store.WebhookImportResult)
	writeJSON(w, http.StatusOK, map[string]any{"dry_run": request.DryRun, "created": result.Created, "existing": result.Existing})
}

func (s *Server) markAllRead(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	user, ok := s.inboxUser(r)
	if !ok {
		http.Error(w, "reader authentication is required for unread state", http.StatusConflict)
		return
	}
	_, err := s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		return nil, data.MarkAllRead(r.Context(), user.ID, time.Now().UTC())
	})
	if err != nil {
		slog.Error("mark notifications read", "error", err)
		http.Error(w, "unable to update unread state", http.StatusInternalServerError)
		return
	}
	s.publish("")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) markChannelRead(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	user, ok := s.inboxUser(r)
	if !ok {
		http.Error(w, "reader authentication is required for unread state", http.StatusConflict)
		return
	}
	_, err := s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		return nil, data.MarkChannelRead(r.Context(), user, r.PathValue("id"), time.Now().UTC())
	})
	if errors.Is(err, store.ErrForbidden) {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("mark channel read", "error", err)
		http.Error(w, "unable to update unread state", http.StatusInternalServerError)
		return
	}
	s.publish("")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) updateNotificationInbox(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	user, ok := s.inboxUser(r)
	if !ok {
		http.Error(w, "reader authentication is required for inbox state", http.StatusConflict)
		return
	}
	var request notificationInboxRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid inbox action", http.StatusBadRequest)
		return
	}
	switch request.Action {
	case store.InboxMarkRead, store.InboxMarkUnread, store.InboxDismiss, store.InboxRestore:
	default:
		http.Error(w, "invalid inbox action", http.StatusBadRequest)
		return
	}
	notificationID := r.PathValue("id")
	allowed, err := s.store.CanReadNotification(r.Context(), notificationID, user)
	if err != nil {
		slog.Error("authorize inbox action", "error", err)
		http.Error(w, "unable to update inbox state", http.StatusInternalServerError)
		return
	}
	if !allowed {
		http.Error(w, "notification not found", http.StatusNotFound)
		return
	}
	_, err = s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		return nil, data.SetNotificationInboxState(r.Context(), user.ID, notificationID, request.Action, time.Now().UTC())
	})
	if err != nil {
		slog.Error("update notification inbox", "error", err)
		http.Error(w, "unable to update inbox state", http.StatusInternalServerError)
		return
	}
	s.publish(notificationID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) inboxUser(r *http.Request) (store.User, bool) {
	if user, ok := r.Context().Value(userContextKey{}).(store.User); ok {
		return user, true
	}
	if !s.authRequired {
		return store.LocalInboxUser(), true
	}
	return store.User{}, false
}

func encodeNotificationCursor(createdAt int64, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(createdAt, 10) + ":" + id))
}

func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func decodeNotificationCursor(value string) (int64, string, bool) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return 0, "", false
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 || parts[1] == "" || len(parts[1]) > 100 {
		return 0, "", false
	}
	createdAt, err := strconv.ParseInt(parts[0], 10, 64)
	return createdAt, parts[1], err == nil && createdAt > 0
}

func (s *Server) notificationEvents(w http.ResponseWriter, r *http.Request) {
	user, _ := r.Context().Value(userContextKey{}).(store.User)
	allowed, err := s.store.CanReadNotification(r.Context(), r.PathValue("id"), user)
	if err != nil {
		slog.Error("authorize notification events", "error", err)
		http.Error(w, "unable to authorize notification events", http.StatusInternalServerError)
		return
	}
	if !allowed {
		http.Error(w, "notification not found", http.StatusNotFound)
		return
	}
	events, err := s.store.ListNotificationEvents(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotificationNotFound) {
		http.Error(w, "notification not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("list notification events", "error", err)
		http.Error(w, "unable to list notification events", http.StatusInternalServerError)
		return
	}
	result := make([]activityEvent, 0, len(events))
	for _, event := range events {
		title, text, color := eventPresentation(event.RawPayload)
		result = append(result, activityEvent{
			ID: event.ID, State: event.State, Title: title, Text: text,
			Color: color, CreatedAt: event.CreatedAt, Actor: event.Actor,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": result})
}

func (s *Server) setNotificationState(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	actor, ok := r.Context().Value(userContextKey{}).(store.User)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	var request struct {
		State string `json:"state"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid state operation", http.StatusBadRequest)
		return
	}
	err := s.store.SetNotificationState(r.Context(), r.PathValue("id"), actor, request.State)
	switch {
	case errors.Is(err, store.ErrNotificationNotFound):
		http.Error(w, "notification not found", http.StatusNotFound)
	case errors.Is(err, store.ErrForbidden):
		http.Error(w, "operator access is required", http.StatusForbidden)
	case errors.Is(err, store.ErrInvalidTransition):
		http.Error(w, "invalid notification state transition", http.StatusConflict)
	case err != nil:
		slog.Error("update notification state", "error", err)
		http.Error(w, "unable to update notification", http.StatusInternalServerError)
	default:
		s.publish(r.PathValue("id"))
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) setMattermostApproval(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	actor, ok := r.Context().Value(userContextKey{}).(store.User)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	var request struct {
		Decision string `json:"decision"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid approval", http.StatusBadRequest)
		return
	}
	emoji := "white_check_mark"
	if request.Decision == "reject" {
		emoji = "x"
	} else if request.Decision != "approve" {
		http.Error(w, "decision must be approve or reject", http.StatusBadRequest)
		return
	}
	err := s.store.RecordMattermostApproval(r.Context(), r.PathValue("id"), actor, emoji)
	if errors.Is(err, store.ErrNotificationNotFound) {
		http.Error(w, "notification not found", http.StatusNotFound)
		return
	}
	if errors.Is(err, store.ErrForbidden) {
		http.Error(w, "operator access is required", http.StatusForbidden)
		return
	}
	if err != nil {
		http.Error(w, "unable to record approval", http.StatusInternalServerError)
		return
	}
	s.publish(r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

func eventPresentation(raw json.RawMessage) (string, string, string) {
	var payload incomingWebhook
	if json.Unmarshal(raw, &payload) != nil {
		return "", "", ""
	}
	var attachments []struct {
		Title string `json:"title"`
		Text  string `json:"text"`
		Color string `json:"color"`
	}
	if json.Unmarshal(payload.Attachments, &attachments) == nil && len(attachments) > 0 {
		return attachments[0].Title, attachments[0].Text, attachments[0].Color
	}
	return "", payload.Text, ""
}

func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is unavailable", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	controller := http.NewResponseController(w)
	refreshDeadline := func() {
		_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	}
	write := func(value string) bool {
		refreshDeadline()
		if _, err := io.WriteString(w, value); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	events, unsubscribe := s.subscribe()
	defer unsubscribe()
	if !write(": connected\n\n") {
		return
	}

	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-events:
			data, err := json.Marshal(map[string]string{"id": event.ID})
			if err != nil || !write("event: "+event.Kind+"\ndata: "+string(data)+"\n\n") {
				return
			}
		case <-heartbeat.C:
			if !write(": heartbeat\n\n") {
				return
			}
		}
	}
}

func (s *Server) subscribe() (<-chan liveUpdate, func()) {
	updates := make(chan liveUpdate, 16)
	s.subscribersMu.Lock()
	s.subscribers[updates] = struct{}{}
	s.subscribersMu.Unlock()
	return updates, func() {
		s.subscribersMu.Lock()
		delete(s.subscribers, updates)
		s.subscribersMu.Unlock()
	}
}

func (s *Server) publish(id string) {
	s.publishUpdate(liveUpdate{Kind: "notification", ID: id})
}

func (s *Server) publishMessage(id string) {
	s.publishUpdate(liveUpdate{Kind: "channel-message", ID: id})
}

func (s *Server) publishUpdate(update liveUpdate) {
	s.subscribersMu.RLock()
	defer s.subscribersMu.RUnlock()
	for subscriber := range s.subscribers {
		select {
		case subscriber <- update:
		default:
		}
	}
}

func health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) oauthProtectedResourceMetadata(w http.ResponseWriter, _ *http.Request) {
	if s.publicURL == nil {
		http.Error(w, "OAuth protected-resource metadata requires TINTWIRE_PUBLIC_URL", http.StatusNotFound)
		return
	}
	resource := strings.TrimSuffix(s.publicURL.String(), "/") + "/mcp"
	w.Header().Set("Cache-Control", "public, max-age=300")
	metadata := map[string]any{
		"resource":                 resource,
		"resource_name":            "Tintwire MCP",
		"bearer_methods_supported": []string{"header"},
	}
	if s.oauth != nil {
		metadata["authorization_servers"] = []string{s.oauth.issuer}
		metadata["scopes_supported"] = []string{s.oauth.scope}
	}
	writeJSON(w, http.StatusOK, metadata)
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.CheckWritable(ctx); err != nil {
		http.Error(w, "database is not writable", http.StatusServiceUnavailable)
		return
	}
	if s.consensus != nil {
		if !s.consensus.Healthy(30 * time.Second) {
			http.Error(w, "security control quorum is unavailable", http.StatusServiceUnavailable)
			return
		}
	} else {
		valid, err := s.store.ControlLeaseValid(ctx)
		if err != nil || !valid {
			http.Error(w, "security control lease is unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# TYPE tintwire_http_requests_total counter\ntintwire_http_requests_total %d\n", s.requests.Load())
	fmt.Fprintf(w, "# TYPE tintwire_http_errors_total counter\ntintwire_http_errors_total %d\n", s.errors.Load())
	fmt.Fprintln(w, "# HELP tintwire_webhook_rejections_total Rejected incoming webhook requests by reason.")
	fmt.Fprintln(w, "# TYPE tintwire_webhook_rejections_total counter")
	fmt.Fprintf(w, "tintwire_webhook_rejections_total{reason=%q} %d\n", "unknown", s.unknownWebhooks.Load())
	fmt.Fprintf(w, "# TYPE tintwire_uptime_seconds gauge\ntintwire_uptime_seconds %.0f\n", time.Since(s.startedAt).Seconds())
	s.subscribersMu.RLock()
	subscribers := len(s.subscribers)
	s.subscribersMu.RUnlock()
	fmt.Fprintf(w, "# TYPE tintwire_event_subscribers gauge\ntintwire_event_subscribers %d\n", subscribers)
	if s.consensus != nil {
		leader := 0
		if s.consensus.IsLeader() {
			leader = 1
		}
		healthy := 0
		if s.consensus.Healthy(30 * time.Second) {
			healthy = 1
		}
		leaderID, _ := s.consensus.Leader()
		voters, _ := s.consensus.VoterCount()
		fmt.Fprintf(w, "# TYPE tintwire_control_raft_leader gauge\ntintwire_control_raft_leader{leader_id=%q} %d\n", leaderID, leader)
		fmt.Fprintf(w, "# TYPE tintwire_control_raft_quorum_healthy gauge\ntintwire_control_raft_quorum_healthy %d\n", healthy)
		fmt.Fprintf(w, "# TYPE tintwire_control_raft_voters gauge\ntintwire_control_raft_voters %d\n", voters)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	replication, err := s.store.ReplicationMetrics(ctx)
	if err != nil {
		slog.Warn("load replication metrics", "error", err)
		return
	}
	fmt.Fprintln(w, "# TYPE tintwire_replication_operations gauge")
	for origin, count := range replication.Operations {
		fmt.Fprintf(w, "tintwire_replication_operations{origin=%q} %d\n", origin, count)
	}
	fmt.Fprintf(w, "# TYPE tintwire_replication_quarantined gauge\ntintwire_replication_quarantined %d\n", replication.Quarantined)
	var lastSnapshot int64
	if replication.LastSnapshotAt != nil {
		lastSnapshot = replication.LastSnapshotAt.Unix()
	}
	fmt.Fprintf(w, "# TYPE tintwire_replication_snapshot_applications_total counter\ntintwire_replication_snapshot_applications_total %d\n", replication.SnapshotApplications)
	fmt.Fprintf(w, "# TYPE tintwire_replication_snapshot_last_applied_timestamp_seconds gauge\ntintwire_replication_snapshot_last_applied_timestamp_seconds %d\n", lastSnapshot)
	fmt.Fprintln(w, "# TYPE tintwire_replication_peer_up gauge")
	fmt.Fprintln(w, "# TYPE tintwire_replication_peer_failures gauge")
	fmt.Fprintln(w, "# TYPE tintwire_replication_peer_last_success_timestamp_seconds gauge")
	for _, peer := range replication.Peers {
		up := 0
		var lastSuccess int64
		if peer.LastSuccessAt != nil {
			lastSuccess = peer.LastSuccessAt.Unix()
			if peer.ConsecutiveFailures == 0 {
				up = 1
			}
		}
		fmt.Fprintf(w, "tintwire_replication_peer_up{peer=%q,node_id=%q} %d\n", peer.Peer, peer.NodeID, up)
		fmt.Fprintf(w, "tintwire_replication_peer_failures{peer=%q,node_id=%q} %d\n", peer.Peer, peer.NodeID, peer.ConsecutiveFailures)
		fmt.Fprintf(w, "tintwire_replication_peer_last_success_timestamp_seconds{peer=%q,node_id=%q} %d\n", peer.Peer, peer.NodeID, lastSuccess)
	}
}

type observedWriter struct {
	http.ResponseWriter
	status int
}

func (w *observedWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *observedWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *observedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed := &observedWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(observed, r)
		s.requests.Add(1)
		if observed.status >= http.StatusInternalServerError {
			s.errors.Add(1)
		}
	})
}

func serveWeb(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := webFiles.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "web client unavailable", http.StatusInternalServerError)
		return
	}
	document := string(data)
	for _, asset := range []string{"/manifest.webmanifest", "/assets/sentinel.css", "/assets/emoji.js", "/assets/markdown.js", "/assets/app.js"} {
		document = strings.ReplaceAll(document, `"`+asset+`"`, `"`+asset+`?v=`+webAssetVersion+`"`)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = io.WriteString(w, document)
}

func serveAsset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	contentTypes := map[string]string{
		"app.js": "text/javascript; charset=utf-8", "emoji.js": "text/javascript; charset=utf-8", "markdown.js": "text/javascript; charset=utf-8",
		"sentinel.css": "text/css; charset=utf-8",
		"icon.svg":     "image/svg+xml", "icon-192.png": "image/png", "icon-512.png": "image/png",
		"apple-touch-icon.png": "image/png",
	}
	contentType, allowed := contentTypes[name]
	if !allowed {
		http.NotFound(w, r)
		return
	}
	data, err := webFiles.ReadFile("web/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

func serveManifest(w http.ResponseWriter, _ *http.Request) {
	serveWebFile(w, "web/manifest.webmanifest", "application/manifest+json")
}

func serveServiceWorker(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Service-Worker-Allowed", "/")
	w.Header().Set("Cache-Control", "no-cache")
	serveWebFile(w, "web/sw.js", "text/javascript; charset=utf-8")
}

func serveWebFile(w http.ResponseWriter, name, contentType string) {
	data, err := webFiles.ReadFile(name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(data)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func emptyJSONArray(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var values []json.RawMessage
	return json.Unmarshal(raw, &values) == nil && len(values) == 0
}

func validJSONArray(raw json.RawMessage) bool {
	var values []json.RawMessage
	return json.Unmarshal(raw, &values) == nil
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// connect-src additionally names the Tauri desktop client's IPC transport
		// (ipc: on macOS and Linux, http://ipc.localhost on Windows). Browsers
		// never resolve either, so this does not widen the browser surface.
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self' ipc: http://ipc.localhost; img-src 'self' https: data:; style-src 'self' 'unsafe-inline'; script-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}
