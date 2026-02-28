package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

type Settings struct {
	SchemaVersion int              `toml:"schema_version"`
	Server        ServerSettings   `toml:"server"`
	Session       SessionSettings  `toml:"session"`
	Flow          FlowSettings     `toml:"flow"`
	Tunnel        TunnelSettings   `toml:"tunnel"`
	Security      SecuritySettings `toml:"security"`
	UI            UISettings       `toml:"ui"`
	Logging       LoggingSettings  `toml:"logging"`
	MacOS         MacOSSettings    `toml:"macos"`
	Metadata      MetadataSettings `toml:"metadata,omitempty"`
}

type ServerSettings struct {
	Bind string `toml:"bind"`
	Port int    `toml:"port"`
}

type SessionSettings struct {
	DefaultMode        string `toml:"default_mode"`
	TokenTTLSeconds    int    `toml:"token_ttl_seconds"`
	IdleTimeoutSeconds int    `toml:"idle_timeout_seconds"`
	MaxClients         int    `toml:"max_clients"`
}

type FlowSettings struct {
	LowWatermarkBytes  int64 `toml:"low_watermark_bytes"`
	HighWatermarkBytes int64 `toml:"high_watermark_bytes"`
	AckQuantumBytes    int64 `toml:"ack_quantum_bytes"`
}

type TunnelSettings struct {
	Enabled    bool                     `toml:"enabled"`
	Provider   string                   `toml:"provider"`
	Required   bool                     `toml:"required"`
	Mode       string                   `toml:"mode"`
	Named      NamedTunnelSettings      `toml:"named"`
	Cloudflare CloudflareTunnelSettings `toml:"cloudflare"`
}

type NamedTunnelSettings struct {
	Hostname        string `toml:"hostname"`
	TunnelName      string `toml:"tunnel_name"`
	TunnelToken     string `toml:"tunnel_token"`
	ConfigFile      string `toml:"config_file"`
	CredentialsFile string `toml:"credentials_file"`
}

type CloudflareTunnelSettings struct {
	Enabled               bool   `toml:"enabled"`
	Binary                string `toml:"binary"`
	StartupTimeoutSeconds int    `toml:"startup_timeout_seconds"`
}

type SecuritySettings struct {
	ReadOnlyDefault bool   `toml:"readonly_default"`
	MaskTokensInLog bool   `toml:"mask_tokens_in_logs"`
	AccessCode      string `toml:"access_code"`
	TokenInURL      *bool  `toml:"token_in_url,omitempty"`
}

type UISettings struct {
	Emoji bool `toml:"emoji"`
}

type LoggingSettings struct {
	Level string `toml:"level"`
	File  string `toml:"file"`
}

type MacOSSettings struct {
	Caffeinate bool `toml:"caffeinate"`
}

type MetadataSettings struct {
	UpdatedAt string `toml:"updated_at,omitempty"`
}

func defaultSettings() Settings {
	return Settings{
		SchemaVersion: 1,
		Server: ServerSettings{
			Bind: "127.0.0.1",
			Port: 8080,
		},
		Session: SessionSettings{
			DefaultMode:        "attach",
			TokenTTLSeconds:    3600,
			IdleTimeoutSeconds: 900,
			MaxClients:         1,
		},
		Flow: FlowSettings{
			LowWatermarkBytes:  512 * 1024,
			HighWatermarkBytes: 2 * 1024 * 1024,
			AckQuantumBytes:    256 * 1024,
		},
		Tunnel: TunnelSettings{
			Enabled:  true,
			Provider: "cloudflare",
			Required: false,
			Mode:     "ephemeral",
			Cloudflare: CloudflareTunnelSettings{
				Enabled:               true,
				Binary:                "cloudflared",
				StartupTimeoutSeconds: 20,
			},
		},
		Security: SecuritySettings{
			ReadOnlyDefault: true,
			MaskTokensInLog: true,
			AccessCode:      "",
			TokenInURL:      boolPtr(true),
		},
		UI:      UISettings{Emoji: true},
		Logging: LoggingSettings{Level: "info"},
		MacOS:   MacOSSettings{Caffeinate: true},
	}
}

