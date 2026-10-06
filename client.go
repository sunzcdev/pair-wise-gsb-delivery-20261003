// Package rpc2 provides bi-directional RPC client and server similar to net/rpc.
package rpc2

import (
	"context"
	"errors"
	"io"
	"log"
	"reflect"
	"sync"
)

// Client represents an RPC Client.
// There may be multiple outstanding Calls associated
// with a single Client, and a Client may be used by
// multiple goroutines simultaneously.
type Client struct {
	mutex      sync.Mutex // protects pending, seq, request
	sending    sync.Mutex
	request    Request // temp area used in send()
	seq        uint64
	pending    map[uint64]*Call
	incoming   map[uint64]*incomingCall
	peerCancel bool // whether the remote endpoint supports cancel frames
	helloOnce  sync.Once
	closing    bool
	shutdown   bool
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
	return &Client{
		codec:      codec,
		pending:    make(map[uint64]*Call),
		incoming:   make(map[uint64]*incomingCall),
		handlers:   make(map[string]*handler),
		disconnect: make(chan struct{}),
		seq:        1, // 0 means notification.
	}
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
			c.setPeerCancel(req.Capabilities.Cancel)
			c.maybeSendHello()
			// request comes to server
			if err = c.readRequest(&req); err != nil {
				debugln("rpc2: error reading request:", err.Error())
			}
		} else {
			c.setPeerCancel(resp.Capabilities.Cancel)
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
	if err == io.EOF {
		if closing {
			err = ErrShutdown
		} else {
			err = io.ErrUnexpectedEOF
		}
	}
	for _, call := range c.pending {
		if call.finish(err) {
			call.done()
		}
	}
	c.mutex.Unlock()
	c.sending.Unlock()
	if err != io.EOF && !closing && !c.server {
		debugln("rpc2: client protocol error:", err)
	}
	close(c.disconnect)
	if !closing {
		c.codec.Close()
	}
}

// incomingCall tracks a request received from the peer so that it can be
// canceled through its Context while it is queued or executing.
type incomingCall struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func (c *Client) setPeerCancel(cancel bool) {
	c.mutex.Lock()
	c.peerCancel = cancel
	c.mutex.Unlock()
}

// maybeSendHello advertises this endpoint's capabilities once, but only after
// the peer proved it understands extension frames (by setting a capability).
// Old peers never do, so they never receive a hello notification they could
// misinterpret as an unknown method.
func (c *Client) maybeSendHello() {
	c.mutex.Lock()
	supports := c.peerCancel
	c.mutex.Unlock()
	if !supports {
		return
	}
	// Send synchronously so the hello is on the wire before the request that
	// triggered it is dispatched to a handler.
	c.helloOnce.Do(c.sendHello)
}

// sendHello sends the one-shot capability notification.
func (c *Client) sendHello() {
	c.sending.Lock()
	defer c.sending.Unlock()

	c.mutex.Lock()
	if c.shutdown || c.closing {
		c.mutex.Unlock()
		return
	}
	c.mutex.Unlock()

	c.request.Seq = 0
	c.request.Method = HelloMethod
	if err := c.codec.WriteRequest(&c.request, &Capabilities{Cancel: true}); err != nil {
		debugln("rpc2: error writing hello:", err.Error())
	}
}

