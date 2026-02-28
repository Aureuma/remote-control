package websocket

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/si/remote-control/internal/session"
)

type Message struct {
	Type    string `json:"type"`
	Token   string `json:"token,omitempty"`
	Data    string `json:"data,omitempty"`
	Columns int    `json:"columns,omitempty"`
	Rows    int    `json:"rows,omitempty"`
	Bytes   int64  `json:"bytes,omitempty"`
	Message string `json:"message,omitempty"`
}

type ServerOptions struct {
	ReadOnly            bool
	MaxClients          int
	LowWatermarkBytes   int64
	HighWatermarkBytes  int64
	AckQuantumBytes     int64
	TokenExpiresAt      time.Time
	PingInterval        time.Duration
	ClientReadTimeout   time.Duration
	OnClientCountChange func(int)
}

type Server struct {
	terminal  *session.Terminal
	token     string
	readonly  bool
	maxClient int

	onClientCountChange func(int)

	upgrader gws.Upgrader

	ctx    context.Context
	cancel context.CancelFunc

	writeMu sync.Mutex
	connMu  sync.Mutex
	conns   map[*gws.Conn]struct{}

	flowMu sync.Mutex
	flow   flowController

	tokenExpiresAt    time.Time
	ackQuantumBytes   int64
	pingInterval      time.Duration
	clientReadTimeout time.Duration
}

func New(terminal *session.Terminal, token string, opts ServerOptions) *Server {
	maxClients := opts.MaxClients
	if maxClients <= 0 {
		maxClients = 1
	}
	pingInterval := opts.PingInterval
	if pingInterval <= 0 {
		pingInterval = 25 * time.Second
	}
	clientReadTimeout := opts.ClientReadTimeout
	if clientReadTimeout <= 0 {
		clientReadTimeout = 90 * time.Second
	}
	ackQuantumBytes := opts.AckQuantumBytes
	if ackQuantumBytes <= 0 {
		ackQuantumBytes = 256 * 1024
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		terminal:            terminal,
		token:               token,
		readonly:            opts.ReadOnly,
		maxClient:           maxClients,
		onClientCountChange: opts.OnClientCountChange,
		ctx:                 ctx,
		cancel:              cancel,
		conns:               map[*gws.Conn]struct{}{},
		flow:                newFlowController(opts.LowWatermarkBytes, opts.HighWatermarkBytes),
		tokenExpiresAt:      opts.TokenExpiresAt.UTC(),
		ackQuantumBytes:     ackQuantumBytes,
		pingInterval:        pingInterval,
		clientReadTimeout:   clientReadTimeout,
		upgrader: gws.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			CheckOrigin: func(r *http.Request) bool {
				return isOriginAllowed(strings.TrimSpace(r.Header.Get("Origin")), strings.TrimSpace(r.Host))
			},
		},
	}
}

func (s *Server) Start() {
	go s.readTerminalLoop()
}

func (s *Server) Close() {
	s.cancel()
	s.connMu.Lock()
	for conn := range s.conns {
		_ = s.writeControl(conn, gws.CloseMessage, gws.FormatCloseMessage(gws.CloseNormalClosure, "session closed"), 2*time.Second)
		_ = conn.Close()
		delete(s.conns, conn)
	}
	if s.onClientCountChange != nil {
		s.onClientCountChange(0)
	}
	s.connMu.Unlock()
	s.resetFlow()
}

