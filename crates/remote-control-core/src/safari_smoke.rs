use std::{
    ffi::OsString,
    fs,
    io::{BufRead, BufReader, Read},
    net::TcpListener,
    path::{Path, PathBuf},
    process::{Child, Command, Stdio},
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

use anyhow::{Context, Result, anyhow, bail};
use reqwest::blocking::Client;
use serde::Deserialize;
use serde_json::{Value, json};
use tempfile::{NamedTempFile, TempDir};

use crate::config;

const DEFAULT_DRIVER_TIMEOUT: Duration = Duration::from_secs(12);
const DEFAULT_SCENARIO_TIMEOUT: Duration = Duration::from_secs(40);
const DEFAULT_STOP_TIMEOUT: Duration = Duration::from_secs(8);
const USAGE: &str = "rc-safari-smoke [--remote-control-bin <path>] [--safaridriver-bin <path>] [--safaridriver-port <n>] [--bind 127.0.0.1] [--scenarios readwrite,readonly,access-code,no-token] [--driver-timeout 12s] [--scenario-timeout 40s] [--keep-artifacts] [--verbose] [--skip-build] [--ssh-host <host>] [--ssh-port <n>] [--ssh-user <user>] [--no-ssh]\n";

#[derive(Debug, Clone)]
struct CliConfig {
    remote_control_bin: String,
    safaridriver_bin: String,
    safaridriver_port: u16,
    bind: String,
    scenario_csv: String,
    scenarios: Vec<ScenarioDef>,
    driver_startup: Duration,
    scenario_timeout: Duration,
    keep_artifacts: bool,
    verbose: bool,
    skip_build: bool,
    ssh_host: String,
    ssh_port: u16,
    ssh_user: String,
    no_ssh: bool,
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct ScenarioDef {
    name: String,
    readwrite: bool,
    token_in_url: bool,
    access_code: String,
    expected_state: String,
    prompts: Vec<(String, String)>,
}

#[derive(Debug)]
struct Runner {
    cfg: CliConfig,
}

#[derive(Debug)]
struct DriverProcess {
    child: Child,
    base_url: String,
}

#[derive(Debug)]
struct SessionLineState {
    share_url: String,
    access_token: String,
    logs: String,
}

#[derive(Debug)]
struct RemoteControlSession {
    id: String,
    home_dir: TempDir,
    child: Child,
    binary_path: PathBuf,
    state: Arc<Mutex<SessionLineState>>,
}

#[derive(Debug)]
struct WebDriverClient {
    base_url: String,
    client: Client,
}

#[derive(Debug, Deserialize)]
struct WdEnvelope {
    #[serde(default)]
    value: Value,
    #[serde(rename = "sessionId", default)]
    session_id: String,
}

#[derive(Debug, Deserialize)]
struct WdValueError {
    #[serde(default)]
    error: String,
    #[serde(default)]
    message: String,
}

#[derive(Debug)]
struct WdError {
    status: u16,
    code: String,
    message: String,
}

impl std::fmt::Display for WdError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        if self.code.trim().is_empty() {
            write!(
                f,
                "webdriver error (status={}): {}",
                self.status, self.message
            )
        } else {
            write!(
                f,
                "webdriver error ({}, status={}): {}",
                self.code, self.status, self.message
            )
        }
    }
}

impl std::error::Error for WdError {}

pub fn run(args: &[OsString]) -> Result<i32> {
    let cfg = parse_cli(args)?;
    let runner = Runner { cfg };
    runner.run()?;
    Ok(0)
}

impl Runner {
    fn run(&self) -> Result<()> {
        if std::env::consts::OS != "macos" {
            if self.cfg.no_ssh {
                bail!(
                    "real Safari smoke runs only on macOS (current: {})",
                    std::env::consts::OS
                );
            }
            if self.cfg.ssh_host.trim().is_empty()
                || self.cfg.ssh_user.trim().is_empty()
                || self.cfg.ssh_port == 0
            {
                bail!(
                    "real Safari smoke requires macOS; configure SSH via settings or flags (--ssh-host, --ssh-port, --ssh-user)"
                );
            }
            return self.run_remote_over_ssh();
        }

        ensure_binary_exists(&self.cfg.safaridriver_bin)
            .with_context(|| format!("safaridriver not found ({})", self.cfg.safaridriver_bin))?;

        let remote_control_bin = self.prepare_remote_control_binary()?;
        let driver_port = if self.cfg.safaridriver_port == 0 {
            pick_free_port(&self.cfg.bind)?
        } else {
            self.cfg.safaridriver_port
        };
        let mut driver = start_driver(
            &self.cfg.safaridriver_bin,
            driver_port,
            self.cfg.verbose,
            self.cfg.keep_artifacts,
        )?;
        wait_for_driver_ready(&driver.base_url, self.cfg.driver_startup)?;
        let webdriver = WebDriverClient::new(driver.base_url.clone())?;

        for scenario in &self.cfg.scenarios {
            println!("\nScenario: {}", scenario.name);
            self.run_scenario(&webdriver, remote_control_bin.path(), scenario)?;
            println!("Scenario passed: {}", scenario.name);
        }

        driver.stop();
        Ok(())
    }

