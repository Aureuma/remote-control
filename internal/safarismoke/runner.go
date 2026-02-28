package safarismoke

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Aureuma/remote-control/internal/config"
)

const (
	defaultDriverStartupTimeout = 12 * time.Second
	defaultScenarioTimeout      = 40 * time.Second
	defaultStopTimeout          = 8 * time.Second
)

type cliConfig struct {
	remoteControlBin string
	safaridriverBin  string
	safaridriverPort int
	bind             string
	scenarioCSV      string
	scenarios        []scenarioDef
	driverStartup    time.Duration
	scenarioTimeout  time.Duration
	keepArtifacts    bool
	verbose          bool
	skipBuild        bool
	sshHost          string
	sshPort          int
	sshUser          string
	noSSH            bool
}

type scenarioDef struct {
	name          string
	readwrite     bool
	tokenInURL    bool
	accessCode    string
	expectedState string
	prompts       map[string]string
}

type runner struct {
	cfg    cliConfig
	stdout io.Writer
	stderr io.Writer
}

func RunCLI(args []string) int {
	cfg, err := parseCLI(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	r := &runner{cfg: cfg, stdout: os.Stdout, stderr: os.Stderr}
	if err := r.run(); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Safari smoke failed: %v\n", err)
		if runtime.GOOS == "darwin" {
			fmt.Fprintln(os.Stderr, "ℹ️ Make sure Safari Develop -> Allow Remote Automation is enabled, then run 'safaridriver --enable' once.")
		}
		return 1
	}
	fmt.Fprintln(os.Stdout, "✅ Safari smoke finished successfully.")
	return 0
}

func parseCLI(args []string) (cliConfig, error) {
	cfg := cliConfig{}
	settings := config.Settings{}
	if loaded, err := config.Load(); err == nil {
		settings = loaded
	} else {
		settings = config.Settings{}
	}
	defaultSSHHost := config.ResolveSettingValue(settings.Development.Safari.SSH.Host, settings.Development.Safari.SSH.HostEnvKey)
	defaultSSHUser := config.ResolveSettingValue(settings.Development.Safari.SSH.User, settings.Development.Safari.SSH.UserEnvKey)
	defaultSSHPort := 0
	if rawPort := config.ResolveSettingValue(settings.Development.Safari.SSH.Port, settings.Development.Safari.SSH.PortEnvKey); rawPort != "" {
		if parsed, err := strconv.Atoi(rawPort); err == nil && parsed > 0 {
			defaultSSHPort = parsed
		}
	}

	fs := flag.NewFlagSet("rc-safari-smoke", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	remoteControlBin := fs.String("remote-control-bin", "", "path to remote-control binary; defaults to building ./cmd/remote-control")
	safaridriverBin := fs.String("safaridriver-bin", "safaridriver", "path to safaridriver binary")
	safaridriverPort := fs.Int("safaridriver-port", 0, "safaridriver port (0 = auto)")
	bind := fs.String("bind", "127.0.0.1", "remote-control bind host")
	scenarioCSV := fs.String("scenarios", "readwrite,readonly,access-code,no-token", "comma-separated scenarios: readwrite,readonly,access-code,no-token")
	driverStartup := fs.Duration("driver-timeout", defaultDriverStartupTimeout, "safaridriver startup timeout")
	scenarioTimeout := fs.Duration("scenario-timeout", defaultScenarioTimeout, "per-scenario timeout")
	keepArtifacts := fs.Bool("keep-artifacts", false, "keep temporary home/log files")
	verbose := fs.Bool("verbose", false, "enable verbose logging")
	skipBuild := fs.Bool("skip-build", false, "skip automatic remote-control build when --remote-control-bin is empty")
	sshHost := fs.String("ssh-host", defaultSSHHost, "remote macOS host for SSH run when current OS is not macOS")
	sshPort := fs.Int("ssh-port", defaultSSHPort, "remote SSH port when current OS is not macOS")
	sshUser := fs.String("ssh-user", defaultSSHUser, "remote SSH user when current OS is not macOS")
	noSSH := fs.Bool("no-ssh", false, "disable automatic SSH fallback on non-macOS hosts")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stdout, "Usage: go run ./cmd/rc-safari-smoke [flags]")
			fs.SetOutput(os.Stdout)
			fs.PrintDefaults()
			return cfg, flag.ErrHelp
		}
		return cfg, err
	}

	scenarios, err := resolveScenarios(*scenarioCSV)
	if err != nil {
		return cfg, err
	}

	cfg = cliConfig{
		remoteControlBin: *remoteControlBin,
		safaridriverBin:  strings.TrimSpace(*safaridriverBin),
		safaridriverPort: *safaridriverPort,
		bind:             strings.TrimSpace(*bind),
		scenarioCSV:      strings.TrimSpace(*scenarioCSV),
		scenarios:        scenarios,
		driverStartup:    *driverStartup,
		scenarioTimeout:  *scenarioTimeout,
		keepArtifacts:    *keepArtifacts,
		verbose:          *verbose,
		skipBuild:        *skipBuild,
		sshHost:          strings.TrimSpace(*sshHost),
		sshPort:          *sshPort,
		sshUser:          strings.TrimSpace(*sshUser),
		noSSH:            *noSSH,
	}
	if cfg.bind == "" {
		cfg.bind = "127.0.0.1"
	}
	if cfg.safaridriverBin == "" {
		cfg.safaridriverBin = "safaridriver"
	}
	if cfg.driverStartup <= 0 {
		return cfg, fmt.Errorf("--driver-timeout must be positive")
	}
	if cfg.scenarioTimeout <= 0 {
		return cfg, fmt.Errorf("--scenario-timeout must be positive")
	}
	if cfg.sshPort < 0 || cfg.sshPort > 65535 {
		return cfg, fmt.Errorf("--ssh-port must be between 0 and 65535")
	}
	return cfg, nil
}

