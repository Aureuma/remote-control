use std::{
    collections::HashMap,
    net::SocketAddr,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicU64, Ordering},
    },
    time::Duration,
};

use anyhow::Result;
use axum::{
    Json, Router,
    extract::{
        ConnectInfo, Query, State,
        ws::{Message as WsMessage, WebSocket, WebSocketUpgrade},
    },
    http::{HeaderMap, StatusCode},
    response::{Html, IntoResponse},
    routing::get,
};
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use tokio::{
    net::TcpListener,
    sync::{mpsc, oneshot},
    task::JoinHandle,
};

use crate::{
    flow::{FlowController, FlowEvent},
    power_macos,
    runtime_state::{self, SessionState},
    session, tunnel_cloudflare, websocket_support,
};

const UI_HTML: &str = include_str!("../../../internal/httpui/static/index.html");

#[derive(Debug, Clone)]
pub struct RunOptions {
    pub id: String,
    pub bind: String,
    pub port: u16,
    pub readonly: bool,
    pub max_clients: usize,
    pub flow_low_bytes: i64,
    pub flow_high_bytes: i64,
    pub flow_ack_bytes: i64,
    pub access_code: String,
    pub token_in_url: bool,
    pub token_expires_at: DateTime<Utc>,
    pub token: String,
    pub idle_timeout_seconds: i64,
    pub enable_tunnel: bool,
    pub tunnel_required: bool,
    pub cloudflared_binary: String,
    pub cloudflare_timeout: Duration,
    pub tunnel_mode: String,
    pub tunnel_hostname: String,
    pub tunnel_name: String,
    pub tunnel_token: String,
    pub tunnel_config_file: String,
    pub tunnel_credentials_file: String,
    pub enable_caffeinate: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Message {
    #[serde(rename = "type")]
    pub kind: String,
    #[serde(skip_serializing_if = "String::is_empty", default)]
    pub token: String,
    #[serde(skip_serializing_if = "String::is_empty", default)]
    pub code: String,
    #[serde(skip_serializing_if = "String::is_empty", default)]
    pub data: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub columns: Option<u16>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub rows: Option<u16>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub bytes: Option<i64>,
    #[serde(skip_serializing_if = "String::is_empty", default)]
    pub message: String,
}

#[derive(Clone)]
struct AppState {
    terminal: Arc<session::Terminal>,
    token: String,
    readonly: bool,
    max_clients: usize,
    access_code: String,
    token_expires_at: DateTime<Utc>,
    ack_quantum_bytes: i64,
    flow: Arc<Mutex<FlowController>>,
    clients: Arc<Mutex<HashMap<u64, mpsc::UnboundedSender<ServerOutbound>>>>,
    next_client_id: Arc<AtomicU64>,
    session_state: Arc<Mutex<SessionState>>,
    idle_timeout_seconds: i64,
    shutdown: Arc<AtomicBool>,
}

#[derive(Clone)]
enum ServerOutbound {
    Text(Message),
    Binary(Vec<u8>),
}

pub struct RunningServer {
    shutdown_tx: Option<oneshot::Sender<()>>,
    serve_task: JoinHandle<()>,
    terminal_task: JoinHandle<()>,
    shutdown: Arc<AtomicBool>,
}

impl RunningServer {
    pub async fn wait(self) -> Result<()> {
        let _ = self.serve_task.await;
        let _ = self.terminal_task.await;
        Ok(())
    }

