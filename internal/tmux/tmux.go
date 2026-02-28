package tmux

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

type Session struct {
	Name     string
	Attached int
	Windows  int
	Created  string
}

func EnsureInstalled() error {
	if _, err := exec.LookPath("tmux"); err != nil {
		return errors.New("tmux not found in PATH")
	}
	return nil
}

func ListSessions() ([]Session, error) {
	if err := EnsureInstalled(); err != nil {
		return nil, err
	}
	cmd := exec.Command("tmux", "list-sessions", "-F", "#{session_name}|#{session_attached}|#{session_windows}|#{session_created_string}")
	output, err := cmd.CombinedOutput()
	if err != nil {
		lower := strings.ToLower(strings.TrimSpace(string(output)))
		if strings.Contains(lower, "no server running") || strings.Contains(lower, "failed to connect to server") || strings.Contains(lower, "error connecting to") {
			return nil, nil
		}
		if ee := new(exec.ExitError); errors.As(err, &ee) && ee.ExitCode() == 1 && strings.TrimSpace(string(output)) == "" {
			return nil, nil
		}
		return nil, err
	}
	text := strings.TrimRight(string(output), "\r\n")
	if text == "" {
		return nil, nil
	}
	return parseSessionsOutput(text), nil
}

func AttachCommand(session string) (*exec.Cmd, error) {
	session = strings.TrimSpace(session)
	if session == "" {
		return nil, fmt.Errorf("tmux session is required")
	}
	if err := EnsureInstalled(); err != nil {
		return nil, err
	}
	return exec.Command("tmux", "attach-session", "-t", session), nil
}

func parseSessionsOutput(text string) []Session {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	sessions := make([]Session, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 4)
		if len(parts) < 3 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		if name == "" {
			continue
		}
		attached, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
		windows, _ := strconv.Atoi(strings.TrimSpace(parts[2]))
		created := ""
		if len(parts) >= 4 {
			created = strings.TrimSpace(parts[3])
		}
		sessions = append(sessions, Session{
			Name:     name,
			Attached: attached,
			Windows:  windows,
			Created:  created,
		})
	}
	return sessions
}
