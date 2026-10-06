package centrifuge

import (
	"errors"
	"io"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/centrifugal/centrifuge/internal/convert"
	"github.com/centrifugal/centrifuge/internal/readerpool"

	"github.com/centrifugal/protocol"
)

// SSEConfig represents config for SSEHandler.
type SSEConfig struct {
	PingPongConfig
	// MaxRequestBodySize limits initial request body size (when SSE starts with POST).
	MaxRequestBodySize int
	// RecoveryCursor enables the optional recovery cursor: the server then
	// emits signed cursor events into the stream as delivered channel
	// positions advance, and accepts a cursor back on reconnect to seed
	// per-channel recovery positions. Disabled by default.
	RecoveryCursor RecoveryCursorConfig
}

// SSEHandler handles WebSocket client connections. WebSocket protocol
// is a bidirectional connection between a client and a server for low-latency
// communication.
type SSEHandler struct {
	node   *Node
	config SSEConfig
}

// NewSSEHandler creates new SSEHandler.
func NewSSEHandler(node *Node, config SSEConfig) *SSEHandler {
	warnAboutIncorrectPingPongConfig(node, config.PingPongConfig, transportSSE)
	return &SSEHandler{
		node:   node,
		config: config,
	}
}

// Since SSE is usually starts with a GET request (at least in browsers) we are looking
// for connect request in URL params. This should be a properly encoded command(s) in
// Centrifuge protocol.
const connectUrlParam = "cf_connect"

// recoveryCursorUrlParam is the URL parameter clients use to send the last
// received recovery cursor back to the server on reconnect.
const recoveryCursorUrlParam = "cf_recovery_cursor"

const defaultMaxSSEBodySize = 64 * 1024

func (h *SSEHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, ok := w.(http.Flusher)
	if !ok {
		h.node.logger.log(newErrorLogEntry(errors.New("not http.Flusher"), "SSE: ResponseWriter is not a Flusher", map[string]any{}))
		http.Error(w, "expected http.ResponseWriter to be http.Flusher", http.StatusInternalServerError)
		return
	}

	// messageSizeLimit bounds both the request body and a single decoded command;
	// it must be positive since the stream decoder rejects a non-positive limit.
	messageSizeLimit := h.config.MaxRequestBodySize
	if messageSizeLimit <= 0 {
		messageSizeLimit = defaultMaxSSEBodySize
	}

	var requestData []byte
	if r.Method == http.MethodGet {
		requestDataString := r.URL.Query().Get(connectUrlParam)
		if requestDataString != "" {
			requestData = []byte(requestDataString)
		} else {
			h.node.logger.log(newLogEntry(LogLevelDebug, "no connect command", map[string]any{}))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	} else if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, int64(messageSizeLimit))
		var err error
		requestData, err = io.ReadAll(r.Body)
		if err != nil {
			h.node.logger.log(newLogEntry(LogLevelInfo, "error reading sse request body", map[string]any{"error": err.Error()}))
			if len(requestData) >= messageSizeLimit {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				return
			}
			w.WriteHeader(statusCodeClientConnectionClosed)
			return
		}
	} else {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var cursorConfig *RecoveryCursorConfig
	if h.config.RecoveryCursor.enabled() {
		cursorConfig = &h.config.RecoveryCursor
		if token := r.URL.Query().Get(recoveryCursorUrlParam); token != "" {
			payload, err := decodeRecoveryCursor(cursorConfig, token, time.Now())
			if err != nil {
				h.node.logger.log(newLogEntry(LogLevelDebug, "invalid recovery cursor", map[string]any{"error": err.Error()}))
			} else {
				r = r.WithContext(withRecoveryCursor(r.Context(), payload))
			}
		}
	}

	ack := make(chan struct{})
	transport := newSSETransport(r, sseTransportConfig{
		pingPong:   h.config.PingPongConfig,
		protoMajor: uint8(r.ProtoMajor),
	}, ack)

	c, closeFn, err := NewClient(r.Context(), h.node, transport)
	if err != nil {
		h.node.logger.log(newErrorLogEntry(err, "error create client", map[string]any{"error": err.Error(), "transport": transportSSE}))
		return
	}
	defer func() { _ = closeFn() }()
	defer close(transport.closedCh) // need to execute this after client closeFn.

	if h.node.logEnabled(LogLevelDebug) {
		h.node.logger.log(newLogEntry(LogLevelDebug, "client connection established", map[string]any{"transport": transport.Name(), "client": c.ID()}))
		defer func(started time.Time) {
			h.node.logger.log(newLogEntry(LogLevelDebug, "client connection completed", map[string]any{"duration": time.Since(started).String(), "transport": transport.Name(), "client": c.ID()}))
		}(time.Now())
	}

	if r.ProtoMajor == 1 {
		// An endpoint MUST NOT generate an HTTP/2 message containing connection-specific header fields.
		// Source: RFC7540.
		w.Header().Set("Connection", "keep-alive")
	}
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-cache, no-store, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expire", "0")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	err = rc.SetWriteDeadline(time.Now().Add(streamingResponseWriteTimeout))
	if err != nil && h.node.logEnabled(LogLevelTrace) {
		h.node.logger.log(newLogEntry(LogLevelTrace, "can't set custom write deadline", map[string]any{"error": err.Error()}))
	}
	_, err = w.Write([]byte("\r\n"))
	if err != nil {
		return
	}
	_ = rc.Flush()
	_ = rc.SetWriteDeadline(time.Time{})

	reader := readerpool.GetBytesReader(requestData)
	_ = HandleReadFrame(c, reader, int64(messageSizeLimit))
	readerpool.PutBytesReader(reader)

	var cursorTracker *sseRecoveryCursorTracker
	if cursorConfig != nil {
		cursorTracker = newSSERecoveryCursorTracker(c, h.node, cursorConfig, requestData, int64(messageSizeLimit))
	}

	sendAck := func() {
		select {
		case ack <- struct{}{}:
		case <-r.Context().Done():
		}
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case <-transport.disconnectCh:
			return
		case messages, messagesOK := <-transport.messages:
			if !messagesOK {
				sendAck()
				return
			}
			_ = rc.SetWriteDeadline(time.Now().Add(streamingResponseWriteTimeout))
			// Scan position updates before writing, but only apply them after
			// the entire batch has been written and flushed: an SSE event sent
			// half-way must not advance the delivered positions.
			var cursorUpdates map[string]StreamPosition
			if cursorTracker != nil {
				cursorUpdates = cursorTracker.scan(messages)
			}
			for _, msg := range messages {
				_, err := w.Write(convert.StringToBytes("data: " + convert.BytesToString(msg) + "\n\n"))
				if err != nil {
					sendAck()
					return
				}
			}
			_ = rc.Flush()
			if cursorTracker != nil {
				if token, emit := cursorTracker.apply(cursorUpdates); emit {
					_, err := w.Write(convert.StringToBytes("event: cursor\ndata: " + token + "\n\n"))
					if err != nil {
						sendAck()
						return
					}
					_ = rc.Flush()
				}
			}
			_ = rc.SetWriteDeadline(time.Time{})
			sendAck()
		}
	}
}