func applyDefaults(s *Settings) {
	if s == nil {
		return
	}
	if s.SchemaVersion == 0 {
		s.SchemaVersion = 1
	}
	s.Server.Bind = strings.TrimSpace(s.Server.Bind)
	if s.Server.Bind == "" {
		s.Server.Bind = "127.0.0.1"
	}
	if s.Server.Port <= 0 || s.Server.Port > 65535 {
		s.Server.Port = 8080
	}
	s.Session.DefaultMode = strings.TrimSpace(strings.ToLower(s.Session.DefaultMode))
	if s.Session.DefaultMode != "attach" && s.Session.DefaultMode != "cmd" {
		s.Session.DefaultMode = "attach"
	}
	if s.Session.TokenTTLSeconds <= 0 {
		s.Session.TokenTTLSeconds = 3600
	}
	if s.Session.IdleTimeoutSeconds <= 0 {
		s.Session.IdleTimeoutSeconds = 900
	}
	if s.Session.MaxClients <= 0 {
		s.Session.MaxClients = 1
	}
	if s.Flow.LowWatermarkBytes <= 0 {
		s.Flow.LowWatermarkBytes = 512 * 1024
	}
	if s.Flow.HighWatermarkBytes <= 0 {
		s.Flow.HighWatermarkBytes = 2 * 1024 * 1024
	}
	if s.Flow.LowWatermarkBytes > s.Flow.HighWatermarkBytes {
		s.Flow.LowWatermarkBytes = s.Flow.HighWatermarkBytes / 2
		if s.Flow.LowWatermarkBytes <= 0 {
			s.Flow.LowWatermarkBytes = 1
		}
	}
	if s.Flow.AckQuantumBytes <= 0 {
		s.Flow.AckQuantumBytes = 256 * 1024
	}
	s.Tunnel.Provider = strings.TrimSpace(strings.ToLower(s.Tunnel.Provider))
	if s.Tunnel.Provider == "" {
		s.Tunnel.Provider = "cloudflare"
	}
	s.Tunnel.Mode = strings.TrimSpace(strings.ToLower(s.Tunnel.Mode))
	if s.Tunnel.Mode != "named" && s.Tunnel.Mode != "ephemeral" {
		s.Tunnel.Mode = "ephemeral"
	}
	s.Tunnel.Named.Hostname = strings.TrimSpace(s.Tunnel.Named.Hostname)
	s.Tunnel.Named.TunnelName = strings.TrimSpace(s.Tunnel.Named.TunnelName)
	s.Tunnel.Named.TunnelToken = strings.TrimSpace(s.Tunnel.Named.TunnelToken)
	s.Tunnel.Named.ConfigFile = strings.TrimSpace(s.Tunnel.Named.ConfigFile)
	s.Tunnel.Named.CredentialsFile = strings.TrimSpace(s.Tunnel.Named.CredentialsFile)
	s.Tunnel.Cloudflare.Binary = strings.TrimSpace(s.Tunnel.Cloudflare.Binary)
	if s.Tunnel.Cloudflare.Binary == "" {
		s.Tunnel.Cloudflare.Binary = "cloudflared"
	}
	if s.Tunnel.Cloudflare.StartupTimeoutSeconds <= 0 {
		s.Tunnel.Cloudflare.StartupTimeoutSeconds = 20
	}
	s.Security.AccessCode = strings.TrimSpace(s.Security.AccessCode)
	if s.Security.TokenInURL == nil {
		s.Security.TokenInURL = boolPtr(true)
	}
	s.Logging.Level = strings.TrimSpace(strings.ToLower(s.Logging.Level))
	if s.Logging.Level == "" {
		s.Logging.Level = "info"
	}
	s.Logging.File = strings.TrimSpace(s.Logging.File)
}

func HomeDir() (string, error) {
	if override := strings.TrimSpace(os.Getenv("SI_REMOTE_CONTROL_HOME")); override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		if err == nil {
			err = os.ErrNotExist
		}
		return "", err
	}
	return filepath.Join(home, ".si", "remote-control"), nil
}

func SettingsPath() (string, error) {
	if override := strings.TrimSpace(os.Getenv("SI_REMOTE_CONTROL_SETTINGS_FILE")); override != "" {
		return override, nil
	}
	home, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "settings.toml"), nil
}

func RuntimeDir() (string, error) {
	if override := strings.TrimSpace(os.Getenv("SI_REMOTE_CONTROL_RUNTIME_DIR")); override != "" {
		return override, nil
	}
	home, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "runtime"), nil
}

func Load() (Settings, error) {
	settings := defaultSettings()
	path, err := SettingsPath()
	if err != nil {
		return settings, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if saveErr := Save(settings); saveErr != nil {
				return settings, saveErr
			}
			return settings, nil
		}
		return settings, fmt.Errorf("read settings: %w", err)
	}
	if err := toml.Unmarshal(data, &settings); err != nil {
		return defaultSettings(), fmt.Errorf("parse settings: %w", err)
	}
	applyDefaults(&settings)
	return settings, nil
}

func Save(settings Settings) error {
	applyDefaults(&settings)
	settings.Metadata.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	path, err := SettingsPath()
	if err != nil {
		return err
	}
	data, err := toml.Marshal(settings)
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "settings-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func boolPtr(v bool) *bool {
	b := v
	return &b
}
