use std::{
    ffi::OsString,
    io::{self, Write},
    time::Duration,
};

use anyhow::Result;
use nix::{
    sys::signal::{self, Signal},
    unistd::Pid,
};

use chrono::Utc;

use crate::runtime_state::{self, SessionState};
use crate::{auth, server, session, tmux, ttydiscover};

const USAGE_TEXT: &str = "remote-control commands:\n  remote-control sessions [--all]\n  remote-control attach [--tmux-session <name> | --tty-path <path>] [--port <n>] [--bind <addr>] [--readwrite] [--tunnel|--no-tunnel]\n  remote-control start --cmd \"<command>\" [--port <n>] [--bind <addr>] [--readwrite] [--tunnel|--no-tunnel]\n  remote-control status\n  remote-control stop [--id <session-id>]\n";

pub fn run(args: &[OsString]) -> Result<i32> {
    let stdout = io::stdout();
    let stderr = io::stderr();
    let mut stdout = stdout.lock();
    let mut stderr = stderr.lock();
    run_with_io(args, &mut stdout, &mut stderr)
}

fn run_with_io(args: &[OsString], stdout: &mut dyn Write, stderr: &mut dyn Write) -> Result<i32> {
    let args = args
        .iter()
        .map(|arg| arg.to_string_lossy().into_owned())
        .collect::<Vec<_>>();
    if args.is_empty() {
        stdout.write_all(USAGE_TEXT.as_bytes())?;
        return Ok(0);
    }

    match args[0].trim().to_ascii_lowercase().as_str() {
        "sessions" => cmd_sessions(&args[1..], stdout, stderr),
        "attach" => cmd_attach(&args[1..], stdout, stderr),
        "start" | "run" => cmd_start(&args[1..], stdout, stderr),
        "status" => cmd_status(stdout, stderr),
        "stop" => cmd_stop(&args[1..], stdout, stderr),
        "help" | "-h" | "--help" => {
            stdout.write_all(USAGE_TEXT.as_bytes())?;
            Ok(0)
        }
        other => {
            writeln!(stderr, "error: unknown command \"{other}\"")?;
            stdout.write_all(USAGE_TEXT.as_bytes())?;
            Ok(1)
        }
    }
}

fn cmd_sessions(args: &[String], stdout: &mut dyn Write, stderr: &mut dyn Write) -> Result<i32> {
    if has_help(args) {
        stdout.write_all(b"sessions [--all]\n")?;
        return Ok(0);
    }
    let all = args.iter().any(|arg| arg == "--all");
    if args.iter().any(|arg| arg != "--all") {
        writeln!(stderr, "error: unsupported sessions flags")?;
        return Ok(1);
    }
    let mut exit_code = 0;
    match tmux::list_sessions() {
        Ok(sessions) if sessions.is_empty() => {
            writeln!(stdout, "No tmux sessions found.")?;
        }
        Ok(sessions) => {
            writeln!(stdout, "Available tmux sessions")?;
            for session in sessions {
                writeln!(
                    stdout,
                    "- {} (windows={}, attached={}, created={})",
                    session.name, session.windows, session.attached, session.created
                )?;
            }
        }
        Err(err) if all => {
            writeln!(stderr, "warning: Could not list tmux sessions: {err}")?;
            exit_code = 1;
        }
        Err(err) => {
            writeln!(stderr, "error: Could not list tmux sessions: {err}")?;
            return Ok(1);
        }
    }

    if !all {
        return Ok(exit_code);
    }

    match ttydiscover::list() {
        Ok(candidates) if candidates.is_empty() => {
            writeln!(stdout, "No direct TTY candidates found.")?;
        }
        Ok(candidates) => {
            writeln!(stdout, "Direct TTY candidates")?;
            for candidate in candidates {
                writeln!(
                    stdout,
                    "- pid={} tty={} cmd={} args={}",
                    candidate.pid, candidate.tty, candidate.command, candidate.args
                )?;
            }
        }
        Err(err) => {
            writeln!(stderr, "warning: Could not discover TTY candidates: {err}")?;
            exit_code = 1;
        }
    }

    Ok(exit_code)
}

