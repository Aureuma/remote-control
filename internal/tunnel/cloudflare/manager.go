package cloudflare

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

var tunnelURLPattern = regexp.MustCompile(`https://[A-Za-z0-9.-]+(?:\:[0-9]+)?(?:/[\S]*)?`)

type Options struct {
	Binary         string
	LocalURL       string
	StartupTimeout time.Duration
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

	cmd := exec.CommandContext(ctx, resolved, "tunnel", "--url", localURL, "--no-autoupdate")
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
