package cloudflare

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

var tunnelURLPattern = regexp.MustCompile(`https://[A-Za-z0-9.-]+(?:\:[0-9]+)?(?:/[\S]*)?`)

type Options struct {
	Binary          string
	LocalURL        string
	StartupTimeout  time.Duration
	Mode            string
	Hostname        string
	TunnelName      string
	TunnelToken     string
	ConfigFile      string
	CredentialsFile string
}

type Handle struct {
	cmd       *exec.Cmd
	publicURL string
	stopOnce  sync.Once
}

func Start(ctx context.Context, opts Options) (*Handle, error) {
	binary := strings.TrimSpace(opts.Binary)
	if binary == "" {
		binary = "cloudflared"
	}
	localURL := strings.TrimSpace(opts.LocalURL)
	if localURL == "" {
		return nil, fmt.Errorf("local url is required")
	}
	if opts.StartupTimeout <= 0 {
		opts.StartupTimeout = 20 * time.Second
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("cloudflare tunnel binary not found: %w", err)
	}
	args, expectedURL, err := buildInvocation(localURL, opts)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, resolved, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	if expectedURL != "" {
		if err := waitForProcessStartup(ctx, cmd, opts.StartupTimeout); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, err
		}
		return &Handle{cmd: cmd, publicURL: expectedURL}, nil
	}

	lines := make(chan string, 32)
	readPipe := func(scanner *bufio.Scanner) {
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" {
				select {
				case lines <- line:
				default:
				}
			}
		}
	}
	go readPipe(bufio.NewScanner(stdout))
	go readPipe(bufio.NewScanner(stderr))

	timer := time.NewTimer(opts.StartupTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, ctx.Err()
		case <-timer.C:
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, fmt.Errorf("timed out waiting for cloudflare tunnel url")
		case line := <-lines:
			if url, ok := parsePublicURL(line); ok {
				return &Handle{cmd: cmd, publicURL: url}, nil
			}
		}
	}
}

func buildInvocation(localURL string, opts Options) ([]string, string, error) {
	mode := strings.TrimSpace(strings.ToLower(opts.Mode))
	if mode == "" {
		mode = "ephemeral"
	}
	switch mode {
	case "ephemeral":
		return []string{"tunnel", "--url", localURL, "--no-autoupdate"}, "", nil
	case "named":
		publicURL := ""
		if strings.TrimSpace(opts.Hostname) != "" {
			normalizedURL, err := normalizePublicURLFromHostname(opts.Hostname)
			if err != nil {
				return nil, "", fmt.Errorf("named tunnel requires a valid hostname: %w", err)
			}
			publicURL = normalizedURL
		}
		tunnelToken := strings.TrimSpace(opts.TunnelToken)
		if tunnelToken != "" {
			return []string{"tunnel", "run", "--token", tunnelToken}, publicURL, nil
		}
		if publicURL == "" {
			return nil, "", fmt.Errorf("named tunnel requires --tunnel-hostname unless --tunnel-token is provided")
		}
		args := []string{"tunnel", "--url", localURL, "--hostname", strings.TrimSpace(opts.Hostname), "--no-autoupdate"}
		if cfg := strings.TrimSpace(opts.ConfigFile); cfg != "" {
			args = append(args, "--config", cfg)
		}
		if creds := strings.TrimSpace(opts.CredentialsFile); creds != "" {
			args = append(args, "--credentials-file", creds)
		}
		if name := strings.TrimSpace(opts.TunnelName); name != "" {
			args = append(args, "--name", name)
		}
		return args, publicURL, nil
	default:
		return nil, "", fmt.Errorf("unsupported tunnel mode %q (expected ephemeral|named)", mode)
	}
}

func normalizePublicURLFromHostname(raw string) (string, error) {
	host := strings.TrimSpace(raw)
	if host == "" {
		return "", fmt.Errorf("hostname is required")
	}
	if strings.Contains(host, "://") {
		parsed, err := url.Parse(host)
		if err != nil {
			return "", err
		}
		host = strings.TrimSpace(parsed.Host)
	}
	if i := strings.Index(host, "/"); i >= 0 {
		host = strings.TrimSpace(host[:i])
	}
	host = strings.Trim(host, "/")
	if host == "" {
		return "", fmt.Errorf("hostname is empty")
	}
	return "https://" + host, nil
}

func waitForProcessStartup(ctx context.Context, cmd *exec.Cmd, timeout time.Duration) error {
	if cmd == nil || cmd.Process == nil {
		return fmt.Errorf("cloudflared process is not running")
	}
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	deadline := time.Now().Add(timeout)
	readyAfter := 800 * time.Millisecond
	startedAt := time.Now()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if !processRunning(cmd.Process) {
			return fmt.Errorf("cloudflared exited before startup completed")
		}
		if time.Since(startedAt) >= readyAfter {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for cloudflared startup")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func processRunning(proc *os.Process) bool {
	if proc == nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func parsePublicURL(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", false
	}
	match := tunnelURLPattern.FindString(line)
	if match == "" {
		return "", false
	}
	if !strings.HasPrefix(match, "https://") {
		return "", false
	}
	return match, true
}

func (h *Handle) PublicURL() string {
	if h == nil {
		return ""
	}
	return h.publicURL
}

func (h *Handle) PID() int {
	if h == nil || h.cmd == nil || h.cmd.Process == nil {
		return 0
	}
	return h.cmd.Process.Pid
}

func (h *Handle) Stop() error {
	if h == nil {
		return nil
	}
	var retErr error
	h.stopOnce.Do(func() {
		if h.cmd == nil || h.cmd.Process == nil {
			return
		}
		if err := h.cmd.Process.Kill(); err != nil && !errors.Is(err, exec.ErrNotFound) {
			retErr = err
		}
		_ = h.cmd.Wait()
	})
	return retErr
}