fn cmd_attach(args: &[String], stdout: &mut dyn Write, stderr: &mut dyn Write) -> Result<i32> {
    if has_help(args) {
        stdout.write_all(b"attach [--tmux-session <name> | --tty-path <path>]\n")?;
        return Ok(0);
    }
    let tmux_session = value_for_flag(args, "--tmux-session");
    let tty_path = value_for_flag(args, "--tty-path");
    if tmux_session.is_some() && tty_path.is_some() {
        writeln!(
            stderr,
            "error: choose either --tmux-session or --tty-path, not both"
        )?;
        return Ok(1);
    }

    let bind = value_for_flag(args, "--bind").unwrap_or_else(|| "127.0.0.1".to_string());
    let port = value_for_flag(args, "--port")
        .and_then(|value| value.parse::<u16>().ok())
        .unwrap_or(8080);
    let readonly = !has_flag(args, "--readwrite");
    let access_code = value_for_flag(args, "--access-code").unwrap_or_default();
    let token_in_url = !has_flag(args, "--no-token-in-url");
    let id = value_for_flag(args, "--id").unwrap_or_else(default_session_id);
    let issued = auth::new_token_with_ttl(Duration::from_secs(3600))?;

    if let Some(path) = tty_path {
        let term = match session::Terminal::start_tty_path(&path) {
            Ok(term) => term,
            Err(err) => {
                writeln!(stderr, "error: Could not attach tty path \"{path}\": {err}")?;
                return Ok(1);
            }
        };
        let runtime = tokio::runtime::Runtime::new()?;
        return runtime.block_on(server::run_local_server(
            term,
            server::RunOptions {
                id,
                bind,
                port,
                readonly,
                max_clients: 1,
                flow_low_bytes: 512 * 1024,
                flow_high_bytes: 2 * 1024 * 1024,
                flow_ack_bytes: 256 * 1024,
                access_code,
                token_in_url,
                token_expires_at: issued.expires_at,
                token: issued.value,
            },
        ));
    }

    let sessions = match tmux::list_sessions() {
        Ok(sessions) => sessions,
        Err(err) => {
            writeln!(stderr, "error: Could not discover tmux sessions: {err}")?;
            return Ok(1);
        }
    };
    if sessions.is_empty() {
        writeln!(
            stderr,
            "error: No tmux sessions found. Start one with: tmux new -s my-session"
        )?;
        return Ok(1);
    }

    let name = tmux_session.unwrap_or_else(|| sessions[0].name.clone());
    if !sessions.iter().any(|session| session.name == name) {
        let mut names = sessions
            .iter()
            .map(|session| session.name.clone())
            .collect::<Vec<_>>();
        names.sort();
        writeln!(
            stderr,
            "error: tmux session \"{name}\" not found. Available: {}",
            names.join(", ")
        )?;
        return Ok(1);
    }

    let term = match session::Terminal::start_attach(&name) {
        Ok(term) => term,
        Err(err) => {
            writeln!(
                stderr,
                "error: Could not attach tmux session \"{name}\": {err}"
            )?;
            return Ok(1);
        }
    };
    let runtime = tokio::runtime::Runtime::new()?;
    runtime.block_on(server::run_local_server(
        term,
        server::RunOptions {
            id,
            bind,
            port,
            readonly,
            max_clients: 1,
            flow_low_bytes: 512 * 1024,
            flow_high_bytes: 2 * 1024 * 1024,
            flow_ack_bytes: 256 * 1024,
            access_code,
            token_in_url,
            token_expires_at: issued.expires_at,
            token: issued.value,
        },
    ))
}