    pub async fn shutdown(mut self) -> Result<()> {
        self.shutdown.store(true, Ordering::Relaxed);
        if let Some(tx) = self.shutdown_tx.take() {
            let _ = tx.send(());
        }
        let _ = self.serve_task.await;
        let _ = self.terminal_task.await;
        Ok(())
    }
}

pub async fn run_local_server(term: session::Terminal, opts: RunOptions) -> Result<i32> {
    let terminal = Arc::new(term);
    let addr = format!("{}:{}", opts.bind.trim(), opts.port);
    let local_url = format!("http://{addr}/");
    let require_code = !opts.access_code.trim().is_empty();
    let mut share_url =
        crate::app::build_share_url(&local_url, &opts.token, opts.token_in_url, require_code);

    let settings_path = crate::config::settings_path()
        .ok()
        .map(|path| path.display().to_string())
        .unwrap_or_default();

    let state = SessionState {
        id: opts.id.clone(),
        mode: match terminal.mode() {
            session::Mode::Attach => "attach".to_string(),
            session::Mode::Cmd => "cmd".to_string(),
            session::Mode::Tty => "tty".to_string(),
        },
        source: terminal.source().to_string(),
        readonly: opts.readonly,
        pid: std::process::id() as i32,
        addr: addr.clone(),
        url: local_url.clone(),
        local_url: local_url.clone(),
        public_url: String::new(),
        tunnel: "local".to_string(),
        started_at: Some(Utc::now()),
        token_expires_at: Some(opts.token_expires_at),
        idle_timeout_seconds: opts.idle_timeout_seconds.max(0) as i32,
        idle_deadline: idle_deadline_from_timeout(opts.idle_timeout_seconds),
        tunnel_mode: crate::app::normalize_tunnel_mode(&opts.tunnel_mode).to_string(),
        token_in_url: opts.token_in_url,
        access_code_auth: require_code,
        updated_at: None,
        client_count: 0,
        settings_file: settings_path,
        cloudflared_pid: 0,
        caffeinate_pid: 0,
    };
    runtime_state::save_session(&state)?;

    let listener = TcpListener::bind(&addr).await?;
    let state = AppState {
        terminal: terminal.clone(),
        token: opts.token.clone(),
        readonly: opts.readonly,
        max_clients: opts.max_clients.max(1),
        access_code: opts.access_code.clone(),
        token_expires_at: opts.token_expires_at,
        ack_quantum_bytes: opts.flow_ack_bytes.max(1),
        flow: Arc::new(Mutex::new(FlowController::new(
            opts.flow_low_bytes,
            opts.flow_high_bytes,
        ))),
        clients: Arc::new(Mutex::new(HashMap::new())),
        next_client_id: Arc::new(AtomicU64::new(1)),
        session_state: Arc::new(Mutex::new(state)),
        idle_timeout_seconds: opts.idle_timeout_seconds.max(0),
        shutdown: Arc::new(AtomicBool::new(false)),
    };

    let running = start_server(listener, state.clone()).await?;
    let mut tunnel_handle = None;
    let mut caffeinate_handle = None;

    if opts.enable_tunnel {
        match tunnel_cloudflare::start(&tunnel_cloudflare::Options {
            binary: opts.cloudflared_binary.clone(),
            local_url: local_url.trim_end_matches('/').to_string(),
            startup_timeout: opts.cloudflare_timeout,
            mode: opts.tunnel_mode.clone(),
            hostname: opts.tunnel_hostname.clone(),
            tunnel_name: opts.tunnel_name.clone(),
            tunnel_token: opts.tunnel_token.clone(),
            config_file: opts.tunnel_config_file.clone(),
            credentials_file: opts.tunnel_credentials_file.clone(),
        }) {
            Ok(handle) => {
                let public_url = handle.public_url().trim().to_string();
                share_url = crate::app::build_share_url(
                    &public_url,
                    &opts.token,
                    opts.token_in_url,
                    require_code,
                );
                {
                    let mut session = state
                        .session_state
                        .lock()
                        .unwrap_or_else(|poisoned| poisoned.into_inner());
                    session.tunnel = format!(
                        "cloudflare-{}",
                        crate::app::normalize_tunnel_mode(&opts.tunnel_mode)
                    );
                    session.public_url = public_url.clone();
                    session.url = public_url;
                    session.cloudflared_pid = handle.pid() as i32;
                    let _ = runtime_state::save_session(&session);
                }
                tunnel_handle = Some(handle);
            }
            Err(err) if opts.tunnel_required => {
                let _ = terminal.close();
                let _ = running.shutdown().await;
                let _ = runtime_state::remove_session(&opts.id);
                return Err(err);
            }
            Err(err) => {
                eprintln!("Tunnel unavailable; continuing in local mode: {err}");
            }
        }
    }

    if opts.enable_caffeinate {
        match power_macos::start() {
            Ok(Some(handle)) => {
                {
                    let mut session = state
                        .session_state
                        .lock()
                        .unwrap_or_else(|poisoned| poisoned.into_inner());
                    session.caffeinate_pid = handle.pid() as i32;
                    let _ = runtime_state::save_session(&session);
                }
                caffeinate_handle = Some(handle);
            }
            Ok(None) => {}
            Err(err) => {
                eprintln!("Could not start caffeinate: {err}");
            }
        }
    }

    println!("SI remote-control is live");
    println!("Session ID: {}", opts.id);
    println!("Share URL: {share_url}");
    println!("Local URL: {local_url}");
    if !opts.token_in_url {
        println!("Access Token: {}", opts.token);
    }
    if require_code {
        println!("Access Code: {}", opts.access_code);
    }
    println!("Token expires: {}", opts.token_expires_at.to_rfc3339());
    if let Some(public_url) = tunnel_handle.as_ref().map(|handle| handle.public_url()) {
        println!("Tunnel URL: {public_url}");
    }
    println!(
        "Mode: {}",
        if opts.readonly {
            "read-only"
        } else {
            "read-write"
        }
    );
    if opts.idle_timeout_seconds > 0 {
        println!("Idle timeout: {}s", opts.idle_timeout_seconds);
    }
    println!("Open the URL in Chrome or Safari.");
    println!("Press Ctrl+C to stop sharing.");

    let mut idle_task = if opts.idle_timeout_seconds > 0 {
        Some(tokio::spawn(watch_idle_timeout(state.clone())))
    } else {
        None
    };

    tokio::select! {
        _ = tokio::signal::ctrl_c() => {}
        result = tokio::task::spawn_blocking({
            let terminal = terminal.clone();
            move || terminal.wait()
        }) => {
            result??;
        }
        _ = async {
            if let Some(task) = idle_task.take() {
                let _ = task.await;
            }
        } => {
            println!("Idle timeout reached. Session stopped.");
        }
    }

    if let Some(task) = idle_task.take() {
        task.abort();
    }
    let _ = terminal.close();
    running.shutdown().await?;
    drop(caffeinate_handle);
    drop(tunnel_handle);
    let _ = runtime_state::remove_session(&opts.id);
    Ok(0)
}

async fn start_server(listener: TcpListener, state: AppState) -> Result<RunningServer> {
    let router = Router::new()
        .route("/", get(index_handler))
        .route("/healthz", get(health_handler))
        .route("/ws", get(ws_handler))
        .with_state(state.clone());

    let (shutdown_tx, shutdown_rx) = oneshot::channel();
    let serve_task = tokio::spawn(async move {
        let server = axum::serve(
            listener,
            router.into_make_service_with_connect_info::<SocketAddr>(),
        )
        .with_graceful_shutdown(async move {
            let _ = shutdown_rx.await;
        });
        let _ = server.await;
    });

    let terminal_state = state.clone();
    let terminal_task = tokio::spawn(async move {
        let _ = tokio::task::spawn_blocking(move || terminal_read_loop(terminal_state)).await;
    });

    Ok(RunningServer {
        shutdown_tx: Some(shutdown_tx),
        serve_task,
        terminal_task,
        shutdown: state.shutdown.clone(),
    })
}

async fn index_handler() -> Html<&'static str> {
    Html(UI_HTML)
}

