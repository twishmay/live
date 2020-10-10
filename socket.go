package live

import (
	"context"
	"fmt"

	"golang.org/x/net/html"
	"nhooyr.io/websocket"
)

const (
	MaxMessageBufferSize = 16
)

// Socket describes a socket from the outside.
type Socket struct {
	Session       Session
	currentRender *html.Node
	Data          interface{}

	msgs      chan SocketMessage
	closeSlow func()
}

// HandleView takes a view and runs a mount and render.
func (s *Socket) HandleView(ctx context.Context, view *View, params map[string]string) error {
	// Mount view.
	if err := view.Mount(ctx, params, s, false); err != nil {
		return fmt.Errorf("mount error: %w", err)
	}

	// Render view.
	output, err := view.Render(ctx, view.t, s)
	if err != nil {
		return fmt.Errorf("render error: %w", err)
	}
	node, err := html.Parse(output)
	if err != nil {
		return fmt.Errorf("html parse error: %w", err)
	}

	// Get diff
	if s.currentRender != nil {
		patches := DiffTrees(s.currentRender, node)
		for _, p := range patches {
			msg := SocketMessage{
				T:    EventPatch,
				Data: p,
			}
			s.msgs <- msg
		}
	}
	s.currentRender = node

	return nil
}

// SocketMessage messages that are sent and received by the
// socket.
type SocketMessage struct {
	T    Event       `json:"t"`
	Data interface{} `json:"d"`
}

// NewSocket creates a new socket.
func NewSocket(s Session) *Socket {
	return &Socket{
		Session: s,
		msgs:    make(chan SocketMessage, MaxMessageBufferSize),
	}
}

// AssignSocket to a socket.
func (c *Socket) AssignWS(ws *websocket.Conn) {
	c.closeSlow = func() {
		ws.Close(websocket.StatusPolicyViolation, "socket too slow to keep up with messages")
	}
}