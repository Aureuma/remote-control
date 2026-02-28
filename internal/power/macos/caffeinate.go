package macos

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

type Handle struct {
	cmd      *exec.Cmd
	stopOnce sync.Once
}

func Start(ctx context.Context) (*Handle, error) {
	if runtime.GOOS != "darwin" {
		return nil, nil
	}
	binary, err := exec.LookPath("caffeinate")
	if err != nil {
		return nil, fmt.Errorf("caffeinate not found: %w", err)
	}
	cmd := exec.CommandContext(ctx, binary, "-dimsu")
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &Handle{cmd: cmd}, nil
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
		if err := h.cmd.Process.Kill(); err != nil {
			if !errors.Is(err, exec.ErrNotFound) && !strings.Contains(strings.ToLower(err.Error()), "process already finished") {
				retErr = err
			}
		}
		_ = h.cmd.Wait()
	})
	return retErr
}
