package centrifuge

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/centrifugal/protocol"
	"github.com/stretchr/testify/require"
)

var testSSECursorConfig = RecoveryCursorConfig{Secret: "sse-cursor-test-secret"}

// newSSECursorTestServer starts a node with a memory broker and an SSE
// endpoint with the recovery cursor enabled. All channels are subscribable
// with recovery and positioning.
func newSSECursorTestServer(t *testing.T, cursorConfig RecoveryCursorConfig) (*Node, *httptest.Server) {
	t.Helper()
	node, err := New(Config{})
	require.NoError(t, err)
	broker, err := NewMemoryBroker(node, MemoryBrokerConfig{})
	require.NoError(t, err)
	node.SetBroker(broker)
	node.OnConnecting(func(ctx context.Context, event ConnectEvent) (ConnectReply, error) {
		return ConnectReply{Credentials: &Credentials{UserID: "42"}}, nil
	})
	node.OnConnect(func(client *Client) {
		client.OnSubscribe(func(event SubscribeEvent, callback SubscribeCallback) {
			callback(SubscribeReply{Options: SubscribeOptions{
				EnableRecovery:    true,
				EnablePositioning: true,
			}}, nil)
		})
	})
	require.NoError(t, node.Run())
	t.Cleanup(func() { _ = node.Shutdown(context.Background()) })
	mux := http.NewServeMux()
	mux.Handle("/connection/sse", NewSSEHandler(node, SSEConfig{RecoveryCursor: cursorConfig}))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return node, server
}

// dialSSECursor opens a real SSE connection with a connect command followed by
// a subscribe command per channel, optionally sending a recovery cursor.
func dialSSECursor(t *testing.T, server *httptest.Server, channels []string, cursor string) (*http.Response, *bufio.Reader) {
	t.Helper()
	var frame strings.Builder
	connectCmd, err := json.Marshal(protocol.Command{Id: 1, Connect: &protocol.ConnectRequest{}})
	require.NoError(t, err)
	frame.Write(connectCmd)
	frame.WriteByte('\n')
	for i, ch := range channels {
		data, err := json.Marshal(protocol.Command{Id: uint32(i + 2), Subscribe: &protocol.SubscribeRequest{Channel: ch}})
		require.NoError(t, err)
		frame.Write(data)
		frame.WriteByte('\n')
	}
	reqURL, err := url.Parse(server.URL + "/connection/sse")
	require.NoError(t, err)
	query := reqURL.Query()
	query.Set(connectUrlParam, frame.String())
	if cursor != "" {
		query.Set(recoveryCursorUrlParam, cursor)
	}
	reqURL.RawQuery = query.Encode()
	resp, err := http.Get(reqURL.String())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return resp, bufio.NewReader(resp.Body)
}

type sseCursorEvent struct {
	event string
	data  []byte
}

// readSSECursorEvent reads one SSE event (terminated by a blank line) from
// the stream, skipping keep-alive blank lines.
func readSSECursorEvent(r *bufio.Reader) (sseCursorEvent, error) {
	var ev sseCursorEvent
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return sseCursorEvent{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if ev.event != "" || ev.data != nil {
				return ev, nil
			}
		case strings.HasPrefix(line, "event: "):
			ev.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			ev.data = []byte(strings.TrimPrefix(line, "data: "))
		}
	}
}

// readSSECursorReply reads the next protocol reply from the stream, skipping
// cursor events.
func readSSECursorReply(t *testing.T, r *bufio.Reader) *protocol.Reply {
	t.Helper()
	for {
		ev, err := readSSECursorEvent(r)
		require.NoError(t, err)
		if ev.event != "" || len(ev.data) == 0 {
			continue
		}
		var reply protocol.Reply
		require.NoError(t, json.Unmarshal(ev.data, &reply))
		return &reply
	}
}

// readSSECursorToken reads events until a cursor event arrives and returns
// the cursor token.
func readSSECursorToken(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	for {
		ev, err := readSSECursorEvent(r)
		require.NoError(t, err)
		if ev.event == "cursor" {
			return string(ev.data)
		}
	}
}

