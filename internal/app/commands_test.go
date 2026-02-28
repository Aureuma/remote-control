package app

import (
	"os/exec"
	"runtime"
	"testing"
	"time"
)

func setTestHome(t *testing.T) {
	t.Helper()
	t.Setenv("SI_REMOTE_CONTROL_HOME", t.TempDir())
}

func TestRunHelpAndUnknown(t *testing.T) {
	setTestHome(t)
	if got := Run([]string{"help"}); got != 0 {
		t.Fatalf("Run(help)=%d want 0", got)
	}
	if got := Run([]string{"unknown-command"}); got == 0 {
		t.Fatalf("Run(unknown-command)=%d want non-zero", got)
	}
}

func TestRunStartRequiresCmd(t *testing.T) {
	setTestHome(t)
	if got := Run([]string{"start", "--no-tunnel"}); got == 0 {
		t.Fatalf("Run(start without --cmd)=%d want non-zero", got)
	}
}

func TestRunAttachAndStartHelpExitZero(t *testing.T) {
	setTestHome(t)
	if got := Run([]string{"attach", "--help"}); got != 0 {
		t.Fatalf("Run(attach --help)=%d want 0", got)
	}
	if got := Run([]string{"start", "--help"}); got != 0 {
		t.Fatalf("Run(start --help)=%d want 0", got)
	}
	if got := Run([]string{"stop", "--help"}); got != 0 {
		t.Fatalf("Run(stop --help)=%d want 0", got)
	}
}

func TestRunStartRejectsInvalidPort(t *testing.T) {
	setTestHome(t)
	if got := Run([]string{"start", "--cmd", "sleep 1", "--port", "70000", "--no-tunnel"}); got == 0 {
		t.Fatalf("Run(start invalid port)=%d want non-zero", got)
	}
}

func TestRunStatusAndStopOnEmptyState(t *testing.T) {
	setTestHome(t)
	if got := Run([]string{"status"}); got != 0 {
		t.Fatalf("Run(status)=%d want 0", got)
	}
	if got := Run([]string{"stop"}); got != 0 {
		t.Fatalf("Run(stop)=%d want 0", got)
	}
}

func TestRunAttachRejectsMissingTmuxSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tmux attach validation test unsupported on windows")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not found")
	}
	setTestHome(t)
	name := "rc-test-existing-" + time.Now().Format("150405")
	create := exec.Command("tmux", "new-session", "-d", "-s", name, "sleep 30")
	if out, err := create.CombinedOutput(); err != nil {
		t.Fatalf("create tmux session: %v\n%s", err, string(out))
	}
	defer func() {
		kill := exec.Command("tmux", "kill-session", "-t", name)
		_, _ = kill.CombinedOutput()
	}()

	if got := Run([]string{"attach", "--tmux-session", name + "-missing", "--no-tunnel"}); got == 0 {
		t.Fatalf("Run(attach missing session)=%d want non-zero", got)
	}
}
