package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/si/remote-control/internal/auth"
	"github.com/si/remote-control/internal/config"
	"github.com/si/remote-control/internal/httpui"
	"github.com/si/remote-control/internal/power/macos"
	runtimeState "github.com/si/remote-control/internal/runtime"
	"github.com/si/remote-control/internal/session"
	"github.com/si/remote-control/internal/tmux"
	"github.com/si/remote-control/internal/tunnel/cloudflare"
	ws "github.com/si/remote-control/internal/websocket"
)

const usageText = `remote-control commands:
  remote-control sessions
  remote-control attach --tmux-session <name> [--port <n>] [--bind <addr>] [--readwrite] [--tunnel|--no-tunnel]
  remote-control start --cmd "<command>" [--port <n>] [--bind <addr>] [--readwrite] [--tunnel|--no-tunnel]
  remote-control status
  remote-control stop [--id <session-id>]
`

type launchOptions struct {
	id                string
	bind              string
	port              int
	readonly          bool
	maxClients        int
	flowLowBytes      int64
	flowHighBytes     int64
	flowAckBytes      int64
	tokenTTL          time.Duration
	idleTimeout       time.Duration
	enableTunnel      bool
	tunnelRequired    bool
	cloudflaredBinary string
	cloudflareTimeout time.Duration
	enableCaffeinate  bool
}

type managedProcess interface {
	PID() int
	Stop() error
}

func Run(args []string) int {
	if len(args) == 0 {
		fmt.Print(usageText)
		return 0
	}
	settings, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	sub := strings.ToLower(strings.TrimSpace(args[0]))
	rest := args[1:]
	switch sub {
	case "sessions":
		return cmdSessions()
	case "attach":
		return cmdAttach(settings, rest)
	case "start", "run":
		return cmdStart(settings, rest)
	case "status":
		return cmdStatus()
	case "stop":
		return cmdStop(rest)
	case "help", "-h", "--help":
		fmt.Print(usageText)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "error: unknown command %q\n", sub)
		fmt.Print(usageText)
		return 1
	}
}

func cmdSessions() int {
	sessions, err := tmux.ListSessions()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Could not list tmux sessions: %v\n", err)
		return 1
	}
	if len(sessions) == 0 {
		fmt.Println("ℹ️ No tmux sessions found.")
		return 0
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].Name < sessions[j].Name })
	fmt.Println("🧭 Available tmux sessions")
	for _, s := range sessions {
		fmt.Printf("- %s (windows=%d, attached=%d, created=%s)\n", s.Name, s.Windows, s.Attached, s.Created)
	}
	return 0
}

func cmdAttach(settings config.Settings, args []string) int {
	pruneStaleRuntimeState()
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	tmuxSession := fs.String("tmux-session", "", "tmux session name")
	bind := fs.String("bind", settings.Server.Bind, "server bind address")
	port := fs.Int("port", settings.Server.Port, "server port")
	readwrite := fs.Bool("readwrite", !settings.Security.ReadOnlyDefault, "enable remote typing")
	tunnel := fs.Bool("tunnel", settings.Tunnel.Enabled, "start public tunnel")
	noTunnel := fs.Bool("no-tunnel", false, "disable public tunnel")
	tunnelRequired := fs.Bool("tunnel-required", settings.Tunnel.Required, "fail if tunnel cannot start")
	cloudflaredBin := fs.String("cloudflared-bin", settings.Tunnel.Cloudflare.Binary, "cloudflared binary path")
	caffeinate := fs.Bool("caffeinate", settings.MacOS.Caffeinate, "prevent macOS sleep while active")
	noCaffeinate := fs.Bool("no-caffeinate", false, "disable caffeinate even if enabled in settings")
	sessionID := fs.String("id", "", "runtime session id")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	name := strings.TrimSpace(*tmuxSession)
	if name == "" {
		list, err := tmux.ListSessions()
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Could not discover tmux sessions: %v\n", err)
			return 1
		}
		if len(list) == 0 {
			fmt.Fprintln(os.Stderr, "❌ No tmux sessions found. Start one with: tmux new -s my-session")
			return 1
		}
		name = list[0].Name
		fmt.Printf("ℹ️ Using tmux session: %s\n", name)
	}
	term, err := session.StartAttach(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Could not attach tmux session %q: %v\n", name, err)
		return 1
	}
	defer term.Close()
	opts := launchOptions{
		id:                strings.TrimSpace(*sessionID),
		bind:              strings.TrimSpace(*bind),
		port:              *port,
		readonly:          !*readwrite,
		maxClients:        settings.Session.MaxClients,
		flowLowBytes:      settings.Flow.LowWatermarkBytes,
		flowHighBytes:     settings.Flow.HighWatermarkBytes,
		flowAckBytes:      settings.Flow.AckQuantumBytes,
		tokenTTL:          time.Duration(settings.Session.TokenTTLSeconds) * time.Second,
		idleTimeout:       time.Duration(settings.Session.IdleTimeoutSeconds) * time.Second,
		enableTunnel:      *tunnel && !*noTunnel,
		tunnelRequired:    *tunnelRequired,
		cloudflaredBinary: strings.TrimSpace(*cloudflaredBin),
		cloudflareTimeout: time.Duration(settings.Tunnel.Cloudflare.StartupTimeoutSeconds) * time.Second,
		enableCaffeinate:  *caffeinate && !*noCaffeinate,
	}
	return runServer(term, opts)
}

