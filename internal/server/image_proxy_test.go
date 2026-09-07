package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kilo666mj/tintwire/internal/store"
)

func TestImageProxyBoundaries(t *testing.T) {
	p, err := newImageProxy(`[{"origin":"https://camera.example","upstream":"http://192.168.1.10","path_prefix":"/thumb-"}]`, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{
		"https://camera.example/thumb-kitchen-123.webp",
		"https://camera.example/thumb-front.jpg",
	} {
		upstream, host, ok := p.resolve(input)
		if !ok || host != "camera.example" || !strings.HasPrefix(upstream, "http://192.168.1.10/thumb-") {
			t.Fatalf("resolve(%q) = %q %q %v", input, upstream, host, ok)
		}
	}
	for _, input := range []string{
		"https://evil.example/thumb-a.webp", "https://camera.example.evil/thumb-a.webp",
		"http://camera.example/thumb-a.webp", "https://camera.example:443/thumb-a.webp",
		"https://user@camera.example/thumb-a.webp", "https://camera.example/secret.webp",
		"https://camera.example/thumb-../secret.webp", "https://camera.example/thumb-%2e%2e%2fsecret.webp",
		"https://camera.example/thumb-a.webp?url=http://evil.example", "https://camera.example/thumb-a.webp?",
		"https://camera.example/thumb-a.svg", "https://camera.example/thumb-a.webp#fragment",
		"https://camera.example/thumb-a/b.webp", "https://camera.example/thumb-%61.webp",
	} {
		if _, _, ok := p.resolve(input); ok {
			t.Fatalf("accepted %q", input)
		}
	}
	if _, err := newImageProxy(`[{"origin":"https://camera.example","upstream":"http://192.168.1.10","path_prefix":"/thumb-"}]`, false); err == nil {
		t.Fatal("enabled without authentication")
	}
}

func TestNotificationImagePermissionsAndResponses(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "images.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.BootstrapWebhook(ctx, "images-hook", "cameras"); err != nil {
		t.Fatal(err)
	}
	admin, err := db.CreateUser(ctx, "admin", "a sufficiently long password", true)
	if err != nil {
		t.Fatal(err)
	}
	outsider, err := db.CreateUser(ctx, "outsider", "a sufficiently long password", false)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := db.CreateUser(ctx, "reader", "a sufficiently long password", false)
	if err != nil {
		t.Fatal(err)
	}
	channels, err := db.ListChannels(ctx, admin)
	if err != nil || len(channels) != 1 {
		t.Fatalf("channels: %v %v", channels, err)
	}
	if _, err := db.UpdateChannel(ctx, channels[0].ID, store.CreateChannelInput{Name: "cameras", Visibility: "private"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetChannelMember(ctx, channels[0].ID, reader.Username, "viewer"); err != nil {
		t.Fatal(err)
	}
	makeCookie := func(user store.User) *http.Cookie {
		t.Helper()
		token, _, err := db.CreateSession(ctx, user.ID, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Cookie{Name: sessionCookieName, Value: token}
	}
	adminCookie, readerCookie, outsiderCookie := makeCookie(admin), makeCookie(reader), makeCookie(outsider)
	calls := 0
	mode := "image"
	webp := "RIFF\x10\x00\x00\x00WEBPVP8 test image bytes"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Host != "camera.example" || r.URL.Path != "/thumb-kitchen.webp" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Errorf("unsafe upstream request: %v", r)
		}
		switch mode {
		case "redirect":
			w.Header().Set("Location", "/secret")
			w.WriteHeader(302)
		case "html":
			_, _ = w.Write([]byte("<html>not an image</html>"))
		case "missing":
			w.WriteHeader(404)
		case "large":
			w.Header().Set("Content-Length", "99999999")
			w.WriteHeader(200)
		default:
			_, _ = w.Write([]byte(webp))
		}
	}))
	defer upstream.Close()
	config, _ := json.Marshal([]imageSource{{Origin: "https://camera.example", Upstream: upstream.URL, PathPrefix: "/thumb-"}})
	handler, err := NewWithOptions(db, Options{AuthRequired: true, ImageProxySources: string(config)})
	if err != nil {
		t.Fatal(err)
	}
	card := json.RawMessage(`{"title":"Motion", "images":[{"url":"https://camera.example/thumb-kitchen.webp","alt":"Kitchen"},{"url":"https://other.example/a.png","alt":"Other"}]}`)
	n, err := db.CreateFromWebhook(ctx, "images-hook", store.IncomingNotification{Text: "Motion", Card: card, RawPayload: card})
	if err != nil {
		t.Fatal(err)
	}
	imagePath := "/api/v1/notifications/" + n.ID + "/images/0"
	get := func(path string, cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct {
		path   string
		cookie *http.Cookie
		status int
	}{
		{imagePath, nil, 401}, {imagePath, outsiderCookie, 404},
		{imagePath + "1", adminCookie, 404}, {strings.TrimSuffix(imagePath, "0") + "-1", adminCookie, 404},
		{strings.TrimSuffix(imagePath, "0") + "1", adminCookie, 404},
		{"/api/v1/notifications/missing/images/0", adminCookie, 404},
	} {
		if w := get(tc.path, tc.cookie); w.Code != tc.status {
			t.Fatalf("%s status=%d want=%d body=%s", tc.path, w.Code, tc.status, w.Body.String())
		}
	}
	if calls != 0 {
		t.Fatal("unauthorized requests reached upstream")
	}
	for _, cookie := range []*http.Cookie{adminCookie, readerCookie} {
		w := get(imagePath, cookie)
		if w.Code != 200 || w.Body.String() != webp || w.Header().Get("Content-Type") != "image/webp" || w.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatalf("image response: %d %v %s", w.Code, w.Header(), w.Body.String())
		}
	}
	for _, tc := range []struct {
		mode   string
		status int
	}{{"redirect", 502}, {"html", 502}, {"missing", 404}, {"large", 502}} {
		mode = tc.mode
		before := calls
		if w := get(imagePath, adminCookie); w.Code != tc.status {
			t.Fatalf("%s: %d", mode, w.Code)
		}
		if calls != before+1 {
			t.Fatal("followed redirect")
		}
	}
	w := get("/api/v1/notifications", readerCookie)
	if w.Code != 200 || !strings.Contains(w.Body.String(), imagePath) || !strings.Contains(w.Body.String(), "https://other.example/a.png") {
		t.Fatalf("rewritten inbox: %d %s", w.Code, w.Body.String())
	}
	saved, err := db.NotificationCardForReader(ctx, n.ID, reader)
	if err != nil || string(saved) != string(card) {
		t.Fatalf("stored card changed: %s %v", saved, err)
	}
}