    fn run_scenario(
        &self,
        webdriver: &WebDriverClient,
        remote_control_bin: &Path,
        scenario: &ScenarioDef,
    ) -> Result<()> {
        let port = pick_free_port(&self.cfg.bind)?;
        let mut session = start_remote_control_session(
            remote_control_bin,
            &self.cfg.bind,
            port,
            scenario,
            self.cfg.verbose,
        )?;

        let session_id = webdriver.new_session().context("webdriver new session")?;
        let session_guard = SessionDeleteGuard {
            webdriver,
            session_id: session_id.clone(),
        };

        webdriver
            .navigate(&session_id, &session.share_url()?)
            .context("webdriver navigate")?;
        wait_for_expected_status(
            webdriver,
            &session_id,
            &scenario.expected_state,
            &mut session,
            self.cfg.scenario_timeout,
            &scenario.prompts,
        )?;

        let title = webdriver
            .eval_string(&session_id, "return document.title || '';")
            .context("read page title")?;
        if !title.to_ascii_lowercase().contains("remote control") {
            bail!("unexpected page title {:?}", title);
        }

        drop(session_guard);
        session.stop(DEFAULT_STOP_TIMEOUT)?;
        Ok(())
    }

    fn run_remote_over_ssh(&self) -> Result<()> {
        ensure_binary_exists("ssh").context("ssh binary not found")?;
        ensure_binary_exists("scp").context("scp binary not found")?;

        let addr = format!("{}@{}", self.cfg.ssh_user, self.cfg.ssh_host);
        let remote_arch = self.remote_darwin_arch(&addr)?;
        let remote_home = self
            .run_ssh_command_output(&addr, "printf %s \"$HOME\"")
            .context("resolve remote home")?;
        let remote_home = remote_home.trim();
        if remote_home.is_empty() {
            bail!("resolve remote home: empty HOME response");
        }

        let stage_dir = TempDir::new().context("create local stage dir")?;
        let stage_path = stage_dir.path().to_path_buf();
        let remote_control_out = stage_path.join("remote-control");
        let safari_out = stage_path.join("rc-safari-smoke");
        let target = target_triple_for_arch(remote_arch)?;
        self.build_binary("remote-control", &remote_control_out, Some(target))?;
        self.build_binary("rc-safari-smoke", &safari_out, Some(target))?;

        let remote_dir = format!("{remote_home}/Development/remote-control");
        self.run_ssh_command(&addr, &format!("mkdir -p {}", shell_quote(&remote_dir)))
            .context("prepare remote directory")?;
        self.run_scp(
            &remote_control_out,
            &format!("{addr}:{remote_dir}/remote-control"),
        )
        .context("copy remote-control binary")?;
        self.run_scp(&safari_out, &format!("{addr}:{remote_dir}/rc-safari-smoke"))
            .context("copy rc-safari-smoke binary")?;

        let mut remote_args = vec![
            "./rc-safari-smoke".to_string(),
            "--remote-control-bin".to_string(),
            "./remote-control".to_string(),
            "--scenarios".to_string(),
            self.cfg.scenario_csv.clone(),
            "--driver-timeout".to_string(),
            format_duration(self.cfg.driver_startup),
            "--scenario-timeout".to_string(),
            format_duration(self.cfg.scenario_timeout),
            "--bind".to_string(),
            self.cfg.bind.clone(),
            "--no-ssh".to_string(),
        ];
        if self.cfg.keep_artifacts {
            remote_args.push("--keep-artifacts".to_string());
        }
        if self.cfg.verbose {
            remote_args.push("--verbose".to_string());
        }
        if !self.cfg.safaridriver_bin.trim().is_empty() {
            remote_args.push("--safaridriver-bin".to_string());
            remote_args.push(self.cfg.safaridriver_bin.clone());
        }
        if self.cfg.safaridriver_port > 0 {
            remote_args.push("--safaridriver-port".to_string());
            remote_args.push(self.cfg.safaridriver_port.to_string());
        }

        let command = [
            "set -e".to_string(),
            format!("cd {}", shell_quote(&remote_dir)),
            "chmod +x ./remote-control ./rc-safari-smoke".to_string(),
            shell_join(&remote_args),
        ]
        .join(" && ");

        println!(
            "Running Safari smoke over SSH on {}:{} ({})",
            self.cfg.ssh_host, self.cfg.ssh_port, remote_arch
        );
        self.run_ssh_command(&addr, &command)
            .context("remote safari smoke over ssh failed")?;
        Ok(())
    }