func (s *Server) HandleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	if err := s.authenticate(conn); err != nil {
		_ = s.writeMessage(conn, gws.TextMessage, mustJSON(Message{Type: "auth_error", Message: err.Error()}), 3*time.Second)
		_ = s.writeControl(conn, gws.CloseMessage, gws.FormatCloseMessage(gws.ClosePolicyViolation, "auth failed"), 2*time.Second)
		_ = conn.Close()
		return
	}
	if err := s.addConn(conn); err != nil {
		_ = s.writeMessage(conn, gws.TextMessage, mustJSON(Message{Type: "info", Message: err.Error()}), 3*time.Second)
		_ = s.writeControl(conn, gws.CloseMessage, gws.FormatCloseMessage(gws.CloseTryAgainLater, "client limit reached"), 2*time.Second)
		_ = conn.Close()
		return
	}
	defer s.removeConn(conn)

	_ = conn.SetReadDeadline(time.Now().Add(s.clientReadTimeout))
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(s.clientReadTimeout))
		return nil
	})

	done := make(chan struct{})
	defer close(done)
	go s.pingLoop(conn, done)

	_ = s.writeMessage(conn, gws.TextMessage, mustJSON(Message{Type: "auth_ok"}), 3*time.Second)
	_ = s.writeMessage(conn, gws.TextMessage, mustJSON(Message{Type: "prefs", Bytes: s.ackQuantumBytes}), 3*time.Second)
	if s.readonly {
		_ = s.writeMessage(conn, gws.TextMessage, mustJSON(Message{Type: "readonly", Message: "🔒 Read-only mode enabled"}), 3*time.Second)
	}

	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}
		msgType, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if msgType != gws.TextMessage {
			continue
		}
		var msg Message
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}
		s.handleClientMessage(conn, msg)
	}
}

func (s *Server) pingLoop(conn *gws.Conn, done <-chan struct{}) {
	ticker := time.NewTicker(s.pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			err := s.writeControl(conn, gws.PingMessage, []byte("ping"), 3*time.Second)
			if err != nil {
				return
			}
		}
	}
}

func (s *Server) authenticate(conn *gws.Conn) error {
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	msgType, payload, err := conn.ReadMessage()
	if err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Time{})
	if msgType != gws.TextMessage {
		return errors.New("expected auth message")
	}
	var msg Message
	if err := json.Unmarshal(payload, &msg); err != nil {
		return errors.New("invalid auth payload")
	}
	if msg.Type != "auth" {
		return errors.New("auth required")
	}
	if tokenExpired(s.tokenExpiresAt, time.Now().UTC()) {
		return errors.New("token expired")
	}
	if subtle.ConstantTimeCompare([]byte(msg.Token), []byte(s.token)) != 1 {
		return errors.New("invalid token")
	}
	if msg.Columns > 0 && msg.Rows > 0 {
		_ = s.terminal.Resize(msg.Columns, msg.Rows)
	}
	return nil
}

func (s *Server) addConn(conn *gws.Conn) error {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if len(s.conns) >= s.maxClient {
		return fmt.Errorf("another client is already connected")
	}
	s.conns[conn] = struct{}{}
	if s.onClientCountChange != nil {
		s.onClientCountChange(len(s.conns))
	}
	return nil
}

func (s *Server) removeConn(conn *gws.Conn) {
	s.connMu.Lock()
	delete(s.conns, conn)
	connCount := len(s.conns)
	if s.onClientCountChange != nil {
		s.onClientCountChange(connCount)
	}
	s.connMu.Unlock()
	_ = conn.Close()
	if connCount == 0 {
		s.resetFlow()
	}
}

func (s *Server) handleClientMessage(conn *gws.Conn, msg Message) {
	switch strings.ToLower(strings.TrimSpace(msg.Type)) {
	case "input":
		if s.readonly {
			_ = s.writeMessage(conn, gws.TextMessage, mustJSON(Message{Type: "readonly", Message: "🔒 Input blocked: read-only session"}), 3*time.Second)
			return
		}
		_ = s.terminal.WriteInput([]byte(msg.Data))
	case "resize":
		_ = s.terminal.Resize(msg.Columns, msg.Rows)
	case "ping":
		_ = s.writeMessage(conn, gws.TextMessage, mustJSON(Message{Type: "pong"}), 3*time.Second)
	case "ack":
		event := s.onBytesAcked(msg.Bytes)
		if event == flowEventResume {
			s.broadcastText(Message{Type: "flow_resume", Message: "⚡ Output resumed"})
		}
	case "pong":
		// no-op; browser may send app-level pong in addition to ws control pong.
	}
}