// readSSECursorPublication reads replies until a publication push arrives.
func readSSECursorPublication(t *testing.T, r *bufio.Reader) *protocol.Push {
	t.Helper()
	for {
		reply := readSSECursorReply(t, r)
		if reply.Push != nil && reply.Push.Pub != nil {
			return reply.Push
		}
		require.Fail(t, "unexpected reply while waiting for publication")
	}
}

func publishSSECursorMessage(t *testing.T, node *Node, channel string, data string, historySize int) {
	t.Helper()
	quoted, err := json.Marshal(data)
	require.NoError(t, err)
	_, err = node.Publish(channel, quoted, WithHistory(historySize, time.Minute))
	require.NoError(t, err)
}

func decodeSSECursorPositions(t *testing.T, config *RecoveryCursorConfig, token string) map[string]StreamPosition {
	t.Helper()
	payload, err := decodeRecoveryCursor(config, token, time.Now())
	require.NoError(t, err)
	positions := make(map[string]StreamPosition, len(payload.Positions))
	for ch, p := range payload.Positions {
		positions[ch] = StreamPosition{Offset: p.Offset, Epoch: p.Epoch}
	}
	return positions
}

// readSSECursorHandshake reads the connect reply, one subscribe reply per
// channel (in command order) and the initial cursor emitted after them.
func readSSECursorHandshake(t *testing.T, r *bufio.Reader, channels []string) ([]*protocol.SubscribeResult, string) {
	t.Helper()
	reply := readSSECursorReply(t, r)
	require.NotNil(t, reply.Connect)
	results := make([]*protocol.SubscribeResult, 0, len(channels))
	for range channels {
		reply = readSSECursorReply(t, r)
		require.NotNil(t, reply.Subscribe)
		require.Nil(t, reply.Error)
		results = append(results, reply.Subscribe)
	}
	token := readSSECursorToken(t, r)
	return results, token
}

// TestSSERecoveryCursorReconnectAfterFullEvent checks the happy path: the
// client disconnects after fully received events, and the cursor carried to
// the next connection recovers exactly the missed publications in every
// channel.
func TestSSERecoveryCursorReconnectAfterFullEvent(t *testing.T) {
	t.Parallel()
	node, server := newSSECursorTestServer(t, testSSECursorConfig)
	channels := []string{"ch1", "ch2"}

	resp, reader := dialSSECursor(t, server, channels, "")
	_, token := readSSECursorHandshake(t, reader, channels)
	positions := decodeSSECursorPositions(t, &testSSECursorConfig, token)
	require.Equal(t, uint64(0), positions["ch1"].Offset)
	require.Equal(t, uint64(0), positions["ch2"].Offset)

	// Publish and read back, advancing the cursor per channel.
	publishSSECursorMessage(t, node, "ch1", "m1", 100)
	push := readSSECursorPublication(t, reader)
	require.Equal(t, "ch1", push.Channel)
	require.Equal(t, uint64(1), push.Pub.Offset)
	token = readSSECursorToken(t, reader)

	publishSSECursorMessage(t, node, "ch1", "m2", 100)
	readSSECursorPublication(t, reader)
	token = readSSECursorToken(t, reader)

	publishSSECursorMessage(t, node, "ch2", "n1", 100)
	readSSECursorPublication(t, reader)
	token = readSSECursorToken(t, reader)

	positions = decodeSSECursorPositions(t, &testSSECursorConfig, token)
	require.Equal(t, uint64(2), positions["ch1"].Offset)
	require.Equal(t, uint64(1), positions["ch2"].Offset)
	require.NotEmpty(t, positions["ch1"].Epoch)
	require.NotEmpty(t, positions["ch2"].Epoch)

	// Disconnect after fully received events.
	require.NoError(t, resp.Body.Close())

	// More publications while disconnected.
	publishSSECursorMessage(t, node, "ch1", "m3", 100)
	publishSSECursorMessage(t, node, "ch2", "n2", 100)

	// Reconnect with the cursor.
	resp2, reader2 := dialSSECursor(t, server, channels, token)
	defer func() { _ = resp2.Body.Close() }()
	results, _ := readSSECursorHandshake(t, reader2, channels)

	// ch1 recovered m3 (offset 3).
	require.True(t, results[0].WasRecovering)
	require.True(t, results[0].Recovered)
	require.Len(t, results[0].Publications, 1)
	require.Equal(t, uint64(3), results[0].Publications[0].Offset)
	require.JSONEq(t, `"m3"`, string(results[0].Publications[0].Data))
	// On successful recovery the result echoes the requested position; the
	// client catches up to the stream top via the recovered publications.
	require.Equal(t, uint64(2), results[0].Offset)

	// ch2 recovered n2 (offset 2).
	require.True(t, results[1].WasRecovering)
	require.True(t, results[1].Recovered)
	require.Len(t, results[1].Publications, 1)
	require.Equal(t, uint64(2), results[1].Publications[0].Offset)
	require.JSONEq(t, `"n2"`, string(results[1].Publications[0].Data))

	// New publications stream from the exact boundary, no duplicates.
	publishSSECursorMessage(t, node, "ch1", "m4", 100)
	push = readSSECursorPublication(t, reader2)
	require.Equal(t, uint64(4), push.Pub.Offset)
	require.JSONEq(t, `"m4"`, string(push.Pub.Data))

	publishSSECursorMessage(t, node, "ch2", "n3", 100)
	push = readSSECursorPublication(t, reader2)
	require.Equal(t, uint64(3), push.Pub.Offset)
	require.JSONEq(t, `"n3"`, string(push.Pub.Data))
}

