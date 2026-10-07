// The MIT License (MIT)
//
// Copyright (c) 2015 xtaci
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

package kcp

import (
	"container/heap"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/xtaci/lossyconn"
)

const repeat = 16

func TestLossyConn1(t *testing.T) {
	t.Log("testing loss rate 10%, rtt 200ms")
	t.Log("testing link with nodelay parameters:1 10 2 1")
	client, err := lossyconn.NewLossyConn(0.1, 100)
	if err != nil {
		t.Fatal(err)
		return
	}

	server, err := lossyconn.NewLossyConn(0.1, 100)
	if err != nil {
		t.Fatal(err)
		return
	}
	testlink(t, client, server, 1, 10, 2, 1)
}

func TestLossyConn2(t *testing.T) {
	t.Log("testing loss rate 20%, rtt 200ms")
	t.Log("testing link with nodelay parameters:1 10 2 1")
	client, err := lossyconn.NewLossyConn(0.2, 100)
	if err != nil {
		t.Fatal(err)
		return
	}

	server, err := lossyconn.NewLossyConn(0.2, 100)
	if err != nil {
		t.Fatal(err)
		return
	}
	testlink(t, client, server, 1, 10, 2, 1)
}

func TestLossyConn3(t *testing.T) {
	t.Log("testing loss rate 30%, rtt 200ms")
	t.Log("testing link with nodelay parameters:1 10 2 1")
	client, err := lossyconn.NewLossyConn(0.3, 100)
	if err != nil {
		t.Fatal(err)
		return
	}

	server, err := lossyconn.NewLossyConn(0.3, 100)
	if err != nil {
		t.Fatal(err)
		return
	}
	testlink(t, client, server, 1, 10, 2, 1)
}

func TestLossyConn4(t *testing.T) {
	t.Log("testing loss rate 10%, rtt 200ms")
	t.Log("testing link with nodelay parameters:1 10 2 0")
	client, err := lossyconn.NewLossyConn(0.1, 100)
	if err != nil {
		t.Fatal(err)
		return
	}

	server, err := lossyconn.NewLossyConn(0.1, 100)
	if err != nil {
		t.Fatal(err)
		return
	}
	testlink(t, client, server, 1, 10, 2, 0)
}

func testlink(t *testing.T, client *lossyconn.LossyConn, server *lossyconn.LossyConn, nodelay, interval, resend, nc int) {
	t.Log("testing with nodelay parameters:", nodelay, interval, resend, nc)
	sess, _ := NewConn2(server.LocalAddr(), nil, 0, 0, client)
	listener, _ := ServeConn(nil, 0, 0, server)
	echoServer := func(l *Listener) {
		for {
			conn, err := l.AcceptKCP()
			if err != nil {
				return
			}
			go func() {
				conn.SetNoDelay(nodelay, interval, resend, nc)
				buf := make([]byte, 65536)
				for {
					n, err := conn.Read(buf)
					if err != nil {
						return
					}
					conn.Write(buf[:n])
				}
			}()
		}
	}

	echoTester := func(s *UDPSession) {
		s.SetNoDelay(nodelay, interval, resend, nc)
		buf := make([]byte, 64)
		var rtt time.Duration
		for range repeat {
			start := time.Now()
			s.Write(buf)
			io.ReadFull(s, buf)
			rtt += time.Since(start)
		}

		t.Log("client:", client)
		t.Log("server:", server)
		t.Log("avg rtt:", rtt/repeat)
		t.Logf("total time: %v for %v round trip:", rtt, repeat)
	}

	go echoServer(listener)
	echoTester(sess)
}

func BenchmarkFlush(b *testing.B) {
	kcp := NewKCP(1, func(buf []byte, size int) {})
	kcp.snd_buf = NewRingBuffer[segment](1024)
	for range kcp.snd_buf.MaxLen() {
		kcp.snd_buf.Push(segment{xmit: 1, resendts: currentMs() + 10000})
	}

	b.ReportAllocs()
	var mu sync.Mutex
	for b.Loop() {
		mu.Lock()
		kcp.flush(IKCP_FLUSH_FULL)
		mu.Unlock()
	}
}

