// MIT License
//
// Copyright (c) 2016-2017 xtaci
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package smux

import (
	"encoding/binary"
	"sync/atomic"
	"time"
)

// timer is a minimal time.Timer interface used by writeCoalescer so that the
// commit timing can be driven deterministically in tests.
type timer interface {
	Ch() <-chan time.Time
	Reset(d time.Duration)
	Stop()
}

// clock creates timers for the write coalescer.
type clock interface {
	NewTimer(d time.Duration) timer
}

type realTimer struct {
	t *time.Timer
}

func (r *realTimer) Ch() <-chan time.Time { return r.t.C }

func (r *realTimer) Reset(d time.Duration) {
	if !r.t.Stop() {
		select {
		case <-r.t.C:
		default:
		}
	}
	r.t.Reset(d)
}

func (r *realTimer) Stop() {
	if !r.t.Stop() {
		select {
		case <-r.t.C:
		default:
		}
	}
}

type realClock struct{}

func (realClock) NewTimer(d time.Duration) timer {
	return &realTimer{t: time.NewTimer(d)}
}

// writeCoalescer assembles several complete, ready-to-send frames into one
// contiguous byte buffer so that the session can issue a single underlying
// conn.Write. It does not alter the wire format: frames are serialized in
// exactly the same order they are popped from the shaper, so per-stream
// ordering is preserved.
//
// All methods are called from sendLoop only and require no synchronization.
type writeCoalescer struct {
	maxBytes int           // commit once the pending batch reaches this size
	delay    time.Duration // maximum age of the oldest pending frame
	clock    clock
	timer    timer

	buf  []byte         // serialized frames waiting to be committed
	reqs []writeRequest // requests corresponding to buf, in order
	nreq int32          // atomic mirror of len(reqs), safe for test observers
}

func newWriteCoalescer(maxBytes int, delay time.Duration, clk clock) *writeCoalescer {
	c := &writeCoalescer{
		maxBytes: maxBytes,
		delay:    delay,
		clock:    clk,
		buf:      make([]byte, 0, maxBytes),
		reqs:     make([]writeRequest, 0, 16),
	}
	c.timer = clk.NewTimer(delay)
	c.timer.Stop()
	return c
}

// pending returns the number of frames not committed yet.
func (c *writeCoalescer) pending() int {
	return int(atomic.LoadInt32(&c.nreq))
}

// pendingLen returns the exact pending count without an atomic load; callers
// must run on sendLoop.
func (c *writeCoalescer) pendingLen() int {
	return len(c.reqs)
}

// full reports whether the byte threshold of a commit has been reached.
func (c *writeCoalescer) full() bool {
	return len(c.buf) >= c.maxBytes
}

// append serializes one complete frame into the pending batch. The commit
// timer is armed when the first frame enters a batch and is not reset by
// later frames, so every frame is committed after at most one delay period.
func (c *writeCoalescer) append(req writeRequest) {
	if len(c.reqs) == 0 {
		c.timer.Stop()
		c.timer.Reset(c.delay)
	}

	var hdr rawHeader
	hdr[0] = req.frame.ver
	hdr[1] = req.frame.cmd
	binary.LittleEndian.PutUint16(hdr[2:], uint16(len(req.frame.data)))
	binary.LittleEndian.PutUint32(hdr[4:], req.frame.sid)

	c.buf = append(c.buf, hdr[:]...)
	c.buf = append(c.buf, req.frame.data...)
	c.reqs = append(c.reqs, req)
	atomic.AddInt32(&c.nreq, 1)
}

// take removes the pending batch from the coalescer and disarms its timer.
// The timer only ever guards frames that have not been committed yet.
func (c *writeCoalescer) take() (buf []byte, reqs []writeRequest) {
	c.timer.Stop()
	return c.buf, c.reqs
}

// reset clears the batch last taken from the coalescer and makes its backing
// storage available for the next batch. It must be called only after the
// taken batch has been fully handled by the caller.
func (c *writeCoalescer) reset() {
	c.buf = c.buf[:0]
	c.reqs = c.reqs[:0]
	atomic.StoreInt32(&c.nreq, 0)
}

// fail completes every pending request with err without writing anything to
// the underlying connection. It is used when the session shuts down while a
// batch is still uncommitted.
func (c *writeCoalescer) fail(err error) {
	c.timer.Stop()
	reqs := c.reqs
	c.reset()
	for _, req := range reqs {
		req.result <- writeResult{n: 0, err: err}
	}
}