    fn prepare_remote_control_binary(&self) -> Result<TempBinary> {
        if !self.cfg.remote_control_bin.trim().is_empty() {
            let path = PathBuf::from(self.cfg.remote_control_bin.trim());
            if path.exists() {
                return Ok(TempBinary::borrowed(path));
            }
            let resolved = look_path(self.cfg.remote_control_bin.trim()).with_context(|| {
                format!(
                    "remote-control binary not found ({})",
                    self.cfg.remote_control_bin
                )
            })?;
            return Ok(TempBinary::borrowed(PathBuf::from(resolved)));
        }
        if self.cfg.skip_build {
            bail!("--skip-build requires --remote-control-bin");
        }
        let out = NamedTempFile::new().context("create temp binary path")?;
        self.build_binary("remote-control", out.path(), None)?;
        Ok(TempBinary::owned(out))
    }

    fn build_binary(&self, package: &str, out: &Path, target: Option<&str>) -> Result<()> {
        let root = repo_root();
        let mut cmd = Command::new("cargo");
        cmd.arg("build").arg("-p").arg(package);
        if let Some(target) = target {
            ensure_rust_target(target)?;
            cmd.arg("--target").arg(target);
        }
        cmd.current_dir(&root);
        let output = cmd.output().context("run cargo build")?;
        if !output.status.success() {
            bail!(
                "cargo build -p {} failed:\n{}{}",
                package,
                String::from_utf8_lossy(&output.stdout),
                String::from_utf8_lossy(&output.stderr)
            );
        }

        let built = built_binary_path(&root, package, target);
        fs::copy(&built, out)
            .with_context(|| format!("copy built binary from {}", built.display()))?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let mut perms = fs::metadata(out)?.permissions();
            perms.set_mode(0o755);
            fs::set_permissions(out, perms)?;
        }
        Ok(())
    }

    fn remote_darwin_arch(&self, addr: &str) -> Result<&'static str> {
        let output = self
            .run_ssh_command_output(addr, "uname -m")
            .context("probe remote architecture")?;
        match output.trim() {
            "x86_64" | "amd64" => Ok("amd64"),
            "arm64" | "aarch64" => Ok("arm64"),
            other => bail!("unsupported remote architecture {:?}", other),
        }
    }

    fn run_ssh_command(&self, addr: &str, remote_command: &str) -> Result<()> {
        let status = Command::new("ssh")
            .args([
                "-p",
                &self.cfg.ssh_port.to_string(),
                "-o",
                "BatchMode=yes",
                addr,
                remote_command,
            ])
            .status()
            .context("run ssh command")?;
        if !status.success() {
            bail!(
                "ssh command failed{}",
                status
                    .code()
                    .map(|code| format!(" with exit code {code}"))
                    .unwrap_or_default()
            );
        }
        Ok(())
    }

    fn run_ssh_command_output(&self, addr: &str, remote_command: &str) -> Result<String> {
        let output = Command::new("ssh")
            .args([
                "-p",
                &self.cfg.ssh_port.to_string(),
                "-o",
                "BatchMode=yes",
                addr,
                remote_command,
            ])
            .output()
            .context("run ssh command")?;
        if !output.status.success() {
            bail!(
                "ssh command failed:\n{}{}",
                String::from_utf8_lossy(&output.stdout),
                String::from_utf8_lossy(&output.stderr)
            );
        }
        Ok(String::from_utf8_lossy(&output.stdout).to_string())
    }

    fn run_scp(&self, local: &Path, remote: &str) -> Result<()> {
        let status = Command::new("scp")
            .args(["-P", &self.cfg.ssh_port.to_string()])
            .arg(local)
            .arg(remote)
            .status()
            .context("run scp")?;
        if !status.success() {
            bail!(
                "scp failed{}",
                status
                    .code()
                    .map(|code| format!(" with exit code {code}"))
                    .unwrap_or_default()
            );
        }
        Ok(())
    }
}

impl DriverProcess {
    fn stop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

impl WebDriverClient {
    fn new(base_url: String) -> Result<Self> {
        Ok(Self {
            base_url: base_url.trim_end_matches('/').to_string(),
            client: Client::builder()
                .timeout(Duration::from_secs(8))
                .build()
                .context("build webdriver client")?,
        })
    }

    fn new_session(&self) -> Result<String> {
        let (value, session_id) = self.do_json(
            reqwest::Method::POST,
            "/session",
            Some(&json!({
                "capabilities": {
                    "alwaysMatch": {
                        "browserName": "safari"
                    }
                }
            })),
        )?;
        if !session_id.is_empty() {
            return Ok(session_id);
        }
        value
            .get("sessionId")
            .and_then(Value::as_str)
            .map(ToOwned::to_owned)
            .ok_or_else(|| anyhow!("webdriver new session response missing session id"))
    }

    fn delete_session(&self, session_id: &str) -> Result<()> {
        let _ = self.do_json(
            reqwest::Method::DELETE,
            &format!("/session/{}", urlencoding::encode(session_id)),
            None,
        )?;
        Ok(())
    }

    fn navigate(&self, session_id: &str, target_url: &str) -> Result<()> {
        let _ = self.do_json(
            reqwest::Method::POST,
            &format!("/session/{}/url", urlencoding::encode(session_id)),
            Some(&json!({ "url": target_url })),
        )?;
        Ok(())
    }

