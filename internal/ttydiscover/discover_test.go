package ttydiscover

import "testing"

func TestParsePSOutput(t *testing.T) {
	raw := `
123 pts/1 bash -i
124 ? cron /usr/sbin/cron -f
abc pts/2 zsh -l
222 tty1 login -
333 - kworker/u16:2
`
	got := parsePSOutput(raw)
	if len(got) != 2 {
		t.Fatalf("len=%d want 2", len(got))
	}
	if got[0].PID != 123 || got[0].TTY != "/dev/pts/1" || got[0].Command != "bash" {
		t.Fatalf("unexpected first candidate: %+v", got[0])
	}
	if got[1].PID != 222 || got[1].TTY != "/dev/tty1" || got[1].Command != "login" {
		t.Fatalf("unexpected second candidate: %+v", got[1])
	}
}

func TestTTYPath(t *testing.T) {
	if got := ttyPath("pts/3"); got != "/dev/pts/3" {
		t.Fatalf("ttyPath pts: %q", got)
	}
	if got := ttyPath("/dev/tty7"); got != "/dev/tty7" {
		t.Fatalf("ttyPath abs: %q", got)
	}
}