func cmdStart(settings config.Settings, args []string) int {
	pruneStaleRuntimeState()
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cmdValue := fs.String("cmd", "", "command to run in a pty")
	bind := fs.String("bind", settings.Server.Bind, "server bind address")
	port := fs.Int("port", settings.Server.Port, "server port")
	readwrite := fs.Bool("readwrite", !settings.Security.ReadOnlyDefault, "enable remote typing")
	tunnel := fs.Bool("tunnel", settings.Tunnel.Enabled, "start public tunnel")
	noTunnel := fs.Bool("no-tunnel", false, "disable public tunnel")
	tunnelRequired := fs.Bool("tunnel-required", settings.Tunnel.Required, "fail if tunnel cannot start")
	cloudflaredBin := fs.String("cloudflared-bin", settings.Tunnel.Cloudflare.Binary, "cloudflared binary path")
	caffeinate := fs.Bool("caffeinate", settings.MacOS.Caffeinate, "prevent macOS sleep while active")
	noCaffeinate := fs.Bool("no-caffeinate", false, "disable caffeinate even if enabled in settings")
	sessionID := fs.String("id", "", "runtime session id")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	command := strings.TrimSpace(*cmdValue)
	if command == "" {
		fmt.Fprintln(os.Stderr, "❌ --cmd is required")
		return 1
	}
	term, err := session.StartCommand(command)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Could not start command session: %v\n", err)
		return 1
	}
	defer term.Close()
	opts := launchOptions{
		id:                strings.TrimSpace(*sessionID),
		bind:              strings.TrimSpace(*bind),
		port:              *port,
		readonly:          !*readwrite,
		maxClients:        settings.Session.MaxClients,
		flowLowBytes:      settings.Flow.LowWatermarkBytes,
		flowHighBytes:     settings.Flow.HighWatermarkBytes,
		flowAckBytes:      settings.Flow.AckQuantumBytes,
		tokenTTL:          time.Duration(settings.Session.TokenTTLSeconds) * time.Second,
		idleTimeout:       time.Duration(settings.Session.IdleTimeoutSeconds) * time.Second,
		enableTunnel:      *tunnel && !*noTunnel,
		tunnelRequired:    *tunnelRequired,
		cloudflaredBinary: strings.TrimSpace(*cloudflaredBin),
		cloudflareTimeout: time.Duration(settings.Tunnel.Cloudflare.StartupTimeoutSeconds) * time.Second,
		enableCaffeinate:  *caffeinate && !*noCaffeinate,
	}
	return runServer(term, opts)
}