func (s *Server) readTerminalLoop() {
	buf := make([]byte, 4096)
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}
		if !s.waitForFlowResume() {
			return
		}
		n, err := s.terminal.Read(buf)
		if n > 0 {
			s.broadcastBinary(buf[:n])
		}
		if err != nil {
			s.broadcastText(Message{Type: "info", Message: "ℹ️ Session ended"})
			return
		}
	}
}

func (s *Server) waitForFlowResume() bool {
	for {
		if !s.isFlowPaused() {
			return true
		}
		select {
		case <-s.ctx.Done():
			return false
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (s *Server) isFlowPaused() bool {
	s.flowMu.Lock()
	defer s.flowMu.Unlock()
	return s.flow.paused
}

func (s *Server) onBytesSent(n int) flowEvent {
	s.flowMu.Lock()
	defer s.flowMu.Unlock()
	return s.flow.onSent(n)
}

func (s *Server) onBytesAcked(n int64) flowEvent {
	s.flowMu.Lock()
	defer s.flowMu.Unlock()
	return s.flow.onAck(n)
}

func (s *Server) resetFlow() {
	s.flowMu.Lock()
	s.flow.reset()
	s.flowMu.Unlock()
}

func (s *Server) broadcastBinary(data []byte) {
	s.connMu.Lock()
	conns := make([]*gws.Conn, 0, len(s.conns))
	for conn := range s.conns {
		conns = append(conns, conn)
	}
	s.connMu.Unlock()
	if len(conns) == 0 {
		return
	}
	s.writeMu.Lock()
	sentBytes := 0
	for _, conn := range conns {
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := conn.WriteMessage(gws.BinaryMessage, data); err != nil {
			_ = conn.Close()
			continue
		}
		if len(data) > sentBytes {
			sentBytes = len(data)
		}
	}
	s.writeMu.Unlock()
	if sentBytes > 0 {
		event := s.onBytesSent(sentBytes)
		if event == flowEventPause {
			s.broadcastText(Message{Type: "flow_pause", Message: "⏸️ Network is slow; pausing output to protect session"})
		}
	}
}

func (s *Server) broadcastText(msg Message) {
	payload := mustJSON(msg)
	s.connMu.Lock()
	conns := make([]*gws.Conn, 0, len(s.conns))
	for conn := range s.conns {
		conns = append(conns, conn)
	}
	s.connMu.Unlock()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	for _, conn := range conns {
		if err := s.writeMessageNoLock(conn, gws.TextMessage, payload, 5*time.Second); err != nil {
			_ = conn.Close()
		}
	}
}

func (s *Server) writeControl(conn *gws.Conn, messageType int, data []byte, timeout time.Duration) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return conn.WriteControl(messageType, data, time.Now().Add(timeout))
}

func (s *Server) writeMessage(conn *gws.Conn, messageType int, payload []byte, timeout time.Duration) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.writeMessageNoLock(conn, messageType, payload, timeout)
}

func (s *Server) writeMessageNoLock(conn *gws.Conn, messageType int, payload []byte, timeout time.Duration) error {
	_ = conn.SetWriteDeadline(time.Now().Add(timeout))
	return conn.WriteMessage(messageType, payload)
}

func mustJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"type":"info","message":"serialization error"}`)
	}
	return data
}

func tokenExpired(expiresAt, now time.Time) bool {
	if expiresAt.IsZero() {
		return false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return !now.Before(expiresAt)
}

func isOriginAllowed(origin, host string) bool {
	if strings.TrimSpace(origin) == "" {
		return true
	}
	originURL, err := url.Parse(origin)
	if err != nil {
		return false
	}
	originHost := strings.ToLower(strings.TrimSpace(originURL.Hostname()))
	if originHost == "" {
		return false
	}
	requestHost := strings.ToLower(strings.TrimSpace(parseHostname(host)))
	if requestHost != "" && originHost == requestHost {
		return true
	}
	return originHost == "localhost" || originHost == "127.0.0.1" || originHost == "::1"
}

func parseHostname(host string) string {
	if strings.TrimSpace(host) == "" {
		return ""
	}
	u, err := url.Parse("//" + host)
	if err != nil {
		return strings.TrimSpace(host)
	}
	return strings.TrimSpace(u.Hostname())
}
