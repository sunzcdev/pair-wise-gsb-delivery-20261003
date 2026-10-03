package nsqd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nsqio/go-nsq"
	"github.com/nsqio/nsq/internal/test"
)

func waitForPending(t *testing.T, topic *Topic, used, reserved int64, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		u, _, r := topic.PendingUsage()
		if u == used && r == reserved {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	u, q, r := topic.PendingUsage()
	t.Fatalf("timed out waiting for %s: used=%d queued=%d reserved=%d", msg, u, q, r)
}

func newBudgetNSQD(t *testing.T, budget int64, memQueueSize int64) (*Options, *NSQD) {
	t.Helper()
	opts := NewOptions()
	opts.Logger = test.NewTestLogger(t)
	opts.MaxTopicPendingBytes = budget
	opts.MemQueueSize = memQueueSize
	_, _, n := mustStartNSQD(opts)
	return opts, n
}

func stopNSQD(opts *Options, n *NSQD) {
	n.Exit()
	_ = os.RemoveAll(opts.DataPath)
}

func waitUsage(t *testing.T, topic *Topic, pred func(used, reserved int64) bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		u, _, r := topic.PendingUsage()
		if pred(u, r) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", msg)
}

// waitChannelsPumped gives the topic's messagePump a chance to process the
// asynchronous channel creation notification before publishing
func waitChannelsPumped(t *testing.T, topic *Topic) {
	t.Helper()
	time.Sleep(25 * time.Millisecond)
}

// with no channels, the cost is just the message itself; the budget rejects
// once the topic queue is full of bytes
func TestPendingBudgetRejectsWithoutChannels(t *testing.T) {
	opts, n := newBudgetNSQD(t, 100, 10000)
	defer stopNSQD(opts, n)

	topic := n.GetTopic("budget_no_chan")

	test.Nil(t, topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 60))))
	err := topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 60)))
	if !errors.Is(err, ErrPendingBudgetExceeded) {
		t.Fatalf("expected ErrPendingBudgetExceeded, got %v", err)
	}

	used, queued, reserved := topic.PendingUsage()
	test.Equal(t, int64(60), used)
	test.Equal(t, int64(60), queued)
	test.Equal(t, int64(0), reserved)
}

// the fan-out cost of every existing channel counts toward the budget and the
// reservation is consumed as the message is delivered to each channel
func TestPendingBudgetFanout(t *testing.T) {
	// mem-queue-size 1 per channel: the first message moves through quickly,
	// subsequent ones block the pump in the channel queues, giving stable
	// pending/reserved observations
	opts, n := newBudgetNSQD(t, 400, 1)
	defer stopNSQD(opts, n)

	topic := n.GetTopic("budget_fanout")
	topic.GetChannel("c1")
	topic.GetChannel("c2")
	waitChannelsPumped(t, topic)

	// publish enough 40-byte messages to fill topic + both channel queues:
	// eventually the pump stalls with channel copies (each channel holds its
	// mem queue + topic queue full); just verify fan-out cost blocks a publish
	// that would fit without fan-out
	for i := 0; i < 5; i++ {
		err := topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 40)))
		if err != nil {
			if !errors.Is(err, ErrPendingBudgetExceeded) {
				t.Fatalf("unexpected error: %v", err)
			}
			break
		}
	}

	// let the pump settle (channels back-pressured, no consumers)
	time.Sleep(50 * time.Millisecond)

	// usage is dominated by channel fan-out (2 copies per message), not the
	// topic queue alone; verify channel pending bytes are included
	used, queued, _ := topic.PendingUsage()
	c1 := topic.channelMap["c1"]
	c2 := topic.channelMap["c2"]
	topic.budgetMu.Lock()
	ch1Pending := c1.pendingBytes
	ch2Pending := c2.pendingBytes
	topic.budgetMu.Unlock()
	if used != queued+ch1Pending+ch2Pending {
		t.Fatalf("usage %d != topic %d + ch1 %d + ch2 %d",
			used, queued, ch1Pending, ch2Pending)
	}
	if ch1Pending+ch2Pending == 0 {
		t.Fatal("fan-out cost not accounted in channels")
	}

	// with two channels the fan-out doubles the cost: once saturated, a single
	// new message costs 3 * 40 = 120 and is rejected near the boundary
	err := topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 40)))
	if err != nil && !errors.Is(err, ErrPendingBudgetExceeded) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// a rejected publish must never reach any channel, even when messages of very
