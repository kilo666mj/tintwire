package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/kilo666mj/tintwire/internal/store"
)

const maxPushSubscriptionBody = 16 << 10
const maxConcurrentPushDeliveries = 4

type pushService struct {
	store        *store.Store
	authRequired bool
	publicKey    string
	privateKey   string
	contact      string
	client       *http.Client
}

type pushSubscriptionRequest struct {
	Endpoint       string `json:"endpoint"`
	ExpirationTime *int64 `json:"expirationTime"`
	Keys           struct {
		P256DH string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

type pushPayload struct {
	Title     string `json:"title"`
	Body      string `json:"body,omitempty"`
	URL       string `json:"url"`
	Tag       string `json:"tag"`
	State     string `json:"state"`
	Timestamp int64  `json:"timestamp"`
}

func newPushService(data *store.Store, contact string, authRequired bool) (*pushService, error) {
	contact = strings.TrimSpace(contact)
	if contact == "" {
		return nil, nil
	}
	normalizedContact, ok := normalizeVAPIDContact(contact)
	if !ok {
		return nil, errors.New("VAPID contact must be an email address, mailto: address, or HTTPS URL")
	}
	contact = normalizedContact

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	publicKey, publicOK, err := data.Setting(ctx, "vapid_public_key")
	if err != nil {
		return nil, err
	}
	privateKey, privateOK, err := data.Setting(ctx, "vapid_private_key")
	if err != nil {
		return nil, err
	}
	if !publicOK || !privateOK || publicKey == "" || privateKey == "" {
		privateKey, publicKey, err = webpush.GenerateVAPIDKeys()
		if err != nil {
			return nil, err
		}
		if err := data.SaveSettings(ctx, map[string]string{
			"vapid_public_key": publicKey, "vapid_private_key": privateKey,
		}); err != nil {
			return nil, err
		}
	}
	client := actionHTTPClient(false)
	client.Timeout = 20 * time.Second
	return &pushService{
		store: data, authRequired: authRequired, publicKey: publicKey, privateKey: privateKey, contact: contact,
		client: client,
	}, nil
}

func normalizeVAPIDContact(value string) (string, bool) {
	parsed, err := url.Parse(value)
	if err != nil {
		return "", false
	}
	if parsed.Scheme == "mailto" {
		return parsed.Opaque, strings.Contains(parsed.Opaque, "@")
	}
	if parsed.Scheme == "https" && parsed.Host != "" {
		return value, true
	}
	if parsed.Scheme == "" && strings.Contains(value, "@") && !strings.ContainsAny(value, " /\t\r\n") {
		return value, true
	}
	return "", false
}

func (s *Server) pushConfig(w http.ResponseWriter, _ *http.Request) {
	if s.push == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": true, "public_key": s.push.publicKey,
	})
}

