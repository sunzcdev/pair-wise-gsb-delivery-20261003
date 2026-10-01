// Copyright 2026 The Gorilla WebSocket Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package websocket

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// defaultDrainLimitBytes is the default maximum number of wire payload
	// bytes that Cancel discards while skipping the remainder of a message.
	defaultDrainLimitBytes = 1 << 20 // 1 MiB

	// defaultDrainTimeout is the default maximum time spent in Cancel while
	// draining the remainder of a message.
	defaultDrainTimeout = 5 * time.Second
)

// CancellableReader is the reader returned by NextReaderCancellable. Reading
// from it works exactly like the reader returned from NextReader, but the
// application can give up on the current message through Cancel and continue
// reading subsequent messages on the same connection.
type CancellableReader struct {
	c       *Conn
	r       io.ReadCloser  // messageReader or decompression wrapper.
	mu      sync.Mutex     // guards r, settled and close(done).
	fin     atomic.Bool    // message ended naturally or canceled.
	settled bool           // done has been closed.
	done    chan struct{}  // closed once the reader is settled (drain finished).
	wg      sync.WaitGroup // outstanding Read calls (read handoff for Cancel).
}

// Read delivers payload bytes of the current message. Once Cancel has been
// called, Read returns ErrMessageCanceled. A Read interrupted by Cancel while
// blocked on the network also returns ErrMessageCanceled.
func (cr *CancellableReader) Read(p []byte) (int, error) {
	if cr.fin.Load() {
		return 0, ErrMessageCanceled
	}
	r := cr.r
	cr.wg.Add(1)
	n, err := r.Read(p)
	cr.wg.Done()

	if err == io.EOF {
		if cr.fin.CompareAndSwap(false, true) {
			cr.settle(naturalSettle)
			return n, io.EOF
		}
		return n, ErrMessageCanceled
	}
	if err != nil {
		if cr.fin.Load() {
			return n, ErrMessageCanceled
		}
		return n, err
	}
	return n, nil
}

// Close is a no-op kept to satisfy io.ReadCloser. Use Cancel to give up the
// current message.
func (cr *CancellableReader) Close() error { return nil }

// Cancel gives up the current message. It interrupts any Read blocked on the
// connection (that Read returns ErrMessageCanceled), then drains the rest of
// the message so that the connection is positioned at the start of the next
// message.
//
// During the drain, ping, pong and close frames interleaved with the
// fragmented message are handled normally, and any per-message decompression
// state is discarded rather than carried over to the following message.
//
// The drain is bounded by the byte budget set with SetCancelDrainLimit and
// the time budget set with SetCancelDrainTimeout. If the start of the next
// message cannot be reached within those budgets, Cancel closes the
// connection (after a best-effort close handshake) and returns ErrDrainLimit
// or ErrDrainTimeout. After that, every read on the connection fails.
//
// Cancel is idempotent and returns nil when the message was already fully
// consumed or already canceled.
func (cr *CancellableReader) Cancel() error {
	c := cr.c
	if c == nil {
		return nil
	}

	if !cr.fin.CompareAndSwap(false, true) {
		// Another Cancel is in progress, or the message ended naturally.
		// Wait for the settlement so callers observe the drain outcome.
		<-cr.done
		if c.readErr != nil {
			return c.readErr
		}
		return nil
	}

	// Force any Read blocked on the network to return immediately. This must
	// not wait on a read lock: the in-flight Read owns the frame reader until
	// the deadline releases it.
	deadline := c.readDeadline
	if c.conn != nil {
		_ = c.conn.SetReadDeadline(time.Now())
	}

	// Wait for the interrupted (or any in-flight) Read to release the frame
	// reader before draining the connection.
	cr.wg.Wait()

	// Drop the decompressor (if any): its buffered residual state must never
	// leak into the next message. Draining proceeds directly on wire bytes.
	cr.mu.Lock()
	if cr.r != nil {
		_ = cr.r.Close()
		cr.r = nil
	}
	cr.mu.Unlock()

	return cr.drain(c, deadline)
}

type settleMode int

const (
	naturalSettle settleMode = iota
	retireSettle
)

// settle closes done exactly once. naturalSettle is used when the message was
// fully read, retireSettle when the reader is abandoned via plain NextReader.
func (cr *CancellableReader) settle(mode settleMode) {
	c := cr.c
	cr.mu.Lock()
	defer cr.mu.Unlock()
	if cr.settled {
		return
	}
	cr.settled = true
	if c != nil {
		if mode == naturalSettle && c.readErr == io.EOF {
			// io.EOF recorded on a zero-byte network read is a bookkeeping
			// marker, not a failed connection.
			c.readErr = nil
		}
		if c.cancelReader == cr {
			c.cancelReader = nil
			c.reader = nil
		}
		if mode == retireSettle && cr.r != nil {
			_ = cr.r.Close()
			cr.r = nil
		}
	}
	close(cr.done)
}

// retire abandons a reader without canceling (plain NextReader usage). The
// remainder of the message is discarded through the normal frame skipping
// path; no drain budget is enforced.
func (cr *CancellableReader) retire() {
	if !cr.fin.CompareAndSwap(false, true) {
		<-cr.done
		return
	}
	cr.settle(retireSettle)
}

// await blocks until a previously started drain (or natural end) has settled.
func (cr *CancellableReader) await() error {
	<-cr.done
	c := cr.c
	if c != nil && c.readErr != nil {
		return c.readErr
	}
	return nil
}

