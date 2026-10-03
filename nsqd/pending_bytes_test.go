package nsqd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nsqio/go-nsq"
	"github.com/nsqio/nsq/internal/test"
)

func pendingTestOpts(t *testing.T, maxPendingBytes int64) *Options {
	opts := NewOptions()
	opts.Logger = test.NewTestLogger(t)
	opts.MemQueueSize = 10000
	opts.MaxTopicPendingBytes = maxPendingBytes
	return opts
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitForChannelPending(t *testing.T, c *Channel, want int64) {
	t.Helper()
	waitFor(t, fmt.Sprintf("channel %s pending=%d", c.name, want), func() bool {
		return c.PendingBytes() == want
	})
}

func waitForTopicUsed(t *testing.T, topic *Topic, want int64) {
	t.Helper()
	waitFor(t, fmt.Sprintf("topic %s used=%d", topic.name, want), func() bool {
		used, reserved, _ := topic.PendingByteStats()
		return used == want && reserved == 0
	})
}

// one published message costs one topic copy plus one copy per channel;
// once delivered the bytes are counted against the receiving channels.
func TestPendingBytesFanoutAccounting(t *testing.T) {
	opts := pendingTestOpts(t, 0)
	_, _, nsqd := mustStartNSQD(opts)
	defer func() { _ = os.RemoveAll(opts.DataPath) }()
	defer nsqd.Exit()

	topicName := "test_pending_fanout" + strconv.Itoa(int(time.Now().UnixNano()))
	topic := nsqd.GetTopic(topicName)
	c1 := topic.GetChannel("c1")
	c2 := topic.GetChannel("c2")
	test.Nil(t, topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 100))))

	waitForChannelPending(t, c1, 100)
	waitForChannelPending(t, c2, 100)
	waitForTopicUsed(t, topic, 200)

	used, reserved, limit := topic.PendingByteStats()
	test.Equal(t, int64(200), used)
	test.Equal(t, int64(0), reserved)
	test.Equal(t, int64(0), limit) // unlimited

	stats := nsqd.GetStats(topicName, "", false).Topics
	test.Equal(t, 1, len(stats))
	test.Equal(t, int64(200), stats[0].PendingBytes)
	test.Equal(t, int64(0), stats[0].PendingBytesReserved)
	test.Equal(t, 2, len(stats[0].Channels))
	for _, cs := range stats[0].Channels {
		test.Equal(t, int64(100), cs.PendingBytes)
	}
}

// while messages sit on the topic (paused) the used bytes include the
// topic copy plus one reserved copy per existing channel.
func TestPendingBytesPausedTopic(t *testing.T) {
	opts := pendingTestOpts(t, 0)
	_, _, nsqd := mustStartNSQD(opts)
	defer func() { _ = os.RemoveAll(opts.DataPath) }()
	defer nsqd.Exit()

	topicName := "test_pending_paused" + strconv.Itoa(int(time.Now().UnixNano()))
	topic := nsqd.GetTopic(topicName)
	topic.GetChannel("c1")
	topic.GetChannel("c2")
	test.Nil(t, topic.Pause())
	// let the messagePump observe the paused state
	waitFor(t, "paused pump", func() bool {
		return topic.IsPaused() && topic.Depth() == 0
	})
	time.Sleep(20 * time.Millisecond)

	test.Nil(t, topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 50))))
	test.Nil(t, topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 150))))

	// (50+150) topic bytes, twice for two channels => 3*200
	used, reserved, _ := topic.PendingByteStats()
	test.Equal(t, int64(600), used)
	test.Equal(t, int64(0), reserved)
	test.Equal(t, int64(2), topic.Depth())
}