async fn health_handler(State(state): State<AppState>) -> Json<serde_json::Value> {
    let session = state
        .session_state
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner());
    Json(serde_json::json!({
        "ok": true,
        "id": session.id,
    }))
}

async fn ws_handler(
    ws: WebSocketUpgrade,
    State(state): State<AppState>,
    headers: HeaderMap,
    ConnectInfo(addr): ConnectInfo<SocketAddr>,
    Query(_query): Query<HashMap<String, String>>,
) -> impl IntoResponse {
    let host = headers
        .get("host")
        .and_then(|value| value.to_str().ok())
        .unwrap_or_default();
    let origin = headers
        .get("origin")
        .and_then(|value| value.to_str().ok())
        .unwrap_or_default();
    if !websocket_support::is_origin_allowed(origin, host) {
        return StatusCode::FORBIDDEN.into_response();
    }
    ws.on_upgrade(move |socket| handle_socket(socket, state, addr))
        .into_response()
}

async fn handle_socket(mut socket: WebSocket, state: AppState, _addr: SocketAddr) {
    let auth_payload = tokio::time::timeout(Duration::from_secs(20), socket.recv()).await;
    let Some(Ok(WsMessage::Text(text))) = auth_payload.ok().flatten() else {
        let _ = socket
            .send(WsMessage::Text(
                serde_json::to_string(&Message {
                    kind: "auth_error".to_string(),
                    token: String::new(),
                    code: String::new(),
                    data: String::new(),
                    columns: None,
                    rows: None,
                    bytes: None,
                    message: "auth required".to_string(),
                })
                .unwrap()
                .into(),
            ))
            .await;
        return;
    };

    let Ok(auth) = serde_json::from_str::<Message>(&text) else {
        let _ = socket
            .send(WsMessage::Text(
                serde_json::to_string(&Message {
                    kind: "auth_error".to_string(),
                    token: String::new(),
                    code: String::new(),
                    data: String::new(),
                    columns: None,
                    rows: None,
                    bytes: None,
                    message: "invalid auth payload".to_string(),
                })
                .unwrap()
                .into(),
            ))
            .await;
        return;
    };

    if auth.kind != "auth" {
        let _ = socket
            .send(WsMessage::Text(
                serde_json::to_string(&Message {
                    kind: "auth_error".to_string(),
                    token: String::new(),
                    code: String::new(),
                    data: String::new(),
                    columns: None,
                    rows: None,
                    bytes: None,
                    message: "auth required".to_string(),
                })
                .unwrap()
                .into(),
            ))
            .await;
        return;
    }
    if websocket_support::token_expired(Some(state.token_expires_at), Some(Utc::now())) {
        let _ = socket
            .send(WsMessage::Text(
                serde_json::to_string(&Message {
                    kind: "auth_error".to_string(),
                    token: String::new(),
                    code: String::new(),
                    data: String::new(),
                    columns: None,
                    rows: None,
                    bytes: None,
                    message: "token expired".to_string(),
                })
                .unwrap()
                .into(),
            ))
            .await;
        return;
    }
    if auth.token != state.token {
        let _ = socket
            .send(WsMessage::Text(
                serde_json::to_string(&Message {
                    kind: "auth_error".to_string(),
                    token: String::new(),
                    code: String::new(),
                    data: String::new(),
                    columns: None,
                    rows: None,
                    bytes: None,
                    message: "invalid token".to_string(),
                })
                .unwrap()
                .into(),
            ))
            .await;
        return;
    }
    if !state.access_code.trim().is_empty() {
        if auth.code.trim().is_empty() {
            let _ = socket
                .send(WsMessage::Text(
                    serde_json::to_string(&Message {
                        kind: "auth_error".to_string(),
                        token: String::new(),
                        code: String::new(),
                        data: String::new(),
                        columns: None,
                        rows: None,
                        bytes: None,
                        message: "access code required".to_string(),
                    })
                    .unwrap()
                    .into(),
                ))
                .await;
            return;
        }
        if auth.code.trim() != state.access_code.trim() {
            let _ = socket
                .send(WsMessage::Text(
                    serde_json::to_string(&Message {
                        kind: "auth_error".to_string(),
                        token: String::new(),
                        code: String::new(),
                        data: String::new(),
                        columns: None,
                        rows: None,
                        bytes: None,
                        message: "invalid access code".to_string(),
                    })
                    .unwrap()
                    .into(),
                ))
                .await;
            return;
        }
    }
    if let (Some(cols), Some(rows)) = (auth.columns, auth.rows) {
        let _ = state.terminal.resize(cols, rows);
    }

    let (tx, mut rx) = mpsc::unbounded_channel();
    let client_id = state.next_client_id.fetch_add(1, Ordering::Relaxed);
    let at_limit = {
        let clients = state
            .clients
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        clients.len() >= state.max_clients
    };
    if at_limit {
        let _ = socket
            .send(WsMessage::Text(
                serde_json::to_string(&Message {
                    kind: "info".to_string(),
                    token: String::new(),
                    code: String::new(),
                    data: String::new(),
                    columns: None,
                    rows: None,
                    bytes: None,
                    message: "another client is already connected".to_string(),
                })
                .unwrap()
                .into(),
            ))
            .await;
        let _ = socket.send(WsMessage::Close(None)).await;
        return;
    }
    state
        .clients
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner())
        .insert(client_id, tx);
    update_client_count(&state);

    if let Err(err) = socket
        .send(WsMessage::Text(
            serde_json::to_string(&Message {
                kind: "auth_ok".to_string(),
                token: String::new(),
                code: String::new(),
                data: String::new(),
                columns: None,
                rows: None,
                bytes: None,
                message: String::new(),
            })
            .unwrap()
            .into(),
        ))
        .await
    {
        eprintln!("ws send auth_ok failed: {err}");
    }
    if let Err(err) = socket
        .send(WsMessage::Text(
            serde_json::to_string(&Message {
                kind: "prefs".to_string(),
                token: String::new(),
                code: String::new(),
                data: String::new(),
                columns: None,
                rows: None,
                bytes: Some(state.ack_quantum_bytes),
                message: String::new(),
            })
            .unwrap()
            .into(),
        ))
        .await
    {
        eprintln!("ws send prefs failed: {err}");
    }
    if state.readonly {
        if let Err(err) = socket
            .send(WsMessage::Text(
                serde_json::to_string(&Message {
                    kind: "readonly".to_string(),
                    token: String::new(),
                    code: String::new(),
                    data: String::new(),
                    columns: None,
                    rows: None,
                    bytes: None,
                    message: "Read-only mode enabled".to_string(),
                })
                .unwrap()
                .into(),
            ))
            .await
        {
            eprintln!("ws send readonly failed: {err}");
        }
    }

    loop {
        tokio::select! {
            outbound = rx.recv() => {
                let Some(outbound) = outbound else { break; };
                match outbound {
                    ServerOutbound::Text(payload) => {
                        let _ = socket.send(WsMessage::Text(serde_json::to_string(&payload).unwrap().into())).await;
                    }
                    ServerOutbound::Binary(data) => {
                        let _ = socket.send(WsMessage::Binary(data.into())).await;
                    }
                }
            }
            inbound = socket.recv() => {
                let Some(Ok(message)) = inbound else { break; };
                match message {
                    WsMessage::Text(text) => {
                        if let Ok(payload) = serde_json::from_str::<Message>(&text) {
                            handle_client_message(&state, client_id, payload);
                        }
                    }
                    WsMessage::Binary(_) => {}
                    WsMessage::Close(_) => break,
                    WsMessage::Ping(data) => {
                        let _ = socket.send(WsMessage::Text(
                            serde_json::to_string(&Message {
                                kind: "pong".to_string(),
                                token: String::new(),
                                code: String::new(),
                                data: String::from_utf8_lossy(&data).to_string(),
                                columns: None,
                                rows: None,
                                bytes: None,
                                message: String::new(),
                            }).unwrap().into()
                        )).await;
                    }
                    WsMessage::Pong(_) => {}
                }
            }
        }
    }

    {
        let mut clients = state
            .clients
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        clients.remove(&client_id);
    }
    update_client_count(&state);
    if state
        .clients
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner())
        .is_empty()
    {
        state
            .flow
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
            .reset();
    }
}

