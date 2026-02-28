package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/si/remote-control/internal/config"
)

type SessionState struct {
	ID           string    `json:"id"`
	Mode         string    `json:"mode"`
	Source       string    `json:"source"`
	ReadOnly     bool      `json:"readonly"`
	PID          int       `json:"pid"`
	Addr         string    `json:"addr"`
	URL          string    `json:"url"`
	StartedAt    time.Time `json:"started_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	ClientCount  int       `json:"client_count"`
	SettingsFile string    `json:"settings_file,omitempty"`
}

func SaveSession(state SessionState) error {
	runtimeDir, err := config.RuntimeDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return err
	}
	if strings.TrimSpace(state.ID) == "" {
		return fmt.Errorf("session id is required")
	}
	now := time.Now().UTC()
	if state.StartedAt.IsZero() {
		state.StartedAt = now
	}
	state.UpdatedAt = now
	path := filepath.Join(runtimeDir, state.ID+".json")
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(runtimeDir, "session-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func RemoveSession(id string) error {
	runtimeDir, err := config.RuntimeDir()
	if err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("session id is required")
	}
	path := filepath.Join(runtimeDir, id+".json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func LoadSession(id string) (SessionState, error) {
	runtimeDir, err := config.RuntimeDir()
	if err != nil {
		return SessionState{}, err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return SessionState{}, fmt.Errorf("session id is required")
	}
	path := filepath.Join(runtimeDir, id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return SessionState{}, err
	}
	var state SessionState
	if err := json.Unmarshal(data, &state); err != nil {
		return SessionState{}, err
	}
	return state, nil
}

func ListSessions() ([]SessionState, error) {
	runtimeDir, err := config.RuntimeDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	states := make([]SessionState, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(runtimeDir, entry.Name()))
		if err != nil {
			continue
		}
		var state SessionState
		if err := json.Unmarshal(data, &state); err != nil {
			continue
		}
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool {
		return states[i].StartedAt.After(states[j].StartedAt)
	})
	return states, nil
}

func ProcessAlive(pid int) bool {
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

func PruneStaleSessions() ([]string, error) {
	states, err := ListSessions()
	if err != nil {
		return nil, err
	}
	removed := make([]string, 0, len(states))
	for _, state := range states {
		if strings.TrimSpace(state.ID) == "" {
			continue
		}
		if ProcessAlive(state.PID) {
			continue
		}
		if err := RemoveSession(state.ID); err != nil {
			return removed, err
		}
		removed = append(removed, state.ID)
	}
	return removed, nil
}
