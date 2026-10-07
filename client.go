// Package rpc2 provides bi-directional RPC client and server similar to net/rpc.
package rpc2

import (
	"context"
	"errors"
	"io"
	"log"
	"reflect"
	"sync"
	"time"
)

// halfWriteCloser is implemented by codecs whose underlying connection can
// shut down the write direction independently (TCP half close). Drain uses it
// to send a FIN after the final writes instead of an abortive close.
type halfWriteCloser interface {
	CloseWrite() error
}

// Client represents an RPC Client.
// There may be multiple outstanding Calls associated
// with a single Client, and a Client may be used by
// multiple goroutines simultaneously.
type Client struct {
	mutex      sync.Mutex // protects pending, seq, request and drain state
	sending    sync.Mutex
	request    Request // temp area used in send()
	seq        uint64
	pending    map[uint64]*Call
	closing    bool
	shutdown   bool
	draining   bool // Drain has been initiated on this client
	peerDrain  bool // the peer has asked us to stop sending new calls
	peerReady  bool // the peer has finished draining its own work
	activeIn   int  // admitted inbound requests whose handlers have not returned
	idle       *sync.Cond
	drainRes   error // terminal result of the active drain, set on disconnect
	drainClean bool  // the active drain reached an agreed close
	server     bool
	codec      Codec
	handlers   map[string]*handler
	disconnect chan struct{}
	State      *State // additional information to associate with client
	blocking   bool   // whether to block request handling
}

// NewClient returns a new Client to handle requests to the
// set of services at the other end of the connection.
// It adds a buffer to the write side of the connection so
// the header and payload are sent as a unit.
func NewClient(conn io.ReadWriteCloser) *Client {
	return NewClientWithCodec(NewGobCodec(conn))
}

// NewClientWithCodec is like NewClient but uses the specified
// codec to encode requests and decode responses.
func NewClientWithCodec(codec Codec) *Client {
	c := &Client{
		codec:      codec,
		pending:    make(map[uint64]*Call),
		handlers:   make(map[string]*handler),
		disconnect: make(chan struct{}),
		seq:        1, // 0 means notification.
	}
	c.idle = sync.NewCond(&c.mutex)
	return c
}

// goAwayMethod is an internal notification sent to tell the peer that this
// client is draining and that it must not start new outbound calls.
// Peers that do not implement draining answer with a "method not found"
// error; because it is a notification (Seq == 0) no response is read and it
// has no effect on them.
const goAwayMethod = "_rpc2.goAway"

// drainReadyMethod is an internal notification sent, after the local side
// has drained, to tell the peer that no more work remains here. The
// connection is closed once both sides are ready and idle; this is what
// lets an admitted handler finish a nested callback into a peer that is
// draining simultaneously.
const drainReadyMethod = "_rpc2.drainReady"

// ErrDraining is returned for locally initiated calls after this client or
// the peer has started draining and the call cannot be part of work that
// was already admitted.
var ErrDraining = errors.New("connection is draining")

// IsDraining reports whether Drain has been initiated on this client.
// Handlers can use it to decline extra work that was not required to finish
// an already admitted request.
func (c *Client) IsDraining() bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.draining
}

// SetBlocking puts the client in blocking mode.
// In blocking mode, received requests are processes synchronously.
// If you have methods that may take a long time, other subsequent requests may time out.
func (c *Client) SetBlocking(blocking bool) {
	c.blocking = blocking
}

// Run the client's read loop.
// You must run this method before calling any methods on the server.
func (c *Client) Run() {
	c.readLoop()
}

// DisconnectNotify returns a channel that is closed
// when the client connection has gone away.
func (c *Client) DisconnectNotify() chan struct{} {
	return c.disconnect
}

// Handle registers the handler function for the given method. If a handler already exists for method, Handle panics.
func (c *Client) Handle(method string, handlerFunc interface{}) {
	addHandler(c.handlers, method, handlerFunc)
}

