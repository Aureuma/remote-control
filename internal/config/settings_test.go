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
			Mode:     "",
			Cloudflare: CloudflareTunnelSettings{
				Binary:                "",
				StartupTimeoutSeconds: 0,
			},
		},
		Security: SecuritySettings{
			TokenInURL: nil,
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
	if s.Tunnel.Mode != "ephemeral" {
		t.Fatalf("expected ephemeral tunnel mode default, got %q", s.Tunnel.Mode)
	}
	if s.Tunnel.Cloudflare.Binary != "cloudflared" {
		t.Fatalf("expected cloudflared binary default, got %q", s.Tunnel.Cloudflare.Binary)
	}
	if s.Tunnel.Cloudflare.StartupTimeoutSeconds <= 0 {
		t.Fatalf("expected positive tunnel timeout default")
	}
	if s.Security.TokenInURL == nil || !*s.Security.TokenInURL {
		t.Fatalf("expected token_in_url default true")
	}
	if s.Development.Safari.SSH.HostEnvKey != "SHAWN_MAC_HOST" {
		t.Fatalf("expected default host env key, got %q", s.Development.Safari.SSH.HostEnvKey)
	}
	if s.Development.Safari.SSH.PortEnvKey != "SHAWN_MAC_PORT" {
		t.Fatalf("expected default port env key, got %q", s.Development.Safari.SSH.PortEnvKey)
	}
	if s.Development.Safari.SSH.UserEnvKey != "SHAWN_MAC_USER" {
		t.Fatalf("expected default user env key, got %q", s.Development.Safari.SSH.UserEnvKey)
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
	if got.Tunnel.Mode != "ephemeral" {
		t.Fatalf("expected tunnel mode ephemeral, got %q", got.Tunnel.Mode)
	}
	if got.Security.TokenInURL == nil || !*got.Security.TokenInURL {
		t.Fatalf("expected token_in_url default true")
	}
	if got.Development.Safari.SSH.HostEnvKey != "SHAWN_MAC_HOST" {
		t.Fatalf("expected default host env key, got %q", got.Development.Safari.SSH.HostEnvKey)
	}
	if got.Flow.LowWatermarkBytes <= 0 || got.Flow.HighWatermarkBytes <= 0 {
		t.Fatalf("expected flow defaults")
	}
	settingsPath := filepath.Join(home, "settings.toml")
	if _, err := os.Stat(settingsPath); err != nil {
		t.Fatalf("expected settings file at %s: %v", settingsPath, err)
	}
}

func TestResolveSettingValue(t *testing.T) {
	t.Setenv("RC_TEST_HOST", "example.local")
	if got := ResolveSettingValue("", "RC_TEST_HOST"); got != "example.local" {
		t.Fatalf("env-key resolve mismatch: %q", got)
	}
	if got := ResolveSettingValue("${RC_TEST_HOST}", ""); got != "example.local" {
		t.Fatalf("${} resolve mismatch: %q", got)
	}
	if got := ResolveSettingValue("env:RC_TEST_HOST", ""); got != "example.local" {
		t.Fatalf("env: resolve mismatch: %q", got)
	}
	if got := ResolveSettingValue("RC_TEST_HOST", ""); got != "example.local" {
		t.Fatalf("bare key resolve mismatch: %q", got)
	}
	if got := ResolveSettingValue("literal-host", ""); got != "literal-host" {
		t.Fatalf("literal resolve mismatch: %q", got)
	}
}