const (
	transportSSE = "sse"
)

type sseTransport struct {
	mu           sync.Mutex
	req          *http.Request
	ack          chan struct{}
	messages     chan [][]byte
	disconnectCh chan struct{}
	closedCh     chan struct{}
	config       sseTransportConfig
	closed       bool
}

type sseTransportConfig struct {
	pingPong   PingPongConfig
	protoMajor uint8
}

func newSSETransport(req *http.Request, config sseTransportConfig, ack chan struct{}) *sseTransport {
	return &sseTransport{
		messages:     make(chan [][]byte),
		disconnectCh: make(chan struct{}),
		closedCh:     make(chan struct{}),
		req:          req,
		config:       config,
		ack:          ack,
	}
}

func (t *sseTransport) Name() string {
	return transportSSE
}

func (t *sseTransport) AcceptProtocol() string {
	return getAcceptProtocolLabel(int8(t.config.protoMajor))
}

func (t *sseTransport) Protocol() ProtocolType {
	return ProtocolTypeJSON
}

// ProtocolVersion returns transport protocol version.
func (t *sseTransport) ProtocolVersion() ProtocolVersion {
	return ProtocolVersion2
}

// Unidirectional returns whether transport is unidirectional.
func (t *sseTransport) Unidirectional() bool {
	return false
}

// Emulation ...
func (t *sseTransport) Emulation() bool {
	return true
}

// DisabledPushFlags ...
func (t *sseTransport) DisabledPushFlags() uint64 {
	return 0
}

// PingPongConfig ...
func (t *sseTransport) PingPongConfig() PingPongConfig {
	return t.config.pingPong
}

func (t *sseTransport) Write(message []byte) error {
	return t.WriteMany(message)
}

func (t *sseTransport) WriteMany(messages ...[]byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	select {
	case t.messages <- messages:
	case <-t.closedCh:
	}
	select {
	case <-t.ack:
	case <-t.closedCh:
		return nil
	}
	return nil
}

func (t *sseTransport) Close(_ Disconnect) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	close(t.disconnectCh)
	<-t.closedCh
	return nil
}