    fn eval_string(&self, session_id: &str, script: &str) -> Result<String> {
        let payload = json!({ "script": script, "args": [] });
        let path = format!("/session/{}/execute/sync", urlencoding::encode(session_id));
        let result = self.do_json(reqwest::Method::POST, &path, Some(&payload));
        let (value, _) = match result {
            Ok(ok) => ok,
            Err(err) => {
                let path = format!("/session/{}/execute", urlencoding::encode(session_id));
                if err.to_string().contains("status=404") {
                    self.do_json(reqwest::Method::POST, &path, Some(&payload))?
                } else {
                    return Err(err);
                }
            }
        };
        value
            .as_str()
            .map(ToOwned::to_owned)
            .ok_or_else(|| anyhow!("decode webdriver script result"))
    }

    fn alert_text(&self, session_id: &str) -> Result<String> {
        let (value, _) = self.do_json(
            reqwest::Method::GET,
            &format!("/session/{}/alert/text", urlencoding::encode(session_id)),
            None,
        )?;
        value
            .as_str()
            .map(ToOwned::to_owned)
            .ok_or_else(|| anyhow!("decode alert text"))
    }

    fn set_alert_text(&self, session_id: &str, text: &str) -> Result<()> {
        let _ = self.do_json(
            reqwest::Method::POST,
            &format!("/session/{}/alert/text", urlencoding::encode(session_id)),
            Some(&json!({ "text": text })),
        )?;
        Ok(())
    }

    fn accept_alert(&self, session_id: &str) -> Result<()> {
        let _ = self.do_json(
            reqwest::Method::POST,
            &format!("/session/{}/alert/accept", urlencoding::encode(session_id)),
            Some(&json!({})),
        )?;
        Ok(())
    }

    fn do_json(
        &self,
        method: reqwest::Method,
        path: &str,
        payload: Option<&Value>,
    ) -> Result<(Value, String)> {
        let url = format!("{}{}", self.base_url, path);
        let mut req = self.client.request(method, url);
        if let Some(payload) = payload {
            req = req.json(payload);
        }
        let resp = req.send().context("webdriver request")?;
        let status = resp.status().as_u16();
        let text = resp.text().context("read webdriver response")?;
        let env = if text.trim().is_empty() {
            WdEnvelope {
                value: Value::Null,
                session_id: String::new(),
            }
        } else {
            serde_json::from_str::<WdEnvelope>(&text).unwrap_or(WdEnvelope {
                value: Value::Null,
                session_id: String::new(),
            })
        };
        if let Some(err) = extract_wd_error(status, &env.value) {
            return Err(err.into());
        }
        if !(200..300).contains(&status) {
            return Err(anyhow!(
                "webdriver error (status={}): {}",
                status,
                text.trim()
            ));
        }
        Ok((env.value, env.session_id))
    }
}

impl RemoteControlSession {
    fn share_url(&self) -> Result<String> {
        let state = self.state.lock().unwrap();
        if state.share_url.trim().is_empty() {
            bail!("missing share URL");
        }
        Ok(state.share_url.clone())
    }

    fn current_access_token(&self) -> String {
        self.state.lock().unwrap().access_token.clone()
    }

    fn logs(&self) -> String {
        self.state.lock().unwrap().logs.clone()
    }

    fn stop(&mut self, timeout: Duration) -> Result<()> {
        let _ = Command::new(&self.binary_path)
            .args(["stop", "--id", &self.id])
            .env("SI_REMOTE_CONTROL_HOME", self.home_dir.path())
            .output();

        let start = Instant::now();
        loop {
            if self.child.try_wait()?.is_some() {
                return Ok(());
            }
            if start.elapsed() >= timeout {
                let _ = self.child.kill();
                let _ = self.child.wait();
                bail!("timed out waiting for remote-control process to stop");
            }
            std::thread::sleep(Duration::from_millis(50));
        }
    }
}

struct SessionDeleteGuard<'a> {
    webdriver: &'a WebDriverClient,
    session_id: String,
}

impl Drop for SessionDeleteGuard<'_> {
    fn drop(&mut self) {
        let _ = self.webdriver.delete_session(&self.session_id);
    }
}

enum TempBinary {
    Owned(NamedTempFile),
    Borrowed(PathBuf),
}

impl TempBinary {
    fn owned(file: NamedTempFile) -> Self {
        Self::Owned(file)
    }

    fn borrowed(path: PathBuf) -> Self {
        Self::Borrowed(path)
    }

    fn path(&self) -> &Path {
        match self {
            Self::Owned(file) => file.path(),
            Self::Borrowed(path) => path.as_path(),
        }
    }
}