// TestSSERecoveryCursorMidEventDisconnect checks that an SSE event sent only
// half-way does not advance the delivered position: the reconnect recovers
// the interrupted publication.
func TestSSERecoveryCursorMidEventDisconnect(t *testing.T) {
	t.Parallel()
	node, server := newSSECursorTestServer(t, testSSECursorConfig)
	channels := []string{"ch1"}

	resp, reader := dialSSECursor(t, server, channels, "")
	_, token := readSSECursorHandshake(t, reader, channels)

	publishSSECursorMessage(t, node, "ch1", "small", 100)
	push := readSSECursorPublication(t, reader)
	require.Equal(t, uint64(1), push.Pub.Offset)
	token = readSSECursorToken(t, reader)
	positions := decodeSSECursorPositions(t, &testSSECursorConfig, token)
	require.Equal(t, uint64(1), positions["ch1"].Offset)

	// Publish a large message, then read only part of its SSE event before
	// dropping the connection.
	bigPayload := strings.Repeat("x", 256*1024)
	publishSSECursorMessage(t, node, "ch1", bigPayload, 100)
	partial := make([]byte, 128)
	_, err := io.ReadFull(reader, partial)
	require.NoError(t, err)
	require.Contains(t, string(partial), "data: ")
	require.NoError(t, resp.Body.Close())

	// Reconnect with the last complete cursor: the half-delivered publication
	// must be recovered in full.
	resp2, reader2 := dialSSECursor(t, server, channels, token)
	defer func() { _ = resp2.Body.Close() }()
	results, _ := readSSECursorHandshake(t, reader2, channels)
	require.True(t, results[0].WasRecovering)
	require.True(t, results[0].Recovered)
	require.Len(t, results[0].Publications, 1)
	require.Equal(t, uint64(2), results[0].Publications[0].Offset)
	require.JSONEq(t, strconvQuoteForTest(bigPayload), string(results[0].Publications[0].Data))
}

func strconvQuoteForTest(s string) string {
	quoted, _ := json.Marshal(s)
	return string(quoted)
}

