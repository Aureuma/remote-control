package websocket

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/Aureuma/remote-control/internal/session"
)

func TestWSAuthInputPingAndPrefs(t *testing.T) {
	term, err := session.StartCommand("printf 'ready\\n'; while IFS= read -r line; do echo \"ECHO:$line\"; done")
	if err != nil {
		t.Fatalf("start command: %v", err)
	}
	t.Cleanup(func() { _ = term.Close() })

	wsURL, shutdown := startWSTestServer(t, term, "token-ok", ServerOptions{
		ReadOnly:          false,
		MaxClients:        1,
		AckQuantumBytes:   12345,
		PingInterval:      time.Second,
		ClientReadTimeout: 3 * time.Second,
	})
	defer shutdown()

	conn := dialAndAuth(t, wsURL, "token-ok", nil)
	defer conn.Close()

	expectTextType(t, conn, "auth_ok", 2*time.Second)
	prefs := expectTextType(t, conn, "prefs", 2*time.Second)
	if prefs.Bytes != 12345 {
		t.Fatalf("prefs.Bytes=%d want 12345", prefs.Bytes)
	}
	expectBinaryContaining(t, conn, "ready", 3*time.Second)

	if err := conn.WriteMessage(gws.TextMessage, mustJSON(Message{Type: "ping"})); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	expectTextType(t, conn, "pong", 2*time.Second)

	input := "hello-from-websocket"
	if err := conn.WriteMessage(gws.TextMessage, mustJSON(Message{Type: "input", Data: input})); err != nil {
		t.Fatalf("write input: %v", err)
	}
	if err := conn.WriteMessage(gws.TextMessage, mustJSON(Message{Type: "input", Data: "\n"})); err != nil {
		t.Fatalf("write newline: %v", err)
	}
	want := "ECHO:" + input
	got := expectBinaryContaining(t, conn, want, 4*time.Second)
	if !strings.Contains(got, want) {
		t.Fatalf("binary payload did not include %q; got %q", want, got)
	}
}

func TestWSReadOnlyBlocksInput(t *testing.T) {
	term, err := session.StartCommand("cat")
	if err != nil {
		t.Fatalf("start command: %v", err)
	}
	t.Cleanup(func() { _ = term.Close() })

	wsURL, shutdown := startWSTestServer(t, term, "token-ro", ServerOptions{
		ReadOnly:          true,
		MaxClients:        1,
		PingInterval:      time.Second,
		ClientReadTimeout: 3 * time.Second,
	})
	defer shutdown()

	conn := dialAndAuth(t, wsURL, "token-ro", nil)
	defer conn.Close()

	expectTextType(t, conn, "auth_ok", 2*time.Second)
	expectTextType(t, conn, "prefs", 2*time.Second)
	expectTextType(t, conn, "readonly", 2*time.Second)

	if err := conn.WriteMessage(gws.TextMessage, mustJSON(Message{Type: "input", Data: "blocked\n"})); err != nil {
		t.Fatalf("write readonly input: %v", err)
	}
	block := expectTextType(t, conn, "readonly", 2*time.Second)
	if !strings.Contains(strings.ToLower(block.Message), "input blocked") {
		t.Fatalf("unexpected readonly message: %q", block.Message)
	}
}

