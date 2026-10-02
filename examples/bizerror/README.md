# Business errors over go-plugin

This example shows the optional structured **business error** protocol in
`github.com/hashicorp/go-plugin/bizerror`. It lets a host tell
"resource not found" apart from "temporarily unavailable" without matching
error text, using `errors.Is` / `errors.As` on both transports:

- standard library `net/rpc`
- gRPC

It runs entirely in one process over in-memory listeners, so no separate
plugin binary is needed.

Run it with:

```sh
go run ./examples/bizerror
```

## Scenarios demonstrated

For each transport the same set of errors crosses the boundary:

1. A registered typed error, reconstructed with `errors.As`.
2. Registered type with exported fields; a sensitive field is stripped by the
   registry allow-list.
3. A type carried through explicit `Encode`/`Decode` hooks instead of
   reflection.
4. Multiple causes forming an error tree.
5. A cyclic error graph (`a -> b -> a`); the cycle is broken into a terminal
   `bizerror.ErrCyclicReference`, so traversal always terminates.
6. A type the receiver registered but the sender did not; it is still emitted
   by stable type id and reconstructed concretely on the host.
7. A type neither side registered; it arrives as a generic `*bizerror.Error`
   carrying `bizerror.ErrUnknownType`.
8. An old plugin that returns a plain error; it stays a plain message error.
9. Missing methods / unknown services; these remain ordinary transport
   errors with their original classification.

## Layout

- `shared/shared.go` - the agreed type ids, concrete error structs and the
  host/plugin registries. The host registers one extra type the plugin does
  not know.
- `main.go` - scenario definitions and the host-side classification printed
  for `net/rpc` and gRPC.
- `grpc_service.go`, `grpc_json.go` - a hand-written gRPC service used only
  so the example needs no generated protobuf for its trivial request/response.
  Business error details always use the well-known `google.protobuf.Struct`.

## Key idea

Both sides agree on stable string identifiers (for example
`"store.v1.not_found"`), never on Go type names. The sending registry chooses
which fields are allowed on the wire; everything else stays in the process.
The receiving registry only reconstructs types it has registered; unknown
identifiers come back as `*bizerror.Error` with `bizerror.ErrUnknownType`,
they are never coerced into an unrelated local type.