fn handle_client_message(state: &AppState, client_id: u64, message: Message) {
    match message.kind.trim().to_ascii_lowercase().as_str() {
        "input" => {
            if state.readonly {
                broadcast_one(
                    state,
                    client_id,
                    ServerOutbound::Text(Message {
                        kind: "readonly".to_string(),
                        token: String::new(),
                        code: String::new(),
                        data: String::new(),
                        columns: None,
                        rows: None,
                        bytes: None,
                        message: "Input blocked: read-only session".to_string(),
                    }),
                );
                return;
            }
            let _ = state.terminal.write_input(message.data.as_bytes());
        }
        "resize" => {
            let _ = state
                .terminal
                .resize(message.columns.unwrap_or(0), message.rows.unwrap_or(0));
        }
        "ping" => {
            broadcast_one(
                state,
                client_id,
                ServerOutbound::Text(Message {
                    kind: "pong".to_string(),
                    token: String::new(),
                    code: String::new(),
                    data: String::new(),
                    columns: None,
                    rows: None,
                    bytes: None,
                    message: String::new(),
                }),
            );
        }
        "ack" => {
            let bytes = message.bytes.unwrap_or(0);
            let mut flow = state
                .flow
                .lock()
                .unwrap_or_else(|poisoned| poisoned.into_inner());
            if flow.on_ack(bytes) == FlowEvent::Resume {
                drop(flow);
                broadcast_all(
                    state,
                    ServerOutbound::Text(Message {
                        kind: "flow_resume".to_string(),
                        token: String::new(),
                        code: String::new(),
                        data: String::new(),
                        columns: None,
                        rows: None,
                        bytes: None,
                        message: "Output resumed".to_string(),
                    }),
                );
            }
        }
        _ => {}
    }
}

