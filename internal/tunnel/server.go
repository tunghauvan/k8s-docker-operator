package tunnel

import (
	"io"
	"log"
	"net"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

// Server is the tunnel server running in K8s
type Server struct {
	listenAddr string // TCP Address for K8s Service (e.g. :8080)
	wsAddr     string // WebSocket Address for Client (e.g. :8081)
	session    *yamux.Session
	mu         sync.Mutex
}

func NewServer(listenAddr, wsAddr string) *Server {
	return &Server{
		listenAddr: listenAddr,
		wsAddr:     wsAddr,
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
	s.mu.Lock()
	sess := s.session
	s.mu.Unlock()

	if sess == nil {
		log.Println("No tunnel client connected, dropping connection")
		return
	}

	// Open a stream to the client
	// This tells the client "I have a connection, carry it for me"
	stream, err := sess.Open()
	if err != nil {
		log.Printf("Failed to open stream: %v", err)
		return
	}
	defer stream.Close()

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

	<-errChan // Wait for one side to close
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
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
	if s.session != nil {
		s.session.Close()
	}
	s.session = session
	s.mu.Unlock()

	// Wait for session to close
	// Yamux doesn't export CloseChan directly in older versions, but we can use a KeepAlive or just block?
	// Actually, if we return, the underlying http handler finishes.
	// Since we hijacked, it's fine, but let's block to be safe and clean.
	// In newer yamux, there isn't a Wait.
	// Let's just block forever or on a context.
	select {}
}