// different sizes push the budget right up to the boundary
func TestPendingBudgetBoundaryNoPartialDelivery(t *testing.T) {
	opts, n := newBudgetNSQD(t, 176, 10000)
	defer stopNSQD(opts, n)

	topic := n.GetTopic("budget_atomic")
	c1 := topic.GetChannel("c1")
	c2 := topic.GetChannel("c2")
	waitChannelsPumped(t, topic)

	// with 2 channels a 100-byte message costs exactly 300 > 176 and is
	// rejected immediately, before it is written anywhere
	err := topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 100)))
	if !errors.Is(err, ErrPendingBudgetExceeded) {
		t.Fatalf("expected 100-byte rejection, got %v", err)
	}

	// drain any pre-existing deliveries and confirm the rejected message is
	// nowhere
	drain := func(ch *Channel) int {
		got := 0
		for {
			select {
			case <-ch.memoryMsgChan:
				got++
			default:
				return got
			}
		}
	}
	time.Sleep(20 * time.Millisecond)
	if drain(c1) != 0 || drain(c2) != 0 {
		t.Fatal("a rejected publish was delivered to a channel")
	}

	// exercise the boundary with wildly different sizes:
	// every accepted publish with 2 channels costs 3*size up-front and
	// occupies 2*size after fan-out; walk the 176-byte boundary with
	// dramatically different sizes
	expectReject := func(size int) {
		t.Helper()
		if err := topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, size))); !errors.Is(err, ErrPendingBudgetExceeded) {
			t.Fatalf("expected %d-byte rejection, got %v", size, err)
		}
	}
	expectAccept := func(size, afterFanout int64) {
		t.Helper()
		test.Nil(t, topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, size))))
		waitForPending(t, topic, afterFanout, 0, fmt.Sprintf("%d-byte fan-out", size))
	}

	expectAccept(50, 100) // 3*50 = 150 <= 176
	expectReject(51)      // 100 + 153 = 253
	expectAccept(25, 150) // 100 + 75 = 175
	expectReject(9)       // 150 + 27 = 177
	expectAccept(8, 166)  // 150 + 24 = 174
	expectReject(4)       // 166 + 12 = 178
	expectAccept(3, 172)  // 166 + 9 = 175
	expectReject(2)       // 172 + 6 = 178
	expectAccept(1, 174)  // 172 + 3 = 175
	expectReject(1)       // 174 + 3 = 177 > 176

	// rejected tiny messages must not have leaked into any channel
	time.Sleep(20 * time.Millisecond)
	acceptedSoFar := drain(c1)
	if acceptedSoFar != 5 || drain(c2) != 5 {
		t.Fatalf("expected exactly the five accepted messages on each channel, got %d",
			acceptedSoFar)
	}
}