// TestSegmentHeap tests the segmentHeap data structure
func TestSegmentHeap(t *testing.T) {
	h := newSegmentHeap()
	segments := []segment{
		{sn: 1},
		{sn: 2},
		{sn: 3},
	}

	for _, seg := range segments {
		heap.Push(h, seg)
		t.Logf("pushed segment with seq %d", seg.sn)
	}

	if h.Len() != len(segments) {
		t.Errorf("expected length %d, got %d", len(segments), h.Len())
	}

	for i := range segments {
		seg := heap.Pop(h).(segment)
		if seg.sn != segments[i].sn {
			t.Errorf("expected seq %d, got %d", segments[i].sn, seg.sn)
		}
	}
}

// BenchmarkDebugLog test DebugLog cost time with build tags debug on/off
// trace log on:
//
//	go test -benchmem -run=^$ -bench ^BenchmarkDebugLog$ -tags debug
//
// trace log off:
func TestSetMtuBoundary(t *testing.T) {
	kcp := NewKCP(0, func(buf []byte, size int) {})

	tests := []struct {
		name string
		mtu  int
		want int // 0 = success, -1 = failure
	}{
		{"negative", -1, -1},
		{"zero", 0, -1},
		{"below overhead", IKCP_OVERHEAD - 1, -1},
		{"equal overhead", IKCP_OVERHEAD, -1},
		{"one above overhead (minimum valid)", IKCP_OVERHEAD + 1, 0},
		{"typical MTU", 1400, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := kcp.SetMtu(tt.mtu)
			if got != tt.want {
				t.Errorf("SetMtu(%d) = %d, want %d", tt.mtu, got, tt.want)
			}
		})
	}

	// verify mss is correctly derived after a valid SetMtu
	kcp.SetMtu(1400)
	if kcp.mss != 1400-IKCP_OVERHEAD {
		t.Errorf("mss = %d, want %d", kcp.mss, 1400-IKCP_OVERHEAD)
	}

	// minimum valid MTU yields mss = 1
	kcp.SetMtu(IKCP_OVERHEAD + 1)
	if kcp.mss != 1 {
		t.Errorf("mss = %d, want 1 for minimum valid MTU", kcp.mss)
	}
}

// go test -benchmem -run=^$ -bench ^BenchmarkDebugLog$
func BenchmarkDebugLog(b *testing.B) {
	kcp := &KCP{
		conv:    123,
		snd_wnd: 456,
	}
	kcp.log = slog.Debug

	for b.Loop() {
		// In release mode, this line of code will be completely 'erased' by the compiler,
		// as if it doesn't exist at all, and even the parameter's interface conversion will not occur.
		kcp.debugLog(IKCP_LOG_OUT_WASK, "conv", kcp.conv, "wnd", kcp.snd_wnd)
	}
}

// encodeSegments serializes segments into a single datagram, the same wire
// format produced by KCP.flush.
func encodeSegments(segs ...segment) []byte {
	var buf []byte
	for i := range segs {
		hdr := make([]byte, IKCP_OVERHEAD)
		segs[i].encode(hdr)
		buf = append(buf, hdr...)
		buf = append(buf, segs[i].data...)
	}
	return buf
}

// kcpCoreState captures the mutable core state that Input may touch, used to
// prove that a rejected datagram leaves the state machine untouched and that
// InputStrict matches Input on valid datagrams.
type kcpCoreState struct {
	snd_una  uint32
	snd_nxt  uint32
	rcv_nxt  uint32
	rmt_wnd  uint32
	cwnd     uint32
	incr     uint32
	probe    uint32
	sndBuf   int
	sndQueue int
	rcvBuf   int
	rcvQueue int
	acklist  int
}

func snapshotKCP(kcp *KCP) kcpCoreState {
	return kcpCoreState{
		snd_una:  kcp.snd_una,
		snd_nxt:  kcp.snd_nxt,
		rcv_nxt:  kcp.rcv_nxt,
		rmt_wnd:  kcp.rmt_wnd,
		cwnd:     kcp.cwnd,
		incr:     kcp.incr,
		probe:    kcp.probe,
		sndBuf:   kcp.snd_buf.Len(),
		sndQueue: kcp.snd_queue.Len(),
		rcvBuf:   kcp.rcv_buf.Len(),
		rcvQueue: kcp.rcv_queue.Len(),
		acklist:  len(kcp.acklist),
	}
}

// newTestKCP returns a KCP with one in-flight segment (sn=0) in its send
// buffer, so that incoming ACKs actually mutate the state.
func newTestKCP(conv uint32) *KCP {
	kcp := NewKCP(conv, func(buf []byte, size int) {})
	kcp.WndSize(128, 128)
	kcp.NoDelay(1, 10, 2, 1)
	kcp.Send([]byte("hello"))
	kcp.flush(IKCP_FLUSH_FULL)
	return kcp
}

