package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/kilo666mj/tintwire/internal/store"
)

func (s *Server) incidents(w http.ResponseWriter, r *http.Request) {
	u, _ := s.inboxUser(r)
	if channel := r.URL.Query().Get("channel_id"); channel != "" {
		values, err := s.store.IncidentNotifications(r.Context(), u, channel, r.URL.Query().Get("key"))
		if err != nil {
			workflowError(w, err)
			return
		}
		sanitizeNotificationCards(values)
		sanitizeMattermostActions(values, nil)
		s.proxyNotificationImages(values)
		writeJSON(w, 200, map[string]any{"notifications": values})
		return
	}
	values, err := s.store.Incidents(r.Context(), u)
	if err != nil {
		workflowError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"incidents": values})
}

func (s *Server) acknowledgeIncident(w http.ResponseWriter, r *http.Request) {
	u, _ := s.inboxUser(r)
	var input struct {
		ChannelID string `json:"channel_id"`
		Key       string `json:"key"`
	}
	if !workflowDecode(w, r, &input) {
		return
	}
	if err := s.store.AcknowledgeIncident(r.Context(), u, input.ChannelID, input.Key); err != nil {
		workflowError(w, err)
		return
	}
	s.publish("")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) monitors(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	if r.Method == "POST" {
		var input store.ProducerMonitor
		if !workflowDecode(w, r, &input) {
			return
		}
		m, err := s.store.SaveMonitor(r.Context(), u, input)
		if err != nil {
			workflowError(w, err)
			return
		}
		writeJSON(w, 201, m)
		return
	}
	if r.Method == "DELETE" {
		if err := s.store.DeleteMonitor(r.Context(), u, r.PathValue("id")); err != nil {
			workflowError(w, err)
			return
		}
		w.WriteHeader(204)
		return
	}
	values, err := s.store.Monitors(r.Context(), u)
	if err != nil {
		workflowError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"monitors": values})
}

