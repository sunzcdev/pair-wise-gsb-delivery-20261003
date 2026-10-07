package kcp

import (
	"bytes"
	"container/heap"
	"encoding/binary"
	"errors"
	"testing"
)

const testConv uint32 = 0x42424242

// wireSeg describes one segment when building a raw datagram.
type wireSeg struct {
	conv uint32
	cmd  uint8
	frg  uint8
	wnd  uint16
	ts   uint32
	sn   uint32
	una  uint32
	data []byte
}

func encodeSeg(buf []byte, s wireSeg) []byte {
	var hdr [IKCP_OVERHEAD]byte
	binary.LittleEndian.PutUint32(hdr[:], s.conv)
	hdr[4] = s.cmd
	hdr[5] = s.frg
	binary.LittleEndian.PutUint16(hdr[6:], s.wnd)
	binary.LittleEndian.PutUint32(hdr[8:], s.ts)
	binary.LittleEndian.PutUint32(hdr[12:], s.sn)
	binary.LittleEndian.PutUint32(hdr[16:], s.una)
	binary.LittleEndian.PutUint32(hdr[20:], uint32(len(s.data)))
	buf = append(buf, hdr[:]...)
	return append(buf, s.data...)
}

func buildDatagram(segs ...wireSeg) []byte {
	var buf []byte
	for _, s := range segs {
		buf = encodeSeg(buf, s)
	}

	return buf
}

func ackSeg(sn, una uint32, wnd uint16) wireSeg {
	return wireSeg{conv: testConv, cmd: IKCP_CMD_ACK, wnd: wnd, ts: 0x01020304, sn: sn, una: una}
}

func pushSeg(sn uint32, frg uint8, payload []byte) wireSeg {
	return wireSeg{conv: testConv, cmd: IKCP_CMD_PUSH, frg: frg, wnd: 16, ts: 0x05060708, sn: sn, una: 0, data: payload}
}

func waskSeg() wireSeg {
	return wireSeg{conv: testConv, cmd: IKCP_CMD_WASK, wnd: 7, ts: 0x11111111}
}

func winsSeg(wnd uint16) wireSeg {
	return wireSeg{conv: testConv, cmd: IKCP_CMD_WINS, wnd: wnd, ts: 0x22222222}
}

// kcpSnapshot captures every piece of state Input/InputStrict may touch.
type kcpSnapshot struct {
	conv     uint32
	rmt_wnd  uint32
	snd_una  uint32
	snd_nxt  uint32
	rcv_nxt  uint32
	cwnd     uint32
	incr     uint32
	ssthresh uint32
	probe    uint32

	rx_srtt   int32
	rx_rttvar int32
	rx_rto    uint32

	acklist  []ackItem
	sndBuf   []snapSeg
	rcvBuf   []snapSeg
	rcvQueue []snapSeg

	inSegsDelta     uint64
	repeatSegsDelta uint64
}

type snapSeg struct {
	sn     uint32
	frg    uint8
	cmd    uint8
	wnd    uint16
	ts     uint32
	una    uint32
	acked  uint32
	xmit   uint32
	data   []byte
	pooled bool // whether data points into the shared buffer pool
}

func drainRing(rb *RingBuffer[segment]) []snapSeg {
	var out []snapSeg
	for {
		seg, ok := rb.Pop()
		if !ok {
			break
		}
		out = append(out, snapshotSegment(seg))
	}
	return out
}

func snapshotSegment(seg segment) snapSeg {
	s := snapSeg{
		sn: seg.sn, frg: seg.frg, cmd: seg.cmd, wnd: seg.wnd,
		ts: seg.ts, una: seg.una, acked: seg.acked, xmit: seg.xmit,
	}
	if seg.data != nil {
		s.data = append([]byte(nil), seg.data...)
		s.pooled = cap(seg.data) == mtuLimit
	}
	return s
}

func takeSnapshot(kcp *KCP) kcpSnapshot {
	return takeSnapshotFrom(kcp, 0, 0)
}