// disparate message sizes fill the budget right up to the boundary and
// the next publish is rejected atomically (no partial acceptance).
func TestPendingByteBudgetBoundary(t *testing.T) {
	opts := pendingTestOpts(t, 1000)
	_, _, nsqd := mustStartNSQD(opts)
	defer func() { _ = os.RemoveAll(opts.DataPath) }()
	defer nsqd.Exit()

	topicName := "test_pending_boundary" + strconv.Itoa(int(time.Now().UnixNano()))
	topic := nsqd.GetTopic(topicName)
	test.Nil(t, topic.Pause())
	topic.GetChannel("c1")

	put := func(size int) error {
		return topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, size)))
	}

	// one channel => every body byte costs 2
	test.Nil(t, put(300)) // cost 600, used 600
	test.Nil(t, put(200)) // cost 400, used 1000 (exactly at the boundary)

	if err := put(1); err != ErrPendingByteBudgetExceeded {
		t.Fatalf("want ErrPendingByteBudgetExceeded, got %v", err)
	}
	if err := put(500); err != ErrPendingByteBudgetExceeded {
		t.Fatalf("want ErrPendingByteBudgetExceeded, got %v", err)
	}

	// exact-size message still fits (cost 0 bytes is rejected earlier,
	// but 0 remaining means only a zero-byte message would)
	used, _, limit := topic.PendingByteStats()
	test.Equal(t, int64(1000), used)
	test.Equal(t, int64(1000), limit)
	test.Equal(t, int64(2), topic.Depth())

	// batch reserve is also atomic: neither message lands and no
	// channel receives anything
	msgs := []*Message{
		NewMessage(topic.GenerateID(), make([]byte, 1)),
		NewMessage(topic.GenerateID(), make([]byte, 1)),
	}
	test.Equal(t, ErrPendingByteBudgetExceeded, topic.PutMessages(msgs))
	test.Equal(t, int64(2), topic.Depth())

	// reservations of failed publishes are returned
	_, reserved, _ := topic.PendingByteStats()
	test.Equal(t, int64(0), reserved)
}

// concurrent publishes never oversubscribe the budget; losers see the
// budget error and winners' reservations are all accounted for.
func TestPendingByteConcurrentReserve(t *testing.T) {
	opts := pendingTestOpts(t, 1000)
	_, _, nsqd := mustStartNSQD(opts)
	defer func() { _ = os.RemoveAll(opts.DataPath) }()
	defer nsqd.Exit()

	topicName := "test_pending_conc" + strconv.Itoa(int(time.Now().UnixNano()))
	topic := nsqd.GetTopic(topicName)
	test.Nil(t, topic.Pause())
	topic.GetChannel("c1") // cost factor 2

	const body = 100 // cost 200 each
	const n = 50
	var wg sync.WaitGroup
	var accepted, rejected int64
	var mux sync.Mutex
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, body)))
			mux.Lock()
			if err == nil {
				accepted++
			} else if err == ErrPendingByteBudgetExceeded {
				rejected++
			} else {
				t.Errorf("unexpected err %v", err)
			}
			mux.Unlock()
		}()
	}
	wg.Wait()

	test.Equal(t, int64(5), accepted) // 5 * 200 == 1000
	test.Equal(t, int64(n-5), rejected)
	used, reserved, _ := topic.PendingByteStats()
	test.Equal(t, int64(1000), used)
	test.Equal(t, int64(0), reserved)
	test.Equal(t, int64(5), topic.Depth())
}

// a backend write failure must return the reservation; the budget is
// reusable for a later successful publish.
func TestPendingByteWriteFailureReturnsReservation(t *testing.T) {
	opts := pendingTestOpts(t, 1000)
	opts.MemQueueSize = 0
	_, _, nsqd := mustStartNSQD(opts)
	defer func() { _ = os.RemoveAll(opts.DataPath) }()
	defer nsqd.Exit()

	topicName := "test_pending_writefail" + strconv.Itoa(int(time.Now().UnixNano()))
	topic := nsqd.GetTopic(topicName)
	topic.backend = &errorBackendQueue{}

	err := topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 400)))
	test.NotNil(t, err)

	used, reserved, _ := topic.PendingByteStats()
	test.Equal(t, int64(0), used)
	test.Equal(t, int64(0), reserved)

	// after recovery the full budget is available again
	topic.backend = &errorRecoveredBackendQueue{}
	test.Nil(t, topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 900))))
	waitForTopicUsed(t, topic, 900)
}

