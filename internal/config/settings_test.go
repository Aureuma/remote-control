package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApplyDefaultsFlowAndTunnel(t *testing.T) {
	s := Settings{
		Flow: FlowSettings{
			LowWatermarkBytes:  999,
			HighWatermarkBytes: 100,
		},
		Tunnel: TunnelSettings{
			Provider: "",
			Cloudflare: CloudflareTunnelSettings{
				Binary:                "",
				StartupTimeoutSeconds: 0,
			},
		},
	}
	applyDefaults(&s)
	if s.Flow.LowWatermarkBytes <= 0 || s.Flow.HighWatermarkBytes <= 0 {
		t.Fatalf("expected positive flow watermarks")
	}
	if s.Flow.LowWatermarkBytes > s.Flow.HighWatermarkBytes {
		t.Fatalf("expected low <= high")
	}
	if s.Flow.AckQuantumBytes <= 0 {
		t.Fatalf("expected ack quantum default")
	}
	if s.Tunnel.Provider != "cloudflare" {
		t.Fatalf("expected cloudflare provider default, got %q", s.Tunnel.Provider)
	}
	if s.Tunnel.Cloudflare.Binary != "cloudflared" {
		t.Fatalf("expected cloudflared binary default, got %q", s.Tunnel.Cloudflare.Binary)
	}
	if s.Tunnel.Cloudflare.StartupTimeoutSeconds <= 0 {
		t.Fatalf("expected positive tunnel timeout default")
	}
}

func TestLoadCreatesDefaultSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SI_REMOTE_CONTROL_HOME", home)

	got, err := Load()
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if !got.Tunnel.Enabled {
		t.Fatalf("expected tunnel enabled by default")
	}
	if got.Tunnel.Provider != "cloudflare" {
		t.Fatalf("expected provider cloudflare, got %q", got.Tunnel.Provider)
	}
	if got.Flow.LowWatermarkBytes <= 0 || got.Flow.HighWatermarkBytes <= 0 {
		t.Fatalf("expected flow defaults")
	}
	settingsPath := filepath.Join(home, "settings.toml")
	if _, err := os.Stat(settingsPath); err != nil {
		t.Fatalf("expected settings file at %s: %v", settingsPath, err)
	}
}
