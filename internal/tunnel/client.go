package tunnel

import (
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

// Client is the tunnel client running on Docker Host
type Client struct {
	serverAddr string // WebSocket URL (e.g. ws://tunnel-server/ws)
	targetAddr string // Target Container Address (e.g. localhost:6379)
	token      string // Authentication Token
}

func NewClient(serverAddr, targetAddr, token string) *Client {
	return &Client{
		serverAddr: serverAddr,
		targetAddr: targetAddr,
		token:      token,
	}
}

func (c *Client) Start() error {
	for {
		err := c.connectAndServe()
		if err != nil {
			log.Printf("Tunnel Client disconnected: %v. Retrying in 5s...", err)
			time.Sleep(5 * time.Second)
		}
	}
}

func (c *Client) connectAndServe() error {
	log.Printf("Connecting to Tunnel Server: %s", c.serverAddr)
	header := http.Header{}
	if c.token != "" {
		header.Set("X-Tunnel-Token", c.token)
	}
	ws, _, err := websocket.DefaultDialer.Dial(c.serverAddr, header)
	if err != nil {
		return err
	}
	defer ws.Close()

	conn := NewWebSocketConn(ws)

	// We use yamux.Client here. The Server uses yamux.Server.
	session, err := yamux.Client(conn, nil)
	if err != nil {
		return err
	}
	defer session.Close()

	log.Printf("Connected to Tunnel Server. Waiting for connections...")

	// Authenticate? FUTURE WORK.

	// Accept streams from Server
	for {
		stream, err := session.Accept()
		if err != nil {
			return err
		}
		go c.handleStream(stream)
	}
}

func (c *Client) handleStream(stream net.Conn) {
	defer stream.Close()

	// Dial Target
	target, err := net.Dial("tcp", c.targetAddr)
	if err != nil {
		log.Printf("Failed to dial target %s: %v", c.targetAddr, err)
		return
	}
	defer target.Close()

	// Pipe
	errChan := make(chan error, 2)
	go func() {
		_, err := io.Copy(stream, target)
		errChan <- err
	}()
	go func() {
		_, err := io.Copy(target, stream)
		errChan <- err
	}()

	<-errChan
}
