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
	token      string // Authentication Token
}

func NewClient(serverAddr, token string) *Client {
	return &Client{
		serverAddr: serverAddr,
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

	// Custom Yamux config for KeepAlive
	yamuxConfig := yamux.DefaultConfig()
	yamuxConfig.EnableKeepAlive = true
	yamuxConfig.KeepAliveInterval = 10 * time.Second

	// We use yamux.Client here. The Server uses yamux.Server.
	session, err := yamux.Client(conn, yamuxConfig)
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

	// Read Target Address from Stream Header (First line ending with \n)
	// We need to buffer the read to avoid consuming too much
	// Implementation simplified: read byte by byte until \n
	targetAddr := ""
	buf := make([]byte, 1)
	for {
		_, err := stream.Read(buf)
		if err != nil {
			log.Printf("Failed to read header: %v", err)
			return
		}
		if buf[0] == '\n' {
			break
		}
		targetAddr += string(buf[0])
		if len(targetAddr) > 256 {
			log.Printf("Header too long")
			return
		}
	}

	log.Printf("Tunnel request for target: %s", targetAddr)

	// Dial Target
	target, err := net.Dial("tcp", targetAddr)
	if err != nil {
		log.Printf("Failed to dial target %s: %v", targetAddr, err)
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
