package server

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kilo666mj/tintwire/internal/store"
)

func TestUnsafeActionIPBlocksCarrierGradeNAT(t *testing.T) {
	for _, value := range []string{"100.64.0.1", "100.127.255.254"} {
		if !unsafeActionIP(net.ParseIP(value)) {
			t.Fatalf("CGNAT address %s was accepted", value)
		}
	}
	if unsafeActionIP(net.ParseIP("100.128.0.1")) {
		t.Fatal("public address adjacent to CGNAT range was blocked")
	}
}

func TestUnsafeActionIPMatrix(t *testing.T) {
	blocked := []string{
		"0.0.0.0", "127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1",
		"169.254.1.1", "100.64.0.1", "::", "::1", "fe80::1", "fc00::1",
	}
	for _, value := range blocked {
		if !unsafeActionIP(net.ParseIP(value)) {
			t.Errorf("unsafe address %s was accepted", value)
		}
	}
	for _, value := range []string{"8.8.8.8", "1.1.1.1", "2001:4860:4860::8888"} {
		if unsafeActionIP(net.ParseIP(value)) {
			t.Errorf("public address %s was blocked", value)
		}
	}
}

func TestActionHTTPClientRevalidatesResolvedAddresses(t *testing.T) {
	lookups := 0
	dials := 0
	lookup := func(context.Context, string) ([]net.IPAddr, error) {
		lookups++
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	dial := func(context.Context, string, string) (net.Conn, error) {
		dials++
		return nil, errors.New("unexpected dial")
	}
	client := actionHTTPClientWithNetwork(false, lookup, dial)
	for range 2 {
		request, err := http.NewRequest(http.MethodGet, "https://callback.example/", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Do(request); err == nil || !strings.Contains(err.Error(), "blocked addresses") {
			t.Fatalf("private DNS result was not blocked: %v", err)
		}
	}
	if lookups != 2 || dials != 0 {
		t.Fatalf("lookups=%d dials=%d, want per-request validation and no dial", lookups, dials)
	}
}

func TestActionServiceUsesReplicatedSettingKey(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "actions-key.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	settingKey := base64.StdEncoding.EncodeToString(make([]byte, 32))
	if err := db.SaveSettings(ctx, map[string]string{"action_encryption_key": settingKey}); err != nil {
		t.Fatal(err)
	}

	actions, err := newActionService(db, "")
	if err != nil {
		t.Fatal(err)
	}
	if actions == nil {
		t.Fatal("expected action service to initialize from DB setting")
	}
	encrypted, err := actions.encrypt("callback-secret")
	if err != nil {
		t.Fatal(err)
	}
	decrypted, err := actions.decrypt(encrypted)
	if err != nil || decrypted != "callback-secret" {
		t.Fatalf("decrypt mismatch: %v %q", err, decrypted)
	}
}

func TestActionServiceStoresMissingSettingKey(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "actions-settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	seedKey := base64.StdEncoding.EncodeToString(make([]byte, 32))

	actions, err := newActionService(db, seedKey)
	if err != nil {
		t.Fatal(err)
	}
	if actions == nil {
		t.Fatal("expected action service to initialize from provided key")
	}
	if _, err := actions.encrypt("callback-secret"); !errors.Is(err, errActionEncryptionKeyRequired) {
		t.Fatalf("environment seed was used before persistence: %v", err)
	}
	if err := actions.ensureStoredKey(ctx, db); err != nil {
		t.Fatal(err)
	}
	value, ok, err := db.Setting(ctx, "action_encryption_key")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || value != seedKey {
		t.Fatalf("expected persisted action key in settings, got %q ok=%v", value, ok)
	}
}

func TestActionServiceRecoversFromBlankStoredKey(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "actions-blank-key.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if err := db.SaveSettings(ctx, map[string]string{actionEncryptionKeySetting: "   "}); err != nil {
		t.Fatal(err)
	}
	actions, err := newActionService(db, "")
	if err != nil {
		t.Fatal(err)
	}
	if actions == nil {
		t.Fatal("expected action service to initialize with blank key in settings")
	}
	if _, err := actions.encrypt("callback-secret"); err == nil {
		t.Fatal("expected blank setting to block encryption until key is provided")
	}

	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = 9
	}
	validKey := base64.StdEncoding.EncodeToString(seed)
	if err := db.SaveSettings(ctx, map[string]string{actionEncryptionKeySetting: validKey}); err != nil {
		t.Fatal(err)
	}
	ciphertext, err := actions.encrypt("callback-secret")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := actions.decrypt(ciphertext)
	if err != nil || plain != "callback-secret" {
		t.Fatalf("decrypt mismatch after recovery: %v %q", err, plain)
	}
}

func TestActionServiceRecoversFromInvalidStoredKey(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "actions-invalid-key.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if err := db.SaveSettings(ctx, map[string]string{actionEncryptionKeySetting: "not-base64"}); err != nil {
		t.Fatal(err)
	}
	actions, err := newActionService(db, "")
	if err != nil {
		t.Fatal(err)
	}
	if actions == nil {
		t.Fatal("expected action service to initialize with invalid key in settings")
	}
	if _, err := actions.encrypt("callback-secret"); err == nil {
		t.Fatal("expected invalid setting to block encryption until key is corrected")
	}

	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = 9
	}
	validKey := base64.StdEncoding.EncodeToString(seed)
	if err := db.SaveSettings(ctx, map[string]string{actionEncryptionKeySetting: validKey}); err != nil {
		t.Fatal(err)
	}
	ciphertext, err := actions.encrypt("callback-secret")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := actions.decrypt(ciphertext)
	if err != nil || plain != "callback-secret" {
		t.Fatalf("decrypt mismatch after recovery: %v %q", err, plain)
	}
}
