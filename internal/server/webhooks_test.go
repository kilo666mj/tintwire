package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/kilo666mj/tintwire/internal/store"
)

func TestAdminManagesWebhooks(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "webhooks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.BootstrapUser(context.Background(), "admin", "secure webhook password"); err != nil {
		t.Fatal(err)
	}
	channel, _, err := db.CreateChannel(context.Background(), store.CreateChannelInput{Name: "alerts", Visibility: "public"})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewWithOptions(db, Options{AuthRequired: true})
	if err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewBufferString(`{"username":"admin","password":"secure webhook password"}`))
	login.Header.Set("Content-Type", "application/json")
	login.Header.Set("Origin", "http://example.com")
	login.Host = "example.com"
	loginRecorder := httptest.NewRecorder()
	handler.ServeHTTP(loginRecorder, login)
	if loginRecorder.Code != http.StatusOK || len(loginRecorder.Result().Cookies()) == 0 {
		t.Fatalf("login status = %d", loginRecorder.Code)
	}
	cookie := loginRecorder.Result().Cookies()[0]

	create := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks", bytes.NewBufferString(`{"channel_id":"`+channel.ID+`"}`))
	create.Header.Set("Content-Type", "application/json")
	create.Header.Set("Origin", "http://example.com")
	create.Host = "example.com"
	create.AddCookie(cookie)
	createdRecorder := httptest.NewRecorder()
	handler.ServeHTTP(createdRecorder, create)
	if createdRecorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %q", createdRecorder.Code, createdRecorder.Body.String())
	}
	var created struct {
		Webhook store.Webhook `json:"webhook"`
		Token   string        `json:"token"`
		Path    string        `json:"path"`
	}
	if err := json.NewDecoder(createdRecorder.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.Token == "" || created.Path != "/hooks/"+created.Token {
		t.Fatalf("created response = %#v", created)
	}
	if created.Webhook.ChannelLocked {
		t.Fatalf("new webhook should allow channel overrides by default: %#v", created.Webhook)
	}

	update := httptest.NewRequest(http.MethodPut, "/api/v1/webhooks/"+created.Webhook.ID, bytes.NewBufferString(`{"channel_locked":true}`))
	update.SetPathValue("id", created.Webhook.ID)
	update.Header.Set("Content-Type", "application/json")
	update.Header.Set("Origin", "http://example.com")
	update.Host = "example.com"
	update.AddCookie(cookie)
	updateRecorder := httptest.NewRecorder()
	handler.ServeHTTP(updateRecorder, update)
	if updateRecorder.Code != http.StatusNoContent {
		t.Fatalf("update status = %d, body = %q", updateRecorder.Code, updateRecorder.Body.String())
	}

	list := httptest.NewRequest(http.MethodGet, "/api/v1/webhooks", nil)
	list.AddCookie(cookie)
	listRecorder := httptest.NewRecorder()
	handler.ServeHTTP(listRecorder, list)
	if listRecorder.Code != http.StatusOK || bytes.Contains(listRecorder.Body.Bytes(), []byte(created.Token)) {
		t.Fatalf("list status = %d, body = %q", listRecorder.Code, listRecorder.Body.String())
	}
	if !bytes.Contains(listRecorder.Body.Bytes(), []byte(`"channel_locked":true`)) {
		t.Fatalf("updated lock missing from list: %s", listRecorder.Body.String())
	}

	newURL := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/"+created.Webhook.ID+"/new-url", nil)
	newURL.SetPathValue("id", created.Webhook.ID)
	newURL.Header.Set("Origin", "http://example.com")
	newURL.Host = "example.com"
	newURL.AddCookie(cookie)
	newURLRecorder := httptest.NewRecorder()
	handler.ServeHTTP(newURLRecorder, newURL)
	if newURLRecorder.Code != http.StatusCreated {
		t.Fatalf("new URL status = %d, body = %q", newURLRecorder.Code, newURLRecorder.Body.String())
	}
	var additional struct {
		Webhook store.Webhook `json:"webhook"`
		Token   string        `json:"token"`
		Path    string        `json:"path"`
	}
	if err := json.NewDecoder(newURLRecorder.Body).Decode(&additional); err != nil {
		t.Fatal(err)
	}
	if additional.Webhook.ID == created.Webhook.ID || additional.Token == "" || additional.Path != "/hooks/"+additional.Token || !additional.Webhook.ChannelLocked {
		t.Fatalf("additional URL response = %#v", additional)
	}
	oldPublish := httptest.NewRequest(http.MethodPost, "/hooks/"+created.Token, bytes.NewBufferString(`{"text":"old URL"}`))
	oldPublish.Header.Set("Content-Type", "application/json")
	oldPublishRecorder := httptest.NewRecorder()
	handler.ServeHTTP(oldPublishRecorder, oldPublish)
	if oldPublishRecorder.Code != http.StatusOK {
		t.Fatalf("existing URL publish status = %d", oldPublishRecorder.Code)
	}
	created.Webhook = additional.Webhook

	revoke := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/"+created.Webhook.ID+"/revoke", nil)
	revoke.SetPathValue("id", created.Webhook.ID)
	revoke.Header.Set("Origin", "http://example.com")
	revoke.Host = "example.com"
	revoke.AddCookie(cookie)
	revokeRecorder := httptest.NewRecorder()
	handler.ServeHTTP(revokeRecorder, revoke)
	if revokeRecorder.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d, body = %q", revokeRecorder.Code, revokeRecorder.Body.String())
	}
}
