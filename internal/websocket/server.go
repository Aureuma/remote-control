package websocket

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
		pingInterval:        pingInterval,
		clientReadTimeout:   clientReadTimeout,
		upgrader: gws.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			CheckOrigin: func(r *http.Request) bool {
				origin := strings.TrimSpace(r.Header.Get("Origin"))
				if origin == "" {
					return true
				}
				return strings.Contains(origin, "localhost") || strings.Contains(origin, "127.0.0.1")
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
		_ = conn.WriteControl(gws.CloseMessage, gws.FormatCloseMessage(gws.CloseNormalClosure, "session closed"), time.Now().Add(2*time.Second))
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
		_ = conn.WriteMessage(gws.TextMessage, mustJSON(Message{Type: "auth_error", Message: err.Error()}))
		_ = conn.WriteControl(gws.CloseMessage, gws.FormatCloseMessage(gws.ClosePolicyViolation, "auth failed"), time.Now().Add(2*time.Second))
		_ = conn.Close()
		return
	}
	if err := s.addConn(conn); err != nil {
		_ = conn.WriteMessage(gws.TextMessage, mustJSON(Message{Type: "info", Message: err.Error()}))
		_ = conn.WriteControl(gws.CloseMessage, gws.FormatCloseMessage(gws.CloseTryAgainLater, "client limit reached"), time.Now().Add(2*time.Second))
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

	_ = conn.WriteMessage(gws.TextMessage, mustJSON(Message{Type: "auth_ok"}))
	if s.readonly {
		_ = conn.WriteMessage(gws.TextMessage, mustJSON(Message{Type: "readonly", Message: "🔒 Read-only mode enabled"}))
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
			s.writeMu.Lock()
			err := conn.WriteControl(gws.PingMessage, []byte("ping"), time.Now().Add(3*time.Second))
			s.writeMu.Unlock()
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
			_ = conn.WriteMessage(gws.TextMessage, mustJSON(Message{Type: "readonly", Message: "🔒 Input blocked: read-only session"}))
			return
		}
		_ = s.terminal.WriteInput([]byte(msg.Data))
	case "resize":
		_ = s.terminal.Resize(msg.Columns, msg.Rows)
	case "ping":
		_ = conn.WriteMessage(gws.TextMessage, mustJSON(Message{Type: "pong"}))
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
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := conn.WriteMessage(gws.TextMessage, payload); err != nil {
			_ = conn.Close()
		}
	}
}

func mustJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"type":"info","message":"serialization error"}`)
	}
	return data
}