func TestWSInvalidAndExpiredTokenDenied(t *testing.T) {
	term, err := session.StartCommand("cat")
	if err != nil {
		t.Fatalf("start command: %v", err)
	}
	t.Cleanup(func() { _ = term.Close() })

	wsURL, shutdown := startWSTestServer(t, term, "token-good", ServerOptions{
		TokenExpiresAt:    time.Now().UTC().Add(-time.Second),
		PingInterval:      time.Second,
		ClientReadTimeout: 3 * time.Second,
	})
	defer shutdown()

	{
		conn := dialWS(t, wsURL, nil)
		defer conn.Close()
		if err := conn.WriteMessage(gws.TextMessage, mustJSON(Message{Type: "auth", Token: "token-good"})); err != nil {
			t.Fatalf("write auth(expired): %v", err)
		}
		msg := expectTextType(t, conn, "auth_error", 2*time.Second)
		if !strings.Contains(strings.ToLower(msg.Message), "expired") {
			t.Fatalf("expected token expired auth error, got %q", msg.Message)
		}
	}

	term2, err := session.StartCommand("cat")
	if err != nil {
		t.Fatalf("start command 2: %v", err)
	}
	t.Cleanup(func() { _ = term2.Close() })
	wsURL2, shutdown2 := startWSTestServer(t, term2, "token-good", ServerOptions{
		PingInterval:      time.Second,
		ClientReadTimeout: 3 * time.Second,
	})
	defer shutdown2()

	conn := dialWS(t, wsURL2, nil)
	defer conn.Close()
	if err := conn.WriteMessage(gws.TextMessage, mustJSON(Message{Type: "auth", Token: "token-bad"})); err != nil {
		t.Fatalf("write auth(invalid): %v", err)
	}
	msg := expectTextType(t, conn, "auth_error", 2*time.Second)
	if !strings.Contains(strings.ToLower(msg.Message), "invalid token") {
		t.Fatalf("expected invalid token auth error, got %q", msg.Message)
	}
}

func TestWSAuthRequiredAndMalformedMessageIgnored(t *testing.T) {
	term, err := session.StartCommand("cat")
	if err != nil {
		t.Fatalf("start command: %v", err)
	}
	t.Cleanup(func() { _ = term.Close() })

	wsURL, shutdown := startWSTestServer(t, term, "token-auth", ServerOptions{
		PingInterval:      time.Second,
		ClientReadTimeout: 3 * time.Second,
	})
	defer shutdown()

	conn := dialWS(t, wsURL, nil)
	defer conn.Close()
	if err := conn.WriteMessage(gws.TextMessage, mustJSON(Message{Type: "input", Data: "must-fail"})); err != nil {
		t.Fatalf("write pre-auth input: %v", err)
	}
	msg := expectTextType(t, conn, "auth_error", 2*time.Second)
	if !strings.Contains(strings.ToLower(msg.Message), "auth") {
		t.Fatalf("expected auth required error, got %q", msg.Message)
	}

	conn2 := dialAndAuth(t, wsURL, "token-auth", nil)
	defer conn2.Close()
	expectTextType(t, conn2, "auth_ok", 2*time.Second)
	expectTextType(t, conn2, "prefs", 2*time.Second)
	if err := conn2.WriteMessage(gws.TextMessage, []byte("{")); err != nil {
		t.Fatalf("write malformed json: %v", err)
	}
	if err := conn2.WriteMessage(gws.TextMessage, mustJSON(Message{Type: "ping"})); err != nil {
		t.Fatalf("write ping after malformed json: %v", err)
	}
	expectTextType(t, conn2, "pong", 2*time.Second)
}

func TestWSMaxClientsAndOriginChecks(t *testing.T) {
	term, err := session.StartCommand("cat")
	if err != nil {
		t.Fatalf("start command: %v", err)
	}
	t.Cleanup(func() { _ = term.Close() })

	wsURL, shutdown := startWSTestServer(t, term, "token-limit", ServerOptions{
		MaxClients:        1,
		PingInterval:      time.Second,
		ClientReadTimeout: 3 * time.Second,
	})
	defer shutdown()

	first := dialAndAuth(t, wsURL, "token-limit", nil)
	defer first.Close()
	expectTextType(t, first, "auth_ok", 2*time.Second)
	expectTextType(t, first, "prefs", 2*time.Second)

	second := dialWS(t, wsURL, nil)
	defer second.Close()
	if err := second.WriteMessage(gws.TextMessage, mustJSON(Message{Type: "auth", Token: "token-limit"})); err != nil {
		t.Fatalf("write auth second client: %v", err)
	}
	limit := expectTextType(t, second, "info", 2*time.Second)
	if !strings.Contains(strings.ToLower(limit.Message), "already connected") {
		t.Fatalf("expected client limit info, got %q", limit.Message)
	}

	_, resp, err := gws.DefaultDialer.Dial(wsURL, http.Header{"Origin": []string{"https://evil.example.com"}})
	if err == nil {
		t.Fatalf("expected cross-origin dial to fail")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		if resp != nil {
			t.Fatalf("expected status 403, got %d", resp.StatusCode)
		}
		t.Fatalf("expected HTTP response for rejected origin")
	}
}