// TestSSERecoveryCursorServerClose checks recovery after the server closes
// the connection.
func TestSSERecoveryCursorServerClose(t *testing.T) {
	t.Parallel()
	node, server := newSSECursorTestServer(t, testSSECursorConfig)
	channels := []string{"ch1", "ch2"}

	resp, reader := dialSSECursor(t, server, channels, "")
	_, token := readSSECursorHandshake(t, reader, channels)

	publishSSECursorMessage(t, node, "ch1", "m1", 100)
	readSSECursorPublication(t, reader)
	token = readSSECursorToken(t, reader)

	// Server closes the connection.
	require.NoError(t, node.Disconnect("42"))
	for {
		_, err := readSSECursorEvent(reader)
		if err != nil {
			break
		}
	}
	_ = resp.Body.Close()

	publishSSECursorMessage(t, node, "ch1", "m2", 100)
	publishSSECursorMessage(t, node, "ch2", "n1", 100)

	resp2, reader2 := dialSSECursor(t, server, channels, token)
	defer func() { _ = resp2.Body.Close() }()
	results, _ := readSSECursorHandshake(t, reader2, channels)

	require.True(t, results[0].WasRecovering)
	require.True(t, results[0].Recovered)
	require.Len(t, results[0].Publications, 1)
	require.Equal(t, uint64(2), results[0].Publications[0].Offset)
	require.JSONEq(t, `"m2"`, string(results[0].Publications[0].Data))

	// ch2 had no publications before the disconnect: its cursor position is
	// zero, so it recovers n1 from the start of the stream.
	require.True(t, results[1].WasRecovering)
	require.True(t, results[1].Recovered)
	require.Len(t, results[1].Publications, 1)
	require.Equal(t, uint64(1), results[1].Publications[0].Offset)
	require.JSONEq(t, `"n1"`, string(results[1].Publications[0].Data))
}

// TestSSERecoveryCursorHistoryExpiredPartialRecovery checks that when one
// channel's history expired, only that channel reports Recovered=false - the
// other channel's successful recovery is not hidden nor wrapped into a
// connection-level "all recovered" outcome.
func TestSSERecoveryCursorHistoryExpiredPartialRecovery(t *testing.T) {
	t.Parallel()
	node, server := newSSECursorTestServer(t, testSSECursorConfig)
	channels := []string{"ch1", "ch2"}

	resp, reader := dialSSECursor(t, server, channels, "")
	_, token := readSSECursorHandshake(t, reader, channels)

	publishSSECursorMessage(t, node, "ch1", "h1", 2)
	readSSECursorPublication(t, reader)
	publishSSECursorMessage(t, node, "ch2", "x1", 100)
	readSSECursorPublication(t, reader)
	token = readSSECursorToken(t, reader)
	require.NoError(t, resp.Body.Close())

	// Push ch1's position out of its (size 2) history; ch2 stays recoverable.
	publishSSECursorMessage(t, node, "ch1", "h2", 2)
	publishSSECursorMessage(t, node, "ch1", "h3", 2)
	publishSSECursorMessage(t, node, "ch1", "h4", 2)
	publishSSECursorMessage(t, node, "ch2", "x2", 100)

	resp2, reader2 := dialSSECursor(t, server, channels, token)
	defer func() { _ = resp2.Body.Close() }()
	results, _ := readSSECursorHandshake(t, reader2, channels)

	// ch1: history expired, recovery failed for this channel only.
	require.True(t, results[0].WasRecovering)
	require.False(t, results[0].Recovered)
	require.Empty(t, results[0].Publications)

	// ch2: recovered the missed publication.
	require.True(t, results[1].WasRecovering)
	require.True(t, results[1].Recovered)
	require.Len(t, results[1].Publications, 1)
	require.Equal(t, uint64(2), results[1].Publications[0].Offset)
	require.JSONEq(t, `"x2"`, string(results[1].Publications[0].Data))
}