func resolveScenarios(csv string) ([]scenarioDef, error) {
	parts := strings.Split(csv, ",")
	defs := make([]scenarioDef, 0, len(parts))
	seen := map[string]struct{}{}
	for _, raw := range parts {
		name := strings.TrimSpace(strings.ToLower(raw))
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		def, ok := scenarioByName(name)
		if !ok {
			return nil, fmt.Errorf("unknown scenario %q (allowed: readwrite, readonly, access-code, no-token)", name)
		}
		seen[name] = struct{}{}
		defs = append(defs, def)
	}
	if len(defs) == 0 {
		return nil, fmt.Errorf("no scenarios selected")
	}
	return defs, nil
}

func scenarioByName(name string) (scenarioDef, bool) {
	switch name {
	case "readwrite":
		return scenarioDef{
			name:          "readwrite",
			readwrite:     true,
			tokenInURL:    true,
			expectedState: "Live session",
		}, true
	case "readonly":
		return scenarioDef{
			name:          "readonly",
			readwrite:     false,
			tokenInURL:    true,
			expectedState: "Read-only",
		}, true
	case "access-code":
		return scenarioDef{
			name:          "access-code",
			readwrite:     true,
			tokenInURL:    true,
			accessCode:    "2468",
			expectedState: "Live session",
			prompts: map[string]string{
				"access code": "2468",
			},
		}, true
	case "no-token":
		return scenarioDef{
			name:          "no-token",
			readwrite:     true,
			tokenInURL:    false,
			expectedState: "Live session",
			prompts: map[string]string{
				"access token": "<dynamic-token>",
			},
		}, true
	default:
		return scenarioDef{}, false
	}
}