func (s *Server) savePushSubscription(w http.ResponseWriter, r *http.Request) {
	if s.push == nil {
		http.Error(w, "web push is not configured", http.StatusServiceUnavailable)
		return
	}
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	var request pushSubscriptionRequest
	if err := decodePushRequest(w, r, &request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	endpoint, err := url.Parse(request.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" ||
		request.Keys.P256DH == "" || request.Keys.Auth == "" {
		http.Error(w, "a valid HTTPS endpoint and both subscription keys are required", http.StatusBadRequest)
		return
	}
	if err := validateActionTargetURL(request.Endpoint, false); err != nil {
		http.Error(w, "push endpoint must be a public HTTPS URL", http.StatusBadRequest)
		return
	}
	if len(request.Endpoint) > 4096 || len(request.Keys.P256DH) > 512 || len(request.Keys.Auth) > 512 {
		http.Error(w, "subscription is too large", http.StatusBadRequest)
		return
	}
	userID := ""
	if user, ok := r.Context().Value(userContextKey{}).(store.User); ok {
		userID = user.ID
	}
	_, err = s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		return nil, data.SavePushSubscription(r.Context(), store.PushSubscription{
			UserID: userID, Endpoint: request.Endpoint, P256DH: request.Keys.P256DH, Auth: request.Keys.Auth,
		})
	})
	if err != nil {
		if errors.Is(err, store.ErrInvalidCredentials) {
			http.Error(w, "subscription belongs to another user", http.StatusConflict)
			return
		}
		slog.Error("save push subscription", "error", err)
		http.Error(w, "unable to save subscription", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) removePushSubscription(w http.ResponseWriter, r *http.Request) {
	if s.push == nil {
		http.Error(w, "web push is not configured", http.StatusServiceUnavailable)
		return
	}
	if !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	var request struct {
		Endpoint string `json:"endpoint"`
	}
	if err := decodePushRequest(w, r, &request); err != nil || request.Endpoint == "" {
		http.Error(w, "endpoint is required", http.StatusBadRequest)
		return
	}
	userID := ""
	if user, ok := r.Context().Value(userContextKey{}).(store.User); ok {
		userID = user.ID
	}
	_, err := s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		return nil, data.RemoveUserPushSubscription(r.Context(), userID, request.Endpoint)
	})
	if errors.Is(err, store.ErrInvalidCredentials) {
		http.Error(w, "subscription not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("remove push subscription", "error", err)
		http.Error(w, "unable to remove subscription", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func decodePushRequest(w http.ResponseWriter, r *http.Request, destination any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxPushSubscriptionBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return errors.New("invalid subscription")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("invalid subscription")
	}
	return nil
}

func (s *Server) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	if s.publicURL != nil {
		return strings.EqualFold(parsed.Scheme, s.publicURL.Scheme) && strings.EqualFold(parsed.Host, s.publicURL.Host) && strings.EqualFold(r.Host, s.publicURL.Host)
	}
	return strings.EqualFold(parsed.Host, r.Host)
}

func (s *Server) secureCookies(r *http.Request) bool {
	return r.TLS != nil || (s.publicURL != nil && s.publicURL.Scheme == "https")
}

func (p *pushService) deliver(notification store.Notification) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	subscriptions, err := p.store.ListPushSubscriptionsForNotification(ctx, notification.ID, !p.authRequired)
	if err != nil {
		slog.Error("load push subscriptions", "error", err)
		return
	}
	if len(subscriptions) == 0 {
		return
	}
	payload, err := json.Marshal(notificationPushPayload(notification))
	if err != nil {
		return
	}
	p.deliverSubscriptions(ctx, payload, notification.State, subscriptions)
}

func (p *pushService) deliverMessage(message store.ChannelMessage) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	subscriptions, err := p.store.ListPushSubscriptionsForChannelMessage(ctx, message.ID)
	if err != nil {
		slog.Error("load message push subscriptions", "error", err)
		return
	}
	if len(subscriptions) == 0 {
		return
	}
	payload, err := json.Marshal(pushPayload{
		Title: "Message · " + message.ChannelName,
		Body:  truncatePushText(message.Author+": "+strings.TrimSpace(message.Text), 180),
		URL:   "/?message=" + url.QueryEscape(message.ID), Tag: "tintwire-" + message.ID,
		State: "message", Timestamp: message.CreatedAt.UnixMilli(),
	})
	if err != nil {
		return
	}
	p.deliverSubscriptions(ctx, payload, "message", subscriptions)
}

func (p *pushService) deliverCommandResponse(response store.SlashCommandResponse, channelName string) {
	if response.ResponseType != "in_channel" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	subscriptions, err := p.store.ListPushSubscriptionsForCommandResponse(ctx, response.ID)
	if err != nil {
		slog.Error("load command push subscriptions", "error", err)
		return
	}
	payload, err := json.Marshal(pushPayload{
		Title: "Command · " + channelName, Body: truncatePushText(strings.TrimSpace(response.Text), 180),
		URL: "/?channel=" + url.QueryEscape(channelName), Tag: "tintwire-" + response.ID,
		State: "command", Timestamp: response.CreatedAt.UnixMilli(),
	})
	if err != nil {
		return
	}
	p.deliverSubscriptions(ctx, payload, "command", subscriptions)
}

func (p *pushService) deliverSubscriptions(ctx context.Context, payload []byte, state string, subscriptions []store.PushSubscription) {
	if len(subscriptions) == 0 {
		return
	}
	jobs := make(chan store.PushSubscription)
	var workers sync.WaitGroup
	workerCount := min(maxConcurrentPushDeliveries, len(subscriptions))
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for subscription := range jobs {
				p.send(ctx, payload, state, subscription)
			}
		}()
	}
	for _, subscription := range subscriptions {
		jobs <- subscription
	}
	close(jobs)
	workers.Wait()
}

func (p *pushService) send(ctx context.Context, payload []byte, state string, subscription store.PushSubscription) {
	urgency := webpush.UrgencyNormal
	if state == "firing" {
		urgency = webpush.UrgencyHigh
	}
	response, err := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{
		Endpoint: subscription.Endpoint,
		Keys:     webpush.Keys{P256dh: subscription.P256DH, Auth: subscription.Auth},
	}, &webpush.Options{
		HTTPClient: p.client, VAPIDPublicKey: p.publicKey, VAPIDPrivateKey: p.privateKey,
		Subscriber: p.contact, TTL: 86400, Urgency: urgency,
	})
	if err != nil {
		// Push endpoints are secret capabilities and can be embedded in transport
		// errors, so delivery failures deliberately omit the error text.
		slog.Warn("web push delivery failed")
		return
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
	if response.StatusCode == http.StatusGone || response.StatusCode == http.StatusNotFound {
		if err := p.store.RemoveUserPushSubscription(ctx, subscription.UserID, subscription.Endpoint); err != nil && !errors.Is(err, store.ErrInvalidCredentials) {
			slog.Warn("remove expired push subscription", "error", err)
		}
		return
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		slog.Warn("web push service rejected notification", "status", response.StatusCode)
	}
}