func (c *Client) registerIncoming(seq uint64) *incomingCall {
	if seq == 0 {
		// Notifications cannot be canceled.
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	ic := &incomingCall{ctx: ctx, cancel: cancel}
	c.mutex.Lock()
	c.incoming[seq] = ic
	c.mutex.Unlock()
	return ic
}

func (c *Client) unregisterIncoming(seq uint64) {
	if seq == 0 {
		return
	}
	c.mutex.Lock()
	delete(c.incoming, seq)
	c.mutex.Unlock()
}

// cancelIncoming cancels the Context of a received request.
// It is a no-op if the request already produced a response.
func (c *Client) cancelIncoming(seq uint64) {
	if seq == 0 {
		return
	}
	c.mutex.Lock()
	ic := c.incoming[seq]
	c.mutex.Unlock()
	if ic != nil {
		ic.cancel()
	}
}

func (c *Client) handleRequest(req Request, method *handler, argv reflect.Value, ic *incomingCall) {
	// Drop the request from the set of cancelable requests when done.
	defer c.unregisterIncoming(req.Seq)

	// Invoke the method, providing a new value for the reply.
	replyv := reflect.New(method.replyType.Elem())

	var returnValues []reflect.Value
	if method.useContext {
		var ctx context.Context = context.Background()
		if ic != nil {
			ctx = ic.ctx
		}
		returnValues = method.fn.Call([]reflect.Value{reflect.ValueOf(c), reflect.ValueOf(ctx), argv, replyv})
	} else {
		returnValues = method.fn.Call([]reflect.Value{reflect.ValueOf(c), argv, replyv})
	}

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

	// The caller canceled while the request was queued or executing and the
	// context-aware handler did not surface the cancellation itself.
	// Handlers without a Context argument cannot be interrupted.
	if method.useContext && ic != nil && errmsg == "" {
		select {
		case <-ic.ctx.Done():
			errmsg = context.Canceled.Error()
		default:
		}
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
	if req.Method == CancelMethod {
		return c.readCancel()
	}
	if req.Method == HelloMethod {
		// Capability notification; discard its body and do not reply.
		return c.codec.ReadRequestBody(nil)
	}

	method, ok := c.handlers[req.Method]
	if !ok {
		// Consume the request body before replying to keep the stream aligned.
		if err := c.codec.ReadRequestBody(nil); err != nil {
			return err
		}
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

	ic := c.registerIncoming(req.Seq)
	if c.blocking {
		c.handleRequest(*req, method, argv, ic)
	} else {
		go c.handleRequest(*req, method, argv, ic)
	}

	return nil
}

// readCancel reads a cancellation frame. The frame is never dispatched as a
// regular method call: it has a reserved method name and no return value.
func (c *Client) readCancel() error {
	var body CancelRequest
	if err := c.codec.ReadRequestBody(&body); err != nil {
		return err
	}
	c.cancelIncoming(body.Seq)
	return nil
}

func (c *Client) readResponse(resp *Response) error {
	seq := resp.Seq
	c.mutex.Lock()
	call := c.pending[seq]
	delete(c.pending, seq)
	c.mutex.Unlock()

	var err error
	switch {
	case call == nil:
		// We've got no pending call. That usually means that
		// WriteRequest partially failed, the call was already removed,
		// or it was canceled locally. The body must still be consumed to
		// keep the stream aligned, but there is no one to give it to.
		err = c.codec.ReadResponseBody(nil)
		if err != nil {
			err = errors.New("reading error body: " + err.Error())
		}
	case resp.Error != "":
		// Error responses have no body, discard it.
		err = c.codec.ReadResponseBody(nil)
		if err != nil {
			err = errors.New("reading error body: " + err.Error())
		}
		if call.finish(ServerError(resp.Error)) {
			call.done()
		}
	default:
		if call.finish(nil) {
			// Decode into the caller's reply only if this response wins.
			// The call lock is held across decoding, so a context
			// cancellation cannot return until the reply is fully populated.
			call.mu.Lock()
			err = c.codec.ReadResponseBody(call.Reply)
			if err != nil {
				call.Error = errors.New("reading body " + err.Error())
			}
			call.mu.Unlock()
			call.done()
		} else {
			// Caller already returned due to cancellation. Read and discard
			// the late reply so the stream stays aligned and the caller's
			// reply object is never modified.
			err = c.codec.ReadResponseBody(nil)
			if err != nil {
				err = errors.New("reading error body: " + err.Error())
			}
		}
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
	c.mutex.Unlock()
	return c.codec.Close()
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
// returns its error status. If ctx is canceled before the remote handler
// produces a reply, CallWithContext returns ctx.Err() immediately and, when
// the peer supports cancellation, sends a cancellation frame carrying the
// request number. A reply that arrives afterwards is still read to keep the
// stream aligned but is discarded without modifying the reply value.
func (c *Client) CallWithContext(ctx context.Context, method string, args interface{}, reply interface{}) error {
	call := c.Go(method, args, reply, make(chan *Call, 1))
	select {
	case <-call.Done:
		return call.error()
	case <-ctx.Done():
		// Race the cancellation against an incoming response.
		if !call.cancel(ctx.Err()) {
			// The response completed first; report its result.
			<-call.Done
			return call.error()
		}
		c.mutex.Lock()
		delete(c.pending, call.Seq)
		peerCancel := c.peerCancel
		c.mutex.Unlock()
		if peerCancel {
			c.sendCancel(call.Seq)
		}
		call.done()
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
	Seq    uint64      // Request number assigned by the client.

	mu        sync.Mutex // guards canceled, completed, Error, Reply decoding
	canceled  bool
	completed bool
}

// finish records the outcome of a call delivered from the wire.
// It returns false if the call was canceled locally first; in that case the
// reply body must be discarded and Done must not be signaled again.
func (call *Call) finish(err error) bool {
	call.mu.Lock()
	defer call.mu.Unlock()
	if call.canceled || call.completed {
		return false
	}
	call.completed = true
	call.Error = err
	return true
}

// cancel marks the call as canceled locally. It returns false if a response
// was already delivered, in which case the caller must observe that response.
func (call *Call) cancel(err error) bool {
	call.mu.Lock()
	defer call.mu.Unlock()
	if call.canceled || call.completed {
		return false
	}
	call.canceled = true
	call.completed = true
	call.Error = err
	return true
}

func (call *Call) error() error {
	call.mu.Lock()
	defer call.mu.Unlock()
	return call.Error
}

func (c *Client) send(call *Call) {
	c.sending.Lock()
	defer c.sending.Unlock()

	// Register this call.
	c.mutex.Lock()
	if c.shutdown || c.closing {
		call.Error = ErrShutdown
		c.mutex.Unlock()
		call.done()
		return
	}
	seq := c.seq
	c.seq++
	c.pending[seq] = call
	call.Seq = seq
	c.mutex.Unlock()

	// Encode and send the request.
	c.request.Seq = seq
	c.request.Method = call.Method
	err := c.codec.WriteRequest(&c.request, call.Args)
	if err != nil {
		c.mutex.Lock()
		call = c.pending[seq]
		delete(c.pending, seq)
		c.mutex.Unlock()
		if call != nil {
			if call.finish(err) {
				call.done()
			}
		}
	}
}

// sendCancel sends a cancellation frame for an in-flight request.
// It must only be called after the peer advertised cancellation support.
func (c *Client) sendCancel(seq uint64) {
	c.sending.Lock()
	defer c.sending.Unlock()

	c.mutex.Lock()
	if c.shutdown || c.closing {
		c.mutex.Unlock()
		return
	}
	c.mutex.Unlock()

	c.request.Seq = 0
	c.request.Method = CancelMethod
	if err := c.codec.WriteRequest(&c.request, &CancelRequest{Seq: seq}); err != nil {
		debugln("rpc2: error writing cancel:", err.Error())
	}
}

// Notify sends a request to the receiver but does not wait for a return value.
func (c *Client) Notify(method string, args interface{}) error {
	c.sending.Lock()
	defer c.sending.Unlock()

	if c.shutdown || c.closing {
		return ErrShutdown
	}

	c.request.Seq = 0
	c.request.Method = method
	return c.codec.WriteRequest(&c.request, args)
}