// sseRecoveryCursorTracker tracks per-channel positions actually delivered to
// the client over the SSE stream and builds recovery cursor tokens from them.
// Positions advance only after a batch of messages has been fully written to
// the wire and flushed, so a half-sent SSE event never moves the cursor.
type sseRecoveryCursorTracker struct {
	client *Client
	node   *Node
	config *RecoveryCursorConfig
	// subscribeIDs maps command IDs of subscribe commands in the initial
	// request frame to their channel, so their replies (which do not carry
	// the channel) can be folded into delivered positions.
	subscribeIDs map[uint32]string
	delivered    map[string]StreamPosition
}

func newSSERecoveryCursorTracker(c *Client, node *Node, config *RecoveryCursorConfig, requestData []byte, messageSizeLimit int64) *sseRecoveryCursorTracker {
	tracker := &sseRecoveryCursorTracker{
		client:    c,
		node:      node,
		config:    config,
		delivered: make(map[string]StreamPosition),
	}
	reader := readerpool.GetBytesReader(requestData)
	decoder := protocol.GetStreamCommandDecoderLimited(ProtocolTypeJSON.toProto(), reader, messageSizeLimit)
	for {
		cmd, _, err := decoder.Decode()
		if cmd != nil && cmd.Subscribe != nil {
			if tracker.subscribeIDs == nil {
				tracker.subscribeIDs = make(map[uint32]string)
			}
			tracker.subscribeIDs[cmd.Id] = cmd.Subscribe.Channel
		}
		if err != nil {
			break
		}
	}
	protocol.PutStreamCommandDecoder(ProtocolTypeJSON.toProto(), decoder)
	readerpool.PutBytesReader(reader)
	return tracker
}

// scan decodes outgoing protocol replies and computes the position updates
// the batch would bring once delivered. It does not mutate the tracker.
func (t *sseRecoveryCursorTracker) scan(messages [][]byte) map[string]StreamPosition {
	var updates map[string]StreamPosition
	setUpdate := func(ch string, sp StreamPosition) {
		if updates == nil {
			updates = make(map[string]StreamPosition)
		}
		updates[ch] = sp
	}
	for _, msg := range messages {
		reply, err := protocol.NewJSONReplyDecoder(msg).Decode()
		if err != nil {
			continue
		}
		if connect := reply.Connect; connect != nil {
			for ch, res := range connect.Subs {
				if res == nil {
					continue
				}
				setUpdate(ch, StreamPosition{Offset: res.Offset, Epoch: res.Epoch})
			}
		}
		if subscribe := reply.Subscribe; subscribe != nil {
			if ch, ok := t.subscribeIDs[reply.Id]; ok {
				setUpdate(ch, StreamPosition{Offset: subscribe.Offset, Epoch: subscribe.Epoch})
			}
		}
		if push := reply.Push; push != nil && push.Pub != nil && push.Channel != "" {
			pub := push.Pub
			if pub.Offset == 0 || pub.Offset == math.MaxUint64 {
				// Publications without a stream position, and the special
				// insufficient-state marker, carry no deliverable position.
				continue
			}
			epoch := ""
			if sp, ok := updates[push.Channel]; ok {
				epoch = sp.Epoch
			} else if sp, ok := t.delivered[push.Channel]; ok {
				if sp.Offset >= pub.Offset {
					// Already delivered this or a later position.
					continue
				}
				epoch = sp.Epoch
			}
			if epoch == "" {
				// The publication push does not carry the stream epoch. When
				// the channel was subscribed while its stream was empty the
				// epoch is only adopted once the first publication is
				// processed - which happened before this push was queued.
				if sp, ok := t.client.channelStreamPosition(push.Channel); ok {
					epoch = sp.Epoch
				}
			}
			setUpdate(push.Channel, StreamPosition{Offset: pub.Offset, Epoch: epoch})
		}
	}
	return updates
}

// apply folds scanned updates into the delivered positions - called only
// after the batch was fully written and flushed. It returns a fresh cursor
// token to emit when delivered positions advanced.
func (t *sseRecoveryCursorTracker) apply(updates map[string]StreamPosition) (string, bool) {
	changed := false
	for ch, sp := range updates {
		if t.delivered[ch] != sp {
			t.delivered[ch] = sp
			changed = true
		}
	}
	if !changed {
		return "", false
	}
	// Bind the cursor to the current subscription set: drop channels the
	// client is no longer subscribed to.
	subscriptions := t.client.ChannelsWithContext()
	positions := make(map[string]StreamPosition, len(t.delivered))
	for ch, sp := range t.delivered {
		chCtx, ok := subscriptions[ch]
		if !ok || channelHasFlag(chCtx.flags, flagMap) {
			continue
		}
		positions[ch] = sp
	}
	if len(positions) == 0 {
		return "", false
	}
	token, err := encodeRecoveryCursor(t.config, t.client.UserID(), positions, time.Now())
	if err != nil {
		t.node.logger.log(newLogEntry(LogLevelDebug, "can not emit recovery cursor", map[string]any{"error": err.Error()}))
		return "", false
	}
	return token, true
}
