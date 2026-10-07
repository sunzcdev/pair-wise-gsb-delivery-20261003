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

Per-request metadata
----------------------

`NewMetadataCodec` frames each message with a bounded per-request metadata
area (at most `MaxMetadataKeys` entries and `MaxMetadataLen` bytes). Plain
codecs such as `NewGobCodec` are unaffected. Handlers may take an optional
fourth `context.Context` argument and read the values submitted by the peer
and the values confirmed locally:

```go
srv := rpc2.NewServer()
srv.SetMetadataFunc(func(ctx context.Context, c *rpc2.Client, method string, peer rpc2.Metadata) (rpc2.Metadata, error) {
    if _, ok := peer.Get("tenant"); !ok {
        return nil, errors.New("missing tenant")
    }
    local := peer.Clone()
    delete(local, "x-internal")
    local["local-only"] = []string{"confirmed"}
    return local, nil
})
srv.Handle("work", func(c *rpc2.Client, args *Args, reply *Reply, ctx context.Context) error {
    peer, _ := rpc2.FromIncomingContext(ctx)
    local, _ := rpc2.FromLocalContext(ctx)
    // Maps returned here are copies; mutating them cannot affect other calls.
    return nil
})
```

Clients attach metadata with `NewOutgoingContext` and `CallWithContext`.
Nested reverse calls do not copy metadata automatically; only keys in the
server/client whitelist (`SetMetadataWhitelist`) or selected explicitly with
`PropagateMetadata` cross the nesting. Invalid metadata is rejected before the
handler runs.
