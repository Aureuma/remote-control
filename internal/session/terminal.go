package session

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"

	"github.com/creack/pty"

	"github.com/si/remote-control/internal/tmux"
)

type Mode string

const (
	ModeAttach Mode = "attach"
	ModeCmd    Mode = "cmd"
)

type Terminal struct {
	mode      Mode
	source    string
	cmd       *exec.Cmd
	ptyFile   *os.File
	writeMu   sync.Mutex
	closeOnce sync.Once
}

func StartAttach(tmuxSession string) (*Terminal, error) {
	cmd, err := tmux.AttachCommand(tmuxSession)
	if err != nil {
		return nil, err
	}
	return start(ModeAttach, strings.TrimSpace(tmuxSession), cmd)
}

func StartCommand(command string) (*Terminal, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, fmt.Errorf("command is required")
	}
	shell := "/bin/sh"
	arg := "-lc"
	if runtime.GOOS == "windows" {
		shell = "cmd"
		arg = "/C"
	}
	cmd := exec.Command(shell, arg, command)
	return start(ModeCmd, command, cmd)
}

func start(mode Mode, source string, cmd *exec.Cmd) (*Terminal, error) {
	if cmd == nil {
		return nil, fmt.Errorf("command is nil")
	}
	f, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	return &Terminal{mode: mode, source: source, cmd: cmd, ptyFile: f}, nil
}

func (t *Terminal) Mode() Mode {
	if t == nil {
		return ""
	}
	return t.mode
}

func (t *Terminal) Source() string {
	if t == nil {
		return ""
	}
	return t.source
}

func (t *Terminal) PID() int {
	if t == nil || t.cmd == nil || t.cmd.Process == nil {
		return 0
	}
	return t.cmd.Process.Pid
}

func (t *Terminal) Read(p []byte) (int, error) {
	if t == nil || t.ptyFile == nil {
		return 0, os.ErrClosed
	}
	return t.ptyFile.Read(p)
}

func (t *Terminal) WriteInput(data []byte) error {
	if t == nil || t.ptyFile == nil {
		return os.ErrClosed
	}
	if len(data) == 0 {
		return nil
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	_, err := t.ptyFile.Write(data)
	return err
}

func (t *Terminal) Resize(cols, rows int) error {
	if t == nil || t.ptyFile == nil {
		return os.ErrClosed
	}
	if cols <= 0 || rows <= 0 {
		return nil
	}
	return pty.Setsize(t.ptyFile, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

func (t *Terminal) Wait() error {
	if t == nil || t.cmd == nil {
		return nil
	}
	return t.cmd.Wait()
}

func (t *Terminal) Close() error {
	if t == nil {
		return nil
	}
	var retErr error
	t.closeOnce.Do(func() {
		if t.cmd != nil && t.cmd.Process != nil {
			_ = t.cmd.Process.Kill()
		}
		if t.ptyFile != nil {
			if err := t.ptyFile.Close(); err != nil {
				retErr = err
			}
		}
	})
	return retErr
}
