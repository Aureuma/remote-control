# remote-control

`remote-control` serves a local terminal session to a browser.

MVP (Phase 0-1):
- Go-first server with embedded browser UI.
- tmux session discovery and attach mode.
- command mode (`--cmd`) for spawning a new PTY process.
- websocket auth handshake with per-session token.
- settings and runtime state under `~/.si/remote-control`.

## Commands

- `remote-control sessions`
- `remote-control attach --tmux-session <name> [--port 8080]`
- `remote-control start --cmd "bash" [--port 8080]`
- `remote-control status`
- `remote-control stop --id <session-id>`

## Notes

- Default mode is read-only.
- For attach mode, tmux must be installed and session name must exist.