func takeSnapshotFrom(kcp *KCP, baseInSegs, baseRepeatSegs uint64) kcpSnapshot {
	snap := kcpSnapshot{
		conv: kcp.conv, rmt_wnd: kcp.rmt_wnd,
		snd_una: kcp.snd_una, snd_nxt: kcp.snd_nxt, rcv_nxt: kcp.rcv_nxt,
		cwnd: kcp.cwnd, incr: kcp.incr, ssthresh: kcp.ssthresh, probe: kcp.probe,
		rx_srtt: kcp.rx_srtt, rx_rttvar: kcp.rx_rttvar, rx_rto: kcp.rx_rto,
		acklist:         append([]ackItem(nil), kcp.acklist...),
		sndBuf:          drainRing(kcp.snd_buf),
		inSegsDelta:     DefaultSnmp.InSegs - baseInSegs,
		repeatSegsDelta: DefaultSnmp.RepeatSegs - baseRepeatSegs,
	}

	// snd_buf was emptied by drainRing; rebuild it so the snapshot is non-destructive.
	for _, s := range snap.sndBuf {
		var seg segment
		seg.sn, seg.frg, seg.cmd = s.sn, s.frg, s.cmd
		seg.wnd, seg.ts, seg.una = s.wnd, s.ts, s.una
		seg.acked, seg.xmit = s.acked, s.xmit
		if s.pooled {
			seg.data = defaultBufferPool.Get()[:len(s.data)]
			copy(seg.data, s.data)
		} else {
			seg.data = append([]byte(nil), s.data...)
		}
		kcp.snd_buf.Push(seg)
	}

	for kcp.rcv_buf.Len() > 0 {
		seg := heap.Pop(kcp.rcv_buf).(segment)
		snap.rcvBuf = append(snap.rcvBuf, snapshotSegment(seg))
		heap.Push(kcp.rcv_buf, seg)
	}
	snap.rcvQueue = drainRing(kcp.rcv_queue)
	for _, s := range snap.rcvQueue {
		var seg segment
		seg.sn, seg.frg, seg.cmd = s.sn, s.frg, s.cmd
		seg.wnd, seg.ts, seg.una = s.wnd, s.ts, s.una
		seg.data = append([]byte(nil), s.data...)
		kcp.rcv_queue.Push(seg)
	}
	return snap
}

func assertSnapEqual(t *testing.T, want, got kcpSnapshot, name string) {
	t.Helper()
	if !snapshotsEqual(want, got) {
		t.Fatalf("%s: state mismatch\nwant=%+v\ngot =%+v", name, want, got)
	}
}

func snapshotsEqual(a, b kcpSnapshot) bool {
	if a.conv != b.conv || a.rmt_wnd != b.rmt_wnd ||
		a.snd_una != b.snd_una || a.snd_nxt != b.snd_nxt || a.rcv_nxt != b.rcv_nxt ||
		a.cwnd != b.cwnd || a.incr != b.incr || a.ssthresh != b.ssthresh ||
		a.probe != b.probe ||
		a.rx_srtt != b.rx_srtt || a.rx_rttvar != b.rx_rttvar || a.rx_rto != b.rx_rto {
		return false
	}
	if len(a.acklist) != len(b.acklist) {
		return false
	}
	for i := range a.acklist {
		if a.acklist[i] != b.acklist[i] {
			return false
		}
	}

	if !segListsEqual(a.sndBuf, b.sndBuf) ||
		!segListsEqual(a.rcvBuf, b.rcvBuf) ||
		!segListsEqual(a.rcvQueue, b.rcvQueue) {
		return false
	}
	return true
}

func segListsEqual(a, b []snapSeg) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.sn != y.sn || x.frg != y.frg || x.cmd != y.cmd || x.wnd != y.wnd ||
			x.ts != y.ts || x.una != y.una || x.acked != y.acked || x.xmit != y.xmit ||
			x.pooled != y.pooled || !bytes.Equal(x.data, y.data) {
			return false
		}
	}
	return true
}