func (r *runner) run() error {
	if runtime.GOOS != "darwin" {
		if r.cfg.noSSH {
			return fmt.Errorf("real Safari smoke runs only on macOS (current: %s)", runtime.GOOS)
		}
		if strings.TrimSpace(r.cfg.sshHost) == "" || strings.TrimSpace(r.cfg.sshUser) == "" || r.cfg.sshPort <= 0 {
			return fmt.Errorf("real Safari smoke requires macOS; configure SSH via settings or flags (--ssh-host, --ssh-port, --ssh-user)")
		}
		return r.runRemoteOverSSH()
	}

	if _, err := exec.LookPath(r.cfg.safaridriverBin); err != nil {
		return fmt.Errorf("safaridriver not found (%s): %w", r.cfg.safaridriverBin, err)
	}

	rcBin, cleanupBin, err := r.prepareRemoteControlBinary()
	if err != nil {
		return err
	}
	defer cleanupBin()

	driverPort := r.cfg.safaridriverPort
	if driverPort == 0 {
		driverPort, err = pickFreePort(r.cfg.bind)
		if err != nil {
			return fmt.Errorf("allocate safaridriver port: %w", err)
		}
	}

	driver, err := startDriver(r.cfg.safaridriverBin, driverPort, r.cfg.verbose, r.stdout, r.stderr)
	if err != nil {
		return err
	}
	defer driver.stop()

	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.driverStartup)
	defer cancel()
	if err := waitForDriverReady(ctx, driver.baseURL()); err != nil {
		return fmt.Errorf("wait for safaridriver: %w", err)
	}

	wd := &webDriverClient{
		baseURL: strings.TrimRight(driver.baseURL(), "/"),
		http: &http.Client{
			Timeout: 8 * time.Second,
		},
	}

	for _, sc := range r.cfg.scenarios {
		fmt.Fprintf(r.stdout, "\n🧪 Scenario: %s\n", sc.name)
		scCtx, scCancel := context.WithTimeout(context.Background(), r.cfg.scenarioTimeout)
		err := r.runScenario(scCtx, wd, rcBin, sc)
		scCancel()
		if err != nil {
			return fmt.Errorf("scenario %s: %w", sc.name, err)
		}
		fmt.Fprintf(r.stdout, "✅ Scenario passed: %s\n", sc.name)
	}

	return nil
}

func (r *runner) runRemoteOverSSH() error {
	if _, err := exec.LookPath("ssh"); err != nil {
		return fmt.Errorf("ssh binary not found: %w", err)
	}
	if _, err := exec.LookPath("scp"); err != nil {
		return fmt.Errorf("scp binary not found: %w", err)
	}

	addr := fmt.Sprintf("%s@%s", r.cfg.sshUser, r.cfg.sshHost)
	arch, err := r.remoteDarwinArch(addr)
	if err != nil {
		return err
	}
	remoteHome, err := r.runSSHCommandOutput(addr, "printf %s \"$HOME\"")
	if err != nil {
		return fmt.Errorf("resolve remote home: %w", err)
	}
	remoteHome = strings.TrimSpace(remoteHome)
	if remoteHome == "" {
		return fmt.Errorf("resolve remote home: empty HOME response")
	}

	stageDir, err := os.MkdirTemp("", "rc-safari-ssh-*")
	if err != nil {
		return fmt.Errorf("create local stage dir: %w", err)
	}
	defer func() {
		if r.cfg.keepArtifacts {
			fmt.Fprintf(r.stdout, "ℹ️ Keeping local stage dir: %s\n", stageDir)
			return
		}
		_ = os.RemoveAll(stageDir)
	}()

	rcBin := filepath.Join(stageDir, "remote-control")
	safariBin := filepath.Join(stageDir, "rc-safari-smoke")
	if err := r.buildCrossBinary("./cmd/remote-control", rcBin, arch); err != nil {
		return err
	}
	if err := r.buildCrossBinary("./cmd/rc-safari-smoke", safariBin, arch); err != nil {
		return err
	}

	remoteDir := filepath.ToSlash(filepath.Join(remoteHome, "Development", "remote-control"))
	if err := r.runSSHCommand(addr, fmt.Sprintf("mkdir -p %s", shellQuote(remoteDir))); err != nil {
		return fmt.Errorf("prepare remote directory: %w", err)
	}
	if err := r.runSCP(rcBin, fmt.Sprintf("%s:%s/remote-control", addr, remoteDir)); err != nil {
		return fmt.Errorf("copy remote-control binary: %w", err)
	}
	if err := r.runSCP(safariBin, fmt.Sprintf("%s:%s/rc-safari-smoke", addr, remoteDir)); err != nil {
		return fmt.Errorf("copy rc-safari-smoke binary: %w", err)
	}

	remoteArgs := []string{
		"./rc-safari-smoke",
		"--remote-control-bin", "./remote-control",
		"--scenarios", r.cfg.scenarioCSV,
		"--driver-timeout", r.cfg.driverStartup.String(),
		"--scenario-timeout", r.cfg.scenarioTimeout.String(),
		"--bind", r.cfg.bind,
		"--no-ssh",
	}
	if r.cfg.keepArtifacts {
		remoteArgs = append(remoteArgs, "--keep-artifacts")
	}
	if r.cfg.verbose {
		remoteArgs = append(remoteArgs, "--verbose")
	}
	if strings.TrimSpace(r.cfg.safaridriverBin) != "" {
		remoteArgs = append(remoteArgs, "--safaridriver-bin", r.cfg.safaridriverBin)
	}
	if r.cfg.safaridriverPort > 0 {
		remoteArgs = append(remoteArgs, "--safaridriver-port", strconv.Itoa(r.cfg.safaridriverPort))
	}

	commandParts := []string{
		"set -e",
		fmt.Sprintf("cd %s", shellQuote(remoteDir)),
		"chmod +x ./remote-control ./rc-safari-smoke",
		shellJoin(remoteArgs),
	}
	command := strings.Join(commandParts, " && ")
	fmt.Fprintf(r.stdout, "ℹ️ Running Safari smoke on %s:%d (%s)\n", r.cfg.sshHost, r.cfg.sshPort, arch)
	if err := r.runSSHCommand(addr, command); err != nil {
		return fmt.Errorf("remote safari smoke over ssh failed: %w", err)
	}
	return nil
}

