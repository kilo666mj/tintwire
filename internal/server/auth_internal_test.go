package server

import (
	"net/http/httptest"
	"testing"
)

func TestLoginClientIPTrustsDedicatedHeaderOnlyFromLoopback(t *testing.T) {
	proxied := httptest.NewRequest("POST", "/api/v1/session", nil)
	proxied.RemoteAddr = "127.0.0.1:4321"
	proxied.Header.Set("X-Tintwire-Client-IP", "192.0.2.44")
	if got := loginClientIP(proxied); got != "192.0.2.44" {
		t.Fatalf("proxied client IP = %q", got)
	}

	direct := httptest.NewRequest("POST", "/api/v1/session", nil)
	direct.RemoteAddr = "192.0.2.10:4321"
	direct.Header.Set("X-Tintwire-Client-IP", "192.0.2.44")
	if got := loginClientIP(direct); got != "192.0.2.10" {
		t.Fatalf("direct client IP = %q", got)
	}

	invalid := httptest.NewRequest("POST", "/api/v1/session", nil)
	invalid.RemoteAddr = "[::1]:4321"
	invalid.Header.Set("X-Tintwire-Client-IP", "not-an-address")
	if got := loginClientIP(invalid); got != "::1" {
		t.Fatalf("invalid forwarded client IP fallback = %q", got)
	}
}