fn parse_cli(args: &[OsString]) -> Result<CliConfig> {
    let args = args
        .iter()
        .map(|arg| arg.to_string_lossy().into_owned())
        .collect::<Vec<_>>();
    if args.iter().any(|arg| arg == "--help" || arg == "-h") {
        print!("{USAGE}");
        return Ok(default_cli_config()?);
    }

    let settings = config::load().unwrap_or_default();
    let default_ssh_host = config::resolve_setting_value(
        &settings.development.safari.ssh.host,
        &settings.development.safari.ssh.host_env_key,
    );
    let default_ssh_user = config::resolve_setting_value(
        &settings.development.safari.ssh.user,
        &settings.development.safari.ssh.user_env_key,
    );
    let default_ssh_port = config::resolve_setting_value(
        &settings.development.safari.ssh.port,
        &settings.development.safari.ssh.port_env_key,
    )
    .parse::<u16>()
    .unwrap_or(0);

    let scenario_csv = value_for_flag(&args, "--scenarios")
        .unwrap_or_else(|| "readwrite,readonly,access-code,no-token".to_string());
    let scenarios = resolve_scenarios(&scenario_csv)?;
    let driver_startup = value_for_flag(&args, "--driver-timeout")
        .map(|value| parse_duration(&value))
        .transpose()?
        .unwrap_or(DEFAULT_DRIVER_TIMEOUT);
    let scenario_timeout = value_for_flag(&args, "--scenario-timeout")
        .map(|value| parse_duration(&value))
        .transpose()?
        .unwrap_or(DEFAULT_SCENARIO_TIMEOUT);
    let ssh_port = value_for_flag(&args, "--ssh-port")
        .map(|value| value.parse::<u16>().context("parse --ssh-port"))
        .transpose()?
        .unwrap_or(default_ssh_port);

    if driver_startup.is_zero() {
        bail!("--driver-timeout must be positive");
    }
    if scenario_timeout.is_zero() {
        bail!("--scenario-timeout must be positive");
    }

    Ok(CliConfig {
        remote_control_bin: value_for_flag(&args, "--remote-control-bin").unwrap_or_default(),
        safaridriver_bin: value_for_flag(&args, "--safaridriver-bin")
            .unwrap_or_else(|| "safaridriver".to_string()),
        safaridriver_port: value_for_flag(&args, "--safaridriver-port")
            .map(|value| value.parse::<u16>().context("parse --safaridriver-port"))
            .transpose()?
            .unwrap_or(0),
        bind: value_for_flag(&args, "--bind").unwrap_or_else(|| "127.0.0.1".to_string()),
        scenario_csv,
        scenarios,
        driver_startup,
        scenario_timeout,
        keep_artifacts: has_flag(&args, "--keep-artifacts"),
        verbose: has_flag(&args, "--verbose"),
        skip_build: has_flag(&args, "--skip-build"),
        ssh_host: value_for_flag(&args, "--ssh-host").unwrap_or(default_ssh_host),
        ssh_port,
        ssh_user: value_for_flag(&args, "--ssh-user").unwrap_or(default_ssh_user),
        no_ssh: has_flag(&args, "--no-ssh"),
    })
}

fn default_cli_config() -> Result<CliConfig> {
    Ok(CliConfig {
        remote_control_bin: String::new(),
        safaridriver_bin: "safaridriver".to_string(),
        safaridriver_port: 0,
        bind: "127.0.0.1".to_string(),
        scenario_csv: "readwrite,readonly,access-code,no-token".to_string(),
        scenarios: resolve_scenarios("readwrite,readonly,access-code,no-token")?,
        driver_startup: DEFAULT_DRIVER_TIMEOUT,
        scenario_timeout: DEFAULT_SCENARIO_TIMEOUT,
        keep_artifacts: false,
        verbose: false,
        skip_build: false,
        ssh_host: String::new(),
        ssh_port: 0,
        ssh_user: String::new(),
        no_ssh: false,
    })
}

fn resolve_scenarios(csv: &str) -> Result<Vec<ScenarioDef>> {
    let mut defs = Vec::new();
    for raw in csv.split(',') {
        let name = raw.trim().to_ascii_lowercase();
        if name.is_empty() {
            continue;
        }
        if defs.iter().any(|def: &ScenarioDef| def.name == name) {
            continue;
        }
        defs.push(match name.as_str() {
            "readwrite" => ScenarioDef {
                name,
                readwrite: true,
                token_in_url: true,
                access_code: String::new(),
                expected_state: "Live session".to_string(),
                prompts: Vec::new(),
            },
            "readonly" => ScenarioDef {
                name,
                readwrite: false,
                token_in_url: true,
                access_code: String::new(),
                expected_state: "Read-only".to_string(),
                prompts: Vec::new(),
            },
            "access-code" => ScenarioDef {
                name,
                readwrite: true,
                token_in_url: true,
                access_code: "2468".to_string(),
                expected_state: "Live session".to_string(),
                prompts: vec![("access code".to_string(), "2468".to_string())],
            },
            "no-token" => ScenarioDef {
                name,
                readwrite: true,
                token_in_url: false,
                access_code: String::new(),
                expected_state: "Live session".to_string(),
                prompts: vec![("access token".to_string(), "<dynamic-token>".to_string())],
            },
            other => {
                bail!(
                    "unknown scenario {:?} (allowed: readwrite, readonly, access-code, no-token)",
                    other
                )
            }
        });
    }
    if defs.is_empty() {
        bail!("no scenarios selected");
    }
    Ok(defs)
}