// TestInputStrictACKOnly verifies that a valid ACK-only datagram applied
// through InputStrict produces exactly the same state as the legacy Input.
func TestInputStrictACKOnly(t *testing.T) {
	const conv = 0x11223344

	// a future ts keeps update_ack() out of the equation, so the comparison
	// is not sensitive to wall-clock jitter between the two calls.
	ts := currentMs() + 10000
	ack := segment{conv: conv, cmd: IKCP_CMD_ACK, wnd: 99, ts: ts, sn: 0, una: 1}
	dgram := encodeSegments(ack)

	legacy := newTestKCP(conv)
	strict := newTestKCP(conv)

	if ret := legacy.Input(dgram, IKCP_PACKET_REGULAR, false); ret != 0 {
		t.Fatalf("legacy Input returned %d", ret)
	}
	if err := strict.InputStrict(dgram, IKCP_PACKET_REGULAR, false); err != nil {
		t.Fatalf("InputStrict rejected a valid datagram: %v", err)
	}

	if got, want := snapshotKCP(strict), snapshotKCP(legacy); got != want {
		t.Fatalf("state mismatch after valid ACK-only datagram:\nstrict: %+v\nlegacy: %+v", got, want)
	}

	// the ACK must have taken effect: in-flight segment removed, una advanced,
	// remote window updated.
	if strict.snd_buf.Len() != 0 {
		t.Fatalf("snd_buf not drained by ACK, len=%d", strict.snd_buf.Len())
	}
	if strict.snd_una != 1 {
		t.Fatalf("snd_una=%d, want 1", strict.snd_una)
	}
	if strict.rmt_wnd != 99 {
		t.Fatalf("rmt_wnd=%d, want 99", strict.rmt_wnd)
	}
}

// TestInputStrictCompositePush verifies that a composite datagram (multiple
// PUSH segments plus a trailing ACK) behaves identically under both entries
// and delivers the payload in order.
func TestInputStrictCompositePush(t *testing.T) {
	const conv = 0x55667788

	ts := currentMs() + 10000
	dgram := encodeSegments(
		segment{conv: conv, cmd: IKCP_CMD_PUSH, wnd: 64, ts: ts, sn: 0, una: 0, data: []byte("hello ")},
		segment{conv: conv, cmd: IKCP_CMD_PUSH, wnd: 64, ts: ts, sn: 1, una: 0, data: []byte("strict ")},
		segment{conv: conv, cmd: IKCP_CMD_PUSH, wnd: 64, ts: ts, sn: 2, una: 0, data: []byte("world")},
		segment{conv: conv, cmd: IKCP_CMD_ACK, wnd: 64, ts: ts, sn: 0, una: 1},
	)

	legacy := newTestKCP(conv)
	strict := newTestKCP(conv)

	if ret := legacy.Input(dgram, IKCP_PACKET_REGULAR, false); ret != 0 {
		t.Fatalf("legacy Input returned %d", ret)
	}
	if err := strict.InputStrict(dgram, IKCP_PACKET_REGULAR, false); err != nil {
		t.Fatalf("InputStrict rejected a valid datagram: %v", err)
	}

	if got, want := snapshotKCP(strict), snapshotKCP(legacy); got != want {
		t.Fatalf("state mismatch after composite PUSH datagram:\nstrict: %+v\nlegacy: %+v", got, want)
	}

	// all three segments received in order and reassembled
	if strict.rcv_nxt != 3 {
		t.Fatalf("rcv_nxt=%d, want 3", strict.rcv_nxt)
	}
	// message mode: one Recv per message
	var received []byte
	buf := make([]byte, 128)
	for i := 0; i < 3; i++ {
		n := strict.Recv(buf)
		if n < 0 {
			t.Fatalf("Recv()=%d on message %d", n, i)
		}
		received = append(received, buf[:n]...)
	}
	if string(received) != "hello strict world" {
		t.Fatalf("received %q, want %q", received, "hello strict world")
	}
	// the trailing ACK (una=1) triggered an immediate flush, which emitted
	// and drained the 3 pending ACKs for the PUSH segments
	if len(strict.acklist) != 0 {
		t.Fatalf("acklist len=%d, want 0 after flush", len(strict.acklist))
	}
}

