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
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/si/remote-control/internal/auth"
	"github.com/si/remote-control/internal/config"
	"github.com/si/remote-control/internal/httpui"
	runtimeState "github.com/si/remote-control/internal/runtime"
	"github.com/si/remote-control/internal/session"
	"github.com/si/remote-control/internal/tmux"
	ws "github.com/si/remote-control/internal/websocket"
)

const usageText = `remote-control commands:
  remote-control sessions
  remote-control attach --tmux-session <name> [--port <n>] [--bind <addr>] [--readwrite]
  remote-control start --cmd "<command>" [--port <n>] [--bind <addr>] [--readwrite]
  remote-control status
  remote-control stop [--id <session-id>]
`

type launchOptions struct {
	id       string
	bind     string
	port     int
	readonly bool
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
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	tmuxSession := fs.String("tmux-session", "", "tmux session name")
	bind := fs.String("bind", settings.Server.Bind, "server bind address")
	port := fs.Int("port", settings.Server.Port, "server port")
	readwrite := fs.Bool("readwrite", !settings.Security.ReadOnlyDefault, "enable remote typing")
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
		id:       strings.TrimSpace(*sessionID),
		bind:     strings.TrimSpace(*bind),
		port:     *port,
		readonly: !*readwrite,
	}
	return runServer(term, opts)
}

func cmdStart(settings config.Settings, args []string) int {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cmdValue := fs.String("cmd", "", "command to run in a pty")
	bind := fs.String("bind", settings.Server.Bind, "server bind address")
	port := fs.Int("port", settings.Server.Port, "server port")
	readwrite := fs.Bool("readwrite", !settings.Security.ReadOnlyDefault, "enable remote typing")
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
		id:       strings.TrimSpace(*sessionID),
		bind:     strings.TrimSpace(*bind),
		port:     *port,
		readonly: !*readwrite,
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
	token, err := auth.NewToken()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Could not generate access token: %v\n", err)
		return 1
	}
	addr := fmt.Sprintf("%s:%d", opts.bind, opts.port)
	publicURL := fmt.Sprintf("http://%s/?token=%s", addr, token)
	settingsPath, _ := config.SettingsPath()

	var stateMu sync.Mutex
	state := runtimeState.SessionState{
		ID:           id,
		Mode:         string(term.Mode()),
		Source:       term.Source(),
		ReadOnly:     opts.readonly,
		PID:          os.Getpid(),
		Addr:         addr,
		URL:          fmt.Sprintf("http://%s/", addr),
		StartedAt:    time.Now().UTC(),
		ClientCount:  0,
		SettingsFile: settingsPath,
	}
	if err := runtimeState.SaveSession(state); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️ Could not persist runtime state: %v\n", err)
	}
	defer func() {
		_ = runtimeState.RemoveSession(id)
	}()

	bridge := ws.New(term, token, ws.ServerOptions{
		ReadOnly:           opts.readonly,
		MaxClients:         1,
		LowWatermarkBytes:  512 * 1024,
		HighWatermarkBytes: 2 * 1024 * 1024,
		PingInterval:       25 * time.Second,
		ClientReadTimeout:  90 * time.Second,
		OnClientCountChange: func(count int) {
			stateMu.Lock()
			state.ClientCount = count
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
	eventCh := make(chan runtimeEvent, 2)
	go func() {
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			eventCh <- runtimeEvent{source: "server", err: err}
		}
	}()
	go func() {
		eventCh <- runtimeEvent{source: "terminal", err: term.Wait()}
	}()

	fmt.Println("✅ SI remote-control is live")
	fmt.Printf("🆔 Session ID: %s\n", id)
	fmt.Printf("📡 URL: %s\n", publicURL)
	if opts.readonly {
		fmt.Println("🔒 Mode: read-only")
	} else {
		fmt.Println("✍️  Mode: read-write")
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
		if processAlive(st.PID) {
			status = "running"
		}
		fmt.Printf("- %s [%s] mode=%s readonly=%t clients=%d addr=%s started=%s\n",
			st.ID, status, st.Mode, st.ReadOnly, st.ClientCount, st.Addr, st.StartedAt.Local().Format(time.RFC3339))
	}
	return 0
}

func cmdStop(args []string) int {
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
	if !processAlive(target.PID) {
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
	fmt.Printf("✅ Stop signal sent to %s (pid %d)\n", target.ID, target.PID)
	return 0
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	return true
}

func RepoRoot() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Dir(exe), nil
}