fn start_driver(
    binary: &str,
    port: u16,
    verbose: bool,
    _keep_artifacts: bool,
) -> Result<DriverProcess> {
    let mut child = Command::new(binary)
        .args(["-p", &port.to_string()])
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .context("start safaridriver")?;

    let logs = Arc::new(Mutex::new(String::new()));
    if let Some(stdout) = child.stdout.take() {
        let logs = logs.clone();
        std::thread::spawn(move || copy_stream(stdout, logs, verbose, "[safaridriver]"));
    }
    if let Some(stderr) = child.stderr.take() {
        let logs = logs.clone();
        std::thread::spawn(move || copy_stream(stderr, logs, verbose, "[safaridriver]"));
    }

    Ok(DriverProcess {
        child,
        base_url: format!("http://127.0.0.1:{port}"),
    })
}

fn wait_for_driver_ready(base_url: &str, timeout: Duration) -> Result<()> {
    let client = Client::builder()
        .timeout(Duration::from_secs(2))
        .build()
        .context("build driver readiness client")?;
    let deadline = Instant::now() + timeout;
    let status_url = format!("{}/status", base_url.trim_end_matches('/'));
    while Instant::now() < deadline {
        if let Ok(response) = client.get(&status_url).send() {
            if response.status().is_success() {
                let parsed = response.json::<Value>().unwrap_or(Value::Null);
                if parsed
                    .get("value")
                    .and_then(|value| value.get("ready"))
                    .and_then(Value::as_bool)
                    .unwrap_or(false)
                {
                    return Ok(());
                }
            }
        }
        std::thread::sleep(Duration::from_millis(200));
    }
    bail!("wait for safaridriver timed out")
}

fn start_remote_control_session(
    binary: &Path,
    bind: &str,
    port: u16,
    scenario: &ScenarioDef,
    verbose: bool,
) -> Result<RemoteControlSession> {
    let home_dir = TempDir::new().context("create temp home")?;
    let id = session_id(&scenario.name);
    let mut args = vec![
        "start".to_string(),
        "--cmd".to_string(),
        "cat".to_string(),
        "--bind".to_string(),
        bind.to_string(),
        "--port".to_string(),
        port.to_string(),
        "--id".to_string(),
        id.clone(),
        "--no-tunnel".to_string(),
        "--no-caffeinate".to_string(),
    ];
    if scenario.readwrite {
        args.push("--readwrite".to_string());
    }
    if !scenario.token_in_url {
        args.push("--no-token-in-url".to_string());
    }
    if !scenario.access_code.trim().is_empty() {
        args.push("--access-code".to_string());
        args.push(scenario.access_code.clone());
    }

    let mut child = Command::new(binary)
        .args(&args)
        .env("SI_REMOTE_CONTROL_HOME", home_dir.path())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .context("start remote-control")?;

    let state = Arc::new(Mutex::new(SessionLineState {
        share_url: String::new(),
        access_token: String::new(),
        logs: String::new(),
    }));
    if let Some(stdout) = child.stdout.take() {
        let state = state.clone();
        std::thread::spawn(move || copy_session_stream(stdout, state, verbose, "[remote-control]"));
    }
    if let Some(stderr) = child.stderr.take() {
        let state = state.clone();
        std::thread::spawn(move || copy_session_stream(stderr, state, verbose, "[remote-control]"));
    }

    let deadline = Instant::now() + DEFAULT_DRIVER_TIMEOUT;
    while Instant::now() < deadline {
        let snapshot = state.lock().unwrap();
        let ready = !snapshot.share_url.trim().is_empty()
            && (scenario.token_in_url || !snapshot.access_token.trim().is_empty());
        drop(snapshot);
        if ready {
            return Ok(RemoteControlSession {
                id,
                home_dir,
                child,
                binary_path: binary.to_path_buf(),
                state,
            });
        }
        if let Some(status) = child.try_wait()? {
            return Err(anyhow!(
                "remote-control exited early with status {status}\nlogs:\n{}",
                state.lock().unwrap().logs
            ));
        }
        std::thread::sleep(Duration::from_millis(50));
    }

    let _ = child.kill();
    let _ = child.wait();
    bail!("remote-control exited before share URL")
}

fn wait_for_expected_status(
    webdriver: &WebDriverClient,
    session_id: &str,
    expected: &str,
    session: &mut RemoteControlSession,
    timeout: Duration,
    prompts: &[(String, String)],
) -> Result<()> {
    let expected_lower = expected.to_ascii_lowercase();
    let deadline = Instant::now() + timeout;
    while Instant::now() < deadline {
        let _ = handle_prompt_if_present(webdriver, session_id, session, prompts);
        match webdriver.eval_string(
            session_id,
            "const el = document.getElementById('status'); return el ? (el.textContent || '') : '';",
        ) {
            Ok(status) if status.to_ascii_lowercase().contains(&expected_lower) => return Ok(()),
            Ok(_) => {}
            Err(_) => {}
        }
        std::thread::sleep(Duration::from_millis(250));
    }
    bail!(
        "status never reached {:?}\nremote-control logs:\n{}",
        expected,
        session.logs()
    )
}