// readLoop reads messages from codec.
// It reads a reqeust or a response to the previous request.
// If the message is request, calls the handler function.
// If the message is response, sends the reply to the associated call.
func (c *Client) readLoop() {
	var err error
	var req Request
	var resp Response
	for err == nil {
		req = Request{}
		resp = Response{}
		if err = c.codec.ReadHeader(&req, &resp); err != nil {
			break
		}

		if req.Method != "" {
			// request comes to server
			if err = c.readRequest(&req); err != nil {
				debugln("rpc2: error reading request:", err.Error())
			}
		} else {
			// response comes to client
			if err = c.readResponse(&resp); err != nil {
				debugln("rpc2: error reading response:", err.Error())
			}
		}
	}
	// Terminate pending calls.
	c.sending.Lock()
	c.mutex.Lock()
	c.shutdown = true
	closing := c.closing
	// A clean EOF is expected when we ourselves are closing, or when the
	// peer half-closes its write side to finish a mutual drain.
	cleanReadEOF := err == io.EOF
	gracefulClose := closing || (c.draining && cleanReadEOF)
	peerHalfClosed := err == io.EOF && !closing && c.draining
	if err == io.EOF {
		if closing || c.draining {
			err = ErrShutdown
		} else {
			err = io.ErrUnexpectedEOF
		}
	}
	for seq, call := range c.pending {
		call.Error = err
		delete(c.pending, seq)
		call.done()
	}
	// Give the drain (if any) a definite result and release everyone
	// waiting for the connection to reach its terminal state.
	if c.drainRes == nil {
		if closing || (c.draining && gracefulClose) {
			c.drainRes = nil
			c.drainClean = true
		} else {
			c.drainRes = err
		}
	}
	c.idle.Broadcast()
	c.mutex.Unlock()
	c.sending.Unlock()
	if err != io.EOF && !closing && !c.draining && !c.server {
		debugln("rpc2: client protocol error:", err)
	}
	close(c.disconnect)
	if !closing {
		// A clean peer EOF during a drain is its half close: reply with a
		// half close of our own so the FIN handshake completes and final
		// responses stay ordered (a full close on Windows can RST and
		// discard buffered replies). An unexpected error is a real failure:
		// close fully so every pending call reaches an error terminal state.
		if peerHalfClosed {
			if hc, ok := c.codec.(halfWriteCloser); ok {
				if err := hc.CloseWrite(); err == nil {
					return
				}
			}
		}
		c.codec.Close()
	}
}

func (c *Client) handleRequest(req Request, method *handler, argv reflect.Value) {
	// Invoke the method, providing a new value for the reply.
	replyv := reflect.New(method.replyType.Elem())

	returnValues := method.fn.Call([]reflect.Value{reflect.ValueOf(c), argv, replyv})

	// Do not send response if request is a notification.
	if req.Seq == 0 {
		return
	}

	// The return value for the method is an error.
	errInter := returnValues[0].Interface()
	errmsg := ""
	if errInter != nil {
		errmsg = errInter.(error).Error()
	}
	resp := &Response{
		Seq:   req.Seq,
		Error: errmsg,
	}
	if err := c.codec.WriteResponse(resp, replyv.Interface()); err != nil {
		debugln("rpc2: error writing response:", err.Error())
	}
}

func (c *Client) readRequest(req *Request) error {
	// Internal drain notifications from the peer are not user work: update
	// drain state, consume the (empty) body and dispatch nothing.
	if req.Method == goAwayMethod || req.Method == drainReadyMethod {
		ready := req.Method == drainReadyMethod
		c.mutex.Lock()
		if ready {
			c.peerReady = true
		} else {
			c.peerDrain = true
		}
		c.idle.Broadcast()
		c.mutex.Unlock()
		// Control messages carry an int body so the codec has a concrete
		// value to consume (gob encodes an empty struct as zero bytes).
		var body int
		if err := c.codec.ReadRequestBody(&body); err != nil {
			return err
		}
		return nil
	}

	method, ok := c.handlers[req.Method]
	if !ok {
		resp := &Response{
			Seq:   req.Seq,
			Error: "rpc2: can't find method " + req.Method,
		}
		return c.codec.WriteResponse(resp, resp)
	}

	// Decode the argument value.
	var argv reflect.Value
	argIsValue := false // if true, need to indirect before calling.
	if method.argType.Kind() == reflect.Ptr {
		argv = reflect.New(method.argType.Elem())
	} else {
		argv = reflect.New(method.argType)
		argIsValue = true
	}
	// argv guaranteed to be a pointer now.
	if err := c.codec.ReadRequestBody(argv.Interface()); err != nil {
		return err
	}
	if argIsValue {
		argv = argv.Elem()
	}

	// Snapshot before dispatching: readLoop resets req on its next
	// iteration and a handler goroutine must not dereference the shared
	// header, which can be zeroed before the goroutine runs.
	reqCopy := *req
	// The request is admitted now. It counts as active work until the
	// handler returns; nested outbound calls from within the handler are
	// allowed while this is non-zero (see newCallErrLocked).
	dispatch := func() {
		defer func() {
			c.mutex.Lock()
			c.activeIn--
			c.idle.Broadcast()
			c.mutex.Unlock()
		}()
		c.handleRequest(reqCopy, method, argv)
	}
	c.mutex.Lock()
	c.activeIn++
	c.mutex.Unlock()

	if c.blocking {
		dispatch()
	} else {
		go dispatch()
	}

	return nil
}

