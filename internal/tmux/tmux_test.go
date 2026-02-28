package tmux

import "testing"

func TestParseSessionsOutput(t *testing.T) {
	raw := "dev|1|3|2026-02-28 10:00:00\r\nwork|0|2|2026-02-28 11:00:00\ninvalid\n|1|2|missing-name\n"
	got := parseSessionsOutput(raw)
	if len(got) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(got))
	}
	if got[0].Name != "dev" || got[0].Attached != 1 || got[0].Windows != 3 {
		t.Fatalf("unexpected first session: %+v", got[0])
	}
	if got[1].Name != "work" || got[1].Attached != 0 || got[1].Windows != 2 {
		t.Fatalf("unexpected second session: %+v", got[1])
	}
}