// TestSSERecoveryCursorConnectSubscriptions checks cursor tracking for
// server-side subscriptions established in the connect reply.
func TestSSERecoveryCursorConnectSubscriptions(t *testing.T) {
	t.Parallel()
	node, err := New(Config{})
	require.NoError(t, err)
	broker, err := NewMemoryBroker(node, MemoryBrokerConfig{})
	require.NoError(t, err)
	node.SetBroker(broker)
	node.OnConnecting(func(ctx context.Context, event ConnectEvent) (ConnectReply, error) {
		return ConnectReply{
			Credentials: &Credentials{UserID: "42"},
			Subscriptions: map[string]SubscribeOptions{
				"ch1": {EnableRecovery: true, EnablePositioning: true},
			},
		}, nil
	})
	require.NoError(t, node.Run())
	t.Cleanup(func() { _ = node.Shutdown(context.Background()) })
	mux := http.NewServeMux()
	mux.Handle("/connection/sse", NewSSEHandler(node, SSEConfig{RecoveryCursor: testSSECursorConfig}))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	resp, reader := dialSSECursor(t, server, nil, "")
	reply := readSSECursorReply(t, reader)
	require.NotNil(t, reply.Connect)
	require.Contains(t, reply.Connect.Subs, "ch1")
	token := readSSECursorToken(t, reader)

	publishSSECursorMessage(t, node, "ch1", "m1", 100)
	readSSECursorPublication(t, reader)
	token = readSSECursorToken(t, reader)
	require.NoError(t, resp.Body.Close())

	publishSSECursorMessage(t, node, "ch1", "m2", 100)

	resp2, reader2 := dialSSECursor(t, server, nil, token)
	defer func() { _ = resp2.Body.Close() }()
	reply = readSSECursorReply(t, reader2)
	require.NotNil(t, reply.Connect)
	sub := reply.Connect.Subs["ch1"]
	require.NotNil(t, sub)
	require.True(t, sub.WasRecovering)
	require.True(t, sub.Recovered)
	require.Len(t, sub.Publications, 1)
	require.Equal(t, uint64(2), sub.Publications[0].Offset)
	require.JSONEq(t, `"m2"`, string(sub.Publications[0].Data))
}

// dialSSECursorWithToken connects with a token in the connect request.
func dialSSECursorWithToken(t *testing.T, server *httptest.Server, token string, channels []string, cursor string) (*http.Response, *bufio.Reader) {
	t.Helper()
	var frame strings.Builder
	connectCmd, err := json.Marshal(protocol.Command{Id: 1, Connect: &protocol.ConnectRequest{Token: token}})
	require.NoError(t, err)
	frame.Write(connectCmd)
	frame.WriteByte('\n')
	for i, ch := range channels {
		data, err := json.Marshal(protocol.Command{Id: uint32(i + 2), Subscribe: &protocol.SubscribeRequest{Channel: ch}})
		require.NoError(t, err)
		frame.Write(data)
		frame.WriteByte('\n')
	}
	reqURL, err := url.Parse(server.URL + "/connection/sse")
	require.NoError(t, err)
	query := reqURL.Query()
	query.Set(connectUrlParam, frame.String())
	if cursor != "" {
		query.Set(recoveryCursorUrlParam, cursor)
	}
	reqURL.RawQuery = query.Encode()
	resp, err := http.Get(reqURL.String())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return resp, bufio.NewReader(resp.Body)
}

