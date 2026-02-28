package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAppendToken(t *testing.T) {
	got := appendToken("https://example.trycloudflare.com/", "abc123")
	want := "https://example.trycloudflare.com/?token=abc123"
	if got != want {
		t.Fatalf("appendToken()=%q want %q", got, want)
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
