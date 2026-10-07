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
	"io"
	"time"
)

// This file implements the optional send-side write coalescer.
//
// When enabled (Config.WriteCoalesceBytes > 0), ready data frames are
// serialized into a pending batch and submitted to the underlying
// connection with a single Write. A batch is committed when:
//
//   - the pending bytes reach Config.WriteCoalesceBytes, or
//   - Config.WriteCoalesceDelay has elapsed since the batch became
//     non-empty (the commit timer only guards the uncommitted batch), or
//   - a frame that needs immediate handling (SYN/FIN/NOP/UPD) is popped;
//     the batch is committed first, so a FIN never overtakes earlier
//     data of the same stream.
//
// The wire format and the per-stream frame order are untouched; the
// coalescer only changes write boundaries on the underlying connection.
//
// Error delivery semantics:
//
//   - short write (n < len(batch), err == nil) is reported as
//     io.ErrShortWrite;
//   - on a partial write, frames fully written report success, the frame
//     straddling the write boundary reports its written payload bytes
//     along with the error, and the remaining frames report the error
//     with n == 0;
//   - on write failure the error is delivered to every request still
//     pending in the batch and recorded as the session socket write error;
//   - when the session is closed, requests still waiting in an
//     uncommitted batch are failed with io.ErrClosedPipe.

// appendFrame serializes a frame onto the batch buffer.
// The wire format is identical to the non-coalescing path.
func appendFrame(buf []byte, f Frame) []byte {
	var hdr [headerSize]byte
	hdr[0] = f.ver
	hdr[1] = f.cmd
	binary.LittleEndian.PutUint16(hdr[2:], uint16(len(f.data)))
	binary.LittleEndian.PutUint32(hdr[4:], f.sid)
	buf = append(buf, hdr[:]...)
	return append(buf, f.data...)
}

// writeBatch submits the coalesced batch to the underlying connection
// with a single Write and delivers a result to every request in the batch.
// It returns false if a socket-level error occurred and the send loop
// must exit.
func (s *Session) writeBatch(reqs []writeRequest, buf []byte) bool {
	n, err := s.conn.Write(buf)
	if err == nil && n != len(buf) {
		err = io.ErrShortWrite
	}
	deliverBatchResults(reqs, n, err)
	if err != nil {
		s.notifyWriteError(err)
		return false
	}
	return true
}

// deliverBatchResults distributes the outcome of a batch write to every
// request. n is the total number of bytes written (headers included);
// err must be non-nil whenever n is smaller than the batch size.
func deliverBatchResults(reqs []writeRequest, n int, err error) {
	if n < 0 {
		n = 0
	}
	results := make([]writeResult, len(reqs))
	off := 0
	for i, req := range reqs {
		frameLen := headerSize + len(req.frame.data)
		written := n - off // bytes of this frame that made it out
		if written < 0 {
			written = 0
		}
		if written > frameLen {
			written = frameLen
		}
		payload := written - headerSize
		if payload < 0 {
			payload = 0
		}
		results[i].n = payload
		if written < frameLen {
			results[i].err = err
		}
		off += frameLen
	}
	if err != nil && len(results) > 0 {
		// Attribute the error to the last request when every frame was
		// fully written, so the error stays observable; this matches the
		// non-coalescing sendLoop for a connection that returns
		// n == len(p) together with an error.
		delivered := false
		for i := range results {
			if results[i].err != nil {
				delivered = true
				break
			}
		}
		if !delivered {
			results[len(results)-1].err = err
		}
	}
	for i, req := range reqs {
		req.result <- results[i]
	}
}

// writeSingleFrame writes a frame that needs immediate handling (control
// frames and FIN) directly to the underlying connection and delivers the
// result. It returns false if a socket-level error occurred and the send
// loop must exit.
func (s *Session) writeSingleFrame(request writeRequest) bool {
	buf := make([]byte, headerSize, headerSize+len(request.frame.data))
	buf[0] = request.frame.ver
	buf[1] = request.frame.cmd
	binary.LittleEndian.PutUint16(buf[2:], uint16(len(request.frame.data)))
	binary.LittleEndian.PutUint32(buf[4:], request.frame.sid)
	buf = append(buf, request.frame.data...)

	n, err := s.conn.Write(buf)
	if err == nil && n != len(buf) {
		err = io.ErrShortWrite
	}
	n -= headerSize
	if n < 0 {
		n = 0
	}
	request.result <- writeResult{n: n, err: err}
	if err != nil {
		s.notifyWriteError(err)
		return false
	}
	return true
}

// sendLoopCoalesce is the coalescing variant of sendLoop, used when
// Config.WriteCoalesceBytes is positive.
func (s *Session) sendLoopCoalesce() {
	var (
		batchReqs []writeRequest
		batchBuf  []byte
		timerCh   <-chan time.Time
		stopBatch func()
	)

	stopBatchTimer := func() {
		if stopBatch != nil {
			stopBatch()
			stopBatch = nil
			timerCh = nil
		}
	}

	// flush commits the pending batch with a single Write.
	// It returns false if a socket error occurred and the loop must exit.
	flush := func() bool {
		stopBatchTimer()
		if len(batchReqs) == 0 {
			return true
		}
		ok := s.writeBatch(batchReqs, batchBuf)
		batchReqs = batchReqs[:0]
		batchBuf = batchBuf[:0]
		return ok
	}

	for {
		select {
		case <-s.die:
			// fail the requests still waiting in the uncommitted batch
			stopBatchTimer()
			for _, req := range batchReqs {
				req.result <- writeResult{n: 0, err: io.ErrClosedPipe}
			}
			return
		case <-timerCh:
			// the uncommitted batch has waited long enough
			if !flush() {
				return
			}
		case <-s.chShaperPending:
			for {
				request, ok := s.sq.Pop()
				if !ok {
					// notify shaperLoop to accept new requests
					s.notifyShaperConsumed()
					break
				}

				if request.frame.cmd == cmdPSH {
					// coalescible data frame
					batchBuf = appendFrame(batchBuf, request.frame)
					batchReqs = append(batchReqs, request)
					if len(batchReqs) == 1 {
						// the commit timer only guards the uncommitted batch
						timerCh, stopBatch = s.newBatchTimer(s.config.WriteCoalesceDelay)
					}
					if len(batchBuf) >= s.config.WriteCoalesceBytes {
						if !flush() {
							return
						}
					}
					continue
				}

				// A frame that needs immediate handling (SYN/FIN/NOP/UPD):
				// pull earlier frames of the same stream out of the shaper
				// queue first, so e.g. a FIN never overtakes earlier data.
				for _, req := range s.sq.PopStreamBefore(request.frame.sid, request.seq) {
					batchBuf = appendFrame(batchBuf, req.frame)
					batchReqs = append(batchReqs, req)
				}
				// commit the pending batch, then write the frame right away
				if !flush() {
					return
				}
				if !s.writeSingleFrame(request) {
					return
				}
			}
		}
	}
}
