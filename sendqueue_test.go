// The MIT License (MIT)
//
// Copyright (c) 2025 xtaci
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
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, WHETHER IN AN ACTION OF
// CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE
// SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.

package kcp

import (
	"encoding/binary"
	"testing"
)

// newLimitTestKCP builds a KCP with a small mtu (mss payload bytes) and
// congestion control disabled, so queued segments enter snd_buf on flush.
func newLimitTestKCP(mss int) *KCP {
	kcp := NewKCP(1, func(buf []byte, size int) {})
	if kcp.SetMtu(IKCP_OVERHEAD+mss) != 0 {
		panic("SetMtu failed")
	}
	kcp.NoDelay(1, 10, 0, 1) // nc=1: disable congestion control
	return kcp
}

// feedACKs injects ACK segments for sn in [from, to) into kcp, followed by a
// cumulative una=to. Each ACK is a standalone KCP frame on the wire.
func feedACKs(kcp *KCP, from, to uint32) {
	for sn := from; sn < to; sn++ {
		pkt := make([]byte, IKCP_OVERHEAD)
		binary.LittleEndian.PutUint32(pkt[0:], kcp.conv)
		pkt[4] = IKCP_CMD_ACK
		pkt[5] = 0
		binary.LittleEndian.PutUint16(pkt[6:], uint16(kcp.rcv_wnd))
		binary.LittleEndian.PutUint32(pkt[8:], currentMs())
		binary.LittleEndian.PutUint32(pkt[12:], sn)
		binary.LittleEndian.PutUint32(pkt[16:], to) // una
		binary.LittleEndian.PutUint32(pkt[20:], 0)
		if kcp.Input(pkt, IKCP_PACKET_REGULAR, false) != 0 {
			panic("ACK input failed")
		}
	}
}

// TestSendQueueByteLimit exercises the byte budget with a small MTU:
// fitting sends succeed, over-budget sends are temporary congestion, and a
// message larger than the budget itself is permanently too large.
func TestSendQueueByteLimit(t *testing.T) {
	kcp := newLimitTestKCP(10) // mss=10
	if r := kcp.SetSendQueueLimit(0, 20); r != 0 {
		t.Fatalf("SetSendQueueLimit = %d", r)
	}

	if r := kcp.Send(make([]byte, 10)); r != SendOK {
		t.Fatalf("first send = %v, want SendOK", r)
	}
	if got := kcp.WaitSndBytes(); got != 10 {
		t.Fatalf("WaitSndBytes = %d, want 10", got)
	}
	if got := kcp.WaitSnd(); got != 1 {
		t.Fatalf("WaitSnd = %d, want 1", got)
	}

	// 10 more bytes fit exactly.
	if r := kcp.Send(make([]byte, 10)); r != SendOK {
		t.Fatalf("second send = %v, want SendOK", r)
	}
	if got := kcp.WaitSndBytes(); got != 20 {
		t.Fatalf("WaitSndBytes = %d, want 20", got)
	}

	// any further send is temporary congestion.
	if r := kcp.Send([]byte{1}); r != SendCongested {
		t.Fatalf("over-budget send = %v, want SendCongested", r)
	}

	// a message larger than the whole budget is permanently too large.
	if r := kcp.Send(make([]byte, 21)); r != SendMsgTooLarge {
		t.Fatalf("oversize send = %v, want SendMsgTooLarge", r)
	}
}

// TestSendQueueSegmentLimit exercises the segment-count budget.
func TestSendQueueSegmentLimit(t *testing.T) {
	kcp := newLimitTestKCP(10) // mss=10
	if r := kcp.SetSendQueueLimit(2, 0); r != 0 {
		t.Fatalf("SetSendQueueLimit = %d", r)
	}

	if r := kcp.Send(make([]byte, 20)); r != SendOK { // 2 segments
		t.Fatalf("send = %v, want SendOK", r)
	}
	if got := kcp.WaitSnd(); got != 2 {
		t.Fatalf("WaitSnd = %d, want 2", got)
	}
	if r := kcp.Send([]byte{1}); r != SendCongested {
		t.Fatalf("over-budget send = %v, want SendCongested", r)
	}
	// three segments can never fit a two-segment budget.
	if r := kcp.Send(make([]byte, 21)); r != SendMsgTooLarge {
		t.Fatalf("oversize send = %v, want SendMsgTooLarge", r)
	}
}

