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

Cancellation
------------

`CallWithContext` propagates cancellation to the serving endpoint.
When both endpoints support the cancellation extension (negotiated
automatically when the connection starts), cancelling the caller's
context sends a cancel signal carrying the request's sequence number,
and the serving handler can observe it through its own context.
Handlers opt in to receive the context by taking a leading
`context.Context` argument:

```go
srv.Handle("work", func(ctx context.Context, client *rpc2.Client, args *Args, reply *Reply) error {
	select {
	case <-time.After(time.Minute):
		*reply = doWork(args)
		return nil
	case <-ctx.Done():
		return ctx.Err() // caller cancelled
	}
})
```

A request cancelled while still queued is never executed. A cancel
arriving after the reply was sent is ignored. Once `CallWithContext`
returns, a late reply is read and discarded without modifying the
caller's reply value. Endpoints that do not implement the extension
(older rpc2 versions) keep working: no cancel signal is sent to them
and control frames are safely ignored.
