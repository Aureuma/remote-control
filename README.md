# remote-control

`remote-control` serves a local terminal session to a browser from Rust binaries.

Implemented scope:
- Rust server with embedded browser UI.
- tmux session discovery and attach mode.
- command mode (`--cmd`) for spawning a new PTY process.
- websocket auth handshake with per-session token.
- websocket flow control (ACK + high/low watermarks).
- browser reconnect behavior with automatic resume.
- optional Cloudflare tunnel integration.
- optional macOS `caffeinate` integration.
- settings and runtime state under `~/.si/remote-control`.
- token TTL enforcement and idle-timeout session shutdown.
- same-host/origin websocket checks compatible with public tunnel hostnames.
- real Safari smoke coverage via the Rust `rc-safari-smoke` binary.

## Commands

- `cargo run -p remote-control -- sessions [--all]`
- `cargo run -p remote-control -- attach [--tmux-session <name> | --tty-path <path>] [--port 8080] [--tunnel|--no-tunnel]`
- `cargo run -p remote-control -- start --cmd "bash" [--port 8080] [--tunnel|--no-tunnel] [--tunnel-mode ephemeral|named] [--tunnel-hostname <host>]`
- `cargo run -p remote-control -- status`
- `cargo run -p remote-control -- stop --id <session-id>`

You can also build standalone binaries:

```bash
cargo build -p remote-control -p rc-safari-smoke
./target/debug/remote-control --help
./target/debug/rc-safari-smoke --help
```

## Notes

- Default mode is read-only.
- Tunnel is enabled by default; if `cloudflared` is missing, command falls back to local-only mode unless `--tunnel-required` is set.
- For attach mode, tmux must be installed and the session name must exist.
- For direct TTY attach mode, pass an explicit TTY path (`--tty-path /dev/pts/N`).
- For stronger auth, use `--access-code <code>` and optionally `--no-token-in-url`.

## Browser Validation

- Install browser test deps: `npm ci`
- Install Playwright browsers: `npx playwright install chromium webkit`
- Run browser suite against the Rust binary: `npm run test:browser`

## Real Safari Smoke (macOS)

- Command: `cargo run -p rc-safari-smoke --`
- This uses a real local Safari instance via `safaridriver`.
- Default scenarios:
  - `readwrite`
  - `readonly`
  - `access-code`
  - `no-token`

### Prerequisites (once per Mac)

1. Open Safari -> Settings -> Advanced -> enable `Show Develop menu in menu bar`.
2. Safari menu bar -> Develop -> enable `Allow Remote Automation`.
3. Run once in terminal: `safaridriver --enable`

### Useful flags

- `--scenarios readwrite,readonly`
- `--scenario-timeout 60s`
- `--driver-timeout 20s`
- `--verbose`
- `--keep-artifacts`
- `--remote-control-bin /path/to/remote-control`

### Dev SSH fallback (non-macOS host)

When `rc-safari-smoke` runs on a non-macOS host, it can run the smoke suite over SSH on a macOS machine.
Store SSH config in `~/.si/remote-control/settings.toml` under `development.safari.ssh`.

Use env-key references only:

```toml
[development]
enabled = true

[development.safari.ssh]
host_env_key = "SHAWN_MAC_HOST"
port_env_key = "SHAWN_MAC_PORT"
user_env_key = "SHAWN_MAC_USER"
```

Supported reference formats:
- `env:KEY_NAME`
- `${KEY_NAME}`
- bare key names like `KEY_NAME`

CLI overrides:
- `--ssh-host`
- `--ssh-port`
- `--ssh-user`
- `--no-ssh`

## Releases

- Tag pushes matching `v*` trigger `.github/workflows/release.yml`.
- CI and release pipelines build the Rust `remote-control` binary.