func TestWSFlowPauseAndResume(t *testing.T) {
	term, err := session.StartCommand("yes x | head -c 250000")
	if err != nil {
		t.Fatalf("start output command: %v", err)
	}
	t.Cleanup(func() { _ = term.Close() })

	wsURL, shutdown := startWSTestServer(t, term, "token-flow", ServerOptions{
		ReadOnly:           true,
		MaxClients:         1,
		LowWatermarkBytes:  1024,
		HighWatermarkBytes: 2048,
		PingInterval:       time.Second,
		ClientReadTimeout:  3 * time.Second,
	})
	defer shutdown()

	conn := dialAndAuth(t, wsURL, "token-flow", nil)
	defer conn.Close()
	expectTextType(t, conn, "auth_ok", 2*time.Second)
	expectTextType(t, conn, "prefs", 2*time.Second)
	expectTextType(t, conn, "readonly", 2*time.Second)

	pause := expectTextType(t, conn, "flow_pause", 5*time.Second)
	if !strings.Contains(strings.ToLower(pause.Message), "pausing output") {
		t.Fatalf("unexpected flow_pause message: %q", pause.Message)
	}
	if err := conn.WriteMessage(gws.TextMessage, mustJSON(Message{Type: "ack", Bytes: 1_000_000})); err != nil {
		t.Fatalf("write ack: %v", err)
	}
	expectTextType(t, conn, "flow_resume", 2*time.Second)
}

func startWSTestServer(t *testing.T, term *session.Terminal, token string, opts ServerOptions) (string, func()) {
	t.Helper()
	server := New(term, token, opts)
	server.Start()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", server.HandleWS)
	ts := httptest.NewServer(mux)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	return wsURL, func() {
		server.Close()
		ts.Close()
	}
}

func dialWS(t *testing.T, wsURL string, header http.Header) *gws.Conn {
	t.Helper()
	conn, resp, err := gws.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		if resp != nil {
			t.Fatalf("dial websocket failed: %v (status=%d)", err, resp.StatusCode)
		}
		t.Fatalf("dial websocket failed: %v", err)
	}
	return conn
}

func dialAndAuth(t *testing.T, wsURL, token string, header http.Header) *gws.Conn {
	t.Helper()
	conn := dialWS(t, wsURL, header)
	authPayload := Message{
		Type:    "auth",
		Token:   token,
		Columns: 80,
		Rows:    24,
	}
	if err := conn.WriteMessage(gws.TextMessage, mustJSON(authPayload)); err != nil {
		_ = conn.Close()
		t.Fatalf("write auth: %v", err)
	}
	return conn
}

func expectTextType(t *testing.T, conn *gws.Conn, target string, timeout time.Duration) Message {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	for {
		mt, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read text(%s): %v", target, err)
		}
		if mt != gws.TextMessage {
			continue
		}
		var msg Message
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}
		if msg.Type == target {
			return msg
		}
	}
}

func expectBinaryContaining(t *testing.T, conn *gws.Conn, fragment string, timeout time.Duration) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	for {
		mt, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read binary: %v", err)
		}
		if mt != gws.BinaryMessage {
			continue
		}
		text := string(payload)
		if strings.Contains(text, fragment) {
			return text
		}
	}
}
