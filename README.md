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

Limiting inbound requests
---------------

By default every inbound request runs in its own goroutine, so a slow
method can make the peer spawn an unbounded number of goroutines.
An `InboundPool` bounds how many handlers run concurrently and how many
requests may wait for a slot. When the pool is full, calls are rejected
with an `ErrServerBusy` error (testable with `errors.Is`) and
notifications are dropped (counted by `Client.DroppedNotifications`).
Reading replies never goes through the pool, so bidirectional calls
cannot deadlock on a full pool.

```go
pool := rpc2.NewInboundPool(rpc2.InboundConfig{MaxConcurrent: 4, QueueSize: 16})

// Default pool for all methods without their own pool.
srv.SetInboundPool(pool)

// Methods may share a pool or have dedicated limits.
srv.HandleWithPool("add", addFn, pool)
srv.HandleWithPool("export", exportFn, rpc2.NewInboundPool(rpc2.InboundConfig{MaxConcurrent: 1}))
```
