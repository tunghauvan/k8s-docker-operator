package tunnel

import (
	"io"
	"net"
	"time"

	"github.com/gorilla/websocket"
)

// WebSocketConn adapts a websocket.Conn to net.Conn interface
type WebSocketConn struct {
	ws     *websocket.Conn
	reader io.Reader
}

func NewWebSocketConn(ws *websocket.Conn) *WebSocketConn {
	return &WebSocketConn{
		ws: ws,
	}
}

func (c *WebSocketConn) Read(b []byte) (n int, err error) {
	if c.reader == nil {
		_, r, err := c.ws.NextReader()
		if err != nil {
			return 0, err
		}
		c.reader = r
	}
	n, err = c.reader.Read(b)
	if err == io.EOF {
		c.reader = nil
		// Recursive call to read next message if current reader is done
		// This handles the message boundary of websockets transparently
		return c.Read(b)
	}
	return n, err
}

func (c *WebSocketConn) Write(b []byte) (n int, err error) {
	if err := c.ws.WriteMessage(websocket.BinaryMessage, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *WebSocketConn) Close() error {
	return c.ws.Close()
}

func (c *WebSocketConn) LocalAddr() net.Addr {
	return c.ws.LocalAddr()
}

func (c *WebSocketConn) RemoteAddr() net.Addr {
	return c.ws.RemoteAddr()
}

func (c *WebSocketConn) SetDeadline(t time.Time) error {
	if err := c.ws.SetReadDeadline(t); err != nil {
		return err
	}
	return c.ws.SetWriteDeadline(t)
}

func (c *WebSocketConn) SetReadDeadline(t time.Time) error {
	return c.ws.SetReadDeadline(t)
}

func (c *WebSocketConn) SetWriteDeadline(t time.Time) error {
	return c.ws.SetWriteDeadline(t)
}