fn cmd_start(args: &[String], _stdout: &mut dyn Write, stderr: &mut dyn Write) -> Result<i32> {
    if has_help(args) {
        return Ok(0);
    }
    let Some(command) = value_for_flag(args, "--cmd") else {
        writeln!(stderr, "error: --cmd is required")?;
        return Ok(1);
    };
    if command.trim().is_empty() {
        writeln!(stderr, "error: --cmd is required")?;
        return Ok(1);
    }
    let port_value = value_for_flag(args, "--port");
    if let Some(port) = port_value.as_ref() {
        let parsed = port.parse::<u32>().ok();
        if !matches!(parsed, Some(1..=65535)) {
            writeln!(stderr, "error: invalid --port value {port}")?;
            return Ok(1);
        }
    }
    let bind = value_for_flag(args, "--bind").unwrap_or_else(|| "127.0.0.1".to_string());
    let port = port_value
        .and_then(|value| value.parse::<u16>().ok())
        .unwrap_or(8080);
    let readonly = !has_flag(args, "--readwrite");
    let access_code = value_for_flag(args, "--access-code").unwrap_or_default();
    let token_in_url = !has_flag(args, "--no-token-in-url");
    let id = value_for_flag(args, "--id").unwrap_or_else(default_session_id);
    let issued = auth::new_token_with_ttl(Duration::from_secs(3600))?;
    let term = match session::Terminal::start_command(&command) {
        Ok(term) => term,
        Err(err) => {
            writeln!(stderr, "error: Could not start command session: {err}")?;
            return Ok(1);
        }
    };
    let runtime = tokio::runtime::Runtime::new()?;
    runtime.block_on(server::run_local_server(
        term,
        server::RunOptions {
            id,
            bind,
            port,
            readonly,
            max_clients: 1,
            flow_low_bytes: 512 * 1024,
            flow_high_bytes: 2 * 1024 * 1024,
            flow_ack_bytes: 256 * 1024,
            access_code,
            token_in_url,
            token_expires_at: issued.expires_at,
            token: issued.value,
        },
    ))
}

fn cmd_status(stdout: &mut dyn Write, stderr: &mut dyn Write) -> Result<i32> {
    prune_stale_runtime_state(stderr)?;
    let states = runtime_state::list_sessions()?;
    if states.is_empty() {
        writeln!(stdout, "No active remote-control sessions found.")?;
        return Ok(0);
    }
    writeln!(stdout, "remote-control sessions")?;
    for state in states {
        print_status_line(stdout, &state)?;
    }
    Ok(0)
}

fn cmd_stop(args: &[String], stdout: &mut dyn Write, stderr: &mut dyn Write) -> Result<i32> {
    if has_help(args) {
        return Ok(0);
    }
    prune_stale_runtime_state(stderr)?;
    let states = runtime_state::list_sessions()?;
    if states.is_empty() {
        writeln!(stdout, "No active sessions to stop.")?;
        return Ok(0);
    }
    let requested_id = value_for_flag(args, "--id");
    let target = match requested_id {
        Some(id) => states.into_iter().find(|state| state.id == id),
        None if states.len() == 1 => states.into_iter().next(),
        None => {
            writeln!(
                stderr,
                "error: multiple sessions found. use --id <session-id>."
            )?;
            return Ok(1);
        }
    };
    let Some(target) = target else {
        let wanted = value_for_flag(args, "--id").unwrap_or_default();
        writeln!(stderr, "error: session \"{wanted}\" not found")?;
        return Ok(1);
    };

    if !runtime_state::process_alive(target.pid) {
        runtime_state::remove_session(&target.id)?;
        writeln!(
            stdout,
            "Session {} already stopped; cleaned stale state.",
            target.id
        )?;
        return Ok(0);
    }

    terminate_pid(target.pid)?;
    if target.cloudflared_pid > 0 && target.cloudflared_pid != target.pid {
        let _ = terminate_pid(target.cloudflared_pid);
    }
    if target.caffeinate_pid > 0 && target.caffeinate_pid != target.pid {
        let _ = terminate_pid(target.caffeinate_pid);
    }
    writeln!(
        stdout,
        "Stop signal sent to {} (pid {})",
        target.id, target.pid
    )?;
    Ok(0)
}