func (c *Client) readResponse(resp *Response) error {
	seq := resp.Seq
	c.mutex.Lock()
	call := c.pending[seq]
	delete(c.pending, seq)
	c.idle.Broadcast()
	c.mutex.Unlock()

	var err error
	switch {
	case call == nil:
		// We've got no pending call. That usually means that
		// WriteRequest partially failed, and call was already
		// removed; response is a server telling us about an
		// error reading request body. We should still attempt
		// to read error body, but there's no one to give it to.
		err = c.codec.ReadResponseBody(nil)
		if err != nil {
			err = errors.New("reading error body: " + err.Error())
		}
	case resp.Error != "":
		// We've got an error response. Give this to the request;
		// any subsequent requests will get the ReadResponseBody
		// error if there is one.
		call.Error = ServerError(resp.Error)
		err = c.codec.ReadResponseBody(nil)
		if err != nil {
			err = errors.New("reading error body: " + err.Error())
		}
		call.done()
	default:
		err = c.codec.ReadResponseBody(call.Reply)
		if err != nil {
			call.Error = errors.New("reading body " + err.Error())
		}
		call.done()
	}

	return err
}

// Close waits for active calls to finish and closes the codec.
func (c *Client) Close() error {
	c.mutex.Lock()
	if c.shutdown || c.closing {
		c.mutex.Unlock()
		return ErrShutdown
	}
	c.closing = true
	c.idle.Broadcast()
	c.mutex.Unlock()
	return c.codec.Close()
}

// Drain initiates a graceful shutdown of the connection.
//
// After Drain starts, new locally initiated calls (Go/Call/Notify) are
// rejected with ErrDraining. The peer is notified with a go-away message
// that asks a capable peer to stop starting new calls. Already admitted
// inbound handlers and already outstanding outbound requests are allowed to
// complete.
//
// Once local work has drained, Drain sends a "ready" notification to a peer
// that is also draining. The connection is closed only after both sides
// are ready and idle (or ctx expires); this handshake is required in the
// bidirectional case. After announcing ready a side keeps serving inbound
// requests, so an admitted handler that needs one last callback into a
// peer that went idle first still completes instead of having the peer
// close out from under it.
//
// Nested calls: while at least one admitted handler is still running, the
// outbound calls it makes are part of work already accepted and are allowed
// through even while draining (see newCallErrLocked). Inbound requests
// that handlers need to call back keep being dispatched for the same
// reason, so neither peer can end up waiting for the other to close.
//
// A ctx cancellation only ends the wait and triggers the agreed close; it
// never produces synthetic success replies. Calls that cannot complete
// before the connection is closed fail with a real error from the read
// loop.
//
// Drain is safe to call concurrently and repeatedly. The first call drives
// the drain; later calls join it and wait for the terminal state. A joining
// call whose context expires early returns ctx.Err() without aborting the
// in-progress drain. Drain returns nil on a clean close, ctx.Err() for the
// driving call that timed out, or the read error that tore the connection
// down first.
func (c *Client) Drain(ctx context.Context) error {
	c.mutex.Lock()
	switch {
	case c.shutdown:
		clean := c.drainClean
		res := c.drainRes
		c.mutex.Unlock()
		// A drain that completed cleanly is reported as success even to
		// later callers (idempotent graceful close); a non-drain shutdown
		// is ErrShutdown, and a failed drain returns its real error.
		if clean {
			return nil
		}
		if res != nil {
			return res
		}
		return ErrShutdown
	case c.draining:
		// Join the drain already in progress and wait for the terminal
		// state. The join cannot cancel the driving call's deadline.
		for !c.shutdown && c.drainRes == nil {
			if err := c.waitIdle(ctx); err != nil {
				c.mutex.Unlock()
				return err
			}
		}
		res := c.drainRes
		clean := c.drainClean
		c.mutex.Unlock()
		// A close driven by Drain is a clean shutdown for joining calls,
		// regardless of how the read loop recorded the underlying error.
		if clean {
			return nil
		}
		return res
	}
	c.draining = true
	c.idle.Broadcast()
	c.mutex.Unlock()

	// Best effort: tell a capable peer not to start new calls.
	if err := c.writeDrainMsg(goAwayMethod); err != nil {
		debugln("rpc2: error sending drain notification:", err.Error())
	}

	timedOut := false

	// Phase 1: wait for locally admitted work to finish.
	c.mutex.Lock()
	for !c.quiescentLocked() && !c.shutdown {
		if err := c.waitIdle(ctx); err != nil {
			timedOut = true
			break
		}
	}
	localIdle := c.quiescentLocked() && !c.shutdown
	c.mutex.Unlock()

	// Phase 2: coordinate the close with a peer that is also draining.
	//
	// Being locally idle only means we will not initiate more calls; it is
	// not safe to close yet because the peer may still run an admitted
	// handler that needs one more callback into us. So we announce
	// readiness (no more calls will originate here) but keep the read loop
	// serving inbound requests, including nested callbacks, until both
	// sides are idle and ready. If the peer never sent a go-away it is not
	// draining and will not close first, so local idleness is enough then.
	if localIdle {
		if err := c.writeDrainMsg(drainReadyMethod); err != nil {
			debugln("rpc2: error sending drain-ready notification:", err.Error())
		}

		c.mutex.Lock()
		// A nested callback admitted after our ready announcement makes us
		// non-idle again; wait until it also drains and both peers are
		// ready before closing.
		for c.peerDrain && !(c.peerReady && c.quiescentLocked()) && !c.shutdown {
			if err := c.waitIdle(ctx); err != nil {
				timedOut = true
				break
			}
		}
		c.mutex.Unlock()
	}

	c.mutex.Lock()
	c.closing = true
	c.mutex.Unlock()

	done := make(chan struct{})
	go func() {
		<-c.disconnect
		close(done)
	}()
	c.finishClose(done)

	c.mutex.Lock()
	for !c.shutdown && c.drainRes == nil {
		c.idle.Wait()
	}
	res := c.drainRes
	clean := c.drainClean
	c.mutex.Unlock()

	if timedOut && (res == nil || clean) {
		return ctx.Err()
	}
	if res != nil && !clean {
		return res
	}
	return nil
}