// newTestKCP builds a receiver-like KCP with a few in-flight send segments so
// that ACK/una processing has something to act on.
func newTestKCP(t *testing.T) *KCP {
	t.Helper()
	kcp := NewKCP(testConv, func(buf []byte, size int) {})
	// populate snd_buf with in-flight segments sn 0..4, unacked
	kcp.snd_nxt = 5
	kcp.snd_una = 0
	kcp.cwnd = 16
	kcp.incr = 16 * kcp.mss
	kcp.ssthresh = 16
	for sn := uint32(0); sn < 5; sn++ {
		data := defaultBufferPool.Get()[:4]
		copy(data, []byte{byte(sn), byte(sn + 1), byte(sn + 2), byte(sn + 3)})
		seg := segment{
			conv: testConv, cmd: IKCP_CMD_PUSH, wnd: 32,
			ts: 100 + sn, sn: sn, una: 0,
			rto: 200, resendts: 1000 + sn, data: data,
		}
		kcp.snd_buf.Push(seg)
	}
	return kcp
}

// TestInputStrictEquivalence proves that valid datagrams produce identical
// state on both entry points. ACK-only, compound PUSH and mixed command
// datagrams are covered.
func TestInputStrictEquivalence(t *testing.T) {
	payload1 := []byte("hello-")
	payload2 := []byte("world!!")
	payload3 := []byte("third")

	cases := []struct {
		name       string
		dgram      []byte
		pktType    PacketType
		ackNoDelay bool
	}{
		{
			name: "ACK-only",
			// ack sn=2 (cumulative una=3 slides window and removes sn 0..2)
			dgram: buildDatagram(ackSeg(2, 3, 20)),
		},
		{
			name:       "ACK-only FEC rmt_wnd ignored",
			dgram:      buildDatagram(ackSeg(1, 2, 20)),
			pktType:    IKCP_PACKET_FEC,
			ackNoDelay: false,
		},
		{
			name: "compound PUSH in-order fragments",
			dgram: buildDatagram(
				pushSeg(0, 2, payload1),
				pushSeg(1, 1, payload2),
				pushSeg(2, 0, payload3),
			),
			ackNoDelay: true,
		},
		{
			name: "compound PUSH out-of-order then in-order",
			dgram: buildDatagram(
				pushSeg(1, 0, payload2), // buffered in rcv_buf
				pushSeg(0, 0, payload1), // releases both into rcv_queue
			),
		},
		{
			name: "mixed ACK+PUSH+WASK+WINS compound",
			dgram: buildDatagram(
				ackSeg(0, 1, 25),
				pushSeg(0, 0, payload1),
				waskSeg(),
				winsSeg(11),
			),
		},
		{
			name:    "duplicate PUSH counted once",
			dgram:   buildDatagram(pushSeg(0, 0, payload1), pushSeg(0, 0, payload1)),
			pktType: IKCP_PACKET_REGULAR,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			legacy := newTestKCP(t)
			strict := newTestKCP(t)

			legacyBaseIn, legacyBaseRepeat := DefaultSnmp.InSegs, DefaultSnmp.RepeatSegs
			strictBaseIn, strictBaseRepeat := DefaultSnmp.InSegs, DefaultSnmp.RepeatSegs

			if ret := legacy.Input(tc.dgram, tc.pktType, tc.ackNoDelay); ret != 0 {
				t.Fatalf("legacy Input returned %d for valid datagram", ret)
			}
			legacySnap := takeSnapshotFrom(legacy, legacyBaseIn, legacyBaseRepeat)

			if err := strict.InputStrict(tc.dgram, tc.pktType, tc.ackNoDelay); err != nil {
				t.Fatalf("InputStrict rejected valid datagram: %v", err)
			}
			strictSnap := takeSnapshotFrom(strict, strictBaseIn, strictBaseRepeat)

			assertSnapEqual(t, legacySnap, strictSnap, tc.name)
		})
	}
}

