use std::process::Command;

use anyhow::{Result, anyhow};

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Session {
    pub name: String,
    pub attached: i32,
    pub windows: i32,
    pub created: String,
}

pub fn ensure_installed() -> Result<()> {
    match which("tmux") {
        Some(_) => Ok(()),
        None => Err(anyhow!("tmux not found in PATH")),
    }
}

pub fn list_sessions() -> Result<Vec<Session>> {
    ensure_installed()?;
    let output = Command::new("tmux")
        .args([
            "list-sessions",
            "-F",
            "#{session_name}|#{session_attached}|#{session_windows}|#{session_created_string}",
        ])
        .output()?;
    if !output.status.success() {
        let stderr = String::from_utf8_lossy(&output.stderr)
            .trim()
            .to_ascii_lowercase();
        let stdout = String::from_utf8_lossy(&output.stdout)
            .trim()
            .to_ascii_lowercase();
        let combined = format!("{stdout}\n{stderr}");
        if combined.contains("no server running")
            || combined.contains("failed to connect to server")
            || combined.contains("error connecting to")
            || (output.status.code() == Some(1) && combined.trim().is_empty())
        {
            return Ok(Vec::new());
        }
        return Err(anyhow!("tmux list-sessions failed"));
    }
    let text = String::from_utf8_lossy(&output.stdout)
        .trim_end_matches(['\r', '\n'])
        .to_string();
    if text.is_empty() {
        return Ok(Vec::new());
    }
    Ok(parse_sessions_output(&text))
}

pub fn attach_command(session: &str) -> Result<Command> {
    let session = session.trim();
    if session.is_empty() {
        return Err(anyhow!("tmux session is required"));
    }
    ensure_installed()?;
    let mut cmd = Command::new("tmux");
    cmd.args(["attach-session", "-t", session]);
    Ok(cmd)
}

pub fn parse_sessions_output(text: &str) -> Vec<Session> {
    text.trim()
        .lines()
        .filter_map(|line| {
            let line = line.trim_end_matches('\r');
            if line.is_empty() {
                return None;
            }
            let parts = line.splitn(4, '|').collect::<Vec<_>>();
            if parts.len() < 3 {
                return None;
            }
            let name = parts[0].trim();
            if name.is_empty() {
                return None;
            }
            let attached = parts[1].trim().parse().unwrap_or(0);
            let windows = parts[2].trim().parse().unwrap_or(0);
            let created = parts.get(3).copied().unwrap_or_default().trim().to_string();
            Some(Session {
                name: name.to_string(),
                attached,
                windows,
                created,
            })
        })
        .collect()
}

fn which(binary: &str) -> Option<String> {
    let output = Command::new("sh")
        .args(["-lc", &format!("command -v {binary}")])
        .output()
        .ok()?;
    if !output.status.success() {
        return None;
    }
    let value = String::from_utf8_lossy(&output.stdout).trim().to_string();
    (!value.is_empty()).then_some(value)
}

#[cfg(test)]
mod tests {
    #[test]
    fn parse_sessions_output_matches_go_behavior() {
        let raw = "dev|1|3|2026-02-28 10:00:00\r\nwork|0|2|2026-02-28 11:00:00\ninvalid\n|1|2|missing-name\n";
        let sessions = crate::tmux::parse_sessions_output(raw);
        assert_eq!(sessions.len(), 2);
        assert_eq!(sessions[0].name, "dev");
        assert_eq!(sessions[0].attached, 1);
        assert_eq!(sessions[0].windows, 3);
        assert_eq!(sessions[1].name, "work");
        assert_eq!(sessions[1].attached, 0);
        assert_eq!(sessions[1].windows, 2);
    }
}