fn print_status_line(stdout: &mut dyn Write, state: &SessionState) -> Result<()> {
    let status = if runtime_state::process_alive(state.pid) {
        "running"
    } else {
        "stopped"
    };
    let local = if state.local_url.trim().is_empty() {
        state.url.trim()
    } else {
        state.local_url.trim()
    };
    let public = if state.public_url.trim().is_empty() {
        "-"
    } else {
        state.public_url.trim()
    };
    let token_expires = state
        .token_expires_at
        .map(|ts| ts.to_rfc3339())
        .unwrap_or_else(|| "-".to_string());
    let idle_deadline = state
        .idle_deadline
        .map(|ts| ts.to_rfc3339())
        .unwrap_or_else(|| "-".to_string());
    let tunnel_mode = if state.tunnel_mode.trim().is_empty() {
        "-"
    } else {
        state.tunnel_mode.trim()
    };
    writeln!(
        stdout,
        "- {} [{}] mode={} readonly={} code_auth={} token_in_url={} clients={} local={} public={} tunnel_mode={} started={} token_expires={} idle_deadline={} pids(parent={} cf={} caf={})",
        state.id,
        status,
        state.mode,
        state.readonly,
        state.access_code_auth,
        state.token_in_url,
        state.client_count,
        local,
        public,
        tunnel_mode,
        state
            .started_at
            .map(|ts| ts.to_rfc3339())
            .unwrap_or_else(|| "-".to_string()),
        token_expires,
        idle_deadline,
        state.pid,
        state.cloudflared_pid,
        state.caffeinate_pid
    )?;
    Ok(())
}

pub fn build_share_url(
    base_url: &str,
    token: &str,
    include_token: bool,
    require_code: bool,
) -> String {
    let mut base = base_url.trim().trim_end_matches('/').to_string();
    if base.is_empty() {
        return String::new();
    }
    base.push('/');
    let mut params = Vec::new();
    if include_token {
        params.push(format!("token={token}"));
    }
    if require_code {
        params.push("require_code=1".to_string());
    }
    if !params.is_empty() {
        base.push('?');
        base.push_str(&params.join("&"));
    }
    base
}

pub fn normalize_tunnel_mode(mode: &str) -> &'static str {
    match mode.trim().to_ascii_lowercase().as_str() {
        "named" => "named",
        "ephemeral" | "" => "ephemeral",
        _ => "ephemeral",
    }
}

fn prune_stale_runtime_state(stderr: &mut dyn Write) -> Result<()> {
    let removed = runtime_state::prune_stale_sessions()?;
    if !removed.is_empty() {
        writeln!(
            stderr,
            "Cleaned stale session state: {}",
            removed.join(", ")
        )?;
    }
    Ok(())
}

fn terminate_pid(pid: i32) -> Result<()> {
    signal::kill(Pid::from_raw(pid), Signal::SIGTERM)?;
    Ok(())
}

fn value_for_flag(args: &[String], flag: &str) -> Option<String> {
    args.windows(2)
        .find(|window| window[0] == flag)
        .map(|window| window[1].clone())
}

fn has_help(args: &[String]) -> bool {
    args.iter().any(|arg| arg == "--help" || arg == "-h")
}

fn has_flag(args: &[String], flag: &str) -> bool {
    args.iter().any(|arg| arg == flag)
}

fn default_session_id() -> String {
    format!("rc-{}", Utc::now().timestamp())
}

#[cfg(test)]
mod tests {
    use std::ffi::OsString;

    use chrono::Utc;

    use crate::{app, runtime_state};

    #[test]
    fn build_share_url_matches_go_behavior() {
        let got = app::build_share_url("https://example.trycloudflare.com/", "abc123", true, false);
        assert_eq!(got, "https://example.trycloudflare.com/?token=abc123");

        let got = app::build_share_url("https://example.trycloudflare.com/", "abc123", false, true);
        assert_eq!(got, "https://example.trycloudflare.com/?require_code=1");
    }

