package rpc2

import (
	"errors"
	"log"
	"reflect"
	"runtime/debug"
	"sync/atomic"
)

// Context carries information about a single incoming request as it
// travels through the middleware chain.
type Context struct {
	Client       *Client     // connection the request arrived on; use Client.State for per-connection state
	Method       string      // name of the method being called
	Seq          uint64      // request sequence number; 0 for notifications
	Notification bool        // true if the request is a notification; no response is sent back
	Args         interface{} // decoded argument value passed to the handler
	Reply        interface{} // pointer to the reply value filled in by the handler

	argv reflect.Value // argument value used to invoke the handler
}

// HandlerFunc handles a single RPC request. The registered method
// handlers are adapted to this signature at the end of a middleware chain.
type HandlerFunc func(*Context) error

// Middleware wraps a HandlerFunc with additional behavior. A middleware
// may inspect the Context, reject the request by returning an error
// without calling next, or call next exactly once and observe the result
// (ctx.Reply and the returned error) afterwards.
//
// Calling next more than once is a programming error and panics with
// errNextCalledTwice.
type Middleware func(next HandlerFunc) HandlerFunc

// ErrInternal is the generic error returned to the peer when a panic is
// recovered by the Recovery middleware. It deliberately carries no
// internal details so that stacks and panic values are never leaked to
// the remote side.
var ErrInternal = errors.New("rpc2: internal error")

var errNextCalledTwice = errors.New("rpc2: middleware called next multiple times")

// once wraps next so that it can be called at most once per request.
func once(next HandlerFunc) HandlerFunc {
	var called int32
	return func(ctx *Context) error {
		if !atomic.CompareAndSwapInt32(&called, 0, 1) {
			panic(errNextCalledTwice)
		}
		return next(ctx)
	}
}

// runChain executes final through the middlewares in registration order.
// The chain is assembled per request so that the single-call guards on
// next are scoped to a single request.
func runChain(mws []Middleware, final HandlerFunc, ctx *Context) error {
	h := final
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](once(h))
	}
	return h(ctx)
}

// Recovery returns a Middleware that converts panics raised by downstream
// middlewares or handlers into ErrInternal. The panic value and stack
// trace are only logged locally (when DebugLog is enabled) and are never
// sent to the peer.
func Recovery() Middleware {
	return func(next HandlerFunc) HandlerFunc {
		return func(ctx *Context) (err error) {
			defer func() {
				if r := recover(); r != nil {
					if DebugLog {
						log.Printf("rpc2: panic handling %s: %v\n%s", ctx.Method, r, debug.Stack())
					}
					err = ErrInternal
				}
			}()
			return next(ctx)
		}
	}
}