// TestInputStrictRejectsTruncated builds a datagram of a valid ACK followed
// by a truncated PUSH segment, and proves the rejection is atomic: the core
// state after the failed call is identical to the state before it.
func TestInputStrictRejectsTruncated(t *testing.T) {
	const conv = 0x99aabbcc

	ts := currentMs() + 10000
	ack := segment{conv: conv, cmd: IKCP_CMD_ACK, wnd: 99, ts: ts, sn: 0, una: 1}
	dgram := encodeSegments(ack)
	ackLen := len(dgram)

	// append a PUSH header claiming 10 payload bytes, but only provide 3
	push := segment{conv: conv, cmd: IKCP_CMD_PUSH, wnd: 64, ts: ts, sn: 0, una: 0, data: make([]byte, 10)}
	truncated := encodeSegments(push)
	dgram = append(dgram, truncated[:IKCP_OVERHEAD+3]...)

	kcp := newTestKCP(conv)
	before := snapshotKCP(kcp)

	err := kcp.InputStrict(dgram, IKCP_PACKET_REGULAR, false)
	if err == nil {
		t.Fatal("InputStrict accepted a datagram with a truncated tail segment")
	}
	var inputErr *InputError
	if !errors.As(err, &inputErr) {
		t.Fatalf("error type %T, want *InputError", err)
	}
	if inputErr.Offset != ackLen {
		t.Fatalf("InputError.Offset=%d, want %d (start of the truncated segment)", inputErr.Offset, ackLen)
	}
	if inputErr.Reason == "" {
		t.Fatal("InputError.Reason is empty")
	}

	if after := snapshotKCP(kcp); after != before {
		t.Fatalf("state mutated despite rejection:\nbefore: %+v\nafter:  %+v", before, after)
	}

	// the valid ACK prefix must NOT have been applied either
	if kcp.snd_buf.Len() != 1 {
		t.Fatalf("snd_buf len=%d, want 1 (ACK must not be applied)", kcp.snd_buf.Len())
	}
	if kcp.snd_una != 0 {
		t.Fatalf("snd_una=%d, want 0 (una must not advance)", kcp.snd_una)
	}
	if kcp.rmt_wnd == 99 {
		t.Fatal("rmt_wnd updated by a rejected datagram")
	}

	// the legacy entry keeps its partial-apply semantics on the same bytes
	legacy := newTestKCP(conv)
	if ret := legacy.Input(dgram, IKCP_PACKET_REGULAR, false); ret != -2 {
		t.Fatalf("legacy Input returned %d, want -2", ret)
	}
}

// TestInputStrictValidation covers the remaining rejection cases and their
// reported offsets.
func TestInputStrictValidation(t *testing.T) {
	const conv = 0x13572468
	valid := segment{conv: conv, cmd: IKCP_CMD_ACK, wnd: 1, sn: 0, una: 0}

	badConv := valid
	badConv.conv = conv + 1

	badCmd := valid
	badCmd.cmd = 0xff

	oversized := encodeSegments(segment{conv: conv, cmd: IKCP_CMD_PUSH, sn: 0, data: make([]byte, 8)})
	oversized = oversized[:IKCP_OVERHEAD+4] // header promises 8, only 4 present

	tests := []struct {
		name   string
		dgram  []byte
		offset int
	}{
		{"empty datagram", nil, 0},
		{"short datagram", make([]byte, IKCP_OVERHEAD-1), 0},
		{"conversation mismatch", encodeSegments(badConv), 0},
		{"unknown command", encodeSegments(badCmd), 0},
		{"truncated payload", oversized, 0},
		{"trailing garbage", append(encodeSegments(valid), 0xde, 0xad), IKCP_OVERHEAD},
		{"second segment bad conv", append(encodeSegments(valid), encodeSegments(badConv)...), IKCP_OVERHEAD},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kcp := newTestKCP(conv)
			before := snapshotKCP(kcp)

			err := kcp.InputStrict(tt.dgram, IKCP_PACKET_REGULAR, false)
			if err == nil {
				t.Fatal("InputStrict accepted an invalid datagram")
			}
			var inputErr *InputError
			if !errors.As(err, &inputErr) {
				t.Fatalf("error type %T, want *InputError", err)
			}
			if inputErr.Offset != tt.offset {
				t.Fatalf("offset=%d, want %d (reason: %q)", inputErr.Offset, tt.offset, inputErr.Reason)
			}
			if after := snapshotKCP(kcp); after != before {
				t.Fatalf("state mutated despite rejection:\nbefore: %+v\nafter:  %+v", before, after)
			}
		})
	}
}
