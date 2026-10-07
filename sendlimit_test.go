package kcp

import (
	"encoding/binary"
	"io"
	"testing"
	"time"

	"github.com/xtaci/lossyconn"
)

// newTestKCP builds a KCP state machine with a small MTU and congestion
// control disabled, so that segments move from snd_queue into snd_buf on the
// first flush deterministically.
func newTestKCP(t *testing.T, mtu int) *KCP {
	t.Helper()
	kcp := NewKCP(1, func(buf []byte, size int) {})
	if err := kcp.SetMtu(mtu); err != 0 {
		t.Fatalf("SetMtu(%d) failed: %d", mtu, err)
	}
	// nodelay=0 interval=10 fastresend=0 nc=1 (disable congestion window)
	kcp.NoDelay(0, 10, 0, 1)
	return kcp
}

// feedAck injects a single IKCP_CMD_ACK segment into kcp.Input.
func feedAck(kcp *KCP, conv, sn, una uint32, wnd uint16) {
	pkt := make([]byte, IKCP_OVERHEAD)
	binary.LittleEndian.PutUint32(pkt[0:], conv)
	pkt[4] = IKCP_CMD_ACK
	pkt[5] = 0
	binary.LittleEndian.PutUint16(pkt[6:], wnd)
	binary.LittleEndian.PutUint32(pkt[8:], currentMs())
	binary.LittleEndian.PutUint32(pkt[12:], sn)
	binary.LittleEndian.PutUint32(pkt[16:], una)
	binary.LittleEndian.PutUint32(pkt[20:], 0)
	if ret := kcp.Input(pkt, IKCP_PACKET_REGULAR, false); ret != 0 {
		panic("feedAck: Input returned non-zero")
	}
}

// TestSendZeroLength verifies zero-length messages are rejected and never
// touch the send queue or the quota counters.
func TestSendZeroLength(t *testing.T) {
	kcp := newTestKCP(t, IKCP_OVERHEAD+1)
	kcp.SetSndQueueLimits(1, 1)

	if got := kcp.Send(nil); got != SendInvalid {
		t.Fatalf("Send(nil) = %d, want SendInvalid(%d)", got, SendInvalid)
	}
	if got := kcp.Send([]byte{}); got != SendInvalid {
		t.Fatalf("Send(empty) = %d, want SendInvalid(%d)", got, SendInvalid)
	}
	if kcp.WaitSnd() != 0 || kcp.WaitSndBytes() != 0 || kcp.snd_nseg != 0 {
		t.Fatalf("zero-length send mutated the queue: segs=%d bytes=%d nseg=%d",
			kcp.WaitSnd(), kcp.WaitSndBytes(), kcp.snd_nseg)
	}
}

// TestSendTooLarge verifies a message needing >255 segments is permanently
// rejected, distinct from transient congestion, and leaves the queue intact.
func TestSendTooLarge(t *testing.T) {
	// mss == 1 => 256 bytes needs 256 segments, beyond the 255 frg limit.
	kcp := newTestKCP(t, IKCP_OVERHEAD+1)
	kcp.SetSndQueueLimits(0, 0) // unlimited; still must be TooLarge, not Congested

	if got := kcp.Send(make([]byte, 256)); got != SendTooLarge {
		t.Fatalf("Send(256B) = %d, want SendTooLarge(%d)", got, SendTooLarge)
	}
	if kcp.WaitSnd() != 0 || kcp.snd_nseg != 0 || kcp.snd_nbytes != 0 {
		t.Fatalf("too-large send mutated the queue: segs=%d nseg=%d bytes=%d",
			kcp.WaitSnd(), kcp.snd_nseg, kcp.snd_nbytes)
	}

	// 255 segments is still acceptable.
	if got := kcp.Send(make([]byte, 255)); got != SendOK {
		t.Fatalf("Send(255B) = %d, want SendOK(%d)", got, SendOK)
	}
	if kcp.WaitSnd() != 255 || kcp.WaitSndBytes() != 255 {
		t.Fatalf("unexpected queue after accepted send: segs=%d bytes=%d",
			kcp.WaitSnd(), kcp.WaitSndBytes())
	}
}

