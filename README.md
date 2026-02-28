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

## Commands

- `remote-control sessions`
- `remote-control attach --tmux-session <name> [--port 8080] [--tunnel|--no-tunnel]`
- `remote-control start --cmd "bash" [--port 8080] [--tunnel|--no-tunnel]`
- `remote-control status`
- `remote-control stop --id <session-id>`

## Notes

- Default mode is read-only.
- Tunnel is enabled by default; if `cloudflared` is missing, command falls back to local-only mode unless `--tunnel-required` is set.
- For attach mode, tmux must be installed and session name must exist.
