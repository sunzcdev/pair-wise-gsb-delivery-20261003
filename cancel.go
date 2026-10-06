package rpc2

import (
	"context"
	"errors"
	"sync/atomic"
)

// controlMethod is a reserved method name for protocol control frames.
// Control frames are notifications (Seq == 0) carrying a controlMessage
// body. They are intercepted by the read loop and are never dispatched
// to user handlers. Users cannot register or call this method name.
const controlMethod = "rpc2.control"

// Control frame types.
const (
	// controlHello announces support for the cancellation extension.
	// Each endpoint sends it once when its read loop starts.
	controlHello = "hello"
	// controlCancel asks the peer to cancel the in-flight request
	// identified by Seq (in the sender's sequence space).
	controlCancel = "cancel"
)

// controlMessage is the body of a control frame.
type controlMessage struct {
	Type string
	Seq  uint64
}

// errReservedMethod is returned when the reserved control method name
// is used as a regular method.
var errReservedMethod = errors.New("rpc2: " + controlMethod + " is a reserved method name")

// peerSupportsCancel reports whether the peer has announced support for
// the cancellation extension.
func (c *Client) peerSupportsCancel() bool {
	return atomic.LoadInt32(&c.peerCancel) == 1
}

// sendControl writes a control frame. It is safe for concurrent use.
func (c *Client) sendControl(msg controlMessage) error {
	c.sending.Lock()
	defer c.sending.Unlock()

	c.mutex.Lock()
	unavailable := c.shutdown || c.closing
	c.mutex.Unlock()
	if unavailable {
		return ErrShutdown
	}

	c.request.Seq = 0
	c.request.Method = controlMethod
	return c.codec.WriteRequest(&c.request, msg)
}

// readControl reads the body of a control frame and processes it.
func (c *Client) readControl() error {
	var msg controlMessage
	if err := c.codec.ReadRequestBody(&msg); err != nil {
		return err
	}
	switch msg.Type {
	case controlHello:
		atomic.StoreInt32(&c.peerCancel, 1)
	case controlCancel:
		// Cancel the in-flight handler serving the peer's request.
		// If the sequence number is unknown (e.g. the reply was
		// already sent), the cancel is ignored.
		c.servingMu.Lock()
		cancel := c.serving[msg.Seq]
		c.servingMu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
	return nil
}

// registerServing starts tracking an incoming request so it can be
// cancelled by the peer. The returned cancel func must be called when
// the request finishes serving.
func (c *Client) registerServing(seq uint64) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	c.servingMu.Lock()
	c.serving[seq] = cancel
	c.servingMu.Unlock()
	return ctx, cancel
}

// unregisterServing stops tracking an incoming request.
func (c *Client) unregisterServing(seq uint64) {
	c.servingMu.Lock()
	delete(c.serving, seq)
	c.servingMu.Unlock()
}

// cancelCall removes the call from the pending map and, if the peer
// announced support for the cancellation extension, notifies the peer
// that the request is cancelled. It reports whether the call was still
// pending; if not, the response has already been (or is being) read
// and the caller should wait for the call to complete instead.
func (c *Client) cancelCall(call *Call) bool {
	c.mutex.Lock()
	_, ok := c.pending[call.seq]
	if ok {
		delete(c.pending, call.seq)
	}
	supported := c.peerSupportsCancel()
	c.mutex.Unlock()

	if ok && supported {
		// Notify the peer asynchronously so CallWithContext is not
		// blocked by a congested connection.
		go c.sendControl(controlMessage{Type: controlCancel, Seq: call.seq})
	}
	return ok
}
