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
	pwakit "go.michaelspost.com/pwa-kit"

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
	normalizedContact, err := pwakit.NormalizeContact(contact)
	if err != nil {
		return nil, err
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
	if err := (pwakit.Config{PublicKey: publicKey, PrivateKey: privateKey, Contact: contact}).Validate(); err != nil {
		return nil, err
	}
	client := newPushHTTPClient()
	return &pushService{
		store: data, authRequired: authRequired, publicKey: publicKey, privateKey: privateKey, contact: contact,
		client: client,
	}, nil
}

func newPushHTTPClient() *http.Client {
	return pwakit.NewPublicHTTPClient(20 * time.Second)
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
	request, err := pwakit.DecodeSubscription(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := validateActionTargetURL(request.Endpoint, false); err != nil {
		http.Error(w, "push endpoint must be a public HTTPS URL", http.StatusBadRequest)
		return
	}

	userID := ""
	if user, ok := r.Context().Value(userContextKey{}).(store.User); ok {
		userID = user.ID
	}
	_, err = s.mutateControl(r.Context(), func(data *store.Store) (any, error) {
		return nil, data.SavePushSubscription(r.Context(), store.PushSubscription{
			UserID: userID, Endpoint: request.Endpoint, P256DH: request.Keys.P256dh, Auth: request.Keys.Auth,
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
	users, err := p.store.SubscribedUsers(ctx, notification)
	if err != nil {
		slog.Error("load push recipients", "error", err)
		return
	}
	jobs := make(chan store.User)
	var workers sync.WaitGroup
	for range min(maxConcurrentPushDeliveries, len(users)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for user := range jobs {
				p.deliverToUser(ctx, notification, user)
			}
		}()
	}
	for _, user := range users {
		jobs <- user
	}
	close(jobs)
	workers.Wait()
	if !p.authRequired {
		subscriptions, err := p.store.ListPushSubscriptionsForNotification(ctx, notification.ID, true)
		if err != nil {
			return
		}
		payload, err := json.Marshal(notificationPushPayload(notification))
		if err != nil {
			return
		}
		for _, sub := range subscriptions {
			if sub.UserID == "" {
				p.sendRecorded(ctx, payload, notification.State, sub, notification.ID)
			}
		}
	}

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

func (p *pushService) deliverToUser(ctx context.Context, n store.Notification, user store.User) {
	reason, err := p.store.DeliveryReason(ctx, user, n, time.Now())
	if err != nil {
		return
	}
	if reason != "eligible" {
		p.recordDelivery(ctx, n.ID, user.ID, "", reason, 0)
		return
	}
	subs, err := p.store.UserSubscriptions(ctx, user.ID)
	if err != nil {
		return
	}
	payload, err := json.Marshal(notificationPushPayload(n))
	if err != nil {
		return
	}
	for _, sub := range subs {
		p.sendRecorded(ctx, payload, n.State, sub, n.ID)
	}
}

func (p *pushService) recordDelivery(ctx context.Context, id, userID, endpoint, outcome string, attempt int) {
	if id == "" {
		return
	}
	if err := p.store.RecordDelivery(ctx, id, userID, endpoint, outcome, attempt); err != nil {
		slog.Warn("record push outcome", "error", err)
	}
}

func (p *pushService) send(ctx context.Context, payload []byte, state string, subscription store.PushSubscription) {
	p.sendRecorded(ctx, payload, state, subscription, "")
}

func (p *pushService) sendRecorded(ctx context.Context, payload []byte, state string, subscription store.PushSubscription, id string) {
	urgency := "normal"
	if state == "firing" {
		urgency = "high"
	}
	for attempt := 1; attempt <= 3; attempt++ {
		// Recheck current authority and preferences before every attempt.
		if subscription.UserID != "" {

			active, err := p.store.UserSubscriptions(ctx, subscription.UserID)
			if err != nil {
				return
			}
			found := false
			for _, candidate := range active {
				if candidate.Endpoint == subscription.Endpoint {
					subscription = candidate
					found = true
					break
				}
			}
			if !found {
				return
			}
			user, err := p.store.WorkflowUser(ctx, subscription.UserID)
			if err != nil {
				return
			}
			prefs, err := p.store.AttentionPreferences(ctx, user.ID)
			if err != nil {
				return
			}
			if id != "" {
				values, err := p.store.QueryNotifications(ctx, store.NotificationQuery{ID: id, UserID: user.ID, UserAdmin: user.IsAdmin, ShowDismissed: true})
				if err != nil || len(values) != 1 {
					return
				}
				reason, err := p.store.DeliveryReason(ctx, user, values[0], time.Now())
				if err != nil {
					return
				}
				if reason != "eligible" {
					p.recordDelivery(ctx, id, user.ID, subscription.Endpoint, reason, attempt)
					return
				}
			} else {
				if prefs.Quiet(time.Now(), false) {
					return
				}
				var envelope pushPayload
				if err := json.Unmarshal(payload, &envelope); err != nil {
					return
				}
				var allowed []store.PushSubscription
				var err error
				switch state {
				case "message":
					allowed, err = p.store.ListPushSubscriptionsForChannelMessage(ctx, strings.TrimPrefix(envelope.Tag, "tintwire-"))
				case "command":
					allowed, err = p.store.ListPushSubscriptionsForCommandResponse(ctx, strings.TrimPrefix(envelope.Tag, "tintwire-"))
				default:
					allowed = active
				}
				if err != nil {
					return
				}
				found = false
				for _, candidate := range allowed {
					if candidate.Endpoint == subscription.Endpoint && candidate.UserID == user.ID {
						found = true
						break
					}
				}
				if !found {
					return
				}
			}
		}
		result, err := pwakit.Send(ctx, pwakit.Config{PublicKey: p.publicKey, PrivateKey: p.privateKey, Contact: p.contact}, pwakit.Subscription{Endpoint: subscription.Endpoint, Keys: pwakit.Keys{P256dh: subscription.P256DH, Auth: subscription.Auth}}, payload, pwakit.Options{HTTPClient: p.client, TTL: 86400, Urgency: urgency})
		outcome := "provider_accepted"
		if err != nil {
			outcome = "provider_rejected"
			if result.StatusCode == 0 {
				outcome = "transport_error"
			}
		}
		if result.Expired() {
			outcome = "subscription_expired"
		}
		p.recordDelivery(ctx, id, subscription.UserID, subscription.Endpoint, outcome, attempt)
		if result.Expired() {
			if err := p.store.RemoveUserPushSubscription(ctx, subscription.UserID, subscription.Endpoint); err != nil && !errors.Is(err, store.ErrInvalidCredentials) {
				slog.Warn("remove expired push subscription", "error", err)
			}
			return
		}
		if err == nil {
			return
		}
		if attempt == 3 || (!result.Retryable() && result.StatusCode != 0) {
			slog.Warn("web push delivery failed", "error", err)
			return
		}
		timer := time.NewTimer(time.Duration(attempt) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
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