// finishClose tears the connection down after Drain decided to close. If the
// codec supports a half close, the write side is shut first so the FIN lets
// the peer drain in-order final responses instead of receiving an abortive
// RST that (notably on Windows) can discard buffered replies. It then waits
// briefly for the peer to close its side, and otherwise closes fully.
func (c *Client) finishClose(done chan struct{}) {
	halfClosed := false
	if hc, ok := c.codec.(halfWriteCloser); ok {
		if err := hc.CloseWrite(); err == nil {
			halfClosed = true
		} else {
			debugln("rpc2: error half-closing codec during drain:", err.Error())
		}
	}

	if halfClosed {
		t := time.NewTimer(500 * time.Millisecond)
		select {
		case <-done:
			t.Stop()
			return
		case <-t.C:
		}
	}

	if err := c.codec.Close(); err != nil {
		debugln("rpc2: error closing codec during drain:", err.Error())
	}
	<-done
}

// quiescentLocked reports whether all admitted work has finished: no
// inbound handler is still running and no outbound call is pending.
func (c *Client) quiescentLocked() bool {
	return c.activeIn == 0 && len(c.pending) == 0
}

// waitIdle waits for a change in drain-relevant state or for ctx to expire.
// It returns ctx.Err() without leaving any goroutine or waiter behind.
func (c *Client) waitIdle(ctx context.Context) error {
	done := make(chan struct{})
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			c.mutex.Lock()
			c.idle.Broadcast()
			c.mutex.Unlock()
		case <-stop:
		}
		close(done)
	}()
	c.idle.Wait()
	close(stop)
	<-done
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

// writeDrainMsg sends an internal drain notification. It must be called
// without holding either c.mutex or c.sending. These messages are protocol
// control, not user calls, so they bypass the draining admission gate.
func (c *Client) writeDrainMsg(method string) error {
	c.mutex.Lock()
	if c.shutdown {
		c.mutex.Unlock()
		return ErrShutdown
	}
	c.mutex.Unlock()
	c.sending.Lock()
	defer c.sending.Unlock()
	c.request.Seq = 0
	c.request.Method = method
	return c.codec.WriteRequest(&c.request, 0)
}

