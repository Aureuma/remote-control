use std::{fs, path::PathBuf};

use anyhow::{Context, Result, anyhow};
use chrono::{DateTime, Utc};
use nix::{errno::Errno, sys::signal, unistd::Pid};
use serde::{Deserialize, Serialize};

use crate::config;

#[derive(Debug, Clone, Serialize, Deserialize, Default, PartialEq, Eq)]
pub struct SessionState {
    pub id: String,
    pub mode: String,
    pub source: String,
    pub readonly: bool,
    pub pid: i32,
    pub addr: String,
    pub url: String,
    pub local_url: String,
    pub public_url: String,
    pub tunnel: String,
    pub cloudflared_pid: i32,
    pub caffeinate_pid: i32,
    pub started_at: Option<DateTime<Utc>>,
    pub token_expires_at: Option<DateTime<Utc>>,
    pub idle_timeout_seconds: i32,
    pub idle_deadline: Option<DateTime<Utc>>,
    pub tunnel_mode: String,
    pub token_in_url: bool,
    pub access_code_auth: bool,
    pub updated_at: Option<DateTime<Utc>>,
    pub client_count: i32,
    pub settings_file: String,
}

pub fn save_session(state: &SessionState) -> Result<()> {
    let runtime_dir = config::runtime_dir()?;
    fs::create_dir_all(&runtime_dir)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(&runtime_dir, fs::Permissions::from_mode(0o700))?;
    }
    if state.id.trim().is_empty() {
        return Err(anyhow!("session id is required"));
    }
    let mut persisted = state.clone();
    let now = Utc::now();
    if persisted.started_at.is_none() {
        persisted.started_at = Some(now);
    }
    persisted.updated_at = Some(now);
    let path = session_path(&runtime_dir, &persisted.id);
    let data = serde_json::to_vec_pretty(&persisted)?;
    crate::config::atomic_write(&path, &data)
}

pub fn remove_session(id: &str) -> Result<()> {
    let runtime_dir = config::runtime_dir()?;
    let id = id.trim();
    if id.is_empty() {
        return Err(anyhow!("session id is required"));
    }
    let path = session_path(&runtime_dir, id);
    match fs::remove_file(path) {
        Ok(()) => Ok(()),
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(err) => Err(err.into()),
    }
}

pub fn load_session(id: &str) -> Result<SessionState> {
    let runtime_dir = config::runtime_dir()?;
    let id = id.trim();
    if id.is_empty() {
        return Err(anyhow!("session id is required"));
    }
    let data = fs::read(session_path(&runtime_dir, id))?;
    Ok(serde_json::from_slice(&data)?)
}

pub fn list_sessions() -> Result<Vec<SessionState>> {
    let runtime_dir = config::runtime_dir()?;
    let entries = match fs::read_dir(&runtime_dir) {
        Ok(entries) => entries,
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(err) => return Err(err).with_context(|| runtime_dir.display().to_string()),
    };
    let mut states = Vec::new();
    for entry in entries {
        let entry = entry?;
        let path = entry.path();
        if !path.is_file() || path.extension().and_then(|ext| ext.to_str()) != Some("json") {
            continue;
        }
        let Ok(data) = fs::read(&path) else {
            continue;
        };
        let Ok(state) = serde_json::from_slice::<SessionState>(&data) else {
            continue;
        };
        states.push(state);
    }
    states.sort_by(|a, b| b.started_at.cmp(&a.started_at));
    Ok(states)
}

pub fn process_alive(pid: i32) -> bool {
    if pid <= 0 {
        return false;
    }
    match signal::kill(Pid::from_raw(pid), None) {
        Ok(()) => true,
        Err(Errno::EPERM) => true,
        Err(_) => false,
    }
}

pub fn prune_stale_sessions() -> Result<Vec<String>> {
    let states = list_sessions()?;
    let mut removed = Vec::new();
    for state in states {
        if state.id.trim().is_empty() || process_alive(state.pid) {
            continue;
        }
        remove_session(&state.id)?;
        removed.push(state.id);
    }
    Ok(removed)
}

fn session_path(runtime_dir: &PathBuf, id: &str) -> PathBuf {
    runtime_dir.join(format!("{id}.json"))
}

#[cfg(test)]
mod tests {
    use std::sync::{LazyLock, Mutex};

    use chrono::Utc;

    use crate::runtime_state::{
        SessionState, list_sessions, load_session, prune_stale_sessions, save_session,
    };

    static ENV_LOCK: LazyLock<Mutex<()>> = LazyLock::new(|| Mutex::new(()));

    #[test]
    fn prune_stale_sessions_removes_only_dead_processes() {
        let _guard = ENV_LOCK.lock().unwrap();
        let runtime_dir = tempfile::tempdir().unwrap();
        unsafe {
            std::env::set_var("SI_REMOTE_CONTROL_RUNTIME_DIR", runtime_dir.path());
        }
        save_session(&SessionState {
            id: "alive".to_string(),
            pid: std::process::id() as i32,
            started_at: Some(Utc::now()),
            ..SessionState::default()
        })
        .unwrap();
        save_session(&SessionState {
            id: "stale".to_string(),
            pid: 999_999,
            started_at: Some(Utc::now()),
            ..SessionState::default()
        })
        .unwrap();
        let removed = prune_stale_sessions().unwrap();
        assert_eq!(removed, vec!["stale".to_string()]);
        let states = list_sessions().unwrap();
        assert_eq!(states.len(), 1);
        assert_eq!(states[0].id, "alive");
        unsafe {
            std::env::remove_var("SI_REMOTE_CONTROL_RUNTIME_DIR");
        }
    }

    #[test]
    fn save_and_load_session_with_security_fields() {
        let _guard = ENV_LOCK.lock().unwrap();
        let runtime_dir = tempfile::tempdir().unwrap();
        unsafe {
            std::env::set_var("SI_REMOTE_CONTROL_RUNTIME_DIR", runtime_dir.path());
        }
        let now = Utc::now();
        save_session(&SessionState {
            id: "secure".to_string(),
            pid: std::process::id() as i32,
            started_at: Some(now),
            token_expires_at: Some(now + chrono::Duration::minutes(30)),
            idle_timeout_seconds: 900,
            idle_deadline: Some(now + chrono::Duration::minutes(15)),
            ..SessionState::default()
        })
        .unwrap();
        let loaded = load_session("secure").unwrap();
        assert!(loaded.token_expires_at.is_some());
        assert_eq!(loaded.idle_timeout_seconds, 900);
        assert!(loaded.idle_deadline.is_some());
        unsafe {
            std::env::remove_var("SI_REMOTE_CONTROL_RUNTIME_DIR");
        }
    }
}
