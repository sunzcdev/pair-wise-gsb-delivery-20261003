package rpc2

import (
	"errors"
	"log"
	"reflect"
	"runtime/debug"
)

// CallInfo carries everything a Middleware can observe about an incoming call.
// It is created per request and shared by every Middleware in the chain.
type CallInfo struct {
	// Client is the connection that received the request.
	Client *Client
	// Method is the name of the RPC method to be invoked.
	Method string
	// Seq is the request number chosen by the peer. It is 0 for notifications.
	Seq uint64
	// Notification reports whether the request is a notification (Seq == 0).
	// Notifications never receive a response, even when the call is rejected.
	Notification bool
	// State is the per-connection state that persists across RPC calls.
	State *State
	// Args is the decoded argument value that will be passed to the handler.
	Args interface{}
	// Reply is the reply value (a pointer) that will be sent back to the peer.
	Reply interface{}
}

// HandlerFunc is the terminal action of a Middleware chain: it invokes the
// registered RPC method and returns its error.
type HandlerFunc func(*CallInfo) error

// Middleware wraps a HandlerFunc.
//
// A middleware may inspect the CallInfo, reject the call before it runs by
// returning an error without calling next, or observe/modify the result after
// the downstream chain has finished.
//
// next must be called at most once per request. Calling it more than once
// panics with ErrNextCalledTwice. The chain is immutable after it starts
// serving requests, so the same Middleware value is reused concurrently and
// must be safe for concurrent use.
type Middleware func(*CallInfo, HandlerFunc) error

// ErrNextCalledTwice is returned (or recovered by Recover) when a Middleware
// calls next more than once for the same request.
var ErrNextCalledTwice = errors.New("rpc2: middleware called next twice")

// asMiddleware validates and converts a middleware value at registration time.
// mw must have the exact signature "func(*CallInfo, HandlerFunc) error".
// Invalid values panic before the server accepts any connection so that
// reflection type mismatches surface during registration, not at request time.
func asMiddleware(mname string, mw interface{}) Middleware {
	fv := reflect.ValueOf(mw)
	if fv.Kind() != reflect.Func {
		log.Panicf("rpc2: middleware for %q is not a function: %T", mname, mw)
	}
	ft := fv.Type()
	if ft.NumIn() != 2 {
		log.Panicf("rpc2: middleware for %q has wrong number of ins: %d", mname, ft.NumIn())
	}
	if ft.In(0) != reflect.TypeOf((*CallInfo)(nil)) {
		log.Panicf("rpc2: middleware for %q has first argument %s, want *rpc2.CallInfo", mname, ft.In(0))
	}
	if ft.In(1) != reflect.TypeOf((*HandlerFunc)(nil)).Elem() {
		log.Panicf("rpc2: middleware for %q has second argument %s, want rpc2.HandlerFunc", mname, ft.In(1))
	}
	if ft.NumOut() != 1 || ft.Out(0) != typeOfError {
		log.Panicf("rpc2: middleware for %q must return error, got %d outs", mname, ft.NumOut())
	}
	if m, ok := mw.(Middleware); ok {
		return m
	}
	return func(info *CallInfo, next HandlerFunc) error {
		out := fv.Call([]reflect.Value{reflect.ValueOf(info), reflect.ValueOf(next)})
		if out[0].IsNil() {
			return nil
		}
		return out[0].Interface().(error)
	}
}

// buildChain assembles the immutable execution chain.
// The returned HandlerFunc runs middlewares in registration order and finishes
// with terminal, which invokes the registered RPC method.
func buildChain(mws []Middleware, terminal HandlerFunc) HandlerFunc {
	if len(mws) == 0 {
		return terminal
	}
	// Wrap from the last middleware back to the first so that registration
	// order matches the order in which requests enter the chain.
	h := terminal
	for i := len(mws) - 1; i >= 0; i-- {
		mw := mws[i]
		next := h
		h = func(info *CallInfo) error {
			called := false
			onceNext := func(inner *CallInfo) error {
				if called {
					panic(ErrNextCalledTwice)
				}
				called = true
				return next(inner)
			}
			return mw(info, onceNext)
		}
	}
	return h
}

// Recover is an optional Middleware that converts a panic in the downstream
// chain (including a double-next panic) into an error returned to the peer.
//
// The panic value and its stack trace are written to the standard logger
// server-side only; they are never included in the error sent over the wire,
// so internal implementation details do not leak to the remote side.
func Recover(info *CallInfo, next HandlerFunc) (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("rpc2: panic in method %q: %v\n%s", info.Method, r, debug.Stack())
			if r == ErrNextCalledTwice {
				err = ErrNextCalledTwice
				return
			}
			err = errors.New("rpc2: internal server error")
		}
	}()
	return next(info)
}
