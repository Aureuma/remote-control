use std::{
    io::{BufRead, BufReader},
    process::{Child, Command, Stdio},
    sync::mpsc,
    thread,
    time::{Duration, Instant},
};

use anyhow::{Result, anyhow};
use nix::{errno::Errno, sys::signal, unistd::Pid};
use regex::Regex;

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Options {
    pub binary: String,
    pub local_url: String,
    pub startup_timeout: Duration,
    pub mode: String,
    pub hostname: String,
    pub tunnel_name: String,
    pub tunnel_token: String,
    pub config_file: String,
    pub credentials_file: String,
}

#[derive(Debug)]
pub struct Handle {
    child: Child,
    public_url: String,
}

impl Handle {
    pub fn public_url(&self) -> &str {
        &self.public_url
    }

    pub fn pid(&self) -> u32 {
        self.child.id()
    }

    pub fn stop(&mut self) -> Result<()> {
        let pid = self.child.id() as i32;
        match signal::kill(Pid::from_raw(pid), signal::Signal::SIGTERM) {
            Ok(()) | Err(Errno::ESRCH) => {}
            Err(err) => return Err(err.into()),
        }
        let _ = self.child.wait();
        Ok(())
    }
}

impl Drop for Handle {
    fn drop(&mut self) {
        let _ = self.stop();
    }
}

pub fn start(opts: &Options) -> Result<Handle> {
    let binary = if opts.binary.trim().is_empty() {
        "cloudflared"
    } else {
        opts.binary.trim()
    };
    let local_url = opts.local_url.trim();
    if local_url.is_empty() {
        return Err(anyhow!("local url is required"));
    }
    which(binary).ok_or_else(|| anyhow!("cloudflare tunnel binary not found"))?;

    let (args, expected_public_url) = build_invocation(local_url, opts)?;
    let startup_timeout = if opts.startup_timeout.is_zero() {
        Duration::from_secs(20)
    } else {
        opts.startup_timeout
    };
    let mut child = Command::new(binary)
        .args(args)
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()?;

    if !expected_public_url.is_empty() {
        wait_for_process_startup(&child, startup_timeout)?;
        return Ok(Handle {
            child,
            public_url: expected_public_url,
        });
    }

    let stdout = child.stdout.take();
    let stderr = child.stderr.take();
    let (tx, rx) = mpsc::channel();
    if let Some(stdout) = stdout {
        let tx = tx.clone();
        thread::spawn(move || read_pipe(stdout, tx));
    }
    if let Some(stderr) = stderr {
        let tx = tx.clone();
        thread::spawn(move || read_pipe(stderr, tx));
    }
    drop(tx);

    let deadline = Instant::now() + startup_timeout;
    while Instant::now() < deadline {
        if let Ok(Some(status)) = child.try_wait() {
            return Err(anyhow!(
                "cloudflared exited before tunnel url was discovered: {status}"
            ));
        }
        match rx.recv_timeout(Duration::from_millis(200)) {
            Ok(line) => {
                if let Some(url) = parse_public_url(&line) {
                    return Ok(Handle {
                        child,
                        public_url: url,
                    });
                }
            }
            Err(mpsc::RecvTimeoutError::Timeout) => continue,
            Err(mpsc::RecvTimeoutError::Disconnected) => break,
        }
    }

    let _ = child.kill();
    let _ = child.wait();
    Err(anyhow!("timed out waiting for cloudflare tunnel url"))
}

