rpc2
====

[![GoDoc](https://godoc.org/github.com/cenkalti/rpc2?status.png)](https://godoc.org/github.com/cenkalti/rpc2)

rpc2 is a fork of net/rpc package in the standard library.
The main goal is to add bi-directional support to calls.
That means server can call the methods of client.
This is not possible with net/rpc package.
In order to do this it adds a `*Client` argument to method signatures.

Install
--------

    go get github.com/cenkalti/rpc2

Example server
---------------

```go
package main

import (
	"fmt"
	"net"

	"github.com/cenkalti/rpc2"
)

type Args struct{ A, B int }
type Reply int

func main() {
	srv := rpc2.NewServer()
	srv.Handle("add", func(client *rpc2.Client, args *Args, reply *Reply) error {

		// Reversed call (server to client)
		var rep Reply
		client.Call("mult", Args{2, 3}, &rep)
		fmt.Println("mult result:", rep)

		*reply = Reply(args.A + args.B)
		return nil
	})

	lis, _ := net.Listen("tcp", "127.0.0.1:5000")
	srv.Accept(lis)
}
```

Example Client
---------------

```go
package main

import (
	"fmt"
	"net"

	"github.com/cenkalti/rpc2"
)

type Args struct{ A, B int }
type Reply int

func main() {
	conn, _ := net.Dial("tcp", "127.0.0.1:5000")

	clt := rpc2.NewClient(conn)
	clt.Handle("mult", func(client *rpc2.Client, args *Args, reply *Reply) error {
		*reply = Reply(args.A * args.B)
		return nil
	})
	go clt.Run()

	var rep Reply
	clt.Call("add", Args{1, 2}, &rep)
	fmt.Println("add result:", rep)
}
```

Middleware
----------

Cross-cutting concerns (authentication, timing, argument validation, ...)
can be factored out of handlers into a middleware chain. Middlewares run
in registration order around every incoming request, including
notifications, and see the method name, request sequence number,
notification flag and the connection (`ctx.Client.State`). A middleware
may reject the request before it reaches the handler by returning an
error without calling `next`, or observe the reply and error after `next`
returns. Calling `next` more than once panics.

```go
srv := rpc2.NewServer()
srv.Use(rpc2.Recovery()) // converts handler panics into a generic error
srv.Use(func(next rpc2.HandlerFunc) rpc2.HandlerFunc {
	return func(ctx *rpc2.Context) error {
		start := time.Now()
		err := next(ctx) // call the rest of the chain exactly once
		log.Printf("%s took %s (err=%v)", ctx.Method, time.Since(start), err)
		return err
	}
})
```

Register middleware with `Use` before serving; the chain is immutable
once the server starts. `rpc2.Recovery()` is optional: without it a
panicking handler crashes the serving goroutine. Recovered panics are
reported to the peer as the generic `rpc2.ErrInternal`; panic details and
stack traces are never sent over the wire.
