package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kilo666mj/tintwire/internal/store"
)

const maxProxyImageBytes = 8 << 20

type imageSource struct {
	Origin     string `json:"origin"`
	Upstream   string `json:"upstream"`
	PathPrefix string `json:"path_prefix"`
}

type imageProxy struct {
	sources []imageSource
	client  *http.Client
	slots   chan struct{}
}

var imageFilename = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*\.(webp|png|jpg|jpeg|gif)$`)

func newImageProxy(raw string, authRequired bool) (*imageProxy, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var sources []imageSource
	if err := json.Unmarshal([]byte(raw), &sources); err != nil {
		return nil, errors.New("invalid image proxy sources JSON")
	}
	if len(sources) == 0 {
		return nil, nil
	}
	if !authRequired {
		return nil, errors.New("image proxy requires reader authentication")
	}
	for _, source := range sources {
		for _, origin := range []string{source.Origin, source.Upstream} {
			u, err := url.Parse(origin)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
				return nil, errors.New("image proxy mappings require HTTP(S) origins without paths or credentials")
			}
		}
		if !strings.HasPrefix(source.PathPrefix, "/") || strings.ContainsAny(source.PathPrefix, "%?#\\") || strings.Contains(source.PathPrefix, "..") {
			return nil, errors.New("image proxy requires an absolute path prefix without traversal")
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // Explicit LAN mappings must not use ambient outbound proxies.
	transport.MaxResponseHeaderBytes = 32 << 10
	return &imageProxy{sources: sources, slots: make(chan struct{}, 8), client: &http.Client{
		Transport: transport, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// resolve accepts only a configured origin and a single raster filename after
// its path prefix. It never uses a caller-supplied upstream host or query string.
func (p *imageProxy) resolve(raw string) (string, string, bool) {
	if p == nil {
		return "", "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || strings.Contains(u.Path, "..") {
		return "", "", false
	}
	for _, source := range p.sources {
		if u.Scheme+"://"+u.Host != source.Origin || !strings.HasPrefix(u.Path, source.PathPrefix) {
			continue
		}
		if !imageFilename.MatchString(strings.TrimPrefix(u.Path, source.PathPrefix)) {
			continue
		}
		origin, _ := url.Parse(source.Origin)
		return source.Upstream + u.Path, origin.Host, true
	}
	return "", "", false
}

func (s *Server) proxyNotificationImages(notifications []store.Notification) {
	if s.imageProxy == nil {
		return
	}
	for i := range notifications {
		var card map[string]json.RawMessage
		if json.Unmarshal(notifications[i].Card, &card) != nil {
			continue
		}
		var images []cardImage
		if json.Unmarshal(card["images"], &images) != nil {
			continue
		}
		changed := false
		for j := range images {
			if _, _, ok := s.imageProxy.resolve(images[j].URL); ok {
				images[j].URL = "/api/v1/notifications/" + url.PathEscape(notifications[i].ID) + "/images/" + strconv.Itoa(j)
				changed = true
			}
		}
		if changed {
			encoded, err := json.Marshal(images)
			if err != nil {
				continue
			}
			card["images"] = encoded
			if encoded, err := json.Marshal(card); err == nil {
				notifications[i].Card = encoded
			}
		}
	}
}

func (s *Server) notificationImage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	user, _ := r.Context().Value(userContextKey{}).(store.User)
	if s.imageProxy == nil || user.ID == "" {
		http.NotFound(w, r)
		return
	}
	card, err := s.store.NotificationCardForReader(r.Context(), r.PathValue("id"), user)
	if errors.Is(err, store.ErrNotificationNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "unable to read notification", http.StatusInternalServerError)
		return
	}
	var value nativeCard
	index, indexErr := strconv.Atoi(r.PathValue("index"))
	if json.Unmarshal(card, &value) != nil || indexErr != nil || index < 0 || index >= len(value.Images) {
		http.NotFound(w, r)
		return
	}
	upstream, host, ok := s.imageProxy.resolve(value.Images[index].URL)
	if !ok {
		http.NotFound(w, r)
		return
	}
	select {
	case s.imageProxy.slots <- struct{}{}:
		defer func() { <-s.imageProxy.slots }()
	default:
		http.Error(w, "image proxy busy", http.StatusServiceUnavailable)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstream, nil)
	if err != nil {
		http.Error(w, "invalid image source", http.StatusBadGateway)
		return
	}
	req.Host = host
	req.Header.Set("Accept", "image/webp,image/png,image/jpeg,image/gif")
	resp, err := s.imageProxy.client.Do(req)
	if err != nil {
		http.Error(w, "image source unavailable", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		http.NotFound(w, r)
		return
	}
	if resp.StatusCode != http.StatusOK || resp.ContentLength > maxProxyImageBytes {
		http.Error(w, "invalid image response", http.StatusBadGateway)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProxyImageBytes+1))
	if err != nil || len(body) > maxProxyImageBytes {
		http.Error(w, "invalid image response", http.StatusBadGateway)
		return
	}
	contentType := http.DetectContentType(body)
	switch contentType {
	case "image/webp", "image/png", "image/jpeg", "image/gif":
	default:
		http.Error(w, "unsupported image format", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}