func (s *Server) playground(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	var input struct {
		Format    string          `json:"format"`
		Payload   json.RawMessage `json:"payload"`
		ChannelID string          `json:"channel_id"`
		Publish   bool            `json:"publish"`
	}
	if !workflowDecode(w, r, &input) {
		return
	}
	var n store.IncomingNotification
	switch input.Format {
	case "native":
		var card nativeCard
		d := json.NewDecoder(strings.NewReader(string(input.Payload)))
		d.DisallowUnknownFields()
		if err := d.Decode(&card); err != nil {
			http.Error(w, "Invalid native card: "+err.Error(), 400)
			return
		}
		if err := validateNativeCard(card); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		n.State, _ = nativeCardLifecycle(card)
		card.State, card.LifecycleKey = "", ""
		card.Channel = ""
		n.Text, n.Username = card.Summary, card.Source
		var err error
		n.Card, err = s.protectNativeActionContexts(card)
		if err != nil {
			workflowError(w, err)
			return
		}
		n.RawPayload = n.Card
	case "webhook":
		var payload incomingWebhook
		if err := json.Unmarshal(input.Payload, &payload); err != nil {
			http.Error(w, "Invalid webhook JSON", 400)
			return
		}
		if (len(payload.Attachments) > 0 && !validJSONArray(payload.Attachments)) || (len(payload.Blocks) > 0 && !validJSONArray(payload.Blocks)) {
			http.Error(w, "attachments and blocks must be arrays", 400)
			return
		}
		n.Text = strings.TrimSpace(payload.Text + "\n" + normalizeSlackBlocks(payload.Blocks))
		n.Username = payload.Username
		n.Attachments = payload.Attachments
		n.RawPayload = input.Payload
		if n.Text == "" && emptyJSONArray(n.Attachments) {
			http.Error(w, "text, attachments or blocks are required", 400)
			return
		}
		n.State, _ = alertmanagerLifecycle("", payload.Attachments)
	default:
		http.Error(w, "format must be native or webhook", 400)
		return
	}
	preview := store.Notification{ID: "preview", Text: n.Text, Username: n.Username, Card: n.Card, Attachments: n.Attachments, State: n.State, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if input.Publish {
		if err := s.store.RequireChannelOperator(r.Context(), user, input.ChannelID); err != nil {
			workflowError(w, err)
			return
		}
		var err error
		preview, err = s.store.CreatePlaygroundNotification(r.Context(), user, input.ChannelID, n)
		if err != nil {
			workflowError(w, err)
			return
		}
		s.publish(preview.ID)
		if s.push != nil {
			go s.push.deliver(preview)
		}
	}
	values := []store.Notification{preview}
	sanitizeNotificationCards(values)
	sanitizeMattermostActions(values, nil)
	writeJSON(w, 200, map[string]any{"notification": values[0], "published": input.Publish})
}

func (s *Server) desktopAlerts(w http.ResponseWriter, r *http.Request) {
	u, _ := s.inboxUser(r)
	cursor := r.URL.Query().Get("after")
	if cursor == "" {
		writeJSON(w, 200, map[string]any{"alerts": []any{}, "cursor": encodeNotificationCursor(time.Now().UnixMilli(), "!"), "has_more": false})
		return
	}
	at, id, ok := decodeNotificationCursor(cursor)
	if !ok {
		http.Error(w, "invalid cursor", 400)
		return
	}
	arrivals, err := s.store.DesktopArrivals(r.Context(), u, at, id)
	if err != nil {
		workflowError(w, err)
		return
	}
	alerts := []map[string]any{}
	for _, v := range arrivals {
		cursor = encodeNotificationCursor(v.At, v.ID)
		var p pushPayload
		if v.Kind == "notification" {
			values, err := s.store.QueryNotifications(r.Context(), store.NotificationQuery{ID: v.ID, UserID: u.ID, UserAdmin: u.IsAdmin})
			if err != nil {
				workflowError(w, err)
				return
			}
			if len(values) != 1 {
				continue
			}
			reason, err := s.store.DeliveryReason(r.Context(), u, values[0], time.Now())
			if err != nil {
				workflowError(w, err)
				return
			}
			if reason != "eligible" {
				continue
			}
			p = notificationPushPayload(values[0])
		} else if v.Kind == "attention" {
			alert, err := s.store.AttentionAlert(r.Context(), u.ID, v.ID)
			if err != nil {
				workflowError(w, err)
				return
			}
			prefs, err := s.store.AttentionPreferences(r.Context(), u.ID)
			if err != nil {
				workflowError(w, err)
				return
			}
			if prefs.Quiet(time.Now(), false) {
				continue
			}
			if alert.Kind == "snooze" {
				values, err := s.store.QueryNotifications(r.Context(), store.NotificationQuery{ID: alert.Target, UserID: u.ID, UserAdmin: u.IsAdmin})
				if err != nil {
					workflowError(w, err)
					return
				}
				if len(values) != 1 {
					continue
				}
				reason, err := s.store.DeliveryReason(r.Context(), u, values[0], time.Now())
				if err != nil {
					workflowError(w, err)
					return
				}
				if reason != "eligible" {
					continue
				}
			}
			p = pushPayload{Title: alert.Title, Body: alert.Body, State: "reminder"}
		} else {
			m, err := s.store.ChannelMessageByID(r.Context(), u, v.ID)
			if err != nil {
				continue
			}
			level, err := s.store.ChannelNotificationPreference(r.Context(), u, m.ChannelID)
			if err != nil || level != "all" {
				continue
			}
			prefs, err := s.store.AttentionPreferences(r.Context(), u.ID)
			if err != nil {
				workflowError(w, err)
				return
			}
			if prefs.Quiet(time.Now(), false) {
				continue
			}
			p = pushPayload{Title: "Message · " + m.ChannelName, Body: truncatePushText(m.Author+": "+m.Text, 180), State: "message"}
		}
		alerts = append(alerts, map[string]any{"id": v.ID, "version": v.At, "title": p.Title, "body": p.Body, "urgent": p.State == "firing"})
	}
	writeJSON(w, 200, map[string]any{"alerts": alerts, "cursor": cursor, "has_more": len(arrivals) == 200})
}

func (s *Server) recordDesktopDelivery(w http.ResponseWriter, r *http.Request) {
	u, _ := s.inboxUser(r)
	var input struct {
		Outcome string `json:"outcome"`
	}
	if !workflowDecode(w, r, &input) {
		return
	}
	if input.Outcome != "desktop_accepted" && input.Outcome != "desktop_failed" {
		http.Error(w, "invalid desktop outcome", 400)
		return
	}
	allowed, err := s.store.CanReadNotification(r.Context(), r.PathValue("id"), u)
	if err != nil {
		workflowError(w, err)
		return
	}
	if !allowed {
		http.Error(w, "notification access required", http.StatusForbidden)
		return
	}
	if err := s.store.RecordDelivery(r.Context(), r.PathValue("id"), u.ID, "desktop", input.Outcome, 1); err != nil {
		workflowError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
