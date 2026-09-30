package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/kilo666mj/tintwire/internal/store"
)

func (s *Server) workflowRoutes(mux *http.ServeMux) {
	read := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, s.requireReader(h)) }
	write := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, s.requireReader(s.requireControlAuthority(func(w http.ResponseWriter, r *http.Request) {
			if !s.sameOrigin(r) {
				http.Error(w, "cross-origin request rejected", http.StatusForbidden)
				return
			}
			if s.consensus != nil {
				http.Error(w, "workflows require standalone SQLite or shared PostgreSQL", http.StatusConflict)
				return
			}
			h(w, r)
		})))
	}
	read("GET /api/v1/desktop/alerts", s.desktopAlerts)
	read("GET /api/v1/incidents", s.incidents)
	write("POST /api/v1/incidents/acknowledge", s.acknowledgeIncident)
	read("GET /api/v1/monitors", s.monitors)
	write("POST /api/v1/monitors", s.monitors)
	write("DELETE /api/v1/monitors/{id}", s.monitors)
	write("POST /api/v1/playground", s.playground)
	write("PUT /api/v1/channels/{id}/agent-binding", s.bindAgent)
	read("GET /api/v1/channels/{id}/agent-commands", s.listAgentCommands)
	write("POST /api/v1/agent-commands/{id}/control", s.controlAgentCommand)
	read("GET /api/v1/attention", s.getAttention)
	write("PUT /api/v1/attention", s.putAttention)
	write("PUT /api/v1/notifications/{id}/snooze", s.snoozeNotification)
	write("PUT /api/v1/saved-views/{id}/digest", s.saveDigest)
	read("GET /api/v1/notifications/{id}/delivery", s.deliveryInspector)
	write("POST /api/v1/notifications/{id}/desktop-delivery", s.recordDesktopDelivery)
}

func workflowDecode(w http.ResponseWriter, r *http.Request, target any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil || d.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid JSON payload", http.StatusBadRequest)
		return false
	}
	return true
}

func workflowError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, store.ErrForbidden) {
		status = http.StatusForbidden
	}
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrNotificationNotFound) {
		status = http.StatusNotFound
	}
	if status == http.StatusBadRequest {
		slog.Warn("workflow request failed", "error", err)
	}
	http.Error(w, "Unable to complete request: check input and current access.", status)
}

func (s *Server) getAttention(w http.ResponseWriter, r *http.Request) {
	u, _ := s.inboxUser(r)
	p, err := s.store.AttentionPreferences(r.Context(), u.ID)
	if err != nil {
		workflowError(w, err)
		return
	}
	schedules, err := s.store.UserSchedules(r.Context(), u.ID)
	if err != nil {
		workflowError(w, err)
		return
	}
	digests, err := s.store.Digests(r.Context(), u.ID)
	if err != nil {
		workflowError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"preferences": p, "schedules": schedules, "digests": digests})
}

func (s *Server) putAttention(w http.ResponseWriter, r *http.Request) {
	u, _ := s.inboxUser(r)
	var p store.AttentionPreferences
	if !workflowDecode(w, r, &p) {
		return
	}
	if err := p.Validate(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err := s.store.SaveAttentionPreferences(r.Context(), u.ID, p); err != nil {
		workflowError(w, err)
		return
	}
	writeJSON(w, 200, p)
}

func (s *Server) snoozeNotification(w http.ResponseWriter, r *http.Request) {
	u, _ := s.inboxUser(r)
	var input struct {
		Until time.Time `json:"until"`
	}
	if !workflowDecode(w, r, &input) {
		return
	}
	if err := s.store.Snooze(r.Context(), u, r.PathValue("id"), input.Until); err != nil {
		workflowError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) saveDigest(w http.ResponseWriter, r *http.Request) {
	u, _ := s.inboxUser(r)
	var input store.DigestConfig
	if !workflowDecode(w, r, &input) {
		return
	}
	if err := s.store.SaveDigest(r.Context(), u.ID, r.PathValue("id"), input); err != nil {
		workflowError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) deliveryInspector(w http.ResponseWriter, r *http.Request) {
	u, _ := s.inboxUser(r)
	id := r.PathValue("id")
	values, err := s.store.QueryNotifications(r.Context(), store.NotificationQuery{ID: id, UserID: u.ID, UserAdmin: u.IsAdmin, ShowDismissed: true})
	if err != nil {
		workflowError(w, err)
		return
	}
	if len(values) != 1 {
		http.NotFound(w, r)
		return
	}
	events, err := s.store.DeliveryEvents(r.Context(), id, u)
	if err != nil {
		workflowError(w, err)
		return
	}
	reason, err := s.store.DeliveryReason(r.Context(), u, values[0], time.Now())
	if err != nil {
		workflowError(w, err)
		return
	}
	subs, err := s.store.UserSubscriptions(r.Context(), u.ID)
	if err != nil {
		workflowError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"current_policy": reason, "push_configured": s.push != nil, "device_count": len(subs), "events": events, "receipt_note": "Provider acceptance does not confirm device receipt. History starts when this feature is installed."})
}

func (s *Server) runWorkflows(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			workCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
			s.workflowTick(workCtx, time.Now())
			cancel()
		}
	}
}

