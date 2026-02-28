package session

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestStartTTYPathReadWriteAndWait(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	defer func() {
		_ = ptmx.Close()
		_ = tty.Close()
	}()

	term, err := StartTTYPath(tty.Name())
	if err != nil {
		t.Fatalf("StartTTYPath: %v", err)
	}
	defer term.Close()

	if term.Mode() != ModeTTY {
		t.Fatalf("mode=%q want %q", term.Mode(), ModeTTY)
	}
	if term.Source() != tty.Name() {
		t.Fatalf("source=%q want %q", term.Source(), tty.Name())
	}

	writeDone := make(chan error, 1)
	go func() {
		_, err := ptmx.Write([]byte("hello\n"))
		writeDone <- err
	}()
	buf := make([]byte, 32)
	_ = tty.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := term.Read(buf)
	if err != nil {
		t.Fatalf("term.Read: %v", err)
	}
	if string(buf[:n]) != "hello\n" {
		t.Fatalf("unexpected term read payload: %q", string(buf[:n]))
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("ptmx write: %v", err)
	}

	if err := term.WriteInput([]byte("echo\n")); err != nil {
		t.Fatalf("term.WriteInput: %v", err)
	}
	readBack := make([]byte, 32)
	_ = ptmx.SetReadDeadline(time.Now().Add(2 * time.Second))
	m, err := ptmx.Read(readBack)
	if err != nil {
		t.Fatalf("ptmx.Read: %v", err)
	}
	if !strings.Contains(string(readBack[:m]), "echo") {
		t.Fatalf("unexpected ptmx payload: %q", string(readBack[:m]))
	}

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- term.Wait()
	}()
	select {
	case <-waitDone:
		t.Fatalf("Wait returned before Close")
	case <-time.After(200 * time.Millisecond):
	}
	if err := term.Close(); err != nil && !os.IsNotExist(err) {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("Wait returned err after Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Wait did not return after Close")
	}
}