func (r *runner) remoteDarwinArch(addr string) (string, error) {
	output, err := r.runSSHCommandOutput(addr, "uname -m")
	if err != nil {
		return "", fmt.Errorf("probe remote architecture: %w", err)
	}
	raw := strings.TrimSpace(output)
	switch raw {
	case "x86_64", "amd64":
		return "amd64", nil
	case "arm64", "aarch64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported remote architecture %q", raw)
	}
}

func (r *runner) buildCrossBinary(pkg, out, arch string) error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOOS=darwin", "GOARCH="+arch, "CGO_ENABLED=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cross-build %s (%s): %w\n%s", pkg, arch, err, string(output))
	}
	return nil
}

func (r *runner) runSSHCommand(addr, remoteCommand string) error {
	cmd := exec.Command("ssh",
		"-p", strconv.Itoa(r.cfg.sshPort),
		"-o", "BatchMode=yes",
		addr,
		remoteCommand,
	)
	cmd.Stdout = r.stdout
	cmd.Stderr = r.stderr
	return cmd.Run()
}

func (r *runner) runSSHCommandOutput(addr, remoteCommand string) (string, error) {
	cmd := exec.Command("ssh",
		"-p", strconv.Itoa(r.cfg.sshPort),
		"-o", "BatchMode=yes",
		addr,
		remoteCommand,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ssh command failed: %w\n%s", err, string(output))
	}
	return string(output), nil
}

func (r *runner) runSCP(localPath, remotePath string) error {
	cmd := exec.Command("scp",
		"-P", strconv.Itoa(r.cfg.sshPort),
		localPath,
		remotePath,
	)
	cmd.Stdout = r.stdout
	cmd.Stderr = r.stderr
	return cmd.Run()
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func shellJoin(args []string) string {
	if len(args) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func (r *runner) prepareRemoteControlBinary() (string, func(), error) {
	if strings.TrimSpace(r.cfg.remoteControlBin) != "" {
		path, err := exec.LookPath(r.cfg.remoteControlBin)
		if err != nil {
			return "", nil, fmt.Errorf("remote-control binary not found (%s): %w", r.cfg.remoteControlBin, err)
		}
		return path, func() {}, nil
	}
	if r.cfg.skipBuild {
		return "", nil, fmt.Errorf("--skip-build requires --remote-control-bin")
	}

	root, err := repoRoot()
	if err != nil {
		return "", nil, err
	}
	outFile, err := os.CreateTemp("", "remote-control-smoke-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temp binary path: %w", err)
	}
	out := outFile.Name()
	if err := outFile.Close(); err != nil {
		_ = os.Remove(out)
		return "", nil, fmt.Errorf("close temp binary placeholder: %w", err)
	}

	build := exec.Command("go", "build", "-o", out, "./cmd/remote-control")
	build.Dir = root
	logOut, err := build.CombinedOutput()
	if err != nil {
		_ = os.Remove(out)
		return "", nil, fmt.Errorf("go build ./cmd/remote-control failed: %w\n%s", err, string(logOut))
	}

	cleanup := func() {
		if r.cfg.keepArtifacts {
			fmt.Fprintf(r.stdout, "ℹ️ Keeping built binary: %s\n", out)
			return
		}
		_ = os.Remove(out)
	}
	return out, cleanup, nil
}

func repoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("failed to resolve source location")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..")), nil
}