// TestSendCongestedSegmentLimit exercises the segment-count limit, ACK
// release, and duplicate-ACK idempotency using a tiny MTU.
func TestSendCongestedSegmentLimit(t *testing.T) {
	kcp := newTestKCP(t, IKCP_OVERHEAD+1) // mss == 1
	kcp.SetSndQueueLimits(2, 0)

	if got := kcp.Send([]byte{1, 2}); got != SendOK {
		t.Fatalf("first Send = %d, want SendOK", got)
	}
	if kcp.snd_nseg != 2 || kcp.snd_nbytes != 2 {
		t.Fatalf("after fill: nseg=%d bytes=%d", kcp.snd_nseg, kcp.snd_nbytes)
	}

	// queue full -> transient congestion, queue must not change.
	if got := kcp.Send([]byte{3}); got != SendCongested {
		t.Fatalf("full Send = %d, want SendCongested(%d)", got, SendCongested)
	}
	if kcp.snd_nseg != 2 || kcp.snd_nbytes != 2 || kcp.WaitSnd() != 2 {
		t.Fatalf("congested Send mutated the queue: nseg=%d bytes=%d wait=%d",
			kcp.snd_nseg, kcp.snd_nbytes, kcp.WaitSnd())
	}

	// move both segments in-flight.
	kcp.flush(IKCP_FLUSH_FULL)
	if kcp.snd_nseg != 2 || kcp.snd_nbytes != 2 || kcp.snd_buf.Len() != 2 {
		t.Fatalf("after flush: nseg=%d bytes=%d snd_buf=%d",
			kcp.snd_nseg, kcp.snd_nbytes, kcp.snd_buf.Len())
	}

	// ACK for sn 0 alone (una unchanged) frees only bytes, not the slot.
	feedAck(kcp, kcp.conv, 0, 0, 32)
	if kcp.snd_nbytes != 1 {
		t.Fatalf("after ack sn0: bytes=%d, want 1", kcp.snd_nbytes)
	}
	if kcp.snd_nseg != 2 {
		t.Fatalf("segment count before una = %d, want 2", kcp.snd_nseg)
	}

	// duplicate ACK for the same sn must not refund again.
	feedAck(kcp, kcp.conv, 0, 0, 32)
	if kcp.snd_nbytes != 1 {
		t.Fatalf("duplicate ack changed bytes=%d, want 1", kcp.snd_nbytes)
	}

	// only one slot remains used; a fresh segment still hits the slot limit.
	if got := kcp.Send([]byte{9}); got != SendCongested {
		t.Fatalf("Send before slot release = %d, want SendCongested", got)
	}

	// cumulative una=2 releases both slots; sn1's byte is refunded here
	// (it had no explicit ACK yet), sn0 is a no-op due to charge==0.
	feedAck(kcp, kcp.conv, 0, 2, 32)
	if kcp.snd_nseg != 0 || kcp.snd_nbytes != 0 {
		t.Fatalf("after una=2: nseg=%d bytes=%d, want 0/0",
			kcp.snd_nseg, kcp.snd_nbytes)
	}
	if got := kcp.Send([]byte{9}); got != SendOK {
		t.Fatalf("retry after ack = %d, want SendOK", got)
	}
	if kcp.snd_nseg != 1 || kcp.snd_nbytes != 1 || kcp.WaitSnd() != 1 {
		t.Fatalf("after retry: nseg=%d bytes=%d wait=%d, want 1/1/1",
			kcp.snd_nseg, kcp.snd_nbytes, kcp.WaitSnd())
	}
}

// TestSendCongestedByteLimitStreamTail verifies the byte limit, including
// stream-mode coalescing into a tail segment that has spare room, and that a
// rejected send does not modify that tail.
func TestSendCongestedByteLimitStreamTail(t *testing.T) {
	kcp := newTestKCP(t, IKCP_OVERHEAD+10) // mss == 10
	kcp.stream = 1
	kcp.SetSndQueueLimits(0, 10)

	// one 3-byte segment leaves 7 bytes of headroom in the tail.
	if got := kcp.Send([]byte{1, 2, 3}); got != SendOK {
		t.Fatalf("first Send = %d, want SendOK", got)
	}
	tail, ok := kcp.snd_queue.Tail()
	if !ok || len(tail.data) != 3 || tail.charge != 3 {
		t.Fatalf("tail = %+v ok=%v, want 3 charged bytes", tail, ok)
	}

	// 8 bytes would coalesce 7 into the tail + one fresh segment = 11 > 10,
	// so it must be rejected with the tail left at 3 bytes.
	if got := kcp.Send(make([]byte, 8)); got != SendCongested {
		t.Fatalf("over-limit Send = %d, want SendCongested", got)
	}
	if len(tail.data) != 3 || tail.charge != 3 || kcp.snd_nbytes != 3 ||
		kcp.snd_nseg != 1 || kcp.snd_queue.Len() != 1 {
		t.Fatalf("congested send mutated tail/queue: datalen=%d charge=%d bytes=%d nseg=%d qlen=%d",
			len(tail.data), tail.charge, kcp.snd_nbytes, kcp.snd_nseg, kcp.snd_queue.Len())
	}

	// exactly the remaining 7 bytes are fully coalesced into the tail and
	// charged, no new segment is created.
	if got := kcp.Send(make([]byte, 7)); got != SendOK {
		t.Fatalf("exact-fill Send = %d, want SendOK", got)
	}
	if len(tail.data) != 10 || tail.charge != 10 || kcp.snd_nbytes != 10 ||
		kcp.snd_nseg != 1 || kcp.snd_queue.Len() != 1 {
		t.Fatalf("after coalesce: datalen=%d charge=%d bytes=%d nseg=%d qlen=%d",
			len(tail.data), tail.charge, kcp.snd_nbytes, kcp.snd_nseg, kcp.snd_queue.Len())
	}

	// budget fully consumed now.
	if got := kcp.Send([]byte{1}); got != SendCongested {
		t.Fatalf("full-budget Send = %d, want SendCongested", got)
	}

	// flush then ACK; released byte budget must allow more data.
	kcp.flush(IKCP_FLUSH_FULL)
	feedAck(kcp, kcp.conv, 0, 1, 32)
	if kcp.snd_nseg != 0 || kcp.snd_nbytes != 0 {
		t.Fatalf("after ack: nseg=%d bytes=%d, want 0/0",
			kcp.snd_nseg, kcp.snd_nbytes)
	}
	if got := kcp.Send(make([]byte, 10)); got != SendOK {
		t.Fatalf("retry after ack = %d, want SendOK", got)
	}
}