func notificationPushPayload(notification store.Notification) pushPayload {
	label := ""
	switch notification.State {
	case "firing":
		label = "Firing"
	case "resolved":
		label = "Resolved"
	}
	query := url.Values{"channel": {notification.ChannelName}, "notification": {notification.ID}}
	createdAt := notification.UpdatedAt
	if createdAt.IsZero() {
		createdAt = notification.CreatedAt
	}
	var timestamp int64
	if !createdAt.IsZero() {
		timestamp = createdAt.UnixMilli()
	}
	subject, body := pushPresentation(notification)
	title := notification.ChannelName
	if subject != "" {
		parts := make([]string, 0, 3)
		if label != "" {
			parts = append(parts, label)
		}
		parts = append(parts, subject, notification.ChannelName)
		title = truncatePushText(strings.Join(parts, " · "), 100)
	}
	return pushPayload{
		Title: title, Body: body, URL: "/?" + query.Encode(),
		Tag: "tintwire-" + notification.ID, State: notification.State,
		Timestamp: timestamp,
	}
}

func pushPresentation(notification store.Notification) (string, string) {
	var card nativeCard
	if json.Unmarshal(notification.Card, &card) == nil {
		if subject := strings.TrimSpace(card.Title); subject != "" {
			body := firstDistinctPushBody(subject, card.Summary, notification.Text)
			for _, field := range card.Fields {
				body = firstDistinctPushBody(subject, body, field.Label+": "+field.Value)
			}
			for _, metric := range card.Metrics {
				body = firstDistinctPushBody(subject, body, fmt.Sprintf("%s: %v", metric.Label, metric.Value))
			}
			if len(card.Rows) > 0 {
				body = firstDistinctPushBody(subject, body, fmt.Sprintf("%d items · %s", len(card.Rows), card.Rows[0].Primary))
			}
			return normalizePushSubjectAndBody(subject, body)
		}
	}

	var attachments []struct {
		Title    string `json:"title"`
		Text     string `json:"text"`
		Fallback string `json:"fallback"`
		Fields   []struct {
			Title string `json:"title"`
			Value string `json:"value"`
		} `json:"fields"`
	}
	if json.Unmarshal(notification.Attachments, &attachments) == nil && len(attachments) > 0 {
		if subject := strings.TrimSpace(attachments[0].Title); subject != "" {
			body := firstDistinctPushBody(subject, attachments[0].Text, notification.Text, attachments[0].Fallback)
			for _, field := range attachments[0].Fields {
				body = firstDistinctPushBody(subject, body, field.Title+": "+field.Value)
			}
			return normalizePushSubjectAndBody(subject, body)
		}
		if summary := strings.TrimSpace(attachments[0].Fallback); summary != "" {
			return "", truncatePushText(summary, 180)
		}
	}
	return "", truncatePushText(strings.TrimSpace(notification.Text), 180)
}

func firstDistinctPushBody(subject string, candidates ...string) string {
	for _, candidate := range candidates {
		candidate = strings.Join(strings.Fields(candidate), " ")
		if candidate != "" && !strings.EqualFold(strings.Join(strings.Fields(subject), " "), candidate) {
			return candidate
		}
	}
	return ""
}

func normalizePushSubjectAndBody(subject, body string) (string, string) {
	subject = strings.Join(strings.Fields(withoutLifecyclePrefix(subject)), " ")
	body = strings.Join(strings.Fields(body), " ")
	if strings.EqualFold(subject, body) {
		body = ""
	}
	return truncatePushText(subject, 100), truncatePushText(body, 180)
}

func withoutLifecyclePrefix(value string) string {
	closing := strings.IndexByte(value, ']')
	if closing < 0 {
		return value
	}
	prefix := strings.ToUpper(value[:closing+1])
	if strings.HasPrefix(prefix, "[FIRING") || prefix == "[RESOLVED]" {
		return strings.TrimSpace(value[closing+1:])
	}
	return value
}

func truncatePushText(value string, maxRunes int) string {
	value = strings.Join(strings.Fields(value), " ")
	if utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	runes := []rune(value)
	return strings.TrimSpace(string(runes[:maxRunes-1])) + "…"
}
