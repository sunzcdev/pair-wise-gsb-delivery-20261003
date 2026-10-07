package rpc2

import (
	"context"
	"errors"
)

// drainMethod is the reserved method name of the notification sent to the
// peer when this end of the connection starts draining.
const drainMethod = "rpc2.drain"

// ErrDraining is returned when a new outgoing call is attempted on a
// connection that is draining.
var ErrDraining = errors.New("rpc2: connection is draining")

// Drain gracefully shuts down the connection and blocks until the
// connection is closed or ctx expires.
//
// When Drain starts, the connection enters the draining state:
//
//   - New outgoing calls initiated by the application are rejected with
//     ErrDraining.
//   - The peer is notified (best effort) so that a peer supporting this
//     capability also stops initiating new requests.
//   - Incoming requests keep being served, and outgoing calls made while
//     an inbound handler is running are still allowed. This is the rule
//     that lets already accepted work finish: a handler may call back to
//     the peer (nested calls) without both sides waiting for each other.
//
// Once all accepted work (running inbound handlers and outstanding
// outgoing calls) has completed, the connection is closed and Drain
// returns nil. If the connection breaks before that, Drain returns the
// connection error. If ctx expires, Drain stops waiting, closes the
// connection and returns ctx.Err(); in-flight calls are then terminated
// by the read loop with an error, never with a fabricated success.
//
// Drain may be called any number of times; concurrent and subsequent
// callers all observe the same final outcome.
func (c *Client) Drain(ctx context.Context) error {
	c.mutex.Lock()
	if c.draining {
		// A drain is already in progress (or finished); wait for its outcome.
		done := c.drainDone
		c.mutex.Unlock()
		select {
		case <-done:
			return c.drainErr
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if c.shutdown || c.closing {
		c.mutex.Unlock()
		return ErrShutdown
	}
	c.draining = true
	c.drainDone = make(chan struct{})
	done := c.drainDone
	c.mutex.Unlock()

	// Best effort: ask the peer to stop initiating new requests.
	c.notifyDrain()

	// The connection may already be idle.
	c.mutex.Lock()
	c.drainCheckLocked()
	c.mutex.Unlock()

	select {
	case <-done:
		return c.drainErr
	case <-ctx.Done():
		// Stop waiting and close the connection as agreed. Pending calls
		// are terminated with an error by the read loop.
		c.mutex.Lock()
		if !c.shutdown && !c.closing {
			c.closing = true
			go c.codec.Close()
		}
		c.mutex.Unlock()
		return ctx.Err()
	}
}

// Draining reports whether the connection is in the draining state.
func (c *Client) Draining() bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.draining
}

// notifyDrain sends the drain notification to the peer.
func (c *Client) notifyDrain() {
	c.sending.Lock()
	defer c.sending.Unlock()
	c.request.Seq = 0 // notification
	c.request.Method = drainMethod
	if err := c.codec.WriteRequest(&c.request, true); err != nil {
		debugln("rpc2: error writing drain notification:", err.Error())
	}
}

// peerDrain puts the connection into the draining state after the peer
// sent a drain notification. The peer already stops new requests, so no
// notification is sent back.
func (c *Client) peerDrain() {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.shutdown || c.closing || c.draining {
		return
	}
	c.draining = true
	c.drainDone = make(chan struct{})
	c.drainCheckLocked()
}

// drainCheckLocked closes the connection once draining and all accepted
// work has completed. Caller must hold c.mutex.
func (c *Client) drainCheckLocked() {
	if !c.draining || c.shutdown || c.closing {
		return
	}
	if c.activeHandlers > 0 || len(c.pending) > 0 {
		return
	}
	c.drainComplete = true
	c.closing = true
	go c.codec.Close()
}

// handlerDone marks the end of an inbound handler execution.
func (c *Client) handlerDone() {
	c.mutex.Lock()
	c.activeHandlers--
	c.drainCheckLocked()
	c.mutex.Unlock()
}