fn terminal_read_loop(state: AppState) -> Result<()> {
    let mut buf = [0_u8; 4096];
    loop {
        if state.shutdown.load(Ordering::Relaxed) {
            return Ok(());
        }
        {
            let clients = state
                .clients
                .lock()
                .unwrap_or_else(|poisoned| poisoned.into_inner());
            if clients.is_empty() {
                std::thread::sleep(Duration::from_millis(25));
                continue;
            }
        }
        {
            let flow = state
                .flow
                .lock()
                .unwrap_or_else(|poisoned| poisoned.into_inner());
            if flow.paused {
                drop(flow);
                std::thread::sleep(Duration::from_millis(25));
                continue;
            }
        }
        let n = state.terminal.read(&mut buf)?;
        if n == 0 {
            continue;
        }
        broadcast_all(&state, ServerOutbound::Binary(buf[..n].to_vec()));
        let mut flow = state
            .flow
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        if flow.on_sent(n) == FlowEvent::Pause {
            drop(flow);
            broadcast_all(
                &state,
                ServerOutbound::Text(Message {
                    kind: "flow_pause".to_string(),
                    token: String::new(),
                    code: String::new(),
                    data: String::new(),
                    columns: None,
                    rows: None,
                    bytes: None,
                    message: "Network is slow; pausing output to protect session".to_string(),
                }),
            );
        }
    }
}

