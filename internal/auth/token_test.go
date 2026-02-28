package auth

import (
	"testing"
	"time"
)

func TestNewTokenWithTTL(t *testing.T) {
	issued, err := NewTokenWithTTL(2 * time.Minute)
	if err != nil {
		t.Fatalf("NewTokenWithTTL: %v", err)
	}
	if issued.Value == "" {
		t.Fatalf("expected token value")
	}
	if issued.ExpiresAt.IsZero() {
		t.Fatalf("expected expiry timestamp")
	}
	if !issued.ExpiresAt.After(time.Now().UTC()) {
		t.Fatalf("expected expiry in the future")
	}
}

func TestIsExpired(t *testing.T) {
	now := time.Date(2026, 2, 28, 12, 0, 0, 0, time.UTC)
	if IsExpired(time.Time{}, now) {
		t.Fatalf("zero expiry should not expire")
	}
	if IsExpired(now.Add(30*time.Second), now) {
		t.Fatalf("future expiry should not be expired")
	}
	if !IsExpired(now, now) {
		t.Fatalf("exact expiry time should be expired")
	}
	if !IsExpired(now.Add(-time.Second), now) {
		t.Fatalf("past expiry should be expired")
	}
}
