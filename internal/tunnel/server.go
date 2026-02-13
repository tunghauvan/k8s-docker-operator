package tunnel

import (
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

// Server is the tunnel server running in K8s
type Server struct {
	listenAddr string // TCP Address for K8s Service (e.g. :8080)
	wsAddr     string // WebSocket Address for Client (e.g. :8081)
	authToken  string // Shared secret for authenticating tunnel clients
	sessions   []*yamux.Session
	rrIndex    uint64
	mu         sync.Mutex
}

func NewServer(listenAddr, wsAddr, authToken string) *Server {
	return &Server{
		listenAddr: listenAddr,
		wsAddr:     wsAddr,
		authToken:  authToken,
		sessions:   make([]*yamux.Session, 0),
	}
}

func (s *Server) Start() error {
	// Start TCP Listener (Service Port)
	ln, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return err
	}
	log.Printf("Listening for TCP traffic on %s", s.listenAddr)
	go s.acceptTCP(ln)

	// Start WebSocket Server (Tunnel Protocol)
	http.HandleFunc("/ws", s.handleWS)
	log.Printf("Listening for Tunnel Clients on %s", s.wsAddr)
	if s.authToken != "" {
		log.Printf("Tunnel authentication enabled")
	} else {
		log.Printf("WARNING: Tunnel authentication disabled (no auth token set)")
	}
	return http.ListenAndServe(s.wsAddr, nil)
}

func (s *Server) acceptTCP(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("Accept TCP error: %v", err)
			continue
		}
		go s.handleTCP(conn)
	}
}

func (s *Server) handleTCP(conn net.Conn) {
	defer conn.Close()
	log.Printf("handleTCP: Accepted connection from %s", conn.RemoteAddr())

	s.mu.Lock()
	if len(s.sessions) == 0 {
		s.mu.Unlock()
		log.Println("handleTCP: No tunnel client connected, dropping connection")
		return
	}

	// Round Robin Selection
	idx := s.rrIndex % uint64(len(s.sessions))
	sess := s.sessions[idx]
	s.rrIndex++
	s.mu.Unlock()

	log.Printf("handleTCP: Selected session %d", idx)

	// Open a stream to the client
	// This tells the client "I have a connection, carry it for me"
	stream, err := sess.Open()
	if err != nil {
		log.Printf("handleTCP: Failed to open stream: %v", err)
		return
	}
	defer stream.Close()
	log.Printf("handleTCP: Stream opened successfully")

	// Pipe bidirectional
	errChan := make(chan error, 2)
	go func() {
		_, err := io.Copy(stream, conn)
		errChan <- err
	}()
	go func() {
		_, err := io.Copy(conn, stream)
		errChan <- err
	}()

	err = <-errChan
	log.Printf("handleTCP: Connection closed: %v", err)
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	// Validate auth token if configured
	if s.authToken != "" {
		token := r.URL.Query().Get("token")
		if token != s.authToken {
			log.Printf("Tunnel auth failed from %s: invalid token", r.RemoteAddr)
			http.Error(w, "Forbidden: invalid tunnel auth token", http.StatusForbidden)
			return
		}
		log.Printf("Tunnel auth succeeded from %s", r.RemoteAddr)
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WS Upgrade error: %v", err)
		return
	}

	log.Printf("Tunnel Client connected from %s", ws.RemoteAddr())

	conn := NewWebSocketConn(ws)

	// We use yamux.Server here. The Client will use yamux.Client.
	session, err := yamux.Server(conn, nil)
	if err != nil {
		log.Printf("Yamux Server error: %v", err)
		conn.Close()
		return
	}

	s.mu.Lock()
	s.sessions = append(s.sessions, session)
	currentCount := len(s.sessions)
	s.mu.Unlock()

	log.Printf("Registered new session. Total active sessions: %d", currentCount)

	// Clean up on disconnect
	defer func() {
		s.mu.Lock()
		for i, sess := range s.sessions {
			if sess == session {
				// efficient removal
				s.sessions = append(s.sessions[:i], s.sessions[i+1:]...)
				break
			}
		}
		remaining := len(s.sessions)
		s.mu.Unlock()
		session.Close()
		log.Printf("Tunnel Client disconnected. Remaining sessions: %d", remaining)
	}()

	// Block until session is closed (client disconnects or error)
	// Yamux session doesn't have a blocking "Wait" method exposed directly that detects closure easily
	// without opening streams, but we can rely on the underlying connection ping/pong or keepalive.
	// Actually, typically we just wait. If current implementation used a loop sleeping, we can keep that
	// or use session.CloseChan() if available in newer yamux?
	// Old code:
	// for !session.IsClosed() { time.Sleep(time.Second) }

	// Better: Use a keepalive mechanism or just read from the underlying conn if exposed?
	// Since we wrapped it in NewWebSocketConn, the yamux server loop runs in background.
	// We need to block this handler so the WS connection stays open.

	// Let's stick to the loop for now as it matches previous logic, just ensure it works.
	for !session.IsClosed() {
		time.Sleep(1 * time.Second)
	}
}