// Go invokes the function asynchronously.  It returns the Call structure representing
// the invocation.  The done channel will signal when the call is complete by returning
// the same Call object.  If done is nil, Go will allocate a new channel.
// If non-nil, done must be buffered or Go will deliberately crash.
func (c *Client) Go(method string, args interface{}, reply interface{}, done chan *Call) *Call {
	call := new(Call)
	call.Method = method
	call.Args = args
	call.Reply = reply
	if done == nil {
		done = make(chan *Call, 10) // buffered.
	} else {
		// If caller passes done != nil, it must arrange that
		// done has enough buffer for the number of simultaneous
		// RPCs that will be using that channel.  If the channel
		// is totally unbuffered, it's best not to run at all.
		if cap(done) == 0 {
			log.Panic("rpc2: done channel is unbuffered")
		}
	}
	call.Done = done
	c.send(call)
	return call
}

// CallWithContext invokes the named function, waits for it to complete, and
// returns its error status, or an error from Context timeout.
func (c *Client) CallWithContext(ctx context.Context, method string, args interface{}, reply interface{}) error {
	call := c.Go(method, args, reply, make(chan *Call, 1))
	select {
	case <-call.Done:
		return call.Error
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Call invokes the named function, waits for it to complete, and returns its error status.
func (c *Client) Call(method string, args interface{}, reply interface{}) error {
	return c.CallWithContext(context.Background(), method, args, reply)
}

func (call *Call) done() {
	select {
	case call.Done <- call:
		// ok
	default:
		// We don't want to block here.  It is the caller's responsibility to make
		// sure the channel has enough buffer space. See comment in Go().
		debugln("rpc2: discarding Call reply due to insufficient Done chan capacity")
	}
}

// ServerError represents an error that has been returned from
// the remote side of the RPC connection.
type ServerError string

func (e ServerError) Error() string {
	return string(e)
}

// ErrShutdown is returned when the connection is closing or closed.
var ErrShutdown = errors.New("connection is shut down")

// Call represents an active RPC.
type Call struct {
	Method string      // The name of the service and method to call.
	Args   interface{} // The argument to the function (*struct).
	Reply  interface{} // The reply from the function (*struct).
	Error  error       // After completion, the error status.
	Done   chan *Call  // Strobes when call is complete.
}

func (c *Client) send(call *Call) {
	c.sending.Lock()
	defer c.sending.Unlock()

	// Register this call.
	c.mutex.Lock()
	if err := c.newCallErrLocked(); err != nil {
		call.Error = err
		c.mutex.Unlock()
		call.done()
		return
	}
	seq := c.seq
	c.seq++
	c.pending[seq] = call
	c.mutex.Unlock()

	// Encode and send the request.
	c.request.Seq = seq
	c.request.Method = call.Method
	err := c.codec.WriteRequest(&c.request, call.Args)
	if err != nil {
		c.mutex.Lock()
		call = c.pending[seq]
		delete(c.pending, seq)
		c.idle.Broadcast()
		c.mutex.Unlock()
		if call != nil {
			call.Error = err
			call.done()
		}
	}
}

// Notify sends a request to the receiver but does not wait for a return value.
func (c *Client) Notify(method string, args interface{}) error {
	c.sending.Lock()
	defer c.sending.Unlock()

	c.mutex.Lock()
	err := c.newCallErrLocked()
	c.mutex.Unlock()
	if err != nil {
		return err
	}

	c.request.Seq = 0
	c.request.Method = method
	return c.codec.WriteRequest(&c.request, args)
}

// newCallErrLocked decides whether a fresh locally initiated call is
// admitted. It must be called with c.mutex held and c.sending held.
//
// A call is rejected once the connection is shut down or closing, or once
// draining has begun - except that a call made to finish work already
// admitted must still go: while an inbound handler is running (activeIn > 0)
// its nested callbacks are allowed. Calls are also allowed while the peer
// has not started draining, so the very last callbacks an admitted handler
// needs can still be delivered when both sides drain simultaneously.
func (c *Client) newCallErrLocked() error {
	switch {
	case c.shutdown:
		return ErrShutdown
	case c.closing:
		return ErrShutdown
	case (c.draining || c.peerDrain) && c.activeIn == 0:
		// Local drain has begun, or the peer asked us to stop: this is a
		// fresh top-level call rather than a nested callback of an
		// admitted handler, so refuse it.
		return ErrDraining
	default:
		// A call made while an admitted handler is running is work that
		// was already accepted; let it through even while draining on
		// either side, including a callback into a peer that also drained,
		// so the two peers cannot end up waiting on each other's close.
		return nil
	}
}