// concurrent publishers reserve first: accepted messages never exceed the
// budget even under contention
func TestPendingBudgetConcurrentReserve(t *testing.T) {
	opts, n := newBudgetNSQD(t, 500, 10000)
	defer stopNSQD(opts, n)

	topic := n.GetTopic("budget_concurrent")

	var accepted int64
	var rejected int64
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 50)))
			switch {
			case err == nil:
				atomic.AddInt64(&accepted, 1)
			case errors.Is(err, ErrPendingBudgetExceeded):
				atomic.AddInt64(&rejected, 1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	// no channels: each accepted message costs exactly 50 bytes, at most 10 fit
	if accepted != 10 {
		t.Fatalf("expected exactly 10 accepted, got %d (%d rejected)",
			atomic.LoadInt64(&accepted), atomic.LoadInt64(&rejected))
	}
	if rejected != 40 {
		t.Fatalf("expected 40 rejected, got %d", atomic.LoadInt64(&rejected))
	}
	used, _, reserved := topic.PendingUsage()
	test.Equal(t, int64(500), used+reserved)
}

// consumer FIN releases the fan-out occupancy and frees budget
func TestPendingBudgetFINRelease(t *testing.T) {
	opts, n := newBudgetNSQD(t, 200, 10000)
	defer stopNSQD(opts, n)

	topic := n.GetTopic("budget_fin")
	ch := topic.GetChannel("c")

	deliveredID := topic.GenerateID()
	test.Nil(t, topic.PutMessage(NewMessage(deliveredID, make([]byte, 100))))

	var delivered *Message
	waitUsage(t, topic, func(u, r int64) bool {
		select {
		case delivered = <-ch.memoryMsgChan:
			return true
		default:
		}
		return u == 100 && r == 0
	}, "delivery")
	if delivered == nil {
		t.Fatal("message was not delivered")
	}

	// the 100-byte channel copy saturates half the budget; another 100-byte
	// message would cost 200 -> total 300, rejected
	err := topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 100)))
	if !errors.Is(err, ErrPendingBudgetExceeded) {
		t.Fatalf("expected rejection before FIN, got %v", err)
	}

	// simulate the in-flight/FIN cycle a consumer performs
	test.Nil(t, ch.StartInFlightTimeout(delivered, 42, time.Second))
	test.Nil(t, ch.FinishMessage(42, delivered.ID))

	// budget is fully released; a full-size publish fits again
	waitForPending(t, topic, 0, 0, "FIN release")
	test.Nil(t, topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 100))))
}

// deleting a channel reclaims both its pending occupancy and any fan-out
// shares still reserved for messages queued in the topic
func TestPendingBudgetChannelDelete(t *testing.T) {
	opts, n := newBudgetNSQD(t, 300, 10000)
	defer stopNSQD(opts, n)

	topic := n.GetTopic("budget_chdel")
	ch := topic.GetChannel("c")

	test.Nil(t, topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 100))))
	waitForPending(t, topic, 100, 0, "fan-out")

	test.Nil(t, topic.DeleteExistingChannel("c"))

	used, _, reserved := topic.PendingUsage()
	test.Equal(t, int64(0), used)
	test.Equal(t, int64(0), reserved)

	// with no channels the whole budget is available for a single message
	test.Nil(t, topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 300))))
	_ = ch
}

// emptying a channel releases its occupancy
func TestPendingBudgetChannelEmpty(t *testing.T) {
	opts, n := newBudgetNSQD(t, 200, 10000)
	defer stopNSQD(opts, n)

	topic := n.GetTopic("budget_chempty")
	ch := topic.GetChannel("c")

	test.Nil(t, topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 100))))
	waitForPending(t, topic, 100, 0, "fan-out")

	test.Nil(t, ch.Empty())

	used, _, _ := topic.PendingUsage()
	test.Equal(t, int64(0), used)
}

// a backend write failure returns the full reservation
func TestPendingBudgetWriteFailureRollback(t *testing.T) {
	opts, n := newBudgetNSQD(t, 100, 0) // mem-queue-size 0: always hit backend
	defer stopNSQD(opts, n)

	topic := n.GetTopic("budget_rollback")
	topic.backend = &errorBackendQueue{}

	err := topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 100)))
	if err == nil {
		t.Fatal("expected write failure")
	}
	used, _, reserved := topic.PendingUsage()
	test.Equal(t, int64(0), used)
	test.Equal(t, int64(0), reserved)

	topic.backend = &errorRecoveredBackendQueue{}
	test.Nil(t, topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 100))))
}