fn handle_prompt_if_present(
    webdriver: &WebDriverClient,
    session_id: &str,
    session: &RemoteControlSession,
    prompts: &[(String, String)],
) -> Result<()> {
    if prompts.is_empty() {
        return Ok(());
    }
    let alert_text = match webdriver.alert_text(session_id) {
        Ok(text) => text,
        Err(err) if err.to_string().contains("no such alert") => return Ok(()),
        Err(err) => return Err(err),
    };
    let lower = alert_text.to_ascii_lowercase();
    for (key, value) in prompts {
        if !lower.contains(&key.to_ascii_lowercase()) {
            continue;
        }
        let mut actual = value.clone();
        if actual == "<dynamic-token>" {
            actual = session.current_access_token();
        }
        if !actual.trim().is_empty() {
            webdriver.set_alert_text(session_id, &actual)?;
        }
        webdriver.accept_alert(session_id)?;
        return Ok(());
    }
    Ok(())
}

fn copy_stream(
    stream: impl Read + Send + 'static,
    logs: Arc<Mutex<String>>,
    verbose: bool,
    prefix: &str,
) {
    let prefix = prefix.to_string();
    let reader = BufReader::new(stream);
    for line in reader.lines().map_while(Result::ok) {
        {
            let mut buf = logs.lock().unwrap();
            buf.push_str(&line);
            buf.push('\n');
        }
        if verbose {
            println!("{} {}", prefix, line);
        }
    }
}

fn copy_session_stream(
    stream: impl Read + Send + 'static,
    state: Arc<Mutex<SessionLineState>>,
    verbose: bool,
    prefix: &str,
) {
    let prefix = prefix.to_string();
    let reader = BufReader::new(stream);
    for line in reader.lines().map_while(Result::ok) {
        if verbose {
            println!("{} {}", prefix, line);
        }
        let mut state = state.lock().unwrap();
        state.logs.push_str(&line);
        state.logs.push('\n');
        if let Some(value) = parse_labeled_value(&line, "Share URL:") {
            state.share_url = value;
        }
        if let Some(value) = parse_labeled_value(&line, "Access Token:") {
            state.access_token = value;
        }
    }
}

fn parse_labeled_value(line: &str, label: &str) -> Option<String> {
    let index = line.find(label)?;
    let value = line[index + label.len()..].trim();
    (!value.is_empty()).then(|| value.to_string())
}

fn pick_free_port(bind: &str) -> Result<u16> {
    let host = if bind.trim().is_empty() || bind == "0.0.0.0" {
        "127.0.0.1"
    } else {
        bind
    };
    let listener = TcpListener::bind((host, 0)).context("allocate free port")?;
    Ok(listener.local_addr()?.port())
}

fn target_triple_for_arch(arch: &str) -> Result<&'static str> {
    match arch {
        "amd64" => Ok("x86_64-apple-darwin"),
        "arm64" => Ok("aarch64-apple-darwin"),
        other => bail!("unsupported remote architecture {:?}", other),
    }
}

fn ensure_rust_target(target: &str) -> Result<()> {
    let status = Command::new("rustup")
        .args(["target", "add", target])
        .status()
        .context("run rustup target add")?;
    if !status.success() {
        bail!("rustup target add {} failed", target);
    }
    Ok(())
}

fn ensure_binary_exists(binary: &str) -> Result<()> {
    if look_path(binary).is_some() {
        Ok(())
    } else {
        bail!("binary not found")
    }
}

fn look_path(binary: &str) -> Option<String> {
    Command::new("sh")
        .args(["-lc", &format!("command -v {}", shell_quote(binary))])
        .output()
        .ok()
        .filter(|output| output.status.success())
        .and_then(|output| {
            let value = String::from_utf8_lossy(&output.stdout).trim().to_string();
            (!value.is_empty()).then_some(value)
        })
}

fn built_binary_path(root: &Path, package: &str, target: Option<&str>) -> PathBuf {
    let binary = match package {
        "remote-control" => "remote-control",
        "rc-safari-smoke" => "rc-safari-smoke",
        other => other,
    };
    let mut path = root.join("target");
    if let Some(target) = target {
        path.push(target);
    }
    path.push("debug");
    path.push(binary);
    path
}

fn repo_root() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .parent()
        .and_then(|path| path.parent())
        .unwrap()
        .to_path_buf()
}

fn parse_duration(raw: &str) -> Result<Duration> {
    let raw = raw.trim();
    if raw.is_empty() {
        bail!("duration is required");
    }
    let (number, unit) = raw
        .chars()
        .position(|c| !c.is_ascii_digit())
        .map(|index| (&raw[..index], &raw[index..]))
        .unwrap_or((raw, "s"));
    let value = number.parse::<u64>().context("parse duration value")?;
    match unit {
        "ms" => Ok(Duration::from_millis(value)),
        "s" | "" => Ok(Duration::from_secs(value)),
        "m" => Ok(Duration::from_secs(value * 60)),
        "h" => Ok(Duration::from_secs(value * 3600)),
        other => bail!("unsupported duration unit {:?}", other),
    }
}

