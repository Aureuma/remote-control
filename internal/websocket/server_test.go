package websocket

import (
	"testing"
	"time"
)

func TestTokenExpired(t *testing.T) {
	now := time.Date(2026, 2, 28, 18, 0, 0, 0, time.UTC)
	if tokenExpired(time.Time{}, now) {
		t.Fatalf("zero expiry should not be expired")
	}
	if tokenExpired(now.Add(30*time.Second), now) {
		t.Fatalf("future expiry should not be expired")
	}
	if !tokenExpired(now, now) {
		t.Fatalf("exact expiry should be expired")
	}
	if !tokenExpired(now.Add(-time.Second), now) {
		t.Fatalf("past expiry should be expired")
	}
}

func TestIsOriginAllowed(t *testing.T) {
	cases := []struct {
		name   string
		origin string
		host   string
		want   bool
	}{
		{name: "empty origin", origin: "", host: "127.0.0.1:8080", want: true},
		{name: "same host", origin: "https://abc123.trycloudflare.com", host: "abc123.trycloudflare.com", want: true},
		{name: "same host with port", origin: "https://127.0.0.1:8080", host: "127.0.0.1:8080", want: true},
		{name: "localhost origin", origin: "http://localhost:3000", host: "other.example.com", want: true},
		{name: "different host", origin: "https://evil.example.com", host: "abc123.trycloudflare.com", want: false},
		{name: "invalid origin", origin: "://bad", host: "abc123.trycloudflare.com", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isOriginAllowed(tc.origin, tc.host)
			if got != tc.want {
				t.Fatalf("isOriginAllowed(%q, %q)=%t want %t", tc.origin, tc.host, got, tc.want)
			}
		})
	}
}

func TestParseHostname(t *testing.T) {
	if got := parseHostname("abc123.trycloudflare.com:443"); got != "abc123.trycloudflare.com" {
		t.Fatalf("unexpected hostname: %q", got)
	}
	if got := parseHostname("localhost"); got != "localhost" {
		t.Fatalf("unexpected hostname: %q", got)
	}
}