// TestSSERecoveryCursorUserBinding checks that a cursor is bound to the user
// it was issued for and is ignored for another user.
func TestSSERecoveryCursorUserBinding(t *testing.T) {
	t.Parallel()
	node, err := New(Config{})
	require.NoError(t, err)
	broker, err := NewMemoryBroker(node, MemoryBrokerConfig{})
	require.NoError(t, err)
	node.SetBroker(broker)
	node.OnConnecting(func(ctx context.Context, event ConnectEvent) (ConnectReply, error) {
		return ConnectReply{Credentials: &Credentials{UserID: event.Token}}, nil
	})
	node.OnConnect(func(client *Client) {
		client.OnSubscribe(func(event SubscribeEvent, callback SubscribeCallback) {
			callback(SubscribeReply{Options: SubscribeOptions{
				EnableRecovery:    true,
				EnablePositioning: true,
			}}, nil)
		})
	})
	require.NoError(t, node.Run())
	t.Cleanup(func() { _ = node.Shutdown(context.Background()) })
	mux := http.NewServeMux()
	mux.Handle("/connection/sse", NewSSEHandler(node, SSEConfig{RecoveryCursor: testSSECursorConfig}))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	channels := []string{"ch1"}
	resp, reader := dialSSECursorWithToken(t, server, "user1", channels, "")
	_, _ = readSSECursorHandshake(t, reader, channels)
	publishSSECursorMessage(t, node, "ch1", "m1", 100)
	readSSECursorPublication(t, reader)
	token := readSSECursorToken(t, reader)
	require.NoError(t, resp.Body.Close())

	publishSSECursorMessage(t, node, "ch1", "m2", 100)

	// Another user can not use the cursor: no recovery is attempted.
	resp2, reader2 := dialSSECursorWithToken(t, server, "user2", channels, token)
	results, _ := readSSECursorHandshake(t, reader2, channels)
	require.False(t, results[0].WasRecovering)
	require.False(t, results[0].Recovered)
	_ = resp2.Body.Close()

	// The original user still recovers.
	resp3, reader3 := dialSSECursorWithToken(t, server, "user1", channels, token)
	defer func() { _ = resp3.Body.Close() }()
	results, _ = readSSECursorHandshake(t, reader3, channels)
	require.True(t, results[0].WasRecovering)
	require.True(t, results[0].Recovered)
	require.Len(t, results[0].Publications, 1)
	require.JSONEq(t, `"m2"`, string(results[0].Publications[0].Data))
}

// TestSSERecoveryCursorPermissionStillEnforced checks that a valid cursor
// does not act as an authorization credential: subscribe permission checks
// still run before any recovery happens.
func TestSSERecoveryCursorPermissionStillEnforced(t *testing.T) {
	t.Parallel()
	node, err := New(Config{})
	require.NoError(t, err)
	broker, err := NewMemoryBroker(node, MemoryBrokerConfig{})
	require.NoError(t, err)
	node.SetBroker(broker)
	node.OnConnecting(func(ctx context.Context, event ConnectEvent) (ConnectReply, error) {
		return ConnectReply{Credentials: &Credentials{UserID: "42"}}, nil
	})
	node.OnConnect(func(client *Client) {
		client.OnSubscribe(func(event SubscribeEvent, callback SubscribeCallback) {
			if event.Channel == "denied" {
				callback(SubscribeReply{}, ErrorPermissionDenied)
				return
			}
			callback(SubscribeReply{Options: SubscribeOptions{
				EnableRecovery:    true,
				EnablePositioning: true,
			}}, nil)
		})
	})
	require.NoError(t, node.Run())
	t.Cleanup(func() { _ = node.Shutdown(context.Background()) })
	mux := http.NewServeMux()
	mux.Handle("/connection/sse", NewSSEHandler(node, SSEConfig{RecoveryCursor: testSSECursorConfig}))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	// A validly signed cursor claiming a position in the denied channel.
	token, err := encodeRecoveryCursor(&testSSECursorConfig, "42", map[string]StreamPosition{
		"denied": {Offset: 5},
	}, time.Now())
	require.NoError(t, err)

	resp, reader := dialSSECursor(t, server, []string{"denied"}, token)
	defer func() { _ = resp.Body.Close() }()
	reply := readSSECursorReply(t, reader)
	require.NotNil(t, reply.Connect)
	reply = readSSECursorReply(t, reader)
	require.NotNil(t, reply.Error)
	require.Equal(t, ErrorPermissionDenied.Code, reply.Error.Code)
}

// reconnectWithCursor is a helper which connects to ch1 and reports the
// recovery outcome of the subscribe reply.
func reconnectWithCursor(t *testing.T, server *httptest.Server, cursor string) *protocol.SubscribeResult {
	t.Helper()
	resp, reader := dialSSECursor(t, server, []string{"ch1"}, cursor)
	t.Cleanup(func() { _ = resp.Body.Close() })
	reply := readSSECursorReply(t, reader)
	require.NotNil(t, reply.Connect)
	reply = readSSECursorReply(t, reader)
	require.Nil(t, reply.Error)
	require.NotNil(t, reply.Subscribe)
	return reply.Subscribe
}

