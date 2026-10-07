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

Both `Server` and `Client` accept middleware via `Use`.
A middleware receives a `*CallInfo` (method name, request number, notification
flag, connection state, args and reply) and a `next` function:

- inspect the call before it runs, or reject it by returning an error without
  calling `next`;
- observe or modify the result after `next` returns;
- call `next` at most once per request (a second call panics).

The chain is registered before serving starts and becomes immutable afterwards;
`Use` panics once the server is serving or the client is running.
The optional `Recover` middleware converts a handler panic into an error without
sending the internal stack trace to the peer. Notifications never receive a
response, even when rejected or recovered.

```go
srv := rpc2.NewServer()
srv.Use(rpc2.Recover)
srv.Use(func(info *rpc2.CallInfo, next rpc2.HandlerFunc) error {
	start := time.Now()
	err := next(info) // reject by returning an error without calling next
	log.Printf("%s seq=%d notification=%v took=%s err=%v",
		info.Method, info.Seq, info.Notification, time.Since(start), err)
	return err
})
```