pub fn build_invocation(local_url: &str, opts: &Options) -> Result<(Vec<String>, String)> {
    let mode = match opts.mode.trim().to_ascii_lowercase().as_str() {
        "" | "ephemeral" => "ephemeral",
        "named" => "named",
        other => {
            return Err(anyhow!(
                "unsupported tunnel mode \"{other}\" (expected ephemeral|named)"
            ));
        }
    };
    match mode {
        "ephemeral" => Ok((
            vec![
                "tunnel".to_string(),
                "--url".to_string(),
                local_url.to_string(),
                "--no-autoupdate".to_string(),
            ],
            String::new(),
        )),
        "named" => {
            let public_url = if opts.hostname.trim().is_empty() {
                String::new()
            } else {
                normalize_public_url_from_hostname(&opts.hostname)?
            };
            if !opts.tunnel_token.trim().is_empty() {
                return Ok((
                    vec![
                        "tunnel".to_string(),
                        "run".to_string(),
                        "--token".to_string(),
                        opts.tunnel_token.trim().to_string(),
                    ],
                    public_url,
                ));
            }
            if public_url.is_empty() {
                return Err(anyhow!(
                    "named tunnel requires --tunnel-hostname unless --tunnel-token is provided"
                ));
            }
            let mut args = vec![
                "tunnel".to_string(),
                "--url".to_string(),
                local_url.to_string(),
                "--hostname".to_string(),
                opts.hostname.trim().to_string(),
                "--no-autoupdate".to_string(),
            ];
            if !opts.config_file.trim().is_empty() {
                args.push("--config".to_string());
                args.push(opts.config_file.trim().to_string());
            }
            if !opts.credentials_file.trim().is_empty() {
                args.push("--credentials-file".to_string());
                args.push(opts.credentials_file.trim().to_string());
            }
            if !opts.tunnel_name.trim().is_empty() {
                args.push("--name".to_string());
                args.push(opts.tunnel_name.trim().to_string());
            }
            Ok((args, public_url))
        }
        _ => unreachable!(),
    }
}

pub fn normalize_public_url_from_hostname(raw: &str) -> Result<String> {
    let mut host = raw.trim().to_string();
    if host.is_empty() {
        return Err(anyhow!("hostname is required"));
    }
    if let Some(stripped) = host.strip_prefix("https://") {
        host = stripped.to_string();
    } else if let Some(stripped) = host.strip_prefix("http://") {
        host = stripped.to_string();
    }
    if let Some((hostname, _)) = host.split_once('/') {
        host = hostname.trim().to_string();
    }
    host = host.trim_matches('/').to_string();
    if host.is_empty() {
        return Err(anyhow!("hostname is empty"));
    }
    Ok(format!("https://{host}"))
}

pub fn wait_for_process_startup(child: &Child, timeout: Duration) -> Result<()> {
    let timeout = if timeout.is_zero() {
        Duration::from_secs(20)
    } else {
        timeout
    };
    let ready_after = Duration::from_millis(800);
    let start = Instant::now();
    loop {
        let pid = child.id() as i32;
        match signal::kill(Pid::from_raw(pid), None) {
            Ok(()) | Err(Errno::EPERM) => {}
            Err(Errno::ESRCH) => {
                return Err(anyhow!("cloudflared exited before startup completed"));
            }
            Err(err) => return Err(err.into()),
        }
        if start.elapsed() >= ready_after {
            return Ok(());
        }
        if start.elapsed() > timeout {
            return Err(anyhow!("timed out waiting for cloudflared startup"));
        }
        thread::sleep(Duration::from_millis(200));
    }
}

pub fn parse_public_url(line: &str) -> Option<String> {
    let regex = Regex::new(r"https://[A-Za-z0-9.\-]+(?::[0-9]+)?(?:/[^\s|]*)?").unwrap();
    let value = regex.find(line.trim())?.as_str().to_string();
    value.starts_with("https://").then_some(value)
}

