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

Graceful shutdown (Drain)
-------------------------

`Drain(ctx)` performs a rolling restart friendly shutdown: it first drains
RPC connections, then closes them.

After Drain starts the client rejects new locally initiated calls with
`ErrDraining`, and asks a capable peer (via a go-away notification) to stop
starting new requests. Already admitted inbound handlers and already
outstanding outbound requests keep running to completion.

Handlers are still allowed to call back into the peer for work that was
already admitted (the bidirectional case); these nested callbacks are what
handlers need to finish, so both peers can drain simultaneously without
waiting on each other's close.

Drain blocks until the connection is idle and the mutual drain handshake
finishes, or `ctx` expires. A timeout only ends the wait and closes the
connection; it never fabricates successful replies - calls that cannot
finish fail with a real error.

```go
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
if err := clt.Drain(ctx); err != nil {
    log.Print("rpc2 drain: ", err)
}
```