fn format_duration(duration: Duration) -> String {
    if duration.as_secs().is_multiple_of(3600) && !duration.is_zero() {
        format!("{}h", duration.as_secs() / 3600)
    } else if duration.as_secs().is_multiple_of(60) && !duration.is_zero() {
        format!("{}m", duration.as_secs() / 60)
    } else if duration.as_millis() < 1000 {
        format!("{}ms", duration.as_millis())
    } else {
        format!("{}s", duration.as_secs())
    }
}

fn shell_quote(value: &str) -> String {
    if value.is_empty() {
        return "''".to_string();
    }
    format!("'{}'", value.replace('\'', "'\"'\"'"))
}

fn shell_join(args: &[String]) -> String {
    args.iter()
        .map(|arg| shell_quote(arg))
        .collect::<Vec<_>>()
        .join(" ")
}

fn value_for_flag(args: &[String], flag: &str) -> Option<String> {
    args.windows(2)
        .find(|window| window[0] == flag)
        .map(|window| window[1].clone())
}

fn has_flag(args: &[String], flag: &str) -> bool {
    args.iter().any(|arg| arg == flag)
}

fn session_id(name: &str) -> String {
    let mut clean = name
        .trim()
        .to_ascii_lowercase()
        .chars()
        .map(|ch| {
            if ch.is_ascii_lowercase() || ch.is_ascii_digit() || ch == '-' {
                ch
            } else {
                '-'
            }
        })
        .collect::<String>();
    clean = clean.trim_matches('-').to_string();
    if clean.is_empty() {
        clean = "scenario".to_string();
    }
    format!(
        "safari-smoke-{}-{}",
        clean,
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap_or_default()
            .as_nanos()
    )
}

fn extract_wd_error(status: u16, raw: &Value) -> Option<WdError> {
    if raw.is_null() {
        if (200..300).contains(&status) {
            return None;
        }
        return Some(WdError {
            status,
            code: String::new(),
            message: "empty webdriver error payload".to_string(),
        });
    }
    if let Ok(parsed) = serde_json::from_value::<WdValueError>(raw.clone()) {
        if !parsed.error.trim().is_empty() {
            return Some(WdError {
                status,
                code: parsed.error.trim().to_string(),
                message: parsed.message.trim().to_string(),
            });
        }
    }
    None
}

#[cfg(test)]
mod tests {
    use super::{
        extract_wd_error, format_duration, parse_duration, parse_labeled_value, resolve_scenarios,
        session_id, shell_join, shell_quote,
    };
    use serde_json::{Value, json};

    #[test]
    fn resolve_scenarios_matches_supported_values() {
        let defs = resolve_scenarios("readwrite,readonly,readwrite").unwrap();
        assert_eq!(defs.len(), 2);
        assert_eq!(defs[0].name, "readwrite");
        assert_eq!(defs[1].name, "readonly");
        assert!(resolve_scenarios("readwrite,bad").is_err());
        assert!(resolve_scenarios(" , ").is_err());
    }

    #[test]
    fn parse_labeled_value_matches_go_behavior() {
        assert_eq!(
            parse_labeled_value("Share URL: https://example.com/abc", "Share URL:").as_deref(),
            Some("https://example.com/abc")
        );
        assert_eq!(
            parse_labeled_value("Access Token: tok_123", "Access Token:").as_deref(),
            Some("tok_123")
        );
        assert!(parse_labeled_value("nope", "Share URL:").is_none());
    }

    #[test]
    fn extract_wd_error_matches_go_behavior() {
        assert!(extract_wd_error(200, &json!({"ok": true})).is_none());
        let err =
            extract_wd_error(404, &json!({"error":"no such alert","message":"none"})).unwrap();
        assert_eq!(err.code, "no such alert");
        assert!(extract_wd_error(500, &Value::Null).is_some());
    }

    #[test]
    fn session_id_matches_go_shape() {
        let id = session_id("Read Write + Auth");
        assert!(id.starts_with("safari-smoke-read-write---auth-"));
    }

    #[test]
    fn shell_quote_and_join_match_go_behavior() {
        assert_eq!(shell_quote("abc"), "'abc'");
        assert_eq!(shell_quote("a'b"), "'a'\"'\"'b'");
        assert_eq!(
            shell_join(&["./bin".into(), "--name".into(), "A B".into()]),
            "'./bin' '--name' 'A B'"
        );
    }

    #[test]
    fn parse_and_format_duration_cover_supported_units() {
        assert_eq!(parse_duration("20s").unwrap().as_secs(), 20);
        assert_eq!(parse_duration("2m").unwrap().as_secs(), 120);
        assert_eq!(parse_duration("150ms").unwrap().as_millis(), 150);
        assert_eq!(format_duration(parse_duration("20s").unwrap()), "20s");
    }
}