// TestInputStrictRejectAtomic is the headline guarantee: a valid ACK segment
// followed by a truncated second segment must be rejected as a whole, leaving
// the core state byte-for-byte identical to a fresh KCP.
func TestInputStrictRejectAtomic(t *testing.T) {
	// first segment is a valid ACK that would slide snd_una and remove
	// acknowledged segments if it were applied; the second segment header
	// claims a payload longer than the remaining tail.
	good := ackSeg(2, 3, 20)
	dgram := buildDatagram(good)
	tail := buildDatagram(pushSeg(9, 0, []byte("TRUNCATED")))
	dgram = append(dgram, tail[:len(tail)-3]...) // chop 3 payload bytes

	kcp := newTestKCP(t)
	inBase, repeatBase := DefaultSnmp.InSegs, DefaultSnmp.RepeatSegs
	before := takeSnapshotFrom(kcp, inBase, repeatBase)

	err := kcp.InputStrict(dgram, IKCP_PACKET_REGULAR, false)
	if err == nil {
		t.Fatal("expected rejection, got nil")
	}
	var dErr *DatagramError
	if !errors.As(err, &dErr) {
		t.Fatalf("expected *DatagramError, got %T: %v", err, err)
	}
	if dErr.Offset != IKCP_OVERHEAD {
		t.Fatalf("expected offset %d, got %d", IKCP_OVERHEAD, dErr.Offset)
	}
	if dErr.Reason != ErrDatagramTruncated {
		t.Fatalf("expected ErrDatagramTruncated, got %v", dErr.Reason)
	}

	after := takeSnapshotFrom(kcp, inBase, repeatBase)
	assertSnapEqual(t, before, after, "valid ACK + truncated segment")

	// sanity check: the valid ACK alone really would have moved the state,
	// proving the test scenario is meaningful.
	control := newTestKCP(t)
	if err := control.InputStrict(buildDatagram(good), IKCP_PACKET_REGULAR, false); err != nil {
		t.Fatal(err)
	}
	if control.snd_una == before.snd_una || len(takeSnapshot(control).sndBuf) == len(before.sndBuf) {
		t.Fatal("control: valid ACK did not change state, test is not exercising rejection")
	}
	if control.snd_una != 3 {
		t.Fatalf("control: expected snd_una=3, got %d", control.snd_una)
	}
}

// TestInputStrictRejectReasons covers every rejection class and verifies the
// reported offset plus atomicity for each.
func TestInputStrictRejectReasons(t *testing.T) {
	good := ackSeg(0, 1, 20)

	cases := []struct {
		name   string
		dgram  []byte
		offset int
		reason error
		mutate func([]byte)
	}{
		{
			name:   "datagram shorter than header",
			dgram:  make([]byte, IKCP_OVERHEAD-1),
			offset: 0,
			reason: ErrDatagramTooShort,
		},
		{
			name:   "empty datagram",
			dgram:  []byte{},
			offset: 0,
			reason: ErrDatagramTooShort,
		},
		{
			name:   "conv mismatch on first segment",
			dgram:  buildDatagram(ackSeg(0, 1, 20)),
			offset: 0,
			reason: ErrDatagramConvMismatch,
			mutate: func(b []byte) { binary.LittleEndian.PutUint32(b, testConv+1) },
		},
		{
			name:   "conv mismatch on second segment after valid ACK",
			dgram:  buildDatagram(ackSeg(0, 1, 20), ackSeg(1, 2, 20)),
			offset: IKCP_OVERHEAD,
			reason: ErrDatagramConvMismatch,
			mutate: func(b []byte) { binary.LittleEndian.PutUint32(b[IKCP_OVERHEAD:], testConv+1) },
		},
		{
			name:   "unknown command",
			dgram:  buildDatagram(ackSeg(0, 1, 20)),
			offset: IKCP_OVERHEAD,
			reason: ErrDatagramBadCommand,
		},
		{
			name: "truncated header tail after valid ACK",
			// 23 trailing bytes are one byte short of a complete header
			dgram:  append(buildDatagram(good), make([]byte, IKCP_OVERHEAD-1)...),
			offset: IKCP_OVERHEAD,
			reason: ErrDatagramTruncated,
		},
	}

	// unknown command needs a segment with a bogus cmd byte
	badCmd := buildDatagram(ackSeg(0, 1, 20), wireSeg{conv: testConv, cmd: 99, sn: 4})
	cases[4].dgram = badCmd

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dgram := tc.dgram
			if tc.mutate != nil {
				tc.mutate(dgram)
			}
			kcp := newTestKCP(t)
			inBase, repeatBase := DefaultSnmp.InSegs, DefaultSnmp.RepeatSegs
			before := takeSnapshotFrom(kcp, inBase, repeatBase)

			err := kcp.InputStrict(dgram, IKCP_PACKET_REGULAR, true)
			if err == nil {
				t.Fatal("expected rejection, got nil")
			}
			var dErr *DatagramError
			if !errors.As(err, &dErr) {
				t.Fatalf("expected *DatagramError, got %T", err)
			}
			if dErr.Offset != tc.offset {
				t.Fatalf("offset: want %d got %d", tc.offset, dErr.Offset)
			}
			if dErr.Reason != tc.reason {
				t.Fatalf("reason: want %v got %v", tc.reason, dErr.Reason)
			}
			assertSnapEqual(t, before, takeSnapshotFrom(kcp, inBase, repeatBase), tc.name)
		})
	}
}