func (s *Server) workflowTick(ctx context.Context, now time.Time) {
	ids, err := s.store.CheckMonitors(ctx, now)
	if err != nil {
		slog.Warn("check producer monitors", "error", err)
	}
	for _, id := range ids {
		s.publish(id)
		if s.push != nil {
			values, err := s.store.QueryNotifications(ctx, store.NotificationQuery{ID: id})
			if err == nil && len(values) == 1 {
				s.push.deliver(values[0])
			}
		}
	}

	jobs, err := s.store.ClaimSchedules(ctx, now)
	if err != nil {
		slog.Error("claim workflow schedules", "error", err)
		return
	}
	for _, job := range jobs {
		next, err := s.runSchedule(ctx, job, now)
		if err != nil {
			slog.Warn("run workflow schedule", "kind", job.Kind, "error", err)
			next = now.Add(time.Minute)
		}
		if err := s.store.FinishSchedule(ctx, job, next); err != nil {
			slog.Warn("finish workflow schedule", "error", err)
		}
	}
}

func (s *Server) runSchedule(ctx context.Context, job store.WorkflowSchedule, now time.Time) (time.Time, error) {
	u, err := s.store.WorkflowUser(ctx, job.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	switch job.Kind {
	case "snooze":
		values, err := s.store.QueryNotifications(ctx, store.NotificationQuery{ID: job.Target, UserID: u.ID, UserAdmin: u.IsAdmin})
		if err != nil {
			return time.Time{}, err
		}
		if len(values) == 0 {
			return time.Time{}, nil
		}
		p, err := s.store.AttentionPreferences(ctx, u.ID)
		if err != nil {
			return time.Time{}, err
		}
		if p.Quiet(now, false) {
			return now.Add(time.Minute), nil
		}
		if err := s.store.SetNotificationInboxState(ctx, u.ID, job.Target, store.InboxMarkUnread, now); err != nil {
			return time.Time{}, err
		}
		fresh, err := s.store.SaveAttentionAlert(ctx, job, "Reminder", notificationPushPayload(values[0]).Body, now)
		if err != nil {
			return time.Time{}, err
		}
		s.publish(job.Target)
		if s.push != nil && fresh {
			s.push.deliverToUser(ctx, values[0], u)
		}
		return time.Time{}, nil
	case "digest":
		var config store.DigestConfig
		if err := json.Unmarshal([]byte(job.Config), &config); err != nil {
			return time.Time{}, err
		}
		p, err := s.store.AttentionPreferences(ctx, u.ID)
		if err != nil {
			return time.Time{}, err
		}
		if p.Quiet(now, false) {
			return now.Add(time.Minute), nil
		}
		d, err := s.store.CreateDigest(ctx, job, now)
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, nil
		}
		if err != nil {
			return time.Time{}, err
		}
		if d.Count == 0 {
			return config.Next(now)
		}
		fresh, err := s.store.SaveAttentionAlert(ctx, job, "Digest · "+d.Title, fmt.Sprintf("%d matching notifications updated in the past 24 hours", d.Count), now)
		if err != nil {
			return time.Time{}, err
		}
		if s.push != nil && d.Count > 0 && fresh {
			subs, err := s.store.UserSubscriptions(ctx, u.ID)
			if err != nil {
				return time.Time{}, err
			}
			payload, err := json.Marshal(pushPayload{Title: "Digest · " + d.Title, Body: fmt.Sprintf("%d matching notifications updated in the past 24 hours", d.Count), URL: "/?view=" + url.QueryEscape(d.ViewID), Tag: d.ID, State: "digest", Timestamp: now.UnixMilli()})
			if err != nil {
				return time.Time{}, err
			}
			s.push.deliverSubscriptions(ctx, payload, "digest", subs)
		}
		return config.Next(now)
	}
	return time.Time{}, nil
}
