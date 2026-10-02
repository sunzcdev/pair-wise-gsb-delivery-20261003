# Business error wire protocol (v1)

This document specifies the on-the-wire form used by package `bizerror`. It
is transport independent and JSON based, so plugins written in other
languages can participate.

## Envelope

One business error (a tree) is one JSON object:

```json
{
  "version": 1,
  "root": 1,
  "nodes": [
    {
      "id": 1,
      "type_id": "store.v1.not_found",
      "message": "resource not found: doc/1",
      "fields": { "Resource": "doc/1" },
      "causes": [2]
    },
    {
      "id": 2,
      "message": "permission check failed"
    }
  ]
}
```

- `version` must be `1`.
- `root` is the 1-based id of the root node.
- `nodes` is a flat list; causes refer to other nodes by id, which keeps
  shared and cyclic graphs representable.

## Node

| Field      | Meaning                                                                 |
| ---------- | ----------------------------------------------------------------------- |
| `id`       | Unique 1-based id within the envelope.                                  |
| `type_id`  | Stable agreed identifier. Absent for a plain, untyped error.            |
| `message`  | The error text, always safe to display.                                 |
| `fields`   | Object of fields explicitly allowed for export by the sender.           |
| `causes`   | 1-based ids of directly caused errors (multiple causes supported).      |
| `ref`      | When > 0, this node is an alias for the node with that id.              |
| `sentinel` | One of the built-in terminal markers (see below).                       |

Aliases (`ref`) make a node that appears more than once (a diamond) or that
participates in a cycle share one definition. A well-behaved sender breaks
cycles with aliases; a receiver additionally breaks any back-edge it detects
during reconstruction.

### Sentinels

| `sentinel`          | Receiver marker                         |
| ------------------- | --------------------------------------- |
| `limit_exceeded`    | `bizerror.ErrLimitExceeded`             |
| `cyclic_reference`  | `bizerror.ErrCyclicReference`           |
| `bad_reference`     | `bizerror.ErrBadReference`              |

## Bounds

Receivers enforce per-envelope limits and replace anything beyond them with
the `limit_exceeded` sentinel rather than failing the whole call:

- nodes: 64
- fields per node: 32
- encoded bytes per field: 64 KiB
- causes per node: 8
- unfolded depth while walking: 128

## Transport mapping

### net/rpc

`net/rpc` only carries `err.Error()`. The JSON envelope rides inside the
message with an explicit prefix:

```
go-plugin-bizerror-v1\x00{ ...envelope json... }
```

Errors without the prefix are ordinary errors; the client rebuilds them with
the classic `errors.New(message)` semantics. This is why an old plugin keeps
working unchanged.

### gRPC

The envelope is nested under a reserved key inside a
`google.protobuf.Struct` gRPC status detail:

```
detail = Struct{ "__go_plugin_bizerror_v1__": <envelope json> }
```

Using the well-known `Struct` message avoids any generated protobuf and is
trivial to produce from non-Go plugins. A status without this detail is left
completely untouched, so transport failures (e.g. `codes.Unavailable` on a
dead connection) keep their normal code and meaning.

## Safety rules

- Only types registered on the receiver are reconstructed to concrete Go
  types. Unknown `type_id`s become generic `*bizerror.Error` values carrying
  `bizerror.ErrUnknownType`.
- The sender registry alone decides which fields may leave the process; the
  field allow-list is default-deny and an `Encode` hook cannot bypass it.
- Reconstructed graphs are always acyclic and bounded, so standard
  `errors.Is` / `errors.As` traversal terminates.
