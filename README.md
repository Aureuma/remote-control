# remote-control

`remote-control` serves a local terminal session to a browser.

Implemented scope (Phase 0-5):
- Go-first server with embedded browser UI.
- tmux session discovery and attach mode.
- command mode (`--cmd`) for spawning a new PTY process.
- websocket auth handshake with per-session token.
- websocket flow control (ACK + high/low watermarks).
- browser reconnect behavior with automatic resume.
- optional Cloudflare ephemeral tunnel integration.
- optional macOS `caffeinate` integration.
- settings and runtime state under `~/.si/remote-control`.
- token TTL enforcement and idle-timeout session shutdown.
- same-host/origin websocket checks compatible with public tunnel hostnames.
- regression tests for flow, tunnel URL parsing, token expiry, tmux parsing, and runtime state pruning.

Production hardening implemented:
- named Cloudflare tunnel mode (`--tunnel-mode named`) with custom hostname support.
- direct existing TTY attach (`attach --tty-path /dev/pts/N`) in addition to tmux attach.
- optional access-code auth and optional token omission from URL.
- release automation for tagged binaries/checksums.
- expanded soak/chaos coverage for reconnect, auth, and attach modes.
- detailed roadmap in `docs/production-hardening-plan.md`.

## Commands

- `remote-control sessions [--all]`
- `remote-control attach [--tmux-session <name> | --tty-path <path>] [--port 8080] [--tunnel|--no-tunnel]`
- `remote-control start --cmd "bash" [--port 8080] [--tunnel|--no-tunnel] [--tunnel-mode ephemeral|named] [--tunnel-hostname <host>]`
- `remote-control status`
- `remote-control stop --id <session-id>`

## Notes

- Default mode is read-only.
- Tunnel is enabled by default; if `cloudflared` is missing, command falls back to local-only mode unless `--tunnel-required` is set.
- For attach mode, tmux must be installed and session name must exist.
- For direct TTY attach mode, pass an explicit TTY path (`--tty-path /dev/pts/N`).
- For stronger auth, use `--access-code <code>` and optionally `--no-token-in-url`.

## Real Safari Smoke (macOS)

- Command: `go run ./cmd/rc-safari-smoke`
- This uses a real local Safari instance via `safaridriver` (not headless/webkit emulation).
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
- `--remote-control-bin /path/to/remote-control` (skip auto-build)

### Dev SSH fallback (non-macOS host)

When `rc-safari-smoke` runs on Linux, it can automatically run the smoke suite over SSH on a macOS machine.
Store SSH config in `~/.si/remote-control/settings.toml` under `development.safari.ssh`.

Use env-key references only (no hardcoded secrets in repo):

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
- Release workflow builds:
  - `remote-control-linux-amd64`
  - `remote-control-linux-arm64`
  - `remote-control-darwin-amd64`
  - `remote-control-darwin-arm64`
- Workflow publishes binaries and `checksums.txt` to the GitHub Release.
