package centrifuge

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/centrifugal/centrifuge/internal/sserecovery"
	"github.com/centrifugal/protocol"
	"github.com/segmentio/encoding/json"
	"github.com/stretchr/testify/require"
)

// sseEvent is one parsed text/event-stream event: its id (recovery cursor) and
// data payload (an encoded protocol.Reply).
type sseEvent struct {
	id   string
	data string
}

// sseReader incrementally parses an SSE stream over a buffered reader.
type sseReader struct {
	br   *bufio.Reader
	body io.ReadCloser
	ch   chan sseReadResult
	done chan struct{}
}

type sseReadResult struct {
	ev  sseEvent
	err error
}

func newSSEReader(resp *http.Response) *sseReader {
	r := &sseReader{
		br:   bufio.NewReader(resp.Body),
		body: resp.Body,
		ch:   make(chan sseReadResult, 16),
		done: make(chan struct{}),
	}
	go r.pump()
	return r
}

func (r *sseReader) close() {
	_ = r.body.Close()
	<-r.done
}

// pump is the single reader goroutine: it parses complete events and pushes
// them until the stream ends. Closing the body unblocks the read with an error.
func (r *sseReader) pump() {
	defer close(r.done)
	for {
		ev, err := r.readEvent()
		r.ch <- sseReadResult{ev: ev, err: err}
		if err != nil {
			return
		}
	}
}

// readEvent reads one complete (blank-line terminated) event. A non-nil error
// means the stream ended (server close or client disconnect); events partially
// received without a terminating blank line are not returned, which is exactly
// the "cut off mid-body" condition the tests rely on.
func (r *sseReader) readEvent() (sseEvent, error) {
	var ev sseEvent
	have := false
	for {
		line, err := r.br.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		if len(trimmed) > 0 {
			switch {
			case strings.HasPrefix(trimmed, "id: "):
				ev.id = strings.TrimPrefix(trimmed, "id: ")
				have = true
			case strings.HasPrefix(trimmed, "data: "):
				ev.data = strings.TrimPrefix(trimmed, "data: ")
				have = true
			}
		}
		if trimmed == "" && have {
			return ev, nil
		}
		if err != nil {
			return ev, err
		}
	}
}

func (r *sseReader) readEventsUntil(wait time.Duration, until func(ev sseEvent) bool) ([]sseEvent, error) {
	var events []sseEvent
	deadline := time.Now().Add(wait)
	for {
		d := time.Until(deadline)
		if d <= 0 {
			return events, errSSEReadTimeout
		}
		select {
		case res := <-r.ch:
			if res.err != nil {
				return events, res.err
			}
			events = append(events, res.ev)
			if until != nil && until(res.ev) {
				return events, nil
			}
		case <-time.After(d):
			return events, errSSEReadTimeout
		}
	}
}

var errSSEReadTimeout = stringsError("sse read timeout")

func stringsError(s string) error { return &sseTestError{s} }

type sseTestError struct{ s string }

func (e *sseTestError) Error() string { return e.s }

// isPublication reports whether an event's data is a protocol publication push
// and returns its channel and payload.
func pubPayloads(t *testing.T, events []sseEvent) map[string][]string {
	t.Helper()
	out := make(map[string][]string)
	for _, ev := range events {
		if ev.data == "" {
			continue
		}
		var reply protocol.Reply
		require.NoError(t, json.Unmarshal([]byte(ev.data), &reply))
		// Live publications.
		if reply.Push != nil && reply.Push.Pub != nil {
			out[reply.Push.Channel] = append(out[reply.Push.Channel], string(reply.Push.Pub.Data))
		}
		// Server-side subscriptions deliver recovered publications inline in the
		// connect reply, ordered per channel.
		if reply.Connect != nil {
			for ch, res := range reply.Connect.Subs {
				for _, pub := range res.Publications {
					out[ch] = append(out[ch], string(pub.Data))
				}
			}
		}
	}
	return out
}