fn broadcast_all(state: &AppState, message: ServerOutbound) {
    let senders = state
        .clients
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner())
        .values()
        .cloned()
        .collect::<Vec<_>>();
    for sender in senders {
        let _ = sender.send(message.clone());
    }
}

fn broadcast_one(state: &AppState, client_id: u64, message: ServerOutbound) {
    if let Some(sender) = state
        .clients
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner())
        .get(&client_id)
        .cloned()
    {
        let _ = sender.send(message);
    }
}

fn update_client_count(state: &AppState) {
    let count = state
        .clients
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner())
        .len() as i32;
    let mut session = state
        .session_state
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner());
    session.client_count = count;
    session.idle_deadline = if state.idle_timeout_seconds > 0 && count == 0 {
        idle_deadline_from_timeout(state.idle_timeout_seconds)
    } else {
        None
    };
    let _ = runtime_state::save_session(&session);
}

fn idle_deadline_from_timeout(timeout_seconds: i64) -> Option<DateTime<Utc>> {
    if timeout_seconds <= 0 {
        return None;
    }
    Some(Utc::now() + chrono::Duration::seconds(timeout_seconds))
}

async fn watch_idle_timeout(state: AppState) {
    loop {
        tokio::time::sleep(Duration::from_secs(1)).await;
        let idle_deadline = {
            let session = state
                .session_state
                .lock()
                .unwrap_or_else(|poisoned| poisoned.into_inner());
            if session.client_count > 0 {
                None
            } else {
                session.idle_deadline
            }
        };
        let Some(idle_deadline) = idle_deadline else {
            continue;
        };
        if Utc::now() >= idle_deadline {
            return;
        }
    }
}
