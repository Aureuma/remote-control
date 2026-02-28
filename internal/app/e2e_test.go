package app

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestE2EStartStatusStop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty e2e not supported on windows in this test")
	}
	bin := buildRemoteControlBinaryForTest(t)
	home := t.TempDir()
	id := fmt.Sprintf("e2e-start-%d", time.Now().UnixNano())
	port := freeLocalPort(t)

	serverCmd := exec.Command(bin,
		"start",
		"--cmd", "sleep 120",
		"--bind", "127.0.0.1",
		"--port", fmt.Sprintf("%d", port),
		"--id", id,
		"--no-tunnel",
		"--no-caffeinate",
	)
	serverCmd.Env = append(os.Environ(), "SI_REMOTE_CONTROL_HOME="+home)
	startLogFile := mustTempFile(t)
	defer func() { _ = startLogFile.Close() }()
	serverCmd.Stdout = startLogFile
	serverCmd.Stderr = startLogFile
	if err := serverCmd.Start(); err != nil {
		t.Fatalf("start remote-control: %v", err)
	}
	defer killProcess(serverCmd)

	waitForSessionState(t, home, id, 10*time.Second, startLogFile.Name())

	statusOut := runRemoteControlCmd(t, bin, home, "status")
	if !strings.Contains(statusOut, id) {
		t.Fatalf("status output missing session id %q:\n%s", id, statusOut)
	}
	if !strings.Contains(statusOut, "mode=cmd") {
		t.Fatalf("status output missing cmd mode:\n%s", statusOut)
	}

	stopOut := runRemoteControlCmd(t, bin, home, "stop", "--id", id)
	if !strings.Contains(stopOut, "Stop signal sent") {
		t.Fatalf("unexpected stop output:\n%s", stopOut)
	}
	waitForProcessExit(t, serverCmd, 10*time.Second, startLogFile.Name())
}

func TestE2EAttachStatusStop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tmux attach e2e not supported on windows")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not found")
	}

	bin := buildRemoteControlBinaryForTest(t)
	home := t.TempDir()
	id := fmt.Sprintf("e2e-attach-%d", time.Now().UnixNano())
	tmuxSession := fmt.Sprintf("rc-e2e-%d", time.Now().UnixNano())
	port := freeLocalPort(t)

	create := exec.Command("tmux", "new-session", "-d", "-s", tmuxSession, "sleep 120")
	if out, err := create.CombinedOutput(); err != nil {
		t.Fatalf("create tmux session %q failed: %v\n%s", tmuxSession, err, string(out))
	}
	defer func() {
		kill := exec.Command("tmux", "kill-session", "-t", tmuxSession)
		_, _ = kill.CombinedOutput()
	}()

	serverCmd := exec.Command(bin,
		"attach",
		"--tmux-session", tmuxSession,
		"--bind", "127.0.0.1",
		"--port", fmt.Sprintf("%d", port),
		"--id", id,
		"--no-tunnel",
		"--no-caffeinate",
	)
	serverCmd.Env = append(os.Environ(), "SI_REMOTE_CONTROL_HOME="+home)
	attachLogFile := mustTempFile(t)
	defer func() { _ = attachLogFile.Close() }()
	serverCmd.Stdout = attachLogFile
	serverCmd.Stderr = attachLogFile
	if err := serverCmd.Start(); err != nil {
		t.Fatalf("start attach session: %v", err)
	}
	defer killProcess(serverCmd)

	waitForSessionState(t, home, id, 10*time.Second, attachLogFile.Name())

	statusOut := runRemoteControlCmd(t, bin, home, "status")
	if !strings.Contains(statusOut, id) {
		t.Fatalf("status output missing session id %q:\n%s", id, statusOut)
	}
	if !strings.Contains(statusOut, "mode=attach") {
		t.Fatalf("status output missing attach mode:\n%s", statusOut)
	}

	stopOut := runRemoteControlCmd(t, bin, home, "stop", "--id", id)
	if !strings.Contains(stopOut, "Stop signal sent") {
		t.Fatalf("unexpected stop output:\n%s", stopOut)
	}
	waitForProcessExit(t, serverCmd, 10*time.Second, attachLogFile.Name())
}

func buildRemoteControlBinaryForTest(t *testing.T) string {
	t.Helper()
	root := repoRootForTest(t)
	out := filepath.Join(t.TempDir(), "remote-control")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/remote-control")
	cmd.Dir = root
	buildOut, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build remote-control failed: %v\n%s", err, string(buildOut))
	}
	return out
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func freeLocalPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve local port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func runRemoteControlCmd(t *testing.T, bin, home string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "SI_REMOTE_CONTROL_HOME="+home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("remote-control %s failed: %v\n%s", strings.Join(args, " "), err, string(out))
	}
	return string(out)
}

func waitForSessionState(t *testing.T, home, id string, timeout time.Duration, logPath string) {
	t.Helper()
	statePath := filepath.Join(home, "runtime", id+".json")
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(statePath); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for session state %s\nlogs:\n%s", statePath, readLogFile(logPath))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func waitForProcessExit(t *testing.T, cmd *exec.Cmd, timeout time.Duration, logPath string) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("session process exited with error: %v\nlogs:\n%s", err, readLogFile(logPath))
		}
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for session process to exit\nlogs:\n%s", readLogFile(logPath))
	}
}

func mustTempFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "rc-e2e-*.log")
	if err != nil {
		t.Fatalf("create temp log file: %v", err)
	}
	return f
}

func readLogFile(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func killProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
}