// FIN (consumer acknowledgement) releases the channel-held bytes back to
// the budget, allowing new publishes.
func TestPendingByteFINReleases(t *testing.T) {
	opts := pendingTestOpts(t, 1000)
	tcpAddr, _, nsqd := mustStartNSQD(opts)
	defer func() { _ = os.RemoveAll(opts.DataPath) }()
	defer nsqd.Exit()

	topicName := "test_pending_fin" + strconv.Itoa(int(time.Now().UnixNano()))
	topic := nsqd.GetTopic(topicName)
	channel := topic.GetChannel("ch")

	// fill the budget with 10 channel-held messages of 100 bytes each
	// (cost factor 1 at publish time because no channels existed yet is
	// not the case here, so create channel first and use cost 2)
	for i := 0; i < 5; i++ {
		test.Nil(t, topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 100))))
	}
	waitForChannelPending(t, channel, 500)
	waitForTopicUsed(t, topic, 500)

	// budget exhausted
	test.Equal(t, ErrPendingByteBudgetExceeded,
		topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 251))))

	conn, err := mustConnectNSQD(tcpAddr)
	test.Nil(t, err)
	defer func() { _ = conn.Close() }()
	identify(t, conn, nil, frameTypeResponse)
	sub(t, conn, topicName, "ch")

	for i := 0; i < 5; i++ {
		_, err = nsq.Ready(1).WriteTo(conn)
		test.Nil(t, err)
		id := drainMessageID(t, conn)
		_, err = nsq.Finish(nsq.MessageID(id)).WriteTo(conn)
		test.Nil(t, err)
	}

	waitForChannelPending(t, channel, 0)
	waitForTopicUsed(t, topic, 0)

	// budget is usable again
	test.Nil(t, topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 100))))
	waitForChannelPending(t, channel, 100)
}

func drainMessageID(t *testing.T, conn net.Conn) MessageID {
	resp, err := nsq.ReadResponse(conn)
	test.Nil(t, err)
	_, data, err := nsq.UnpackResponse(resp)
	test.Nil(t, err)
	msg, err := decodeMessage(data)
	test.Nil(t, err)
	return msg.ID
}

// deleting a channel releases the bytes it held and its share of
// reservations for topic backlog.
func TestPendingByteChannelDeleteReleases(t *testing.T) {
	opts := pendingTestOpts(t, 0)
	_, _, nsqd := mustStartNSQD(opts)
	defer func() { _ = os.RemoveAll(opts.DataPath) }()
	defer nsqd.Exit()

	topicName := "test_pending_chdelete" + strconv.Itoa(int(time.Now().UnixNano()))
	topic := nsqd.GetTopic(topicName)
	c1 := topic.GetChannel("c1")
	c2 := topic.GetChannel("c2")

	test.Nil(t, topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 100))))
	waitForChannelPending(t, c1, 100)
	waitForChannelPending(t, c2, 100)
	waitForTopicUsed(t, topic, 200)

	test.Nil(t, topic.DeleteExistingChannel("c2"))
	waitForTopicUsed(t, topic, 100)
	test.Equal(t, int64(100), c1.PendingBytes())
}

// emptying a channel discards its held bytes and frees the budget.
func TestPendingByteChannelEmptyReleases(t *testing.T) {
	opts := pendingTestOpts(t, 1000)
	opts.MemQueueSize = 0 // force disk so draining works without consumers
	_, _, nsqd := mustStartNSQD(opts)
	defer func() { _ = os.RemoveAll(opts.DataPath) }()
	defer nsqd.Exit()

	topicName := "test_pending_chempty" + strconv.Itoa(int(time.Now().UnixNano()))
	topic := nsqd.GetTopic(topicName)
	c := topic.GetChannel("ch")

	for i := 0; i < 5; i++ {
		test.Nil(t, topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 100))))
	}
	waitForChannelPending(t, c, 500)
	waitForTopicUsed(t, topic, 500)

	test.Nil(t, c.Empty())
	waitForChannelPending(t, c, 0)
	waitForTopicUsed(t, topic, 0)
}