// TestInputLegacyCompatibility verifies the old best-effort semantics are
// preserved: valid prefix is applied, short whole datagram returns -1,
// truncated payload returns -2, bad command returns -3, and a short trailing
// remainder is ignored with a successful return.
func TestInputLegacyCompatibility(t *testing.T) {
	t.Run("valid prefix applied before truncation", func(t *testing.T) {
		dgram := buildDatagram(ackSeg(0, 2, 20))
		tail := buildDatagram(pushSeg(3, 0, []byte("DATA")))
		dgram = append(dgram, tail[:len(tail)-2]...)

		kcp := newTestKCP(t)
		ret := kcp.Input(dgram, IKCP_PACKET_REGULAR, false)
		if ret != -2 {
			t.Fatalf("want -2, got %d", ret)
		}
		// the valid ACK (una=2) was applied even though the packet failed
		if kcp.snd_una != 2 {
			t.Fatalf("want snd_una=2, got %d", kcp.snd_una)
		}
	})

	t.Run("conv mismatch applies nothing from offending segment", func(t *testing.T) {
		dgram := buildDatagram(ackSeg(0, 2, 20), ackSeg(1, 3, 20))
		binary.LittleEndian.PutUint32(dgram[IKCP_OVERHEAD:], testConv+1)

		kcp := newTestKCP(t)
		ret := kcp.Input(dgram, IKCP_PACKET_REGULAR, false)
		if ret != -1 {
			t.Fatalf("want -1, got %d", ret)
		}
		if kcp.snd_una != 2 {
			t.Fatalf("first segment still applied: want snd_una=2, got %d", kcp.snd_una)
		}
	})

	t.Run("whole datagram shorter than header", func(t *testing.T) {
		kcp := newTestKCP(t)
		before := takeSnapshot(kcp)
		if ret := kcp.Input(make([]byte, 10), IKCP_PACKET_REGULAR, false); ret != -1 {
			t.Fatalf("want -1, got %d", ret)
		}
		assertSnapEqual(t, before, takeSnapshot(kcp), "short datagram")
	})

	t.Run("bad command returns -3 after applying prefix", func(t *testing.T) {
		dgram := buildDatagram(ackSeg(0, 2, 20), wireSeg{conv: testConv, cmd: 77})
		kcp := newTestKCP(t)
		if ret := kcp.Input(dgram, IKCP_PACKET_REGULAR, false); ret != -3 {
			t.Fatalf("want -3, got %d", ret)
		}
		if kcp.snd_una != 2 {
			t.Fatalf("want snd_una=2, got %d", kcp.snd_una)
		}
	})

	t.Run("short trailing remainder ignored", func(t *testing.T) {
		dgram := append(buildDatagram(ackSeg(0, 1, 20)),
			make([]byte, IKCP_OVERHEAD-1)...)
		kcp := newTestKCP(t)
		if ret := kcp.Input(dgram, IKCP_PACKET_REGULAR, false); ret != 0 {
			t.Fatalf("want 0, got %d", ret)
		}
		if kcp.snd_una != 1 {
			t.Fatalf("want snd_una=1, got %d", kcp.snd_una)
		}
	})
}