func runServer(term *session.Terminal, opts launchOptions) int {
	if term == nil {
		fmt.Fprintln(os.Stderr, "❌ session is nil")
		return 1
	}
	if opts.port <= 0 || opts.port > 65535 {
		fmt.Fprintf(os.Stderr, "❌ Invalid --port value %d (expected 1-65535)\n", opts.port)
		return 1
	}
	if strings.TrimSpace(opts.bind) == "" {
		opts.bind = "127.0.0.1"
	}
	id := strings.TrimSpace(opts.id)
	if id == "" {
		id = fmt.Sprintf("rc-%d", time.Now().Unix())
	}
	issuedToken, err := auth.NewTokenWithTTL(opts.tokenTTL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Could not generate access token: %v\n", err)
		return 1
	}
	token := issuedToken.Value
	addr := fmt.Sprintf("%s:%d", opts.bind, opts.port)
	localURL := fmt.Sprintf("http://%s/", addr)
	shareURL := appendToken(localURL, token)
	settingsPath, _ := config.SettingsPath()
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()

	var stateMu sync.Mutex
	state := runtimeState.SessionState{
		ID:             id,
		Mode:           string(term.Mode()),
		Source:         term.Source(),
		ReadOnly:       opts.readonly,
		PID:            os.Getpid(),
		Addr:           addr,
		URL:            localURL,
		LocalURL:       localURL,
		PublicURL:      "",
		Tunnel:         "local",
		StartedAt:      time.Now().UTC(),
		TokenExpiresAt: issuedToken.ExpiresAt,
		IdleTimeoutSec: int(opts.idleTimeout / time.Second),
		ClientCount:    0,
		SettingsFile:   settingsPath,
		CloudflaredPID: 0,
		CaffeinatePID:  0,
	}
	if opts.idleTimeout > 0 {
		state.IdleDeadline = state.StartedAt.Add(opts.idleTimeout)
	}
	if err := runtimeState.SaveSession(state); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️ Could not persist runtime state: %v\n", err)
	}
	defer func() {
		_ = runtimeState.RemoveSession(id)
	}()

	bridge := ws.New(term, token, ws.ServerOptions{
		ReadOnly:           opts.readonly,
		MaxClients:         opts.maxClients,
		LowWatermarkBytes:  opts.flowLowBytes,
		HighWatermarkBytes: opts.flowHighBytes,
		AckQuantumBytes:    opts.flowAckBytes,
		TokenExpiresAt:     issuedToken.ExpiresAt,
		PingInterval:       25 * time.Second,
		ClientReadTimeout:  90 * time.Second,
		OnClientCountChange: func(count int) {
			stateMu.Lock()
			state.ClientCount = count
			if opts.idleTimeout > 0 {
				if count == 0 {
					state.IdleDeadline = time.Now().UTC().Add(opts.idleTimeout)
				} else {
					state.IdleDeadline = time.Time{}
				}
			}
			_ = runtimeState.SaveSession(state)
			stateMu.Unlock()
		},
	})
	bridge.Start()
	defer bridge.Close()

	uiBytes, err := fs.ReadFile(httpui.Files, "static/index.html")
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Could not load web UI: %v\n", err)
		return 1
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(uiBytes)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": id})
	})
	mux.HandleFunc("/ws", bridge.HandleWS)

	srv := &http.Server{Addr: addr, Handler: mux}
	type runtimeEvent struct {
		source string
		err    error
	}
	eventCh := make(chan runtimeEvent, 3)
	go func() {
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			eventCh <- runtimeEvent{source: "server", err: err}
		}
	}()
	go func() {
		eventCh <- runtimeEvent{source: "terminal", err: term.Wait()}
	}()
	if opts.idleTimeout > 0 {
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-runCtx.Done():
					return
				case <-ticker.C:
					stateMu.Lock()
					clientCount := state.ClientCount
					idleDeadline := state.IdleDeadline
					stateMu.Unlock()
					if clientCount > 0 || idleDeadline.IsZero() {
						continue
					}
					if !time.Now().UTC().Before(idleDeadline) {
						select {
						case eventCh <- runtimeEvent{source: "idle"}:
						default:
						}
						return
					}
				}
			}
		}()
	}

	var managed []managedProcess
	defer func() {
		for i := len(managed) - 1; i >= 0; i-- {
			_ = managed[i].Stop()
		}
	}()

	if opts.enableTunnel {
		if err := waitForLocalHealth(runCtx, strings.TrimRight(localURL, "/")+"/healthz", 5*time.Second); err != nil {
			if opts.tunnelRequired {
				fmt.Fprintf(os.Stderr, "❌ Local server did not become ready for tunnel startup: %v\n", err)
				return 1
			}
			fmt.Fprintf(os.Stderr, "⚠️ Tunnel skipped because local server readiness check failed: %v\n", err)
		} else {
			tunnelHandle, err := cloudflare.Start(runCtx, cloudflare.Options{
				Binary:         opts.cloudflaredBinary,
				LocalURL:       strings.TrimRight(localURL, "/"),
				StartupTimeout: opts.cloudflareTimeout,
			})
			if err != nil {
				if opts.tunnelRequired {
					fmt.Fprintf(os.Stderr, "❌ Tunnel startup failed: %v\n", err)
					return 1
				}
				fmt.Fprintf(os.Stderr, "⚠️ Tunnel unavailable; continuing in local mode: %v\n", err)
			} else {
				managed = append(managed, tunnelHandle)
				publicBase := strings.TrimSpace(tunnelHandle.PublicURL())
				shareURL = appendToken(publicBase, token)
				stateMu.Lock()
				state.Tunnel = "cloudflare"
				state.PublicURL = publicBase
				state.URL = publicBase
				state.CloudflaredPID = tunnelHandle.PID()
				_ = runtimeState.SaveSession(state)
				stateMu.Unlock()
			}
		}
	}

	if opts.enableCaffeinate {
		caffeinateHandle, err := macos.Start(runCtx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠️ Could not start caffeinate: %v\n", err)
		} else if caffeinateHandle != nil {
			managed = append(managed, caffeinateHandle)
			stateMu.Lock()
			state.CaffeinatePID = caffeinateHandle.PID()
			_ = runtimeState.SaveSession(state)
			stateMu.Unlock()
		}
	}

	fmt.Println("✅ SI remote-control is live")
	fmt.Printf("🆔 Session ID: %s\n", id)
	fmt.Printf("🌐 Share URL: %s\n", shareURL)
	fmt.Printf("🏠 Local URL: %s\n", localURL)
	fmt.Printf("⏳ Token expires: %s\n", issuedToken.ExpiresAt.Local().Format(time.RFC3339))
	if strings.TrimSpace(state.PublicURL) != "" {
		fmt.Printf("☁️  Tunnel URL: %s\n", state.PublicURL)
	}
	if opts.readonly {
		fmt.Println("🔒 Mode: read-only")
	} else {
		fmt.Println("✍️  Mode: read-write")
	}
	if opts.idleTimeout > 0 {
		fmt.Printf("🕒 Idle timeout: %s\n", opts.idleTimeout.String())
	}
	fmt.Println("📱 Open the URL in Chrome or Safari.")
	fmt.Println("🛑 Press Ctrl+C to stop sharing.")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	exitCode := 0
	select {
	case <-sigCh:
	case event := <-eventCh:
		switch event.source {
		case "terminal":
			if event.err != nil {
				fmt.Fprintf(os.Stderr, "❌ Terminal process exited with error: %v\n", event.err)
				exitCode = 1
			} else {
				fmt.Println("ℹ️ Terminal process exited.")
			}
		default:
			if event.source == "idle" {
				fmt.Println("⏱️ Idle timeout reached. Session stopped.")
				break
			}
			if event.err != nil {
				fmt.Fprintf(os.Stderr, "❌ Server error: %v\n", event.err)
				exitCode = 1
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	return exitCode
}

func cmdStatus() int {
	pruneStaleRuntimeState()
	states, err := runtimeState.ListSessions()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Could not read runtime state: %v\n", err)
		return 1
	}
	if len(states) == 0 {
		fmt.Println("ℹ️ No active remote-control sessions found.")
		return 0
	}
	fmt.Println("📋 remote-control sessions")
	for _, st := range states {
		status := "stopped"
		if runtimeState.ProcessAlive(st.PID) {
			status = "running"
		}
		local := strings.TrimSpace(st.LocalURL)
		if local == "" {
			local = strings.TrimSpace(st.URL)
		}
		public := strings.TrimSpace(st.PublicURL)
		if public == "" {
			public = "-"
		}
		tokenExpiry := "-"
		if !st.TokenExpiresAt.IsZero() {
			tokenExpiry = st.TokenExpiresAt.Local().Format(time.RFC3339)
		}
		idleDeadline := "-"
		if !st.IdleDeadline.IsZero() {
			idleDeadline = st.IdleDeadline.Local().Format(time.RFC3339)
		}
		fmt.Printf("- %s [%s] mode=%s readonly=%t clients=%d local=%s public=%s started=%s token_expires=%s idle_deadline=%s pids(parent=%d cf=%d caf=%d)\n",
			st.ID, status, st.Mode, st.ReadOnly, st.ClientCount, local, public, st.StartedAt.Local().Format(time.RFC3339), tokenExpiry, idleDeadline, st.PID, st.CloudflaredPID, st.CaffeinatePID)
	}
	return 0
}

func cmdStop(args []string) int {
	pruneStaleRuntimeState()
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	id := fs.String("id", "", "session id to stop")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	states, err := runtimeState.ListSessions()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Could not read runtime state: %v\n", err)
		return 1
	}
	if len(states) == 0 {
		fmt.Println("ℹ️ No active sessions to stop.")
		return 0
	}
	targetID := strings.TrimSpace(*id)
	if targetID == "" {
		if len(states) == 1 {
			targetID = states[0].ID
		} else {
			fmt.Fprintln(os.Stderr, "❌ Multiple sessions found. Use --id <session-id>.")
			return 1
		}
	}
	var target *runtimeState.SessionState
	for i := range states {
		if states[i].ID == targetID {
			target = &states[i]
			break
		}
	}
	if target == nil {
		fmt.Fprintf(os.Stderr, "❌ Session %q not found\n", targetID)
		return 1
	}
	if !runtimeState.ProcessAlive(target.PID) {
		_ = runtimeState.RemoveSession(target.ID)
		fmt.Printf("ℹ️ Session %s already stopped; cleaned stale state.\n", target.ID)
		return 0
	}
	proc, err := os.FindProcess(target.PID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Could not resolve process %d: %v\n", target.PID, err)
		return 1
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Could not stop session %s: %v\n", target.ID, err)
		return 1
	}
	if target.CloudflaredPID > 0 && target.CloudflaredPID != target.PID {
		_ = terminatePID(target.CloudflaredPID, syscall.SIGTERM)
	}
	if target.CaffeinatePID > 0 && target.CaffeinatePID != target.PID {
		_ = terminatePID(target.CaffeinatePID, syscall.SIGTERM)
	}
	fmt.Printf("✅ Stop signal sent to %s (pid %d)\n", target.ID, target.PID)
	return 0
}

func processAlive(pid int) bool {
	return runtimeState.ProcessAlive(pid)
}

func RepoRoot() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Dir(exe), nil
}

func waitForLocalHealth(ctx context.Context, healthURL string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err == nil {
			resp, reqErr := client.Do(req)
			if reqErr == nil {
				_ = resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 500 {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s", healthURL)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
}

func appendToken(baseURL, token string) string {
	base := strings.TrimSpace(baseURL)
	if base == "" {
		return ""
	}
	base = strings.TrimRight(base, "/")
	return base + "/?token=" + token
}

func terminatePID(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(sig)
}

func pruneStaleRuntimeState() {
	removed, err := runtimeState.PruneStaleSessions()
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠️ Could not prune stale session state: %v\n", err)
		return
	}
	if len(removed) > 0 {
		fmt.Printf("🧹 Cleaned stale session state: %s\n", strings.Join(removed, ", "))
	}
}