func (r *runner) runScenario(ctx context.Context, wd *webDriverClient, rcBin string, sc scenarioDef) error {
	runtimeHome, err := os.MkdirTemp("", "rc-safari-home-*")
	if err != nil {
		return fmt.Errorf("create temp home: %w", err)
	}
	cleanupHome := func() {
		if r.cfg.keepArtifacts {
			fmt.Fprintf(r.stdout, "ℹ️ Keeping scenario home (%s): %s\n", sc.name, runtimeHome)
			return
		}
		_ = os.RemoveAll(runtimeHome)
	}
	defer cleanupHome()

	port, err := pickFreePort(r.cfg.bind)
	if err != nil {
		return fmt.Errorf("allocate remote-control port: %w", err)
	}

	rcSession, err := startRemoteControlSession(ctx, startSessionConfig{
		binary:     rcBin,
		home:       runtimeHome,
		id:         sessionID(sc.name),
		bind:       r.cfg.bind,
		port:       port,
		readwrite:  sc.readwrite,
		tokenInURL: sc.tokenInURL,
		accessCode: sc.accessCode,
		verbose:    r.cfg.verbose,
		stdout:     r.stdout,
		stderr:     r.stderr,
	})
	if err != nil {
		return err
	}
	defer func() {
		_ = stopRemoteControlSession(rcSession, defaultStopTimeout)
	}()

	sid, err := wd.newSession(ctx)
	if err != nil {
		return fmt.Errorf("webdriver new session: %w", err)
	}
	defer func() {
		_ = wd.deleteSession(context.Background(), sid)
	}()

	if err := wd.navigate(ctx, sid, rcSession.shareURL); err != nil {
		return fmt.Errorf("webdriver navigate: %w", err)
	}

	prompts := map[string]string{}
	for k, v := range sc.prompts {
		prompts[strings.ToLower(strings.TrimSpace(k))] = v
	}
	if val, ok := prompts["access token"]; ok && val == "<dynamic-token>" {
		prompts["access token"] = rcSession.currentAccessToken()
	}

	if err := waitForExpectedStatus(ctx, wd, sid, sc.expectedState, prompts); err != nil {
		return fmt.Errorf("waiting for browser state %q: %w\nremote-control logs:\n%s", sc.expectedState, err, rcSession.logs.String())
	}

	title, err := wd.evalString(ctx, sid, `return document.title || "";`)
	if err != nil {
		return fmt.Errorf("read page title: %w", err)
	}
	if !strings.Contains(strings.ToLower(title), "remote control") {
		return fmt.Errorf("unexpected page title %q", title)
	}

	return nil
}

func sessionID(name string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '-':
			return r
		default:
			return '-'
		}
	}, strings.ToLower(strings.TrimSpace(name)))
	clean = strings.Trim(clean, "-")
	if clean == "" {
		clean = "scenario"
	}
	return fmt.Sprintf("safari-smoke-%s-%d", clean, time.Now().UnixNano())
}