// PUB, DPUB, HTTP /pub, HTTP /mpub and TCP MPUB all share the same
// admission rule and reject with a non-fatal BACKLOG_FULL error.
func TestPendingByteProtocolReject(t *testing.T) {
	opts := pendingTestOpts(t, 200)
	tcpAddr, httpAddr, nsqd := mustStartNSQD(opts)
	defer func() { _ = os.RemoveAll(opts.DataPath) }()
	defer nsqd.Exit()

	topicName := "test_pending_proto" + strconv.Itoa(int(time.Now().UnixNano()))
	topic := nsqd.GetTopic(topicName)
	test.Nil(t, topic.Pause())
	topic.GetChannel("ch") // cost factor 2

	conn, err := mustConnectNSQD(tcpAddr)
	test.Nil(t, err)
	defer func() { _ = conn.Close() }()
	identify(t, conn, nil, frameTypeResponse)

	rejected := func(cmdName string, write func() error) {
		test.Nil(t, write())
		resp, err := nsq.ReadResponse(conn)
		test.Nil(t, err)
		frameType, data, err := nsq.UnpackResponse(resp)
		test.Nil(t, err)
		if frameType != frameTypeError || !bytes.HasPrefix(data, []byte("E_BACKLOG_FULL")) {
			t.Fatalf("%s: want E_BACKLOG_FULL error frame, got %d %q", cmdName, frameType, data)
		}
	}

	// consume the full budget with a TCP PUB of 100 bytes
	_, err = nsq.Publish(topicName, make([]byte, 100)).WriteTo(conn)
	test.Nil(t, err)
	readValidate(t, conn, frameTypeResponse, "OK")

	rejected("PUB", func() error {
		_, e := nsq.Publish(topicName, make([]byte, 1)).WriteTo(conn)
		return e
	})
	rejected("DPUB", func() error {
		_, e := nsq.DeferredPublish(topicName, time.Second, make([]byte, 1)).WriteTo(conn)
		return e
	})
	rejected("MPUB", func() error {
		cmd, e := nsq.MultiPublish(topicName, [][]byte{make([]byte, 1), make([]byte, 1)})
		if e != nil {
			return e
		}
		_, e = cmd.WriteTo(conn)
		return e
	})

	// the connection is still alive after non-fatal budget errors
	_, err = nsq.Nop().WriteTo(conn)
	test.Nil(t, err)

	// HTTP /pub
	url := fmt.Sprintf("http://%s/pub?topic=%s", httpAddr, topicName)
	resp, err := http.Post(url, "application/octet-stream", bytes.NewReader(make([]byte, 1)))
	test.Nil(t, err)
	test.Equal(t, 507, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	test.Equal(t, `{"message":"BACKLOG_FULL"}`, string(body))

	// HTTP /mpub
	url = fmt.Sprintf("http://%s/mpub?topic=%s", httpAddr, topicName)
	resp, err = http.Post(url, "text/plain", bytes.NewReader([]byte("a\n")))
	test.Nil(t, err)
	test.Equal(t, 507, resp.StatusCode)
	_ = resp.Body.Close()

	// no partial delivery: nothing moved into the channel while paused
	// (reservation alone), and stats distinguish used vs reserved
	used, reserved, _ := topic.PendingByteStats()
	test.Equal(t, int64(200), used)
	test.Equal(t, int64(0), reserved)
}

// an over-budget MPUB rejects atomically: the batch is all-or-nothing
// and no channel receives any of the rejected messages.
func TestPendingByteMPUBAtomicNoPartialFanout(t *testing.T) {
	opts := pendingTestOpts(t, 1000)
	_, _, nsqd := mustStartNSQD(opts)
	defer func() { _ = os.RemoveAll(opts.DataPath) }()
	defer nsqd.Exit()

	topicName := "test_pending_mpub" + strconv.Itoa(int(time.Now().UnixNano()))
	topic := nsqd.GetTopic(topicName)
	c1 := topic.GetChannel("c1")
	c2 := topic.GetChannel("c2")

	// prime the budget with one large message (3 copies * 300 = 900)
	test.Nil(t, topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 300))))
	waitForChannelPending(t, c1, 300)
	waitForChannelPending(t, c2, 300)
	waitForTopicUsed(t, topic, 600)

	// disparate sizes: 50*3 + 400*3 = 1350 > 400 remaining -> rejected
	msgs := []*Message{
		NewMessage(topic.GenerateID(), make([]byte, 50)),
		NewMessage(topic.GenerateID(), make([]byte, 400)),
	}
	before := c1.PendingBytes()
	test.Equal(t, ErrPendingByteBudgetExceeded, topic.PutMessages(msgs))
	waitFor(t, "no partial fanout", func() bool {
		return c1.PendingBytes() == before && c2.PendingBytes() == before
	})
	used, reserved, _ := topic.PendingByteStats()
	test.Equal(t, int64(600), used)
	test.Equal(t, int64(0), reserved)

	// a single small message fits (cost 3*50 = 150) and reaches both
	// channels; the next one overshoots
	test.Nil(t, topic.PutMessages([]*Message{NewMessage(topic.GenerateID(), make([]byte, 50))}))
	waitForChannelPending(t, c1, 350)
	waitForChannelPending(t, c2, 350)
	test.Equal(t, ErrPendingByteBudgetExceeded,
		topic.PutMessages([]*Message{NewMessage(topic.GenerateID(), make([]byte, 200))}))
}

