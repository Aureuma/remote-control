package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBuildShareURL(t *testing.T) {
	got := buildShareURL("https://example.trycloudflare.com/", "abc123", true, false)
	want := "https://example.trycloudflare.com/?token=abc123"
	if got != want {
		t.Fatalf("buildShareURL()=%q want %q", got, want)
	}

	got = buildShareURL("https://example.trycloudflare.com/", "abc123", false, true)
	want = "https://example.trycloudflare.com/?require_code=1"
	if got != want {
		t.Fatalf("buildShareURL()=%q want %q", got, want)
	}
}

func TestNormalizeTunnelMode(t *testing.T) {
	if got := normalizeTunnelMode(""); got != "ephemeral" {
		t.Fatalf("normalizeTunnelMode(empty)=%q", got)
	}
	if got := normalizeTunnelMode("named"); got != "named" {
		t.Fatalf("normalizeTunnelMode(named)=%q", got)
	}
	if got := normalizeTunnelMode("bogus"); got != "ephemeral" {
		t.Fatalf("normalizeTunnelMode(bogus)=%q", got)
	}
}

func TestWaitForLocalHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := waitForLocalHealth(ctx, srv.URL, time.Second); err != nil {
		t.Fatalf("waitForLocalHealth: %v", err)
	}
}

func TestWaitForLocalHealthTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := waitForLocalHealth(ctx, "http://127.0.0.1:1/healthz", 350*time.Millisecond)
	if err == nil {
		t.Fatalf("expected timeout error")
	}
}
