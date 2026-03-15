use std::{
    env, fs,
    path::{Path, PathBuf},
};

use anyhow::{Context, Result, anyhow};
use chrono::Utc;
use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct Settings {
    pub schema_version: i64,
    pub server: ServerSettings,
    pub session: SessionSettings,
    pub flow: FlowSettings,
    pub tunnel: TunnelSettings,
    pub security: SecuritySettings,
    pub ui: UISettings,
    pub logging: LoggingSettings,
    pub macos: MacOsSettings,
    pub development: DevelopmentSettings,
    pub metadata: MetadataSettings,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct ServerSettings {
    pub bind: String,
    pub port: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct SessionSettings {
    pub default_mode: String,
    pub token_ttl_seconds: i64,
    pub idle_timeout_seconds: i64,
    pub max_clients: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct FlowSettings {
    pub low_watermark_bytes: i64,
    pub high_watermark_bytes: i64,
    pub ack_quantum_bytes: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct TunnelSettings {
    pub enabled: bool,
    pub provider: String,
    pub required: bool,
    pub mode: String,
    pub named: NamedTunnelSettings,
    pub cloudflare: CloudflareTunnelSettings,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq, Default)]
#[serde(default)]
pub struct NamedTunnelSettings {
    pub hostname: String,
    pub tunnel_name: String,
    pub tunnel_token: String,
    pub config_file: String,
    pub credentials_file: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct CloudflareTunnelSettings {
    pub enabled: bool,
    pub binary: String,
    pub startup_timeout_seconds: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct SecuritySettings {
    pub readonly_default: bool,
    pub mask_tokens_in_logs: bool,
    pub access_code: String,
    pub token_in_url: Option<bool>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct UISettings {
    pub emoji: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct LoggingSettings {
    pub level: String,
    pub file: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct MacOsSettings {
    pub caffeinate: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct DevelopmentSettings {
    pub enabled: bool,
    pub safari: SafariDevelopmentSettings,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq, Default)]
#[serde(default)]
pub struct SafariDevelopmentSettings {
    pub ssh: SafariSshSettings,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq, Default)]
#[serde(default)]
pub struct SafariSshSettings {
    pub host: String,
    pub port: String,
    pub user: String,
    pub host_env_key: String,
    pub port_env_key: String,
    pub user_env_key: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq, Default)]
#[serde(default)]
pub struct MetadataSettings {
    pub updated_at: String,
}

impl Default for Settings {
    fn default() -> Self {
        Self {
            schema_version: 1,
            server: ServerSettings::default(),
            session: SessionSettings::default(),
            flow: FlowSettings::default(),
            tunnel: TunnelSettings::default(),
            security: SecuritySettings::default(),
            ui: UISettings::default(),
            logging: LoggingSettings::default(),
            macos: MacOsSettings::default(),
            development: DevelopmentSettings::default(),
            metadata: MetadataSettings::default(),
        }
    }
}

impl Default for ServerSettings {
    fn default() -> Self {
        Self {
            bind: "127.0.0.1".to_string(),
            port: 8080,
        }
    }
}

impl Default for SessionSettings {
    fn default() -> Self {
        Self {
            default_mode: "attach".to_string(),
            token_ttl_seconds: 3600,
            idle_timeout_seconds: 900,
            max_clients: 1,
        }
    }
}

impl Default for FlowSettings {
    fn default() -> Self {
        Self {
            low_watermark_bytes: 512 * 1024,
            high_watermark_bytes: 2 * 1024 * 1024,
            ack_quantum_bytes: 256 * 1024,
        }
    }
}

impl Default for TunnelSettings {
    fn default() -> Self {
        Self {
            enabled: true,
            provider: "cloudflare".to_string(),
            required: false,
            mode: "ephemeral".to_string(),
            named: NamedTunnelSettings::default(),
            cloudflare: CloudflareTunnelSettings::default(),
        }
    }
}

impl Default for CloudflareTunnelSettings {
    fn default() -> Self {
        Self {
            enabled: true,
            binary: "cloudflared".to_string(),
            startup_timeout_seconds: 20,
        }
    }
}

impl Default for SecuritySettings {
    fn default() -> Self {
        Self {
            readonly_default: true,
            mask_tokens_in_logs: true,
            access_code: String::new(),
            token_in_url: Some(true),
        }
    }
}

impl Default for UISettings {
    fn default() -> Self {
        Self { emoji: true }
    }
}

impl Default for LoggingSettings {
    fn default() -> Self {
        Self {
            level: "info".to_string(),
            file: String::new(),
        }
    }
}

impl Default for MacOsSettings {
    fn default() -> Self {
        Self { caffeinate: true }
    }
}

impl Default for DevelopmentSettings {
    fn default() -> Self {
        Self {
            enabled: false,
            safari: SafariDevelopmentSettings {
                ssh: SafariSshSettings {
                    host: String::new(),
                    port: String::new(),
                    user: String::new(),
                    host_env_key: "SHAWN_MAC_HOST".to_string(),
                    port_env_key: "SHAWN_MAC_PORT".to_string(),
                    user_env_key: "SHAWN_MAC_USER".to_string(),
                },
            },
        }
    }
}

pub fn apply_defaults(settings: &mut Settings) {
    if settings.schema_version == 0 {
        settings.schema_version = 1;
    }

    settings.server.bind = settings.server.bind.trim().to_string();
    if settings.server.bind.is_empty() {
        settings.server.bind = "127.0.0.1".to_string();
    }
    if !(1..=65535).contains(&settings.server.port) {
        settings.server.port = 8080;
    }

    settings.session.default_mode = settings.session.default_mode.trim().to_lowercase();
    if !matches!(settings.session.default_mode.as_str(), "attach" | "cmd") {
        settings.session.default_mode = "attach".to_string();
    }
    if settings.session.token_ttl_seconds <= 0 {
        settings.session.token_ttl_seconds = 3600;
    }
    if settings.session.idle_timeout_seconds <= 0 {
        settings.session.idle_timeout_seconds = 900;
    }
    if settings.session.max_clients <= 0 {
        settings.session.max_clients = 1;
    }

    if settings.flow.low_watermark_bytes <= 0 {
        settings.flow.low_watermark_bytes = 512 * 1024;
    }
    if settings.flow.high_watermark_bytes <= 0 {
        settings.flow.high_watermark_bytes = 2 * 1024 * 1024;
    }
    if settings.flow.low_watermark_bytes > settings.flow.high_watermark_bytes {
        settings.flow.low_watermark_bytes = (settings.flow.high_watermark_bytes / 2).max(1);
    }
    if settings.flow.ack_quantum_bytes <= 0 {
        settings.flow.ack_quantum_bytes = 256 * 1024;
    }

    settings.tunnel.provider = settings.tunnel.provider.trim().to_lowercase();
    if settings.tunnel.provider.is_empty() {
        settings.tunnel.provider = "cloudflare".to_string();
    }
    settings.tunnel.mode = settings.tunnel.mode.trim().to_lowercase();
    if !matches!(settings.tunnel.mode.as_str(), "named" | "ephemeral") {
        settings.tunnel.mode = "ephemeral".to_string();
    }
    settings.tunnel.named.hostname = settings.tunnel.named.hostname.trim().to_string();
    settings.tunnel.named.tunnel_name = settings.tunnel.named.tunnel_name.trim().to_string();
    settings.tunnel.named.tunnel_token = settings.tunnel.named.tunnel_token.trim().to_string();
    settings.tunnel.named.config_file = settings.tunnel.named.config_file.trim().to_string();
    settings.tunnel.named.credentials_file =
        settings.tunnel.named.credentials_file.trim().to_string();
    settings.tunnel.cloudflare.binary = settings.tunnel.cloudflare.binary.trim().to_string();
    if settings.tunnel.cloudflare.binary.is_empty() {
        settings.tunnel.cloudflare.binary = "cloudflared".to_string();
    }
    if settings.tunnel.cloudflare.startup_timeout_seconds <= 0 {
        settings.tunnel.cloudflare.startup_timeout_seconds = 20;
    }

    settings.security.access_code = settings.security.access_code.trim().to_string();
    if settings.security.token_in_url.is_none() {
        settings.security.token_in_url = Some(true);
    }

    settings.logging.level = settings.logging.level.trim().to_lowercase();
    if settings.logging.level.is_empty() {
        settings.logging.level = "info".to_string();
    }
    settings.logging.file = settings.logging.file.trim().to_string();

    settings.development.safari.ssh.host = settings.development.safari.ssh.host.trim().to_string();
    settings.development.safari.ssh.port = settings.development.safari.ssh.port.trim().to_string();
    settings.development.safari.ssh.user = settings.development.safari.ssh.user.trim().to_string();
    settings.development.safari.ssh.host_env_key = settings
        .development
        .safari
        .ssh
        .host_env_key
        .trim()
        .to_string();
    settings.development.safari.ssh.port_env_key = settings
        .development
        .safari
        .ssh
        .port_env_key
        .trim()
        .to_string();
    settings.development.safari.ssh.user_env_key = settings
        .development
        .safari
        .ssh
        .user_env_key
        .trim()
        .to_string();
    if settings.development.safari.ssh.host_env_key.is_empty() {
        settings.development.safari.ssh.host_env_key = "SHAWN_MAC_HOST".to_string();
    }
    if settings.development.safari.ssh.port_env_key.is_empty() {
        settings.development.safari.ssh.port_env_key = "SHAWN_MAC_PORT".to_string();
    }
    if settings.development.safari.ssh.user_env_key.is_empty() {
        settings.development.safari.ssh.user_env_key = "SHAWN_MAC_USER".to_string();
    }
}

pub fn resolve_setting_value(raw: &str, env_key: &str) -> String {
    if let Some(value) = resolve_env_value_by_key(env_key) {
        return value;
    }
    if let Some(key) = parse_env_reference(raw) {
        if let Some(value) = resolve_env_value_by_key(&key) {
            return value;
        }
    }
    raw.trim().to_string()
}

pub fn parse_env_reference(raw: &str) -> Option<String> {
    let value = raw.trim();
    if value.is_empty() {
        return None;
    }
    if let Some(key) = value.strip_prefix("env:") {
        let key = key.trim();
        return (!key.is_empty()).then(|| key.to_string());
    }
    if value.starts_with("${") && value.ends_with('}') {
        let key = value[2..value.len() - 1].trim();
        return (!key.is_empty()).then(|| key.to_string());
    }
    looks_like_env_key(value).then(|| value.to_string())
}

fn resolve_env_value_by_key(key: &str) -> Option<String> {
    let key = key.trim();
    if key.is_empty() {
        return None;
    }
    let value = env::var(key).ok()?;
    let value = value.trim().to_string();
    (!value.is_empty()).then_some(value)
}

fn looks_like_env_key(value: &str) -> bool {
    !value.is_empty()
        && value
            .chars()
            .all(|c| c.is_ascii_uppercase() || c.is_ascii_digit() || c == '_')
}

pub fn home_dir() -> Result<PathBuf> {
    if let Ok(value) = env::var("SI_REMOTE_CONTROL_HOME") {
        let value = value.trim();
        if !value.is_empty() {
            return Ok(PathBuf::from(value));
        }
    }
    let home = env::var("HOME").context("resolve HOME")?;
    let home = home.trim();
    if home.is_empty() {
        return Err(anyhow!("resolve HOME"));
    }
    Ok(Path::new(home).join(".si").join("remote-control"))
}

pub fn settings_path() -> Result<PathBuf> {
    if let Ok(value) = env::var("SI_REMOTE_CONTROL_SETTINGS_FILE") {
        let value = value.trim();
        if !value.is_empty() {
            return Ok(PathBuf::from(value));
        }
    }
    Ok(home_dir()?.join("settings.toml"))
}

pub fn runtime_dir() -> Result<PathBuf> {
    if let Ok(value) = env::var("SI_REMOTE_CONTROL_RUNTIME_DIR") {
        let value = value.trim();
        if !value.is_empty() {
            return Ok(PathBuf::from(value));
        }
    }
    Ok(home_dir()?.join("runtime"))
}

pub fn load() -> Result<Settings> {
    let mut settings = Settings::default();
    let path = settings_path()?;
    match fs::read_to_string(&path) {
        Ok(data) => {
            settings = toml::from_str(&data).context("parse settings")?;
            apply_defaults(&mut settings);
            Ok(settings)
        }
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => {
            save(&settings)?;
            Ok(settings)
        }
        Err(err) => Err(err).with_context(|| format!("read settings: {}", path.display())),
    }
}

pub fn save(settings: &Settings) -> Result<()> {
    let mut settings = settings.clone();
    apply_defaults(&mut settings);
    settings.metadata.updated_at = Utc::now().to_rfc3339();
    let path = settings_path()?;
    let serialized = toml::to_string(&settings).context("marshal settings")?;
    atomic_write(&path, serialized.as_bytes())
}

pub(crate) fn atomic_write(path: &Path, data: &[u8]) -> Result<()> {
    let dir = path
        .parent()
        .ok_or_else(|| anyhow!("missing parent directory"))?;
    fs::create_dir_all(dir)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(dir, fs::Permissions::from_mode(0o700))?;
    }

    let mut tmp = tempfile::NamedTempFile::new_in(dir)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        tmp.as_file()
            .set_permissions(fs::Permissions::from_mode(0o600))?;
    }
    use std::io::Write as _;
    tmp.write_all(data)?;
    tmp.flush()?;
    tmp.persist(path).map_err(|err| err.error)?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use chrono::Utc;

    use super::{
        CloudflareTunnelSettings, FlowSettings, SecuritySettings, Settings, TunnelSettings,
        apply_defaults, load, resolve_setting_value,
    };

    #[test]
    fn apply_defaults_flow_and_tunnel() {
        let mut settings = Settings {
            flow: FlowSettings {
                low_watermark_bytes: 999,
                high_watermark_bytes: 100,
                ack_quantum_bytes: 0,
            },
            tunnel: TunnelSettings {
                provider: String::new(),
                mode: String::new(),
                cloudflare: CloudflareTunnelSettings {
                    enabled: true,
                    binary: String::new(),
                    startup_timeout_seconds: 0,
                },
                ..TunnelSettings::default()
            },
            security: SecuritySettings {
                token_in_url: None,
                ..SecuritySettings::default()
            },
            ..Settings::default()
        };

        apply_defaults(&mut settings);

        assert!(settings.flow.low_watermark_bytes > 0);
        assert!(settings.flow.high_watermark_bytes > 0);
        assert!(settings.flow.low_watermark_bytes <= settings.flow.high_watermark_bytes);
        assert!(settings.flow.ack_quantum_bytes > 0);
        assert_eq!(settings.tunnel.provider, "cloudflare");
        assert_eq!(settings.tunnel.mode, "ephemeral");
        assert_eq!(settings.tunnel.cloudflare.binary, "cloudflared");
        assert!(settings.tunnel.cloudflare.startup_timeout_seconds > 0);
        assert_eq!(settings.security.token_in_url, Some(true));
        assert_eq!(
            settings.development.safari.ssh.host_env_key,
            "SHAWN_MAC_HOST"
        );
        assert_eq!(
            settings.development.safari.ssh.port_env_key,
            "SHAWN_MAC_PORT"
        );
        assert_eq!(
            settings.development.safari.ssh.user_env_key,
            "SHAWN_MAC_USER"
        );
    }

    #[test]
    fn load_creates_default_settings() {
        let _guard = crate::test_support::ENV_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        let home = tempfile::tempdir().unwrap();
        unsafe {
            std::env::set_var("SI_REMOTE_CONTROL_HOME", home.path());
        }
        let settings = load().unwrap();
        assert!(settings.tunnel.enabled);
        assert_eq!(settings.tunnel.provider, "cloudflare");
        assert_eq!(settings.tunnel.mode, "ephemeral");
        assert_eq!(settings.security.token_in_url, Some(true));
        assert_eq!(
            settings.development.safari.ssh.host_env_key,
            "SHAWN_MAC_HOST"
        );
        assert!(settings.flow.low_watermark_bytes > 0);
        assert!(home.path().join("settings.toml").exists());
        let file = std::fs::read_to_string(home.path().join("settings.toml")).unwrap();
        assert!(file.contains("updated_at"));
        assert!(file.contains(&Utc::now().format("%Y-%m-%d").to_string()));
        unsafe {
            std::env::remove_var("SI_REMOTE_CONTROL_HOME");
        }
    }

    #[test]
    fn resolve_setting_value_prefers_env_references() {
        let _guard = crate::test_support::ENV_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        unsafe {
            std::env::set_var("RC_TEST_HOST", "example.local");
        }
        assert_eq!(resolve_setting_value("", "RC_TEST_HOST"), "example.local");
        assert_eq!(
            resolve_setting_value("${RC_TEST_HOST}", ""),
            "example.local"
        );
        assert_eq!(
            resolve_setting_value("env:RC_TEST_HOST", ""),
            "example.local"
        );
        assert_eq!(resolve_setting_value("RC_TEST_HOST", ""), "example.local");
        assert_eq!(resolve_setting_value("literal-host", ""), "literal-host");
        unsafe {
            std::env::remove_var("RC_TEST_HOST");
        }
    }
}