// TestSSERecoveryCursorTamperedRejected checks that a tampered cursor is
// ignored and the connection proceeds without recovery.
func TestSSERecoveryCursorTamperedRejected(t *testing.T) {
	t.Parallel()
	node, server := newSSECursorTestServer(t, testSSECursorConfig)
	publishSSECursorMessage(t, node, "ch1", "m1", 100)
	publishSSECursorMessage(t, node, "ch1", "m2", 100)

	token, err := encodeRecoveryCursor(&testSSECursorConfig, "42", map[string]StreamPosition{
		"ch1": {Offset: 1},
	}, time.Now())
	require.NoError(t, err)

	sub := reconnectWithCursor(t, server, token+"x")
	require.False(t, sub.WasRecovering)
	require.False(t, sub.Recovered)

	// Sanity: the untampered cursor recovers m2.
	sub = reconnectWithCursor(t, server, token)
	require.True(t, sub.WasRecovering)
	require.True(t, sub.Recovered)
	require.Len(t, sub.Publications, 1)
	require.JSONEq(t, `"m2"`, string(sub.Publications[0].Data))
}

// TestSSERecoveryCursorExpiredRejected checks that an expired cursor is
// ignored.
func TestSSERecoveryCursorExpiredRejected(t *testing.T) {
	t.Parallel()
	config := RecoveryCursorConfig{Secret: "exp-secret", TTL: time.Hour}
	node, server := newSSECursorTestServer(t, config)
	publishSSECursorMessage(t, node, "ch1", "m1", 100)
	publishSSECursorMessage(t, node, "ch1", "m2", 100)

	token, err := encodeRecoveryCursor(&config, "42", map[string]StreamPosition{
		"ch1": {Offset: 1},
	}, time.Now().Add(-2*time.Hour))
	require.NoError(t, err)

	sub := reconnectWithCursor(t, server, token)
	require.False(t, sub.WasRecovering)
	require.False(t, sub.Recovered)
}

// TestSSERecoveryCursorKeyRotation checks that cursors signed with a
// recently rotated secret verify within the configured window, and are
// rejected after it.
func TestSSERecoveryCursorKeyRotation(t *testing.T) {
	t.Parallel()
	config := RecoveryCursorConfig{
		Secret: "new-secret",
		PreviousSecrets: []RotatedSecret{
			{Secret: "old-secret", VerifyUntil: time.Now().Add(time.Hour)},
		},
	}
	node, server := newSSECursorTestServer(t, config)
	publishSSECursorMessage(t, node, "ch1", "m1", 100)
	publishSSECursorMessage(t, node, "ch1", "m2", 100)

	oldConfig := RecoveryCursorConfig{Secret: "old-secret"}
	token, err := encodeRecoveryCursor(&oldConfig, "42", map[string]StreamPosition{
		"ch1": {Offset: 1},
	}, time.Now())
	require.NoError(t, err)

	// Within the verification window the old secret still works.
	sub := reconnectWithCursor(t, server, token)
	require.True(t, sub.WasRecovering)
	require.True(t, sub.Recovered)
	require.Len(t, sub.Publications, 1)
	require.JSONEq(t, `"m2"`, string(sub.Publications[0].Data))

	// After the window it does not.
	expiredConfig := RecoveryCursorConfig{
		Secret: "new-secret",
		PreviousSecrets: []RotatedSecret{
			{Secret: "old-secret", VerifyUntil: time.Now().Add(-time.Hour)},
		},
	}
	node2, server2 := newSSECursorTestServer(t, expiredConfig)
	publishSSECursorMessage(t, node2, "ch1", "m1", 100)
	publishSSECursorMessage(t, node2, "ch1", "m2", 100)
	sub = reconnectWithCursor(t, server2, token)
	require.False(t, sub.WasRecovering)
	require.False(t, sub.Recovered)
}