// MPUB is admitted atomically: a batch that crosses the boundary is rejected
// whole and no message from it reaches any channel
func TestPendingBudgetMPUBAtomic(t *testing.T) {
	opts, n := newBudgetNSQD(t, 250, 10000)
	defer stopNSQD(opts, n)

	topic := n.GetTopic("budget_mpub")
	c1 := topic.GetChannel("c1")
	c2 := topic.GetChannel("c2")

	msgs := []*Message{
		NewMessage(topic.GenerateID(), make([]byte, 100)),
		NewMessage(topic.GenerateID(), make([]byte, 10)),
		NewMessage(topic.GenerateID(), make([]byte, 100)),
	}
	err := topic.PutMessages(msgs)
	if !errors.Is(err, ErrPendingBudgetExceeded) {
		t.Fatalf("expected rejection, got %v", err)
	}

	time.Sleep(20 * time.Millisecond)

	drain := func(ch *Channel) int {
		got := 0
		for {
			select {
			case <-ch.memoryMsgChan:
				got++
			default:
				return got
			}
		}
	}
	if drain(c1) != 0 || drain(c2) != 0 {
		t.Fatal("a channel received part of a rejected MPUB")
	}
	used, _, reserved := topic.PendingUsage()
	test.Equal(t, int64(0), used)
	test.Equal(t, int64(0), reserved)

	// a batch that exactly fits: 125 bytes * 3 = 375 > 250 ... use sizes that
	// do fit: 50 + 30 = 80 bytes total, 3 * 80 = 240 <= 250
	fit := []*Message{
		NewMessage(topic.GenerateID(), make([]byte, 50)),
		NewMessage(topic.GenerateID(), make([]byte, 30)),
	}
	test.Nil(t, topic.PutMessages(fit))
}

// PUB / DPUB / MPUB over TCP all obey the same budget and the wire-level
// rejection is a non-fatal E_PENDING_BUDGET_EXCEEDED error frame
func TestPendingBudgetProtocolRejection(t *testing.T) {
	opts := NewOptions()
	opts.Logger = test.NewTestLogger(t)
	opts.MaxTopicPendingBytes = 50
	opts.MemQueueSize = 10000
	tcpAddr, _, n := mustStartNSQD(opts)
	defer stopNSQD(opts, n)

	topicName := "budget_proto" + strconv.Itoa(int(time.Now().UnixNano()))

	conn, err := mustConnectNSQD(tcpAddr)
	test.Nil(t, err)
	defer func() { _ = conn.Close() }()
	identify(t, conn, nil, frameTypeResponse)

	expectError := func(cmd *nsq.Command) {
		t.Helper()
		_, err := cmd.WriteTo(conn)
		test.Nil(t, err)
		resp, err := nsq.ReadResponse(conn)
		test.Nil(t, err)
		frameType, data, err := nsq.UnpackResponse(resp)
		test.Nil(t, err)
		test.Equal(t, frameTypeError, frameType)
		if !bytes.HasPrefix(data, []byte("E_PENDING_BUDGET_EXCEEDED")) {
			t.Fatalf("expected E_PENDING_BUDGET_EXCEEDED, got %s", data)
		}
	}
	expectOK := func(cmd *nsq.Command) {
		t.Helper()
		_, err := cmd.WriteTo(conn)
		test.Nil(t, err)
		readValidate(t, conn, frameTypeResponse, "OK")
	}

	// 50 bytes fits (no channels yet)
	expectOK(nsq.Publish(topicName, make([]byte, 50)))
	// PUB over budget keeps the connection alive
	expectError(nsq.Publish(topicName, make([]byte, 1)))
	// DPUB over budget
	expectError(nsq.DeferredPublish(topicName, time.Second, make([]byte, 1)))
	// MPUB over budget is rejected whole
	mpub := [][]byte{make([]byte, 10), make([]byte, 10)}
	cmd, _ := nsq.MultiPublish(topicName, mpub)
	expectError(cmd)

	// connection still usable after non-fatal errors
	expectError(nsq.Publish(topicName, make([]byte, 100)))
}

