package websocket

import "testing"

func TestFlowControllerPauseAndResume(t *testing.T) {
	f := newFlowController(10, 20)
	if ev := f.onSent(8); ev != flowEventNone {
		t.Fatalf("expected no event, got %v", ev)
	}
	if f.paused {
		t.Fatalf("expected not paused")
	}
	if ev := f.onSent(15); ev != flowEventPause {
		t.Fatalf("expected pause event, got %v", ev)
	}
	if !f.paused {
		t.Fatalf("expected paused")
	}
	if ev := f.onAck(5); ev != flowEventNone {
		t.Fatalf("expected no event, got %v", ev)
	}
	if !f.paused {
		t.Fatalf("expected still paused")
	}
	if ev := f.onAck(20); ev != flowEventResume {
		t.Fatalf("expected resume event, got %v", ev)
	}
	if f.paused {
		t.Fatalf("expected resumed")
	}
}

func TestFlowControllerResetsAndClamps(t *testing.T) {
	f := newFlowController(100, 50)
	if f.low <= 0 || f.high <= 0 {
		t.Fatalf("expected positive low/high")
	}
	if f.low > f.high {
		t.Fatalf("expected low <= high")
	}
	_ = f.onSent(1000)
	if f.pending == 0 {
		t.Fatalf("expected pending bytes")
	}
	_ = f.onAck(2000)
	if f.pending != 0 {
		t.Fatalf("expected pending to clamp to zero, got %d", f.pending)
	}
	_ = f.onSent(100)
	f.reset()
	if f.pending != 0 || f.paused {
		t.Fatalf("expected reset state")
	}
}
