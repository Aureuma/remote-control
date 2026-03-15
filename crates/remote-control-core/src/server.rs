use std::{
    collections::HashMap,
    net::SocketAddr,
    sync::{
        Arc, Mutex,
        atomic::{AtomicU64, Ordering},
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
use futures_util::{SinkExt, StreamExt};
use serde::{Deserialize, Serialize};
use tokio::{
    net::TcpListener,
    sync::{mpsc, oneshot},
    task::JoinHandle,
};

use crate::{
    flow::{FlowController, FlowEvent},
    runtime_state::{self, SessionState},
    session, websocket_support,
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
}

impl RunningServer {
    pub async fn wait(self) -> Result<()> {
        let _ = self.serve_task.await;
        let _ = self.terminal_task.await;
        Ok(())
    }

    pub async fn shutdown(mut self) -> Result<()> {
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
    let share_url =
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
        idle_timeout_seconds: 0,
        idle_deadline: None,
        tunnel_mode: "ephemeral".to_string(),
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
    };

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
    println!(
        "Mode: {}",
        if opts.readonly {
            "read-only"
        } else {
            "read-write"
        }
    );
    println!("Open the URL in Chrome or Safari.");
    println!("Press Ctrl+C to stop sharing.");

    let running = start_server(listener, state.clone()).await?;

    tokio::select! {
        _ = tokio::signal::ctrl_c() => {}
        result = tokio::task::spawn_blocking({
            let terminal = terminal.clone();
            move || terminal.wait()
        }) => {
            result??;
        }
    }

    running.shutdown().await?;
    let _ = terminal.close();
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

async fn handle_socket(socket: WebSocket, state: AppState, _addr: SocketAddr) {
    let (mut sender, mut receiver) = socket.split();

    let auth_payload = tokio::time::timeout(Duration::from_secs(20), receiver.next()).await;
    let Some(Ok(WsMessage::Text(text))) = auth_payload.ok().flatten() else {
        let _ = sender
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
        let _ = sender
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
        let _ = sender
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
        let _ = sender
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
        let _ = sender
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
            let _ = sender
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
            let _ = sender
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
        let _ = sender
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
        let _ = sender.send(WsMessage::Close(None)).await;
        return;
    }
    state
        .clients
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner())
        .insert(client_id, tx);
    update_client_count(&state);

    let send_task = tokio::spawn(async move {
        while let Some(message) = rx.recv().await {
            match message {
                ServerOutbound::Text(payload) => {
                    let _ = sender
                        .send(WsMessage::Text(
                            serde_json::to_string(&payload).unwrap().into(),
                        ))
                        .await;
                }
                ServerOutbound::Binary(data) => {
                    let _ = sender.send(WsMessage::Binary(data.into())).await;
                }
            }
        }
    });

    broadcast_one(
        &state,
        client_id,
        ServerOutbound::Text(Message {
            kind: "auth_ok".to_string(),
            token: String::new(),
            code: String::new(),
            data: String::new(),
            columns: None,
            rows: None,
            bytes: None,
            message: String::new(),
        }),
    );
    broadcast_one(
        &state,
        client_id,
        ServerOutbound::Text(Message {
            kind: "prefs".to_string(),
            token: String::new(),
            code: String::new(),
            data: String::new(),
            columns: None,
            rows: None,
            bytes: Some(state.ack_quantum_bytes),
            message: String::new(),
        }),
    );
    if state.readonly {
        broadcast_one(
            &state,
            client_id,
            ServerOutbound::Text(Message {
                kind: "readonly".to_string(),
                token: String::new(),
                code: String::new(),
                data: String::new(),
                columns: None,
                rows: None,
                bytes: None,
                message: "Read-only mode enabled".to_string(),
            }),
        );
    }

    while let Some(Ok(message)) = receiver.next().await {
        match message {
            WsMessage::Text(text) => {
                if let Ok(payload) = serde_json::from_str::<Message>(&text) {
                    handle_client_message(&state, client_id, payload);
                }
            }
            WsMessage::Binary(_) => {}
            WsMessage::Close(_) => break,
            WsMessage::Ping(data) => {
                broadcast_one(
                    &state,
                    client_id,
                    ServerOutbound::Text(Message {
                        kind: "pong".to_string(),
                        token: String::new(),
                        code: String::new(),
                        data: String::from_utf8_lossy(&data).to_string(),
                        columns: None,
                        rows: None,
                        bytes: None,
                        message: String::new(),
                    }),
                );
            }
            WsMessage::Pong(_) => {}
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
    send_task.abort();
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
    let _ = runtime_state::save_session(&session);
}