fn read_pipe(reader: impl std::io::Read + Send + 'static, tx: mpsc::Sender<String>) {
    let scanner = BufReader::new(reader);
    for line in scanner.lines().map_while(|result| result.ok()) {
        let line = line.trim().to_string();
        if !line.is_empty() {
            let _ = tx.send(line);
        }
    }
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
    use std::{
        process::Command,
        time::{Duration, Instant},
    };

    use crate::tunnel_cloudflare::{
        Options, build_invocation, normalize_public_url_from_hostname, parse_public_url, start,
        wait_for_process_startup,
    };

    #[test]
    fn parse_public_url_matches_go_behavior() {
        let cases = [
            (
                "INF +--------------------------------------------------------------------------------------------+",
                None,
            ),
            (
                "Your quick Tunnel has been created! Visit it at https://abc123.trycloudflare.com",
                Some("https://abc123.trycloudflare.com"),
            ),
            (
                "INF |  https://alliance-naval-licenses-childrens.trycloudflare.com                               |",
                Some("https://alliance-naval-licenses-childrens.trycloudflare.com"),
            ),
            (
                "connected to https://example.com/path",
                Some("https://example.com/path"),
            ),
            ("http://example.com", None),
        ];
        for (line, want) in cases {
            assert_eq!(parse_public_url(line).as_deref(), want);
        }
    }

    #[test]
    fn start_missing_binary_reports_error() {
        let err = start(&Options {
            binary: "definitely-missing-cloudflared-binary".to_string(),
            local_url: "http://127.0.0.1:8080".to_string(),
            startup_timeout: Duration::from_secs(1),
            ..Options::default()
        })
        .unwrap_err();
        assert!(
            err.to_string()
                .to_ascii_lowercase()
                .contains("binary not found")
        );
    }

    #[test]
    fn build_invocation_ephemeral_matches_go_behavior() {
        let (args, public_url) =
            build_invocation("http://127.0.0.1:8080", &Options::default()).unwrap();
        assert_eq!(
            args,
            vec![
                "tunnel".to_string(),
                "--url".to_string(),
                "http://127.0.0.1:8080".to_string(),
                "--no-autoupdate".to_string(),
            ]
        );
        assert!(public_url.is_empty());
    }

    #[test]
    fn build_invocation_named_with_hostname_matches_go_behavior() {
        let (args, public_url) = build_invocation(
            "http://127.0.0.1:8080",
            &Options {
                mode: "named".to_string(),
                hostname: "rc.example.com".to_string(),
                tunnel_name: "rc-tunnel".to_string(),
                config_file: "/tmp/cloudflared.yml".to_string(),
                credentials_file: "/tmp/creds.json".to_string(),
                ..Options::default()
            },
        )
        .unwrap();
        assert_eq!(
            args,
            vec![
                "tunnel".to_string(),
                "--url".to_string(),
                "http://127.0.0.1:8080".to_string(),
                "--hostname".to_string(),
                "rc.example.com".to_string(),
                "--no-autoupdate".to_string(),
                "--config".to_string(),
                "/tmp/cloudflared.yml".to_string(),
                "--credentials-file".to_string(),
                "/tmp/creds.json".to_string(),
                "--name".to_string(),
                "rc-tunnel".to_string(),
            ]
        );
        assert_eq!(public_url, "https://rc.example.com");
    }

    #[test]
    fn build_invocation_named_with_token_matches_go_behavior() {
        let (args, public_url) = build_invocation(
            "http://127.0.0.1:8080",
            &Options {
                mode: "named".to_string(),
                hostname: "rc.example.com".to_string(),
                tunnel_token: "cf-token".to_string(),
                ..Options::default()
            },
        )
        .unwrap();
        assert_eq!(
            args,
            vec![
                "tunnel".to_string(),
                "run".to_string(),
                "--token".to_string(),
                "cf-token".to_string(),
            ]
        );
        assert_eq!(public_url, "https://rc.example.com");
    }

    #[test]
    fn build_invocation_named_token_without_hostname_matches_go_behavior() {
        let (args, public_url) = build_invocation(
            "http://127.0.0.1:8080",
            &Options {
                mode: "named".to_string(),
                tunnel_token: "cf-token".to_string(),
                ..Options::default()
            },
        )
        .unwrap();
        assert_eq!(
            args,
            vec![
                "tunnel".to_string(),
                "run".to_string(),
                "--token".to_string(),
                "cf-token".to_string(),
            ]
        );
        assert!(public_url.is_empty());
    }

    #[test]
    fn build_invocation_named_requires_hostname() {
        let err = build_invocation(
            "http://127.0.0.1:8080",
            &Options {
                mode: "named".to_string(),
                ..Options::default()
            },
        )
        .unwrap_err();
        assert!(err.to_string().contains("requires --tunnel-hostname"));
    }

    #[test]
    fn wait_for_process_startup_returns_quickly_when_alive() {
        let mut child = Command::new("sleep").arg("3").spawn().unwrap();
        let start = Instant::now();
        wait_for_process_startup(&child, Duration::from_secs(5)).unwrap();
        assert!(start.elapsed() <= Duration::from_secs(2));
        let _ = child.kill();
        let _ = child.wait();
    }

    #[test]
    fn normalize_public_url_from_hostname_matches_go_behavior() {
        assert_eq!(
            normalize_public_url_from_hostname("rc.example.com").unwrap(),
            "https://rc.example.com"
        );
        assert_eq!(
            normalize_public_url_from_hostname("https://rc.example.com/path").unwrap(),
            "https://rc.example.com"
        );
        assert!(normalize_public_url_from_hostname(" ").is_err());
    }
}