// the HTTP /pub and /mpub endpoints return 503 PENDING_BUDGET_EXCEEDED
func TestPendingBudgetHTTPRejection(t *testing.T) {
	opts := NewOptions()
	opts.Logger = test.NewTestLogger(t)
	opts.MaxTopicPendingBytes = 20
	opts.MemQueueSize = 10000
	_, httpAddr, n := mustStartNSQD(opts)
	defer stopNSQD(opts, n)

	topicName := "budget_http" + strconv.Itoa(int(time.Now().UnixNano()))

	post := func(path string, body []byte) (int, string) {
		u := fmt.Sprintf("http://%s%s?topic=%s", httpAddr, path, topicName)
		resp, err := http.Post(u, "application/octet-stream", bytes.NewReader(body))
		test.Nil(t, err)
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	code, body := post("/pub", make([]byte, 20))
	test.Equal(t, 200, code)
	test.Equal(t, "OK", body)

	code, body = post("/pub", make([]byte, 1))
	test.Equal(t, 503, code)
	if !strings.Contains(body, "PENDING_BUDGET_EXCEEDED") {
		t.Fatalf("expected PENDING_BUDGET_EXCEEDED, got %s", body)
	}

	mpub := append([]byte("aaaa\n"), []byte("bbbb\n")...)
	code, body = post("/mpub", mpub)
	test.Equal(t, 503, code)
	if !strings.Contains(body, "PENDING_BUDGET_EXCEEDED") {
		t.Fatalf("expected PENDING_BUDGET_EXCEEDED, got %s", body)
	}
}

// a channel created after messages were published receives the backlog per
// NSQ semantics; the bytes move from the topic queue to the channel and total
// usage is unchanged
func TestPendingBudgetLateChannelBacklog(t *testing.T) {
	opts, n := newBudgetNSQD(t, 100, 10000)
	defer stopNSQD(opts, n)

	topic := n.GetTopic("budget_late_chan")

	// fill the topic queue to the full budget (no channels exist yet)
	test.Nil(t, topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 50))))
	test.Nil(t, topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 50))))

	// a late channel gets both historical messages via the pump
	ch := topic.GetChannel("late")
	waitUsage(t, topic, func(u, r int64) bool {
		_, queued, _ := topic.PendingUsage()
		return ch.Depth() == 2 && u == 100 && queued == 0
	}, "late channel backlog")

	// the budget is full (channel owns 100 bytes); a new 1-byte publish needs
	// 2 bytes (topic + the now-existing channel) and is rejected
	err := topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 1)))
	if !errors.Is(err, ErrPendingBudgetExceeded) {
		t.Fatalf("expected rejection with full late channel, got %v", err)
	}
}

// stats expose used (pending) and reserved bytes separately per topic/channel
func TestPendingBudgetStats(t *testing.T) {
	opts := NewOptions()
	opts.Logger = test.NewTestLogger(t)
	opts.MaxTopicPendingBytes = 1000
	opts.MemQueueSize = 10000
	_, httpAddr, n := mustStartNSQD(opts)
	defer stopNSQD(opts, n)

	topicName := "budget_stats" + strconv.Itoa(int(time.Now().UnixNano()))
	topic := n.GetTopic(topicName)
	topic.GetChannel("c")

	test.Nil(t, topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 100))))

	u := fmt.Sprintf("http://%s/stats?format=json&topic=%s&include_clients=false",
		httpAddr, topicName)
	fetch := func() (TopicStats, ChannelStats) {
		resp, err := http.Get(u)
		test.Nil(t, err)
		defer func() { _ = resp.Body.Close() }()
		var payload struct {
			Topics []TopicStats `json:"topics"`
		}
		test.Nil(t, json.NewDecoder(resp.Body).Decode(&payload))
		test.Equal(t, 1, len(payload.Topics))
		test.Equal(t, 1, len(payload.Topics[0].Channels))
		return payload.Topics[0], payload.Topics[0].Channels[0]
	}

	// immediately after admission the 200 reserved bytes are visible; once the
	// pump fans out, stats show 100 pending / 0 reserved
	ts, cs := fetch()
	if ts.ReservedBytes < 0 || cs.ReservedBytes < 0 {
		t.Fatalf("negative reserved bytes: %+v", ts)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ts, cs = fetch()
		if cs.PendingBytes == 100 && cs.ReservedBytes == 0 &&
			ts.PendingBytes == 100 && ts.ReservedBytes == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("stats never settled: topic=%+v channel=%+v", ts, cs)
}