// TestSendAtomicOnCongestion guarantees "all or nothing": a rejected
// fragmented multi-segment message must not leave partial segments behind.
func TestSendAtomicOnCongestion(t *testing.T) {
	kcp := newLimitTestKCP(10) // mss=10
	kcp.SetSendQueueLimit(0, 15)

	if r := kcp.Send(make([]byte, 10)); r != SendOK {
		t.Fatalf("seed send = %v, want SendOK", r)
	}
	beforeSegs := kcp.WaitSnd()
	beforeBytes := kcp.WaitSndBytes()

	// 10 bytes need 1 segment; remaining budget is 5, so this is rejected.
	if r := kcp.Send(make([]byte, 10)); r != SendCongested {
		t.Fatalf("send = %v, want SendCongested", r)
	}
	if kcp.WaitSnd() != beforeSegs || kcp.WaitSndBytes() != beforeBytes {
		t.Fatalf("queue mutated on rejection: segs %d->%d, bytes %d->%d",
			beforeSegs, kcp.WaitSnd(), beforeBytes, kcp.WaitSndBytes())
	}
}

// TestSendEmptyMessage verifies zero-length messages never consume budget
// and are reported as success.
func TestSendEmptyMessage(t *testing.T) {
	kcp := newLimitTestKCP(10)
	kcp.SetSendQueueLimit(1, 1)

	if r := kcp.Send(nil); r != SendOK {
		t.Fatalf("nil send = %v, want SendOK", r)
	}
	if r := kcp.Send([]byte{}); r != SendOK {
		t.Fatalf("empty send = %v, want SendOK", r)
	}
	if kcp.WaitSnd() != 0 || kcp.WaitSndBytes() != 0 {
		t.Fatalf("empty message consumed budget: segs=%d bytes=%d",
			kcp.WaitSnd(), kcp.WaitSndBytes())
	}
}

// TestStreamTailRemainder verifies that stream-mode bytes merged into the
// tail segment's remaining capacity are fully counted against the byte
// budget, and that a partially mergeable message that cannot be fully
// admitted is rejected atomically without mutating the tail segment.
func TestStreamTailRemainder(t *testing.T) {
	kcp := newLimitTestKCP(10) // mss=10
	kcp.stream = 1
	kcp.SetSendQueueLimit(0, 25)

	// seed a short segment: 4 bytes in snd_queue, tail has 6 bytes free.
	if r := kcp.Send(make([]byte, 4)); r != SendOK {
		t.Fatalf("seed send = %v, want SendOK", r)
	}
	if kcp.snd_queue.Len() != 1 || kcp.WaitSndBytes() != 4 {
		t.Fatalf("unexpected seed state: qlen=%d bytes=%d",
			kcp.snd_queue.Len(), kcp.WaitSndBytes())
	}

	// 6 bytes merge entirely into the tail segment, no new segment.
	if r := kcp.Send(make([]byte, 6)); r != SendOK {
		t.Fatalf("tail-fill send = %v, want SendOK", r)
	}
	if kcp.snd_queue.Len() != 1 || kcp.WaitSndBytes() != 10 {
		t.Fatalf("tail not merged in place: qlen=%d bytes=%d",
			kcp.snd_queue.Len(), kcp.WaitSndBytes())
	}
	tail, _ := kcp.snd_queue.Peek()
	if len(tail.data) != 10 {
		t.Fatalf("tail segment len = %d, want 10", len(tail.data))
	}

	// remaining budget = 15: send 10 bytes as a second full segment.
	if r := kcp.Send(make([]byte, 10)); r != SendOK {
		t.Fatalf("second segment send = %v, want SendOK", r)
	}
	if kcp.snd_queue.Len() != 2 || kcp.WaitSndBytes() != 20 {
		t.Fatalf("unexpected state: qlen=%d bytes=%d",
			kcp.snd_queue.Len(), kcp.WaitSndBytes())
	}

	// A 4-byte message could merge 0 bytes into the full tail and would need
	// one new 4-byte segment: 20+4=24 <= 25, so it fits.
	if r := kcp.Send(make([]byte, 4)); r != SendOK {
		t.Fatalf("third segment send = %v, want SendOK", r)
	}

	// budget exhausted (24/25): a 2-byte message that would fit in 1 segment
	// is temporary congestion and must leave the queue untouched.
	qlen := kcp.snd_queue.Len()
	sbytes := kcp.WaitSndBytes()
	if r := kcp.Send(make([]byte, 2)); r != SendCongested {
		t.Fatalf("congested send = %v, want SendCongested", r)
	}
	if kcp.snd_queue.Len() != qlen || kcp.WaitSndBytes() != sbytes {
		t.Fatalf("rejected stream send mutated the queue: qlen=%d bytes=%d",
			kcp.snd_queue.Len(), kcp.WaitSndBytes())
	}
}

