# Remote Control Production Hardening Plan (Post-MVP)

Date: 2026-02-28
Owner: SI core
Status: Implemented (except manual Safari-on-macOS validation)

This plan covers the remaining work after MVP, excluding manual Safari-on-macOS validation.

## Scope

1. Persistent/named Cloudflare tunnel + custom hostname support.
2. Non-tmux existing TTY attach support.
3. Stronger access controls beyond token-in-URL baseline.
4. Standalone release/distribution pipeline.
5. Soak/chaos testing expansion.

## Goals and acceptance criteria

### 1) Tunnel modes

Goals:
- Support both ephemeral and named Cloudflare tunnel modes.
- Support explicit hostname routing in named mode.
- Keep current ephemeral flow as default and backward-compatible.

Acceptance:
- `start` and `attach` can run with `--tunnel-mode named`.
- Named mode can resolve share URL from configured hostname without fragile log parsing.
- Existing ephemeral mode behavior remains unchanged for current users.

### 2) Existing TTY attach (non-tmux)

Goals:
- Allow attaching to an existing PTY device path directly.
- Provide user-friendly discovery output for PTY candidates.

Acceptance:
- `attach --tty-path /dev/pts/N` works.
- `sessions --all` includes PTY-backed process candidates.
- tmux attach remains default path and unchanged.

### 3) Stronger auth controls

Goals:
- Add optional access code requirement in addition to session token.
- Support sessions where token is not embedded in URL.
- Improve browser auth UX for token/code entry.

Acceptance:
- Access-code-protected sessions reject clients without valid code.
- URL-token embedding can be disabled (`--no-token-in-url`) while still allowing auth.
- Browser UI can prompt for missing token/code and proceed.

### 4) Release pipeline

Goals:
- Produce versioned binaries on Git tags.
- Publish checksums and platform artifacts automatically.

Acceptance:
- GitHub Actions release workflow builds `linux`/`darwin` `amd64`/`arm64`.
- Checksums are attached to release assets.
- Workflow is documented in README.

### 5) Soak/chaos testing

Goals:
- Stress core session transport behavior under reconnect and high-output cases.
- Expand unit/integration coverage for new tunnel/auth/TTY features.

Acceptance:
- Added regression tests for tunnel mode selection and URL resolution.
- Added auth tests for access code success/failure.
- Added reconnect/flow-related integration test coverage.

## Implementation order

### Phase A: config + command surface

1. Extend config schema:
   - `tunnel.mode`
   - `tunnel.named.hostname`
   - `tunnel.named.tunnel_name`
   - `tunnel.named.tunnel_token`
   - `tunnel.named.config_file`
   - `tunnel.named.credentials_file`
   - `security.access_code`
   - `security.token_in_url`
2. Extend CLI flags in `start` and `attach` to override settings.
3. Update usage/help and README examples.

Status: done

### Phase B: runtime feature implementation

1. Tunnel manager:
   - Add mode-aware argument building.
   - Determine public URL by hostname in named mode.
2. Session layer:
   - Add `ModeTTY`.
   - Add direct `StartTTYPath`.
3. Sessions command:
   - Add `--all` PTY process discovery.
4. Websocket auth:
   - Add optional access-code check.
   - Keep existing token and origin checks.
5. UI auth:
   - Prompt for token/code when required.
   - Preserve existing reconnect behavior.

Status: done

### Phase C: release + tests + CI hardening

1. Add release workflow for tagged builds and checksums.
2. Add/expand tests:
   - tunnel manager mode behavior
   - TTY attach behavior
   - websocket access-code auth
   - reconnect and flow stress cases
3. Run local test matrix and CI.

Status: done

## Risks and mitigations

1. Cloudflare mode drift:
   - Keep mode defaults stable (`ephemeral`).
   - Add strict argument/unit tests around command construction.
2. Direct TTY attach safety:
   - Explicit opt-in via `--tty-path`.
   - Keep tmux-first default.
3. Auth UX regressions:
   - Keep URL-token mode default.
   - Add browser tests for auth flow paths.
4. Release workflow drift:
   - Keep workflow minimal and deterministic.
   - Pin actions and emit checksums.