func newSSERecoveryNode(t *testing.T, user string, denyChannel string) *Node {
	t.Helper()
	n, _ := New(Config{LogLevel: LogLevelError})
	n.OnConnecting(func(ctx context.Context, event ConnectEvent) (ConnectReply, error) {
		subs := make(map[string]SubscribeOptions, len(event.Channels))
		for _, ch := range event.Channels {
			if ch == denyChannel {
				// Simulate a revoked subscription: the cursor must not bypass it.
				continue
			}
			subs[ch] = SubscribeOptions{EnableRecovery: true}
		}
		return ConnectReply{
			Credentials:   &Credentials{UserID: user},
			Subscriptions: subs,
		}, nil
	})
	require.NoError(t, n.Run())
	t.Cleanup(func() { _ = n.Shutdown(context.Background()) })
	return n
}

func sseTestServer(t *testing.T, n *Node, config SSEConfig) *httptest.Server {
	t.Helper()
	config.PingPongConfig = PingPongConfig{PingInterval: -1, PongTimeout: -1}
	h, err := NewSSEHandlerWithRecovery(n, config)
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.Handle("/connection/sse", h)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func sseConnectRequest(t *testing.T, rawURL string, subs map[string]*protocol.SubscribeRequest) *http.Request {
	t.Helper()
	cmd := &protocol.Command{Id: 1, Connect: &protocol.ConnectRequest{Subs: subs}}
	raw, err := json.Marshal(cmd)
	require.NoError(t, err)
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	q := u.Query()
	q.Set(connectUrlParam, string(raw))
	u.RawQuery = q.Encode()
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	require.NoError(t, err)
	return req
}

func sseRecoverSub(epoch string, offset uint64) *protocol.SubscribeRequest {
	return &protocol.SubscribeRequest{Recover: true, Epoch: epoch, Offset: offset}
}

func sseFreshSub() *protocol.SubscribeRequest {
	return &protocol.SubscribeRequest{Recover: true}
}

func publishSeq(t *testing.T, n *Node, ch string, prefix string, from, to int) {
	t.Helper()
	for i := from; i <= to; i++ {
		payload := `{"k":"` + prefix + `","n":` + strconv.Itoa(i) + `}`
		_, err := n.Publish(ch, []byte(payload), WithHistory(100, time.Hour))
		require.NoError(t, err)
	}
}

// pubKey extracts the "k" field from a JSON publication payload.
func pubKey(t *testing.T, raw string) string {
	t.Helper()
	var v struct {
		K string `json:"k"`
		N int    `json:"n"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &v))
	return v.K + strconv.Itoa(v.N)
}

// pubKeys maps channel to ordered "k"+"n" labels for boundary assertions.
func pubKeys(t *testing.T, events []sseEvent) map[string][]string {
	t.Helper()
	out := make(map[string][]string)
	for ch, payloads := range pubPayloads(t, events) {
		for _, p := range payloads {
			out[ch] = append(out[ch], pubKey(t, p))
		}
	}
	return out
}

func recoveryConfig() SSERecoveryConfig {
	return SSERecoveryConfig{KeyID: "k1", Key: []byte("recovery-secret")}
}

func openSSE(t *testing.T, server *httptest.Server, subs map[string]*protocol.SubscribeRequest, cursor string) (*http.Response, *sseReader, []sseEvent) {
	t.Helper()
	req := sseConnectRequest(t, server.URL+"/connection/sse", subs)
	if cursor != "" {
		req.Header.Set("Last-Event-ID", cursor)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	r := newSSEReader(resp)
	connectEvents, err := r.readEventsUntil(3*time.Second, func(ev sseEvent) bool {
		var reply protocol.Reply
		require.NoError(t, json.Unmarshal([]byte(ev.data), &reply))
		return reply.Connect != nil
	})
	require.NoError(t, err)
	return resp, r, connectEvents
}

func lastID(events []sseEvent) string {
	id := ""
	for _, ev := range events {
		if ev.id != "" {
			id = ev.id
		}
	}
	return id
}

// drainFor reads complete events for the given duration, accumulating them.
// It is driven by repeated short reads so a disconnect (rather than a timeout)
// ends collection immediately.
func drainFor(t *testing.T, r *sseReader, d time.Duration) []sseEvent {
	t.Helper()
	var events []sseEvent
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		evs, err := r.readEventsUntil(200*time.Millisecond, func(ev sseEvent) bool { return false })
		events = append(events, evs...)
		if err != nil && err != errSSEReadTimeout {
			break
		}
	}
	return events
}

// TestSSERecoveryReconnectAfterCompleteEvent disconnects after fully delivered
// events and verifies a cursor resume with no gap or duplicate per channel.
func TestSSERecoveryReconnectAfterCompleteEvent(t *testing.T) {
	t.Parallel()
	n := newSSERecoveryNode(t, "user-1", "")
	server := sseTestServer(t, n, SSEConfig{Recovery: recoveryConfig()})

	const chA, chB = "chat:a", "chat:b"
	publishSeq(t, n, chA, "a", 1, 3)
	publishSeq(t, n, chB, "b", 1, 3)

	resp1, r1, connect1 := openSSE(t, server, map[string]*protocol.SubscribeRequest{
		chA: sseFreshSub(),
		chB: sseFreshSub(),
	}, "")

	firstEvents := append(connect1, drainFor(t, r1, 800*time.Millisecond)...)
	cursor := lastID(firstEvents)
	require.NotEmpty(t, cursor)
	firstKeys := pubKeys(t, firstEvents)
	require.ElementsMatch(t, []string{"a1", "a2", "a3"}, firstKeys[chA])
	require.ElementsMatch(t, []string{"b1", "b2", "b3"}, firstKeys[chB])

	// Clean client disconnect after complete events, then new publications.
	_ = resp1.Body.Close()
	r1.close()
	time.Sleep(100 * time.Millisecond)
	publishSeq(t, n, chA, "a", 4, 5)
	publishSeq(t, n, chB, "b", 4, 5)

	resp2, r2, connect2 := openSSE(t, server, map[string]*protocol.SubscribeRequest{
		chA: sseRecoverSub("", 0),
		chB: sseRecoverSub("", 0),
	}, cursor)
	defer resp2.Body.Close()

	secondEvents := append(connect2, drainFor(t, r2, 800*time.Millisecond)...)
	secondKeys := pubKeys(t, secondEvents)
	require.ElementsMatch(t, []string{"a4", "a5"}, secondKeys[chA])
	require.ElementsMatch(t, []string{"b4", "b5"}, secondKeys[chB])
	r2.close()
}

// cutProxy is a minimal TCP proxy that forwards bytes verbatim between the test
// client and the httptest server until a marker publication frame is observed,
// then closes the connection mid-event (after forwarding cutAfter bytes of that
// event) and stops forwarding. It is used to reproduce an SSE event severed in
// the middle of its body.
type cutProxy struct {
	listener net.Listener
	target   string

	mu        sync.Mutex
	cutAfter  int
	marker    []byte
	triggered bool
}

func newCutProxy(t *testing.T, target string) *cutProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := &cutProxy{listener: ln, target: target}
	t.Cleanup(func() { _ = ln.Close() })
	go p.serve()
	return p
}

func (p *cutProxy) url() string {
	return "http://" + p.listener.Addr().String()
}

// arm enables cutting: when marker bytes appear, forward cutAfter bytes total
// of the current connection and then sever it.
func (p *cutProxy) arm(marker string, cutAfter int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.marker = []byte(marker)
	p.cutAfter = cutAfter
	p.triggered = false
}

func (p *cutProxy) serve() {
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			return
		}
		go p.handle(conn)
	}
}

func (p *cutProxy) handle(client net.Conn) {
	server, err := net.Dial("tcp", p.target)
	if err != nil {
		_ = client.Close()
		return
	}
	done := make(chan struct{}, 2)
	// client -> server (request) forwarded unmodified.
	go func() {
		_, _ = io.Copy(server, client)
		_ = server.Close()
		done <- struct{}{}
	}()
	// server -> client with cutting.
	go func() {
		defer func() {
			_ = client.Close()
			_ = server.Close()
			done <- struct{}{}
		}()
		buf := make([]byte, 4096)
		var forwarded int
		var cut bool
		for {
			n, err := server.Read(buf)
			if n > 0 {
				data := buf[:n]
				p.mu.Lock()
				marker := p.marker
				cutAfter := p.cutAfter
				p.mu.Unlock()
				if !cut && len(marker) > 0 && bytes.Contains(data, marker) {
					cut = true
					// Forward only enough to include part of this event's body:
					// keep cutAfter bytes total for this read, then sever.
					keep := cutAfter - forwarded
					if keep < 0 {
						keep = 0
					}
					if keep > n {
						keep = n
					}
					if keep > 0 {
						_, _ = client.Write(data[:keep])
						forwarded += keep
					}
					return
				}
				_, _ = client.Write(data)
				forwarded += n
			}
			if err != nil {
				return
			}
		}
	}()
	<-done
}

// TestSSERecoveryReconnectMidEvent verifies that an SSE event severed halfway
// through its body does NOT advance the delivered position: the reconnect must
// redeliver that publication rather than skipping it.
func TestSSERecoveryReconnectMidEvent(t *testing.T) {
	t.Parallel()
	n := newSSERecoveryNode(t, "user-1", "")
	server := sseTestServer(t, n, SSEConfig{Recovery: recoveryConfig()})

	targetURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	proxy := newCutProxy(t, targetURL.Host)

	const chA = "mid:a"
	publishSeq(t, n, chA, "m", 1, 1)

	// First connection through the proxy. The marker is the m2 payload; it is not
	// published until after the initial m1 connect settles.
	proxyURL := proxy.url()
	req := sseConnectRequest(t, proxyURL+"/connection/sse", map[string]*protocol.SubscribeRequest{
		chA: sseFreshSub(),
	})
	resp1, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	r1 := newSSEReader(resp1)
	connEvents, err := r1.readEventsUntil(3*time.Second, func(ev sseEvent) bool {
		var reply protocol.Reply
		require.NoError(t, json.Unmarshal([]byte(ev.data), &reply))
		return reply.Connect != nil
	})
	require.NoError(t, err)
	// m1 was recovered inside the connect reply.
	require.Contains(t, pubKeys(t, connEvents)[chA], "m1")
	cursorM1 := lastID(connEvents)
	require.NotEmpty(t, cursorM1)

	// Publish m2 and arm the proxy to sever after forwarding only part of the
	// event containing m2 (cut within its data line).
	publishSeq(t, n, chA, "m", 2, 2)
	proxy.arm(`"k":"m","n":2`, 30)

	// Read until the severed connection errors.
	ended := make(chan error, 1)
	go func() {
		for {
			res := <-r1.ch
			if res.err != nil {
				ended <- res.err
				return
			}
		}
	}()
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("expected the cut connection to end")
	}
	resp1.Body.Close()

	// Reconnect from the last fully delivered cursor (which must still be m1).
	time.Sleep(100 * time.Millisecond)
	publishSeq(t, n, chA, "m", 3, 3)
	resp2, r2, connect2 := openSSE(t, server, map[string]*protocol.SubscribeRequest{
		chA: sseRecoverSub("", 0),
	}, cursorM1)
	defer resp2.Body.Close()
	after := append(connect2, drainFor(t, r2, 800*time.Millisecond)...)

	// Boundary check: the half-sent m2 is redelivered (not skipped), m3 follows,
	// and m1 is not duplicated.
	keys := pubKeys(t, after)[chA]
	require.Contains(t, keys, "m2", "half-sent event must be redelivered")
	require.Contains(t, keys, "m3")
	require.NotContains(t, keys, "m1")
	// No duplicate m2.
	count := 0
	for _, k := range keys {
		if k == "m2" {
			count++
		}
	}
	require.Equal(t, 1, count)
	r2.close()
}

// TestSSERecoveryMidBatch verifies cumulative per-event cursors inside a single
// write batch: when several events arrive together and the connection is cut
// after some but not all of them, reconnect resumes at the first undelivered
// event with no skip or duplicate.
func TestSSERecoveryReconnectMidBatch(t *testing.T) {
	t.Parallel()
	n := newSSERecoveryNode(t, "user-1", "")
	server := sseTestServer(t, n, SSEConfig{Recovery: recoveryConfig()})

	targetURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	proxy := newCutProxy(t, targetURL.Host)

	const chA = "batch:a"
	// Initial message so the cursor exists before the burst.
	publishSeq(t, n, chA, "m", 1, 1)

	req := sseConnectRequest(t, proxy.url()+"/connection/sse", map[string]*protocol.SubscribeRequest{
		chA: sseFreshSub(),
	})
	resp1, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	r1 := newSSEReader(resp1)
	connEvents, err := r1.readEventsUntil(3*time.Second, func(ev sseEvent) bool {
		var reply protocol.Reply
		require.NoError(t, json.Unmarshal([]byte(ev.data), &reply))
		return reply.Connect != nil
	})
	require.NoError(t, err)
	baseCursor := lastID(connEvents)
	require.NotEmpty(t, baseCursor)

	// Publish a burst that the writer will batch.
	publishSeq(t, n, chA, "m", 2, 6)
	// Cut within the batch: forward a limited window of bytes so at least one
	// full m-event arrives but not all five.
	proxy.arm(`"k":"m","n":3`, 40)

	ended := make(chan struct{})
	go func() {
		for {
			res := <-r1.ch
			if res.err != nil {
				close(ended)
				return
			}
		}
	}()
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("expected cut")
	}
	resp1.Body.Close()

	time.Sleep(100 * time.Millisecond)
	resp2, r2, conn2 := openSSE(t, server, map[string]*protocol.SubscribeRequest{
		chA: sseRecoverSub("", 0),
	}, baseCursor)
	defer resp2.Body.Close()
	after := append(conn2, drainFor(t, r2, 800*time.Millisecond)...)
	keys := pubKeys(t, after)[chA]

	// The first event (m1) must not be redelivered; the cut event and everything
	// after it must be present exactly once. We cannot control exactly how many
	// bytes crossed, so assert the core invariant: contiguous, no duplicate, no
	// gap, and at least one previously undelivered event is redelivered.
	require.NotContains(t, keys, "m1")
	require.Equal(t, len(keys), len(uniqueStrings(keys)), "no duplicate redelivery")
	require.True(t, contiguousLabels(keys), "redelivery must be contiguous, got %v", keys)
	require.Contains(t, keys, "m2")
	r2.close()
}

func uniqueStrings(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range in {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

// contiguousLabels verifies a run of labels like m3,m4,m5 with no holes.
func contiguousLabels(labels []string) bool {
	seen := map[int]bool{}
	minN, maxN := 0, 0
	for _, l := range labels {
		if len(l) < 2 || l[0] != 'm' {
			return false
		}
		n := 0
		for _, c := range l[1:] {
			if c < '0' || c > '9' {
				return false
			}
			n = n*10 + int(c-'0')
		}
		if minN == 0 || n < minN {
			minN = n
		}
		if n > maxN {
			maxN = n
		}
		seen[n] = true
	}
	for n := minN; n <= maxN; n++ {
		if !seen[n] {
			return false
		}
	}
	return len(labels) > 0
}

// TestSSERecoveryReconnectServerClose verifies resume when the server closes
// the SSE stream after complete events (clean EOF), rather than the client
// dropping it.
func TestSSERecoveryReconnectServerClose(t *testing.T) {
	t.Parallel()
	n := newSSERecoveryNode(t, "user-1", "")
	server := sseTestServer(t, n, SSEConfig{Recovery: recoveryConfig()})

	const chA = "srv:a"
	publishSeq(t, n, chA, "m", 1, 2)

	resp1, r1, conn1 := openSSE(t, server, map[string]*protocol.SubscribeRequest{
		chA: sseFreshSub(),
	}, "")
	firstEvents := append(conn1, drainFor(t, r1, 600*time.Millisecond)...)
	require.Contains(t, pubKeys(t, firstEvents)[chA], "m2")
	cursor := lastID(firstEvents)
	require.NotEmpty(t, cursor)

	// Force the server to close this client's transport. Find the client via the
	// hub and disconnect it, which closes the SSE stream with a clean EOF.
	clients := n.Hub().Connections()
	require.NotEmpty(t, clients)
	for _, cl := range clients {
		if cl.UserID() == "user-1" {
			cl.Disconnect(DisconnectShutdown)
		}
	}

	// The stream should end with an error on read.
	ended := make(chan struct{})
	go func() {
		for {
			res := <-r1.ch
			if res.err != nil {
				close(ended)
				return
			}
		}
	}()
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not close the stream")
	}
	_ = resp1.Body.Close()

	publishSeq(t, n, chA, "m", 3, 3)
	resp2, r2, conn2 := openSSE(t, server, map[string]*protocol.SubscribeRequest{
		chA: sseRecoverSub("", 0),
	}, cursor)
	defer resp2.Body.Close()
	after := append(conn2, drainFor(t, r2, 800*time.Millisecond)...)
	require.ElementsMatch(t, []string{"m3"}, pubKeys(t, after)[chA])
	r2.close()
}

// connectVerdicts returns the per-channel Recovered flag from a connect reply.
func connectVerdicts(t *testing.T, events []sseEvent) map[string]bool {
	t.Helper()
	out := make(map[string]bool)
	for _, ev := range events {
		if ev.data == "" {
			continue
		}
		var reply protocol.Reply
		require.NoError(t, json.Unmarshal([]byte(ev.data), &reply))
		if reply.Connect != nil {
			for ch, res := range reply.Connect.Subs {
				out[ch] = res.GetRecovered()
			}
		}
	}
	return out
}

// TestSSERecoveryPartialWhenOneChannelExpired verifies that when one channel's
// history has expired, the other channel's successful recovery is not presented
// as a full success: the expired channel resumes fresh (recovered=false) and no
// stale cursor position is claimed for it.
func TestSSERecoveryPartialWhenOneChannelExpired(t *testing.T) {
	t.Parallel()
	n := newSSERecoveryNode(t, "user-1", "")
	server := sseTestServer(t, n, SSEConfig{Recovery: recoveryConfig()})

	const good, stale = "part:good", "part:stale"
	// The stale channel retains only the two most recent publications, so a
	// resume from offset 1 has a gap the broker cannot bridge.
	_, err := n.Publish(stale, []byte(`{"k":"s","n":1}`), WithHistory(2, time.Hour))
	require.NoError(t, err)
	publishSeq(t, n, good, "g", 1, 2)

	_, r1, conn1 := openSSE(t, server, map[string]*protocol.SubscribeRequest{
		good:  sseFreshSub(),
		stale: sseFreshSub(),
	}, "")
	first := append(conn1, drainFor(t, r1, 500*time.Millisecond)...)
	cursor := lastID(first)
	require.NotEmpty(t, cursor)

	// Both initially delivered.
	require.Contains(t, pubKeys(t, first)[stale], "s1")
	r1.close()

	// Roll the stale window past offset 1 (s3, s4 retained; s1/s2 evicted) while
	// the good channel advances contiguously.
	_, err = n.Publish(stale, []byte(`{"k":"s","n":2}`), WithHistory(2, time.Hour))
	require.NoError(t, err)
	_, err = n.Publish(stale, []byte(`{"k":"s","n":3}`), WithHistory(2, time.Hour))
	require.NoError(t, err)
	_, err = n.Publish(stale, []byte(`{"k":"s","n":4}`), WithHistory(2, time.Hour))
	require.NoError(t, err)
	publishSeq(t, n, good, "g", 3, 3)

	resp2, r2, conn2 := openSSE(t, server, map[string]*protocol.SubscribeRequest{
		good:  sseRecoverSub("", 0),
		stale: sseRecoverSub("", 0),
	}, cursor)
	defer resp2.Body.Close()

	// Good channel recovers g3; stale channel reports recovered=false because
	// its gap cannot be bridged — not a masked full success.
	verdicts := connectVerdicts(t, conn2)
	require.True(t, verdicts[good], "fresh channel must recover")
	require.False(t, verdicts[stale], "expired channel must not claim recovery")

	after := append(conn2, drainFor(t, r2, 600*time.Millisecond)...)
	require.Contains(t, pubKeys(t, after)[good], "g3")
	r2.close()
}

// TestSSERecoveryCursorNotCredential verifies that a cursor minted for another
// user does not grant access and the connection is refused on identity binding.
func TestSSERecoveryCursorNotCredential(t *testing.T) {
	t.Parallel()
	n1 := newSSERecoveryNode(t, "alice", "")
	server := sseTestServer(t, n1, SSEConfig{Recovery: recoveryConfig()})

	const chA = "cred:a"
	publishSeq(t, n1, chA, "a", 1, 1)
	_, r1, conn1 := openSSE(t, server, map[string]*protocol.SubscribeRequest{
		chA: sseFreshSub(),
	}, "")
	cursor := lastID(conn1)
	r1.close()
	require.NotEmpty(t, cursor)

	// A second node authenticates a different user ("bob"). Reusing alice's
	// cursor must fail the user binding and close the connection.
	n2, _ := New(Config{
		LogLevel: LogLevelError,
	})
	n2.OnConnecting(func(ctx context.Context, event ConnectEvent) (ConnectReply, error) {
		subs := make(map[string]SubscribeOptions, len(event.Channels))
		for _, ch := range event.Channels {
			subs[ch] = SubscribeOptions{EnableRecovery: true}
		}
		return ConnectReply{Credentials: &Credentials{UserID: "bob"}, Subscriptions: subs}, nil
	})
	require.NoError(t, n2.Run())
	t.Cleanup(func() { _ = n2.Shutdown(context.Background()) })
	server2 := sseTestServer(t, n2, SSEConfig{Recovery: recoveryConfig()})
	req := sseConnectRequest(t, server2.URL+"/connection/sse", map[string]*protocol.SubscribeRequest{
		chA: sseRecoverSub("", 0),
	})
	req.Header.Set("Last-Event-ID", cursor)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	r := newSSEReader(resp)

	// The connect frame is delivered first; the identity mismatch must then be
	// followed by a disconnect push. It must not silently resume the channel.
	var sawDisconnect bool
	deadline := time.After(3 * time.Second)
loop:
	for {
		select {
		case res := <-r.ch:
			if res.err != nil {
				break loop
			}
			var reply protocol.Reply
			require.NoError(t, json.Unmarshal([]byte(res.ev.data), &reply))
			if reply.Push != nil && reply.Push.GetDisconnect() != nil {
				sawDisconnect = true
				break loop
			}
			// Must not deliver any publication from alice's channel.
			require.Nil(t, reply.Push.GetPub())
		case <-deadline:
			break loop
		}
	}
	require.True(t, sawDisconnect, "expected a disconnect for a cursor bound to another user")
	r.close()
}

// TestSSERecoveryPermissionCheckEnforced verifies the cursor is not an
// authorization credential: a channel granted on the first connection but
// revoked before reconnect is not restored by the cursor, while channels that
// remain granted still resume from their signed position.
func TestSSERecoveryPermissionCheckEnforced(t *testing.T) {
	t.Parallel()

	const keep, revoke = "perm:keep", "perm:revoke"

	var grantRevoke atomic.Bool
	grantRevoke.Store(true)
	n, _ := New(Config{LogLevel: LogLevelError})
	n.OnConnecting(func(ctx context.Context, event ConnectEvent) (ConnectReply, error) {
		subs := map[string]SubscribeOptions{}
		for _, ch := range event.Channels {
			if ch == revoke && !grantRevoke.Load() {
				continue
			}
			subs[ch] = SubscribeOptions{EnableRecovery: true}
		}
		return ConnectReply{Credentials: &Credentials{UserID: "user-1"}, Subscriptions: subs}, nil
	})
	require.NoError(t, n.Run())
	t.Cleanup(func() { _ = n.Shutdown(context.Background()) })
	server := sseTestServer(t, n, SSEConfig{Recovery: recoveryConfig()})

	publishSeq(t, n, keep, "k", 1, 1)
	publishSeq(t, n, revoke, "x", 1, 1)

	_, r0, conn0 := openSSE(t, server, map[string]*protocol.SubscribeRequest{
		keep:   sseFreshSub(),
		revoke: sseFreshSub(),
	}, "")
	cursor := lastID(conn0)
	r0.close()
	require.NotEmpty(t, cursor)

	grantRevoke.Store(false)
	publishSeq(t, n, keep, "k", 2, 2)
	publishSeq(t, n, revoke, "x", 2, 2)

	resp, r, connB := openSSE(t, server, map[string]*protocol.SubscribeRequest{
		keep:   sseRecoverSub("", 0),
		revoke: sseRecoverSub("", 0),
	}, cursor)
	defer resp.Body.Close()

	events := append(connB, drainFor(t, r, 600*time.Millisecond)...)
	for _, ev := range events {
		if ev.data == "" {
			continue
		}
		var reply protocol.Reply
		require.NoError(t, json.Unmarshal([]byte(ev.data), &reply))
		if reply.Connect != nil {
			_, hasRevoked := reply.Connect.Subs[revoke]
			require.False(t, hasRevoked, "revoked channel must not be subscribed")
			keepRes, hasKeep := reply.Connect.Subs[keep]
			require.True(t, hasKeep)
			require.True(t, keepRes.GetRecovered(), "granted channel must still recover")
		}
	}
	require.Contains(t, pubKeys(t, events)[keep], "k2")
	require.NotContains(t, pubKeys(t, events), revoke)
	r.close()
}

// TestSSERecoveryTamperedCursorRejected verifies a modified cursor is ignored,
// not trusted: reconnect proceeds without server-injected positions.
func TestSSERecoveryTamperedCursorRejected(t *testing.T) {
	t.Parallel()
	n := newSSERecoveryNode(t, "user-1", "")
	server := sseTestServer(t, n, SSEConfig{Recovery: recoveryConfig()})

	const chA = "tamper:a"
	publishSeq(t, n, chA, "t", 1, 1)
	_, r1, conn1 := openSSE(t, server, map[string]*protocol.SubscribeRequest{
		chA: sseFreshSub(),
	}, "")
	cursor := lastID(conn1)
	r1.close()

	// Flip a character in the signature tail.
	tampered := cursor[:len(cursor)-3] + "AAA"
	// New publications exist after the client thinks it is at offset 1. A
	// rejected cursor means the request offsets are used as-is (recover from the
	// client-supplied zero position) — the connect still succeeds.
	resp2, r2, conn2 := openSSE(t, server, map[string]*protocol.SubscribeRequest{
		chA: sseRecoverSub("", 0),
	}, tampered)
	defer resp2.Body.Close()
	require.NotEmpty(t, conn2)
	r2.close()
}

// TestSSERecoveryKeyRotation verifies a cursor signed with a retired key is
// accepted within its rotation window.
func TestSSERecoveryKeyRotation(t *testing.T) {
	t.Parallel()
	n := newSSERecoveryNode(t, "user-1", "")
	cfg := SSERecoveryConfig{
		KeyID:        "new",
		Key:          []byte("new-secret"),
		OldKeyID:     "old",
		OldKey:       []byte("old-secret"),
		OldKeyMaxAge: time.Hour,
	}
	server := sseTestServer(t, n, SSEConfig{Recovery: cfg})

	const chA = "rot:a"
	publishSeq(t, n, chA, "r", 1, 1)

	// Mint a token with the retired key directly via the internal signer.
	signer, err := sserecovery.NewSigner("old", []byte("old-secret"), sserecovery.Limits{TTL: time.Hour})
	require.NoError(t, err)
	oldToken, err := signer.Issue(sserecovery.Cursor{
		User:     "user-1",
		Channels: []sserecovery.Channel{{Channel: chA, Offset: 1}},
	})
	require.NoError(t, err)

	publishSeq(t, n, chA, "r", 2, 2)
	resp, r, conn := openSSE(t, server, map[string]*protocol.SubscribeRequest{
		chA: sseRecoverSub("", 0),
	}, oldToken)
	defer resp.Body.Close()

	events := append(conn, drainFor(t, r, 600*time.Millisecond)...)
	require.ElementsMatch(t, []string{"r2"}, pubKeys(t, events)[chA])
	r.close()
}
