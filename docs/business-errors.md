# Business Errors

Business errors describe the result returned by plugin code. They are separate
from transport failures such as a closed connection, an unavailable endpoint,
or a malformed gRPC response. Hosts should classify those failures using the
normal transport errors and never infer a business result from error text.

## Registering Types

Both sides register the same stable identifier and the finite set of fields
that may cross the boundary:

```go
type temporarilyUnavailableKind struct{}
func (temporarilyUnavailableKind) Error() string { return "temporarily unavailable" }

var TemporarilyUnavailable = plugin.RegisterBusinessError[temporarilyUnavailableKind](
	"example/v1/temporarily-unavailable",
	map[string]bool{
		"retry_after_seconds": true,
		"upstream_token":      false,
	},
)
```

The identifier must be stable across versions. Every finite field used by the
type must be declared. `true` fields are exported across the plugin boundary,
while `false` fields remain available to local code and are omitted on encode.
Field values are JSON-safe.

Create an error with one or more causes:

```go
return TemporarilyUnavailable.New("cache is warming", map[string]any{
	"retry_after_seconds": 5,
	"upstream_token":      token,
}, upstreamErr)
```

Causes support both `Unwrap() error` and `Unwrap() []error`. The wire graph is
limited in size and depth. Repeated references are retained, while cyclic
references are broken at the back edge so that `errors.Is` and `errors.As`
remain terminating.

## Receiving Errors

Use the registered type instead of matching strings:

```go
typed, ok := TemporarilyUnavailable.As(err)
if ok {
	retryAfter, _ := typed.Field("retry_after_seconds")
	// Retry.
}

if TemporarilyUnavailable.Is(err) {
	// Equivalent tree search.
}
```

Only types registered by the receiver are restored to their concrete generic
business error type. An identifier emitted by a newer plugin and not registered
locally is returned as `*plugin.RemoteBusinessError`, preserving its stable
identifier, exported fields, message, and causes. Plain errors from older
plugins continue to use the existing transport representation.

## Transports

For gRPC unary calls, go-plugin installs server and client interceptors when
it creates its gRPC servers and connections. Plugin code returns business
errors normally; the structured payload is carried in a trailer and the gRPC
status is left unchanged.

net/rpc only supports strings on its native error channel, so generated or
hand-written RPC wrappers should explicitly convert at the boundary:

```go
// server
return plugin.EncodeRPCError(err)

// client
return plugin.DecodeRPCError(client.Call(...))
```

`DecodeRPCError` only converts `net/rpc.ServerError` messages carrying the
protocol marker. Local and connection errors such as `rpc.ErrShutdown` are
returned unchanged.
