package centrifuge

import (
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/centrifugal/centrifuge/internal/convert"
	"github.com/centrifugal/centrifuge/internal/readerpool"
	"github.com/centrifugal/centrifuge/internal/sserecovery"
)

// SSEConfig represents config for SSEHandler.
type SSEConfig struct {
	PingPongConfig
	// MaxRequestBodySize limits initial request body size (when SSE starts with POST).
	MaxRequestBodySize int
	// Recovery, when enabled, attaches a signed recovery cursor to the stream and
	// consumes a cursor presented on reconnect. The zero value disables it.
	Recovery SSERecoveryConfig
}

// SSEHandler handles WebSocket client connections. WebSocket protocol
// is a bidirectional connection between a client and a server for low-latency
// communication.
type SSEHandler struct {
	node   *Node
	config SSEConfig
	signer *sserecovery.Signer
}

// NewSSEHandler creates new SSEHandler.
func NewSSEHandler(node *Node, config SSEConfig) *SSEHandler {
	warnAboutIncorrectPingPongConfig(node, config.PingPongConfig, transportSSE)
	h, err := newSSEHandler(node, config)
	if err != nil {
		// Preserve the historical constructor contract: an invalid recovery
		// configuration disables the feature rather than failing construction.
		node.logger.log(newErrorLogEntry(err, "SSE: recovery cursor disabled due to invalid config", map[string]any{"error": err.Error()}))
		config.Recovery = SSERecoveryConfig{}
		h, err = newSSEHandler(node, config)
		if err != nil {
			panic(err) // unreachable once recovery is disabled.
		}
	}
	return h
}

// NewSSEHandlerWithRecovery creates an SSEHandler with recovery cursors enabled
// and returns a configuration error instead of silently disabling the feature.
func NewSSEHandlerWithRecovery(node *Node, config SSEConfig) (*SSEHandler, error) {
	warnAboutIncorrectPingPongConfig(node, config.PingPongConfig, transportSSE)
	return newSSEHandler(node, config)
}

func newSSEHandler(node *Node, config SSEConfig) (*SSEHandler, error) {
	signer, err := config.Recovery.newRecoverySigner()
	if err != nil {
		return nil, err
	}
	return &SSEHandler{
		node:   node,
		config: config,
		signer: signer,
	}, nil
}

// Since SSE is usually starts with a GET request (at least in browsers) we are looking
// for connect request in URL params. This should be a properly encoded command(s) in
// Centrifuge protocol.
const connectUrlParam = "cf_connect"

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

	// Recovery cursor handling (optional). The cursor is verified cryptographically
	// before it can influence anything, and it never grants access: subscription
	// permissions are evaluated as usual while the connect command runs.
	recoveryEnabled := h.signer != nil
	var (
		tracker     *sserecovery.Tracker
		scan        *frameScan
		verifiedCur *sserecovery.Cursor
	)
	if recoveryEnabled {
		tracker = sserecovery.NewTracker()
		scan = newFrameScan(tracker)
		if token := cursorFromRequest(r); token != "" {
			cur, err := h.signer.Verify(token)
			if err != nil {
				h.node.logger.log(newLogEntry(LogLevelInfo, "SSE: recovery cursor rejected", map[string]any{"error": err.Error()}))
			} else {
				requestData = applyRecoveryCursor(requestData, cur)
				verifiedCur = &cur
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

	// Bind the cursor to the identity the server actually authenticated. The
	// cursor is not a credential: a mismatch means it was minted for a different
	// user, so the connection is dropped with a bad-request disconnect.
	if verifiedCur != nil && verifiedCur.User != "" && c.UserID() != verifiedCur.User {
		h.node.logger.log(newLogEntry(LogLevelInfo, "SSE: recovery cursor user mismatch", map[string]any{
			"client": c.ID(), "cursor_user": verifiedCur.User, "user": c.UserID(),
		}))
		// Close asynchronously (mirroring every other force-close path): the
		// close waits for the writer to drain the disconnect push, which only the
		// loop below can acknowledge. No subscription frames are written before the
		// loop runs, so no unauthorized data leaks.
		go func() { _ = c.close(DisconnectBadRequest) }()
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

			if !recoveryEnabled {
				if !writeSSEBatch(w, rc, messages) {
					sendAck()
					return
				}
			} else {
				// Stage offsets from this batch, then write every event carrying
				// a cumulative cursor. The id line for event k reflects exactly
				// events 1..k, and it follows the event's data, so the browser
				// adopts it atomically only once that whole event arrived: an
				// event cut off mid-body cannot move the delivered position.
				for _, msg := range messages {
					scan.scanFrame(msg)
				}

				cursors := make([]string, len(messages))
				for i := range messages {
					cursors[i] = h.mintCursor(c.UserID(), tracker.SnapshotWith(scan.channelsUpTo(i)))
				}

				if !writeSSEBatchWithCursors(w, rc, messages, cursors) {
					scan.discardAdvances()
					sendAck()
					return
				}
				// Fully written and flushed: now the positions are real.
				scan.commitAdvances()
				sendAck()
				continue
			}
			sendAck()
		}
	}
}

// mintCursor signs a token for the user's delivered positions. Channels with no
// delivered position are omitted by the tracker snapshot, so a channel whose
// history expired cannot be hidden behind a full-success verdict.
func (h *SSEHandler) mintCursor(user string, channels []sserecovery.Channel) string {
	if h.signer == nil {
		return ""
	}
	token, err := h.signer.Issue(sserecovery.Cursor{User: user, Channels: channels})
	if err != nil {
		h.node.logger.log(newLogEntry(LogLevelInfo, "SSE: failed to issue recovery cursor", map[string]any{"error": err.Error()}))
		return ""
	}
	return token
}

// writeSSEBatch writes one writer batch as SSE data events with no id field.
func writeSSEBatch(w http.ResponseWriter, rc *http.ResponseController, messages [][]byte) bool {
	_ = rc.SetWriteDeadline(time.Now().Add(streamingResponseWriteTimeout))
	for _, msg := range messages {
		if _, err := w.Write(convert.StringToBytes("data: " + convert.BytesToString(msg) + "\n\n")); err != nil {
			return false
		}
	}
	return flushSSE(w, rc)
}

// writeSSEBatchWithCursors writes one batch as SSE data events, attaching to
// each event its cumulative recovery cursor (positions of events up to and
// including it). The cursor follows the event's data, so a client that only
// partially received an event never adopts its position. cursors may contain
// empty strings for events that advance no tracked position.
func writeSSEBatchWithCursors(w http.ResponseWriter, rc *http.ResponseController, messages [][]byte, cursors []string) bool {
	_ = rc.SetWriteDeadline(time.Now().Add(streamingResponseWriteTimeout))
	for i, msg := range messages {
		cursorToken := ""
		if i < len(cursors) {
			cursorToken = cursors[i]
		}
		if cursorToken != "" {
			if _, err := w.Write(convert.StringToBytes("id: " + cursorToken + "\n")); err != nil {
				return false
			}
		}
		if _, err := w.Write(convert.StringToBytes("data: " + convert.BytesToString(msg) + "\n\n")); err != nil {
			return false
		}
	}
	return flushSSE(w, rc)
}

func flushSSE(w http.ResponseWriter, rc *http.ResponseController) bool {
	if err := rc.Flush(); err != nil {
		return false
	}
	_ = rc.SetWriteDeadline(time.Time{})
	return true
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