// TestRetransmitNoDoubleCharge verifies that (re)transmitting an in-flight
// segment does not reserve its byte/segment quota a second time.
func TestRetransmitNoDoubleCharge(t *testing.T) {
	kcp := newTestKCP(t, IKCP_OVERHEAD+10) // mss == 10
	kcp.SetSndQueueLimits(0, 10)

	if got := kcp.Send(make([]byte, 10)); got != SendOK {
		t.Fatalf("Send = %d, want SendOK", got)
	}
	kcp.flush(IKCP_FLUSH_FULL) // initial transmit, xmit -> 1

	seg, _ := kcp.snd_buf.Peek()
	if seg.xmit != 1 {
		t.Fatalf("xmit after first flush = %d, want 1", seg.xmit)
	}

	// force an RTO retransmit by expiring the resend timer.
	seg.resendts = currentMs() - 1
	kcp.flush(IKCP_FLUSH_FULL)

	if seg.xmit != 2 {
		t.Fatalf("xmit after retransmit = %d, want 2", seg.xmit)
	}
	if kcp.snd_nseg != 1 || kcp.snd_nbytes != 10 {
		t.Fatalf("retransmit altered quota: nseg=%d bytes=%d, want 1/10",
			kcp.snd_nseg, kcp.snd_nbytes)
	}
}

// TestSndQueueLimitsSession verifies the optional byte limit works through the
// full UDPSession stack with a small MTU: Write blocks while the limit is full
// and resumes once ACKs drain the outstanding data, delivering all bytes.
func TestSndQueueLimitsSession(t *testing.T) {
	client, err := lossyconn.NewLossyConn(0, 20)
	if err != nil {
		t.Fatal(err)
	}
	server, err := lossyconn.NewLossyConn(0, 20)
	if err != nil {
		t.Fatal(err)
	}

	sess, err := NewConn2(server.LocalAddr(), nil, 0, 0, client)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := ServeConn(nil, 0, 0, server)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	// small MTU forces many segments; a tight byte budget keeps only a few KB
	// outstanding, so the writer must block and resume rather than buffering
	// the entire payload in memory.
	const mtu = IKCP_OVERHEAD + 64
	if !sess.SetMtu(mtu) {
		t.Fatal("client SetMtu failed")
	}
	sess.SetNoDelay(1, 10, 2, 1)
	sess.SetSndQueueLimits(0, 2048)

	go func() {
		conn, err := listener.AcceptKCP()
		if err != nil {
			return
		}
		conn.SetMtu(mtu)
		echo := make(chan []byte, 4096)
		go func() {
			for b := range echo {
				if _, err := conn.Write(b); err != nil {
					return
				}
			}
		}()
		go func() {
			buf := make([]byte, 65536)
			for {
				n, err := conn.Read(buf)
				if err != nil {
					close(echo)
					return
				}
				cp := make([]byte, n)
				copy(cp, buf[:n])
				echo <- cp
			}
		}()
	}()

	payload := make([]byte, 60000)
	for i := range payload {
		payload[i] = byte(i)
	}

	// drain echo replies concurrently so the client keeps advertising a
	// non-zero receive window while its writer is backpressured.
	got := make([]byte, len(payload))
	readDone := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(sess, got)
		readDone <- err
	}()

	sess.SetWriteDeadline(time.Now().Add(20 * time.Second))
	if _, err := sess.Write(payload); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("ReadFull failed: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("timeout waiting for echoed payload")
	}
	for i := range payload {
		if got[i] != payload[i] {
			t.Fatalf("byte mismatch at %d", i)
		}
	}

	// after the round trip the outstanding quota should have been released.
	sess.mu.Lock()
	outstanding := sess.kcp.WaitSndBytes()
	sess.mu.Unlock()
	if outstanding != 0 {
		t.Fatalf("outstanding bytes after drain = %d, want 0", outstanding)
	}
	sess.Close()
}