func waitForExpectedStatus(ctx context.Context, wd *webDriverClient, sid, expected string, prompts map[string]string) error {
	expectedLower := strings.ToLower(expected)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		_ = handlePromptIfPresent(ctx, wd, sid, prompts)

		status, err := wd.evalString(ctx, sid, `
			const el = document.getElementById("status");
			return el ? (el.textContent || "") : "";
		`)
		if err == nil && strings.Contains(strings.ToLower(status), expectedLower) {
			return nil
		}

		select {
		case <-ctx.Done():
			if err != nil {
				return fmt.Errorf("status check failed: %w", err)
			}
			return fmt.Errorf("status never reached %q (last=%q)", expected, status)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func handlePromptIfPresent(ctx context.Context, wd *webDriverClient, sid string, prompts map[string]string) error {
	if len(prompts) == 0 {
		return nil
	}
	text, err := wd.alertText(ctx, sid)
	if err != nil {
		if errors.Is(err, errNoSuchAlert) {
			return nil
		}
		return err
	}
	lower := strings.ToLower(text)
	for key, value := range prompts {
		if !strings.Contains(lower, key) {
			continue
		}
		if strings.TrimSpace(value) != "" {
			if err := wd.setAlertText(ctx, sid, value); err != nil {
				return err
			}
		}
		if err := wd.acceptAlert(ctx, sid); err != nil {
			return err
		}
		return nil
	}
	return nil
}

type driverProcess struct {
	cmd    *exec.Cmd
	logs   bytes.Buffer
	base   string
	stdout io.Writer
	stderr io.Writer
}

func startDriver(binary string, port int, verbose bool, stdout, stderr io.Writer) (*driverProcess, error) {
	cmd := exec.Command(binary, "-p", strconv.Itoa(port))
	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("safaridriver stdout pipe: %w", err)
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("safaridriver stderr pipe: %w", err)
	}

	d := &driverProcess{
		cmd:    cmd,
		base:   fmt.Sprintf("http://127.0.0.1:%d", port),
		stdout: stdout,
		stderr: stderr,
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start safaridriver: %w", err)
	}

	go copyStream(&d.logs, outPipe, func(line string) {
		if verbose {
			fmt.Fprintf(stdout, "[safaridriver] %s\n", line)
		}
	})
	go copyStream(&d.logs, errPipe, func(line string) {
		if verbose {
			fmt.Fprintf(stderr, "[safaridriver] %s\n", line)
		}
	})

	return d, nil
}

func (d *driverProcess) baseURL() string {
	return d.base
}

func (d *driverProcess) stop() {
	if d == nil || d.cmd == nil || d.cmd.Process == nil {
		return
	}
	_ = d.cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() {
		_, _ = d.cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = d.cmd.Process.Kill()
		<-done
	}
}

func waitForDriverReady(ctx context.Context, baseURL string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	statusURL := strings.TrimRight(baseURL, "/") + "/status"
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				var parsed map[string]any
				if json.Unmarshal(body, &parsed) == nil {
					if ready, ok := valueBool(parsed, "ready"); ok && ready {
						return nil
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func valueBool(parsed map[string]any, key string) (bool, bool) {
	valueRaw, ok := parsed["value"]
	if !ok {
		return false, false
	}
	valueMap, ok := valueRaw.(map[string]any)
	if !ok {
		return false, false
	}
	v, ok := valueMap[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

type startSessionConfig struct {
	binary     string
	home       string
	id         string
	bind       string
	port       int
	readwrite  bool
	tokenInURL bool
	accessCode string
	verbose    bool
	stdout     io.Writer
	stderr     io.Writer
}

type remoteControlSession struct {
	id          string
	home        string
	shareURL    string
	accessToken string
	cmd         *exec.Cmd
	logs        bytes.Buffer
	waitCh      chan error
	mu          sync.Mutex
}

func startRemoteControlSession(ctx context.Context, cfg startSessionConfig) (*remoteControlSession, error) {
	args := []string{
		"start",
		"--cmd", "cat",
		"--bind", cfg.bind,
		"--port", strconv.Itoa(cfg.port),
		"--id", cfg.id,
		"--no-tunnel",
		"--no-caffeinate",
	}
	if cfg.readwrite {
		args = append(args, "--readwrite")
	}
	if strings.TrimSpace(cfg.accessCode) != "" {
		args = append(args, "--access-code", cfg.accessCode)
	}
	if !cfg.tokenInURL {
		args = append(args, "--no-token-in-url")
	}

	cmd := exec.CommandContext(ctx, cfg.binary, args...)
	cmd.Env = append(os.Environ(), "SI_REMOTE_CONTROL_HOME="+cfg.home)
	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("remote-control stdout pipe: %w", err)
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("remote-control stderr pipe: %w", err)
	}

	rc := &remoteControlSession{id: cfg.id, home: cfg.home, cmd: cmd}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start remote-control: %w", err)
	}

	go copyStream(&rc.logs, outPipe, func(line string) {
		if cfg.verbose {
			fmt.Fprintf(cfg.stdout, "[remote-control %s] %s\n", cfg.id, line)
		}
		ev := parseSessionLine(line)
		rc.recordEvent(ev)
	})
	go copyStream(&rc.logs, errPipe, func(line string) {
		if cfg.verbose {
			fmt.Fprintf(cfg.stderr, "[remote-control %s] %s\n", cfg.id, line)
		}
		ev := parseSessionLine(line)
		rc.recordEvent(ev)
	})

	rc.waitCh = make(chan error, 1)
	go func() { rc.waitCh <- cmd.Wait() }()

	for {
		shareURL, accessToken := rc.snapshot()
		if shareURL != "" && (cfg.tokenInURL || accessToken != "") {
			return rc, nil
		}
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			<-rc.waitCh
			return nil, ctx.Err()
		case err := <-rc.waitCh:
			if err == nil {
				return nil, fmt.Errorf("remote-control exited before share URL")
			}
			return nil, fmt.Errorf("remote-control exited early: %w\nlogs:\n%s", err, rc.logs.String())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (r *remoteControlSession) recordEvent(ev sessionEvent) {
	if ev.shareURL == "" && ev.accessToken == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if ev.shareURL != "" {
		r.shareURL = ev.shareURL
	}
	if ev.accessToken != "" {
		r.accessToken = ev.accessToken
	}
}

func (r *remoteControlSession) currentAccessToken() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.accessToken
}

func (r *remoteControlSession) snapshot() (string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.shareURL, r.accessToken
}

func stopRemoteControlSession(rc *remoteControlSession, timeout time.Duration) error {
	if rc == nil || rc.cmd == nil || rc.waitCh == nil {
		return nil
	}
	stopCmd := exec.Command(rc.cmd.Path, "stop", "--id", rc.id)
	stopCmd.Env = append(os.Environ(), "SI_REMOTE_CONTROL_HOME="+rc.home)
	_, _ = stopCmd.CombinedOutput()

	if rc.cmd.Process == nil {
		return nil
	}
	select {
	case <-time.After(timeout):
		_ = rc.cmd.Process.Kill()
		<-rc.waitCh
		return fmt.Errorf("timed out waiting for remote-control process to stop")
	case err := <-rc.waitCh:
		if err != nil {
			return err
		}
		return nil
	}
}

type sessionEvent struct {
	shareURL    string
	accessToken string
}

func parseSessionLine(line string) sessionEvent {
	ev := sessionEvent{}
	if u := parseLabeledValue(line, "Share URL:"); u != "" {
		ev.shareURL = u
	}
	if t := parseLabeledValue(line, "Access Token:"); t != "" {
		ev.accessToken = t
	}
	return ev
}

func parseLabeledValue(line, label string) string {
	idx := strings.Index(line, label)
	if idx < 0 {
		return ""
	}
	value := strings.TrimSpace(line[idx+len(label):])
	return strings.Trim(value, "\t\r\n")
}

func copyStream(logs *bytes.Buffer, stream io.Reader, onLine func(string)) {
	s := bufio.NewScanner(stream)
	buf := make([]byte, 0, 64*1024)
	s.Buffer(buf, 1024*1024)
	for s.Scan() {
		line := s.Text()
		_, _ = logs.WriteString(line)
		_, _ = logs.WriteString("\n")
		if onLine != nil {
			onLine(line)
		}
	}
}

func pickFreePort(bind string) (int, error) {
	host := bind
	if host == "0.0.0.0" || host == "" {
		host = "127.0.0.1"
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("unexpected listener address type %T", ln.Addr())
	}
	return addr.Port, nil
}

type webDriverClient struct {
	baseURL string
	http    *http.Client
}

type wdEnvelope struct {
	Value     json.RawMessage `json:"value"`
	SessionID string          `json:"sessionId"`
	Status    int             `json:"status"`
}

type wdValueError struct {
	Code    string `json:"error"`
	Message string `json:"message"`
}

type wdError struct {
	status int
	code   string
	msg    string
}

func (e *wdError) Error() string {
	if e == nil {
		return ""
	}
	if e.code == "" {
		return fmt.Sprintf("webdriver error (status=%d): %s", e.status, e.msg)
	}
	return fmt.Sprintf("webdriver error (%s, status=%d): %s", e.code, e.status, e.msg)
}

var errNoSuchAlert = errors.New("webdriver: no such alert")

func (w *webDriverClient) newSession(ctx context.Context) (string, error) {
	payload := map[string]any{
		"capabilities": map[string]any{
			"alwaysMatch": map[string]any{
				"browserName": "safari",
			},
		},
	}
	value, sessionID, err := w.doJSON(ctx, http.MethodPost, "/session", payload)
	if err != nil {
		return "", err
	}
	if sessionID != "" {
		return sessionID, nil
	}
	var parsed struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(value, &parsed); err == nil && parsed.SessionID != "" {
		return parsed.SessionID, nil
	}
	return "", fmt.Errorf("webdriver new session response missing session id")
}

func (w *webDriverClient) deleteSession(ctx context.Context, sid string) error {
	_, _, err := w.doJSON(ctx, http.MethodDelete, "/session/"+url.PathEscape(sid), nil)
	return err
}

func (w *webDriverClient) navigate(ctx context.Context, sid, targetURL string) error {
	_, _, err := w.doJSON(ctx, http.MethodPost, "/session/"+url.PathEscape(sid)+"/url", map[string]string{"url": targetURL})
	return err
}

func (w *webDriverClient) evalString(ctx context.Context, sid, script string) (string, error) {
	value, _, err := w.doJSON(ctx, http.MethodPost, "/session/"+url.PathEscape(sid)+"/execute/sync", map[string]any{
		"script": script,
		"args":   []any{},
	})
	if err != nil {
		var wdErr *wdError
		if errors.As(err, &wdErr) && wdErr.status == http.StatusNotFound {
			value, _, err = w.doJSON(ctx, http.MethodPost, "/session/"+url.PathEscape(sid)+"/execute", map[string]any{
				"script": script,
				"args":   []any{},
			})
		}
	}
	if err != nil {
		return "", err
	}
	var parsed string
	if err := json.Unmarshal(value, &parsed); err != nil {
		return "", fmt.Errorf("decode webdriver script result: %w", err)
	}
	return parsed, nil
}

func (w *webDriverClient) alertText(ctx context.Context, sid string) (string, error) {
	value, _, err := w.doJSON(ctx, http.MethodGet, "/session/"+url.PathEscape(sid)+"/alert/text", nil)
	if err != nil {
		var wdErr *wdError
		if errors.As(err, &wdErr) && strings.EqualFold(strings.TrimSpace(wdErr.code), "no such alert") {
			return "", errNoSuchAlert
		}
		return "", err
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return "", fmt.Errorf("decode alert text: %w", err)
	}
	return text, nil
}

func (w *webDriverClient) setAlertText(ctx context.Context, sid, text string) error {
	_, _, err := w.doJSON(ctx, http.MethodPost, "/session/"+url.PathEscape(sid)+"/alert/text", map[string]string{"text": text})
	return err
}

func (w *webDriverClient) acceptAlert(ctx context.Context, sid string) error {
	_, _, err := w.doJSON(ctx, http.MethodPost, "/session/"+url.PathEscape(sid)+"/alert/accept", map[string]any{})
	return err
}

func (w *webDriverClient) doJSON(ctx context.Context, method, path string, payload any) (json.RawMessage, string, error) {
	fullURL := strings.TrimRight(w.baseURL, "/") + path
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, "", err
		}
		body = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, "", err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := w.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	env := wdEnvelope{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &env); err != nil {
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return nil, "", &wdError{status: resp.StatusCode, msg: strings.TrimSpace(string(raw))}
			}
			return nil, "", fmt.Errorf("decode webdriver response: %w", err)
		}
	}

	if wdErr := extractWDError(resp.StatusCode, env.Value); wdErr != nil {
		return nil, env.SessionID, wdErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, env.SessionID, &wdError{status: resp.StatusCode, msg: strings.TrimSpace(string(raw))}
	}
	if len(env.Value) == 0 {
		env.Value = json.RawMessage("null")
	}
	return env.Value, env.SessionID, nil
}

func extractWDError(statusCode int, raw json.RawMessage) error {
	if len(raw) == 0 {
		if statusCode >= 200 && statusCode < 300 {
			return nil
		}
		return &wdError{status: statusCode, msg: "empty webdriver error payload"}
	}
	var werr wdValueError
	if err := json.Unmarshal(raw, &werr); err == nil && strings.TrimSpace(werr.Code) != "" {
		return &wdError{status: statusCode, code: strings.TrimSpace(werr.Code), msg: strings.TrimSpace(werr.Message)}
	}
	if statusCode >= 200 && statusCode < 300 {
		return nil
	}
	return nil
}
