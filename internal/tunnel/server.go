package tunnel

import (
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

// Server is the tunnel server running in K8s
type Server struct {
	listenAddr  string // TCP Address for K8s Service (e.g. :8080)
	wsAddr      string // WebSocket Address for Client (e.g. :8081)
	authToken   string // Shared secret for authenticating tunnel clients
	sessions    []*yamux.Session
	targets     []string
	targetsFile string
	rrIndex     uint64
	mu          sync.Mutex
}

func NewServer(listenAddr, wsAddr, authToken string, targets []string) *Server {
	return &Server{
		listenAddr: listenAddr,
		wsAddr:     wsAddr,
		authToken:  authToken,
		targets:    targets,
		sessions:   make([]*yamux.Session, 0),
	}
}

func (s *Server) SetTargetsFile(path string) {
	s.mu.Lock()
	s.targetsFile = path
	s.mu.Unlock()
}

func (s *Server) Start() error {
	// Start TCP Listener (Service Port)
	ln, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return err
	}
	log.Printf("Listening for TCP traffic on %s", s.listenAddr)
	go s.acceptTCP(ln)

	// Start Dynamic Targets Watcher if configured
	if s.targetsFile != "" {
		go s.watchTargetsFile()
	}

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
	defer func() {
		if r := recover(); r != nil {
			log.Printf("handleTCP PANIC: %v", r)
		}
	}()
	defer conn.Close()
	log.Printf("handleTCP: Accepted connection from %s", conn.RemoteAddr())

	s.mu.Lock()
	if len(s.sessions) == 0 {
		s.mu.Unlock()
		log.Println("handleTCP: No tunnel client connected, dropping connection")
		return
	}

	if len(s.targets) == 0 {
		s.mu.Unlock()
		log.Println("handleTCP: No targets configured, dropping connection")
		return
	}

	// RR Target
	targetIdx := s.rrIndex % uint64(len(s.targets))
	target := s.targets[targetIdx]

	// RR Session (if multiple clients connected via LoadBalancer redundancy)
	sessionIdx := s.rrIndex % uint64(len(s.sessions))
	sess := s.sessions[sessionIdx]

	s.rrIndex++
	s.mu.Unlock()

	log.Printf("handleTCP: Selected target %s via session %d", target, sessionIdx)

	// Open a stream to the client
	stream, err := sess.Open()
	if err != nil {
		log.Printf("handleTCP: Failed to open stream: %v", err)
		return
	}
	defer stream.Close()

	// Write Target Header
	// Format: "IP:Port\n"
	_, err = stream.Write([]byte(target + "\n"))
	if err != nil {
		log.Printf("handleTCP: Failed to write target header: %v", err)
		return
	}

	// Pipe bidirectional
	errChan := make(chan error, 2)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("handleTCP copy1 PANIC: %v", r)
			}
		}()
		_, err := io.Copy(stream, conn)
		errChan <- err
	}()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("handleTCP copy2 PANIC: %v", r)
			}
		}()
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
	defer func() {
		if r := recover(); r != nil {
			log.Printf("handleWS PANIC: %v", r)
		}
	}()
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

	// Custom Yamux config for KeepAlive
	yamuxConfig := yamux.DefaultConfig()
	yamuxConfig.EnableKeepAlive = true
	yamuxConfig.KeepAliveInterval = 10 * time.Second

	// We use yamux.Server here. The Client will use yamux.Client.
	session, err := yamux.Server(conn, yamuxConfig)
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

func (s *Server) watchTargetsFile() {
	log.Printf("Watching targets file: %s", s.targetsFile)
	// Initial load
	s.loadTargetsFromFile()

	// Simple polling watcher to avoid cgo/fsnotify complexities in a small binary
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		s.loadTargetsFromFile()
	}
}

func (s *Server) loadTargetsFromFile() {
	data, err := os.ReadFile(s.targetsFile)
	if err != nil {
		log.Printf("Error reading targets file %s: %v", s.targetsFile, err)
		return
	}

	content := strings.TrimSpace(string(data))
	var newTargets []string
	if content != "" {
		rawList := strings.Split(content, ",")
		for _, t := range rawList {
			if tVal := strings.TrimSpace(t); tVal != "" {
				newTargets = append(newTargets, tVal)
			}
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Compare to avoid logging if no change
	if len(newTargets) == len(s.targets) {
		match := true
		for i := range newTargets {
			if newTargets[i] != s.targets[i] {
				match = false
				break
			}
		}
		if match {
			return
		}
	}

	log.Printf("Dynamic targets updated from file: %v", newTargets)
	s.targets = newTargets
}
