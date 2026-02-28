package runtime

import (
	"os"
	"testing"
	"time"
)

func TestPruneStaleSessions(t *testing.T) {
	t.Setenv("SI_REMOTE_CONTROL_RUNTIME_DIR", t.TempDir())
	alive := SessionState{
		ID:        "alive",
		PID:       os.Getpid(),
		StartedAt: time.Now().UTC(),
	}
	stale := SessionState{
		ID:        "stale",
		PID:       999999,
		StartedAt: time.Now().UTC(),
	}
	if err := SaveSession(alive); err != nil {
		t.Fatalf("save alive: %v", err)
	}
	if err := SaveSession(stale); err != nil {
		t.Fatalf("save stale: %v", err)
	}
	removed, err := PruneStaleSessions()
	if err != nil {
		t.Fatalf("prune stale: %v", err)
	}
	if len(removed) != 1 || removed[0] != "stale" {
		t.Fatalf("expected stale to be removed, got %v", removed)
	}
	states, err := ListSessions()
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(states) != 1 || states[0].ID != "alive" {
		t.Fatalf("expected only alive state remaining, got %+v", states)
	}
}