// stats JSON exposes used, reserved and limit bytes separately.
func TestPendingByteStatsJSON(t *testing.T) {
	opts := pendingTestOpts(t, 1234)
	_, httpAddr, nsqd := mustStartNSQD(opts)
	defer func() { _ = os.RemoveAll(opts.DataPath) }()
	defer nsqd.Exit()

	topicName := "test_pending_statsjson" + strconv.Itoa(int(time.Now().UnixNano()))
	topic := nsqd.GetTopic(topicName)
	test.Nil(t, topic.Pause())
	c := topic.GetChannel("ch")
	test.Nil(t, topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 100))))

	url := fmt.Sprintf("http://%s/stats?format=json&topic=%s&include_clients=0", httpAddr, topicName)
	resp, err := http.Get(url)
	test.Nil(t, err)
	defer func() { _ = resp.Body.Close() }()
	test.Equal(t, 200, resp.StatusCode)

	var doc struct {
		Topics []struct {
			PendingBytes         int64 `json:"pending_bytes"`
			PendingBytesReserved int64 `json:"pending_bytes_reserved"`
			MaxPendingBytes      int64 `json:"max_pending_bytes"`
			Channels             []struct {
				PendingBytes int64 `json:"pending_bytes"`
			} `json:"channels"`
		} `json:"topics"`
	}
	test.Nil(t, json.NewDecoder(resp.Body).Decode(&doc))
	test.Equal(t, 1, len(doc.Topics))
	test.Equal(t, int64(200), doc.Topics[0].PendingBytes)
	test.Equal(t, int64(0), doc.Topics[0].PendingBytesReserved)
	test.Equal(t, int64(1234), doc.Topics[0].MaxPendingBytes)
	test.Equal(t, int64(0), doc.Topics[0].Channels[0].PendingBytes) // paused: not delivered yet
	test.Equal(t, int64(0), c.PendingBytes())
}

// requeue of an in-flight message keeps its bytes accounted (the bytes
// never leave the channel); only FIN releases them.
func TestPendingByteRequeueKeepsBytes(t *testing.T) {
	opts := pendingTestOpts(t, 1000)
	tcpAddr, _, nsqd := mustStartNSQD(opts)
	defer func() { _ = os.RemoveAll(opts.DataPath) }()
	defer nsqd.Exit()

	topicName := "test_pending_req" + strconv.Itoa(int(time.Now().UnixNano()))
	topic := nsqd.GetTopic(topicName)
	channel := topic.GetChannel("ch")

	test.Nil(t, topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 400))))
	waitForChannelPending(t, channel, 400)
	waitForTopicUsed(t, topic, 400)

	conn, err := mustConnectNSQD(tcpAddr)
	test.Nil(t, err)
	defer func() { _ = conn.Close() }()
	identify(t, conn, nil, frameTypeResponse)
	sub(t, conn, topicName, "ch")

	_, err = nsq.Ready(1).WriteTo(conn)
	test.Nil(t, err)
	id := drainMessageID(t, conn)

	// in-flight bytes are still part of the channel pending bytes
	waitForChannelPending(t, channel, 400)
	test.Equal(t, ErrPendingByteBudgetExceeded,
		topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 301))))

	// requeue immediately: bytes stay accounted
	_, err = nsq.Requeue(nsq.MessageID(id), 0).WriteTo(conn)
	test.Nil(t, err)
	time.Sleep(50 * time.Millisecond)
	waitForChannelPending(t, channel, 400)
	test.Equal(t, ErrPendingByteBudgetExceeded,
		topic.PutMessage(NewMessage(topic.GenerateID(), make([]byte, 301))))

	// consume and finish: budget is freed
	_, err = nsq.Ready(1).WriteTo(conn)
	test.Nil(t, err)
	id = drainMessageID(t, conn)
	_, err = nsq.Finish(nsq.MessageID(id)).WriteTo(conn)
	test.Nil(t, err)
	waitForChannelPending(t, channel, 0)
	waitForTopicUsed(t, topic, 0)
}
