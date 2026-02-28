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
	Message string `json:"message,omitempty"`
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
}

func New(terminal *session.Terminal, token string, readonly bool, maxClient int, onClientCountChange func(int)) *Server {
	if maxClient <= 0 {
		maxClient = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		terminal:            terminal,
		token:               token,
		readonly:            readonly,
		maxClient:           maxClient,
		onClientCountChange: onClientCountChange,
		ctx:                 ctx,
		cancel:              cancel,
		conns:               map[*gws.Conn]struct{}{},
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
	if s.onClientCountChange != nil {
		s.onClientCountChange(len(s.conns))
	}
	s.connMu.Unlock()
	_ = conn.Close()
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
	defer s.writeMu.Unlock()
	for _, conn := range conns {
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := conn.WriteMessage(gws.BinaryMessage, data); err != nil {
			_ = conn.Close()
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