// TestSendBudgetReleasedOnACK verifies that acknowledging segments (both
// via explicit ACKs and via the cumulative una in repeated packets) releases
// the exact byte/segment budget, and that flushing retransmits without any
// additional charge.
func TestSendBudgetReleasedOnACK(t *testing.T) {
	kcp := newLimitTestKCP(10) // mss=10
	kcp.SetSendQueueLimit(3, 30)

	// three full segments admitted and flushed into snd_buf.
	if r := kcp.Send(make([]byte, 30)); r != SendOK {
		t.Fatalf("send = %v, want SendOK", r)
	}
	kcp.flush(IKCP_FLUSH_FULL)
	if kcp.snd_buf.Len() != 3 || kcp.snd_queue.Len() != 0 || kcp.WaitSndBytes() != 30 {
		t.Fatalf("unexpected post-flush state: buflen=%d qlen=%d bytes=%d",
			kcp.snd_buf.Len(), kcp.snd_queue.Len(), kcp.WaitSndBytes())
	}

	// budget is exhausted: new data is rejected even though the window is open.
	if r := kcp.Send(make([]byte, 10)); r != SendCongested {
		t.Fatalf("post-flush send = %v, want SendCongested", r)
	}

	// force an RTO retransmit of the oldest segment; retransmission must not
	// consume extra budget.
	old, _ := kcp.snd_buf.Peek()
	old.resendts = 0
	old.rto = 1
	kcp.flush(IKCP_FLUSH_FULL)
	if kcp.WaitSnd() != 3 || kcp.WaitSndBytes() != 30 {
		t.Fatalf("retransmit changed accounting: segs=%d bytes=%d",
			kcp.WaitSnd(), kcp.WaitSndBytes())
	}

	// acknowledge sn 0 and 1 explicitly (una is still 2 on the wire here).
	feedACKs(kcp, 0, 2)
	if kcp.WaitSnd() != 1 || kcp.WaitSndBytes() != 10 {
		t.Fatalf("after 2 ACKs: segs=%d bytes=%d, want 1/10",
			kcp.WaitSnd(), kcp.WaitSndBytes())
	}

	// repeated (duplicate) ACKs for the same sequence numbers must not
	// release budget twice.
	feedACKs(kcp, 0, 2)
	if kcp.WaitSnd() != 1 || kcp.WaitSndBytes() != 10 {
		t.Fatalf("duplicate ACKs changed accounting: segs=%d bytes=%d",
			kcp.WaitSnd(), kcp.WaitSndBytes())
	}

	// one segment's worth of room: a 10-byte send fits; an 11-byte message
	// needs 2 segments and is temporary congestion (1+2=3 fits the segment
	// limit), while a 31-byte message needs 4 segments and is permanent.
	if r := kcp.Send(make([]byte, 10)); r != SendOK {
		t.Fatalf("post-ACK send = %v, want SendOK", r)
	}
	if r := kcp.Send(make([]byte, 11)); r != SendCongested {
		t.Fatalf("post-ACK 11B send = %v, want SendCongested", r)
	}
	if r := kcp.Send(make([]byte, 31)); r != SendMsgTooLarge {
		t.Fatalf("post-ACK 31B send = %v, want SendMsgTooLarge", r)
	}

	// cumulative una alone (no per-segment ACK) releases the remaining
	// oldest segment when it eventually acknowledges.
	feedACKs(kcp, 2, 3)
	kcp.flush(IKCP_FLUSH_FULL)
	if kcp.WaitSnd() != 1 || kcp.WaitSndBytes() != 10 {
		t.Fatalf("after ACKing sn 2: segs=%d bytes=%d, want 1/10",
			kcp.WaitSnd(), kcp.WaitSndBytes())
	}
}

// TestSetSendQueueLimitValidation checks argument validation and that 0
// disables the corresponding limit.
func TestSetSendQueueLimitValidation(t *testing.T) {
	kcp := newLimitTestKCP(10)
	if r := kcp.SetSendQueueLimit(-1, 10); r != -1 {
		t.Fatalf("negative segments = %d, want -1", r)
	}
	if r := kcp.SetSendQueueLimit(10, -1); r != -1 {
		t.Fatalf("negative bytes = %d, want -1", r)
	}
	if r := kcp.SetSendQueueLimit(0, 0); r != 0 {
		t.Fatalf("zero limits = %d, want 0", r)
	}
	// unlimited: even a huge send is admitted (subject to the 255-fragment
	// protocol limit only).
	if r := kcp.Send(make([]byte, 10*200)); r != SendOK {
		t.Fatalf("unlimited send = %v, want SendOK", r)
	}
}