// drain skips the rest of the current message, honoring the configured byte
// and time budgets. On success the connection is positioned at the start of
// the next message. On failure the connection is closed.
func (cr *CancellableReader) drain(c *Conn, prevDeadline time.Time) error {
	defer c.conn.SetReadDeadline(prevDeadline)

	budget := c.drainLimitBytes
	if c.drainTimeout > 0 {
		_ = c.conn.SetReadDeadline(time.Now().Add(c.drainTimeout))
	}

	// A deadline-induced Read may have recorded a timeout on the connection;
	// clear it because the frame position is still known.
	c.readErr = nil

	// Skip payload of the frame the application was reading, then walk
	// continuation frames until the message's FIN. Control frames encountered
	// between fragments are handled by advanceFrame (ping/pong/close).
	for {
		if c.readRemaining > 0 {
			if budget < c.readRemaining {
				cr.settleFatal(c, ErrDrainLimit, CloseMessageTooBig)
				return ErrDrainLimit
			}
			n, err := io.CopyN(io.Discard, c.br, c.readRemaining)
			budget -= n
			if err != nil {
				if isTimeoutErr(err) {
					cr.settleFatal(c, ErrDrainTimeout, CloseTryAgainLater)
					return ErrDrainTimeout
				}
				cr.settleFatal(c, err, CloseAbnormalClosure)
				return err
			}
			_ = c.setReadRemaining(0)
		}

		if c.readFinal {
			break
		}

		frameType, err := c.advanceFrame()
		if err != nil {
			if e, ok := err.(*CloseError); ok {
				cr.settleClose(c, e)
				return err
			}
			if isTimeoutErr(err) {
				cr.settleFatal(c, ErrDrainTimeout, CloseTryAgainLater)
				return ErrDrainTimeout
			}
			if errors.Is(err, ErrReadLimit) {
				// advanceFrame already sent CloseMessageTooBig.
				cr.settleFatalNoWrite(c, err)
				return err
			}
			cr.settleFatal(c, err, CloseAbnormalClosure)
			return err
		}
		switch frameType {
		case TextMessage, BinaryMessage:
			err = errors.New("websocket: internal error, unexpected data frame while draining")
			cr.settleFatal(c, err, CloseProtocolError)
			return err
		case continuationFrame:
			// Payload is skipped by the readRemaining check at loop top.
		default:
			// Ping/pong already processed by advanceFrame.
		}
	}

	c.readErr = nil
	c.messageReader = nil
	c.readLength = 0

	cr.mu.Lock()
	if c.cancelReader == cr {
		c.cancelReader = nil
	}
	c.reader = nil
	cr.settled = true
	close(cr.done)
	cr.mu.Unlock()
	return nil
}

// settleFatal marks the connection unusable, performs a best-effort close
// handshake and settles the reader.
func (cr *CancellableReader) settleFatal(c *Conn, cause error, code int) {
	data := FormatCloseMessage(code, truncateReason(cause.Error()))
	_ = c.WriteControl(CloseMessage, data, time.Now().Add(writeWait))
	cr.settleFatalNoWrite(c, cause)
}

// settleFatalNoWrite marks the connection unusable without sending a close
// frame (used when the cause already produced a close handshake).
func (cr *CancellableReader) settleFatalNoWrite(c *Conn, cause error) {

	cr.mu.Lock()
	c.readErr = cause
	if c.cancelReader == cr {
		c.cancelReader = nil
	}
	c.reader = nil
	cr.settled = true
	close(cr.done)
	cr.mu.Unlock()

	_ = c.conn.Close()
}

// settleClose handles a close frame received while draining.
func (cr *CancellableReader) settleClose(c *Conn, e *CloseError) {
	cr.mu.Lock()
	c.readErr = e
	if c.cancelReader == cr {
		c.cancelReader = nil
	}
	c.reader = nil
	cr.settled = true
	close(cr.done)
	cr.mu.Unlock()
}

func truncateReason(s string) string {
	const max = maxControlFramePayloadSize - 2
	if len(s) > max {
		return s[:max]
	}
	return s
}

func isTimeoutErr(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// SetCancelDrainLimit sets the maximum number of wire payload bytes that
// CancellableReader.Cancel discards while draining a canceled message. If the
// remainder of the message is larger than the limit, Cancel closes the
// connection and returns ErrDrainLimit. A value of 0 restores the default
// (1 MiB); a negative value disables the byte limit.
func (c *Conn) SetCancelDrainLimit(limit int64) {
	switch {
	case limit == 0:
		c.drainLimitBytes = defaultDrainLimitBytes
	case limit < 0:
		c.drainLimitBytes = 1<<63 - 1
	default:
		c.drainLimitBytes = limit
	}
}

// SetCancelDrainTimeout sets the maximum time that CancellableReader.Cancel
// spends draining a canceled message. If the start of the next message cannot
// be reached in time, Cancel closes the connection and returns ErrDrainTimeout.
// A value of 0 restores the default (5 seconds).
func (c *Conn) SetCancelDrainTimeout(d time.Duration) {
	if d <= 0 {
		c.drainTimeout = defaultDrainTimeout
	} else {
		c.drainTimeout = d
	}
}

// AsCancellableReader returns the reader as a *CancellableReader when the
// reader was obtained from NextReaderCancellable, or nil otherwise.
func AsCancellableReader(r io.Reader) *CancellableReader {
	cr, _ := r.(*CancellableReader)
	return cr
}