    #[test]
    fn normalize_tunnel_mode_matches_go_behavior() {
        assert_eq!(app::normalize_tunnel_mode(""), "ephemeral");
        assert_eq!(app::normalize_tunnel_mode("named"), "named");
        assert_eq!(app::normalize_tunnel_mode("bogus"), "ephemeral");
    }

    #[test]
    fn run_help_and_unknown() {
        let _guard = crate::test_support::ENV_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        let home = tempfile::tempdir().unwrap();
        unsafe {
            std::env::set_var("SI_REMOTE_CONTROL_HOME", home.path());
        }
        assert_eq!(app::run(&[OsString::from("help")]).unwrap(), 0);
        assert_ne!(app::run(&[OsString::from("unknown-command")]).unwrap(), 0);
        unsafe {
            std::env::remove_var("SI_REMOTE_CONTROL_HOME");
        }
    }

    #[test]
    fn run_start_requires_cmd() {
        assert_ne!(
            app::run(&[OsString::from("start"), OsString::from("--no-tunnel")]).unwrap(),
            0
        );
    }

    #[test]
    fn run_attach_and_start_help_exit_zero() {
        assert_eq!(
            app::run(&[OsString::from("attach"), OsString::from("--help")]).unwrap(),
            0
        );
        assert_eq!(
            app::run(&[OsString::from("start"), OsString::from("--help")]).unwrap(),
            0
        );
        assert_eq!(
            app::run(&[OsString::from("stop"), OsString::from("--help")]).unwrap(),
            0
        );
    }

    #[test]
    fn run_start_rejects_invalid_port() {
        assert_ne!(
            app::run(&[
                OsString::from("start"),
                OsString::from("--cmd"),
                OsString::from("sleep 1"),
                OsString::from("--port"),
                OsString::from("70000"),
                OsString::from("--no-tunnel"),
            ])
            .unwrap(),
            0
        );
    }

    #[test]
    fn run_status_and_stop_on_empty_state() {
        let _guard = crate::test_support::ENV_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        let home = tempfile::tempdir().unwrap();
        unsafe {
            std::env::set_var("SI_REMOTE_CONTROL_HOME", home.path());
        }
        assert_eq!(app::run(&[OsString::from("status")]).unwrap(), 0);
        assert_eq!(app::run(&[OsString::from("stop")]).unwrap(), 0);
        unsafe {
            std::env::remove_var("SI_REMOTE_CONTROL_HOME");
        }
    }

    #[test]
    fn run_attach_rejects_conflicting_attach_targets() {
        assert_ne!(
            app::run(&[
                OsString::from("attach"),
                OsString::from("--tmux-session"),
                OsString::from("dev"),
                OsString::from("--tty-path"),
                OsString::from("/dev/pts/1"),
                OsString::from("--no-tunnel"),
            ])
            .unwrap(),
            0
        );
    }

    #[test]
    fn run_stop_cleans_stale_session() {
        let _guard = crate::test_support::ENV_LOCK
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        let runtime_dir = tempfile::tempdir().unwrap();
        unsafe {
            std::env::set_var("SI_REMOTE_CONTROL_RUNTIME_DIR", runtime_dir.path());
        }
        runtime_state::save_session(&runtime_state::SessionState {
            id: "stale".to_string(),
            pid: 999_999,
            started_at: Some(Utc::now()),
            ..runtime_state::SessionState::default()
        })
        .unwrap();
        assert_eq!(
            app::run(&[
                OsString::from("stop"),
                OsString::from("--id"),
                OsString::from("stale"),
            ])
            .unwrap(),
            0
        );
        assert!(runtime_state::list_sessions().unwrap().is_empty());
        unsafe {
            std::env::remove_var("SI_REMOTE_CONTROL_RUNTIME_DIR");
        }
    }
}
