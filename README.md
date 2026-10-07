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
---------------------

Metadata is a small bounded set of string attributes (e.g. trace IDs or
routing tenants) carried in request headers. Only codecs that implement the
optional `rpc2.MetadataCodec` interface provide the metadata area; the
built-in gob codec does not, so plain gob calls are unchanged. The JSON codec
supports it through a `"meta"` object.

Attach attributes to an outgoing call with `CallContext`/`NotifyContext`:

```go
ctx := rpc2.WithMetadata(context.Background(), rpc2.Metadata{"tenant": "acme"})
client.CallContext(ctx, "add", Args{1, 2}, &rep)
```

A handler may take an optional second `context.Context` argument and read the
peer-submitted values separately from the locally confirmed values:

```go
srv.Handle("add", func(client *rpc2.Client, ctx context.Context, args *Args, reply *Reply) error {
    peer := rpc2.PeerMetadata(ctx)      // what the remote submitted (private copy)
    local := rpc2.ConfirmedMetadata(ctx) // what this side acknowledges (private copy)
    return nil
})
```

Both accessors return private copies; mutating them never changes another
handler's view. A server interceptor runs before the handler and can validate,
delete or supplement the local values; returning an error rejects the request
before its body is read or the handler runs:

```go
srv.SetMetadataInterceptor(func(ctx context.Context, peer, confirmed rpc2.Metadata) error {
    if peer["tenant"] == "" {
        return errors.New("missing tenant")
    }
    delete(confirmed, "internal")
    confirmed["route"] = "route-" + peer["tenant"]
    return nil
})
```

Nested reverse RPCs do not copy inbound attributes automatically. Only keys in
an explicit whitelist are propagated, preferring the confirmed value:

```go
srv.SetMetadataWhitelist("trace")                 // connection default
client.CallContext(rpc2.WithMetadataWhitelist(ctx, "trace"), "ping", nil, &rep) // per call
```

The area is bounded by `MaxMetadataKeys`, `MaxMetadataValueLen` and
`MaxMetadataLen`; oversized or malformed metadata is rejected up front.
