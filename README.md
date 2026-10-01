# MessagePack encoding for Golang

[![Build Status](https://travis-ci.org/vmihailenco/msgpack.svg)](https://travis-ci.org/vmihailenco/msgpack)
[![PkgGoDev](https://pkg.go.dev/badge/github.com/vmihailenco/msgpack/v5)](https://pkg.go.dev/github.com/vmihailenco/msgpack/v5)
[![Documentation](https://img.shields.io/badge/msgpack-documentation-informational)](https://msgpack.uptrace.dev/)
[![Chat](https://discordapp.com/api/guilds/752070105847955518/widget.png)](https://discord.gg/rWtp5Aj)

> msgpack is brought to you by :star: [**uptrace/uptrace**](https://github.com/uptrace/uptrace).
> Uptrace is an [open source APM](https://uptrace.dev/get/open-source-apm.html) and blazingly fast
> [distributed tracing tool](https://get.uptrace.dev/compare/distributed-tracing-tools.html) powered
> by OpenTelemetry and ClickHouse. Give it a star as well!

## Resources

- [Documentation](https://msgpack.uptrace.dev)
- [Chat](https://discord.gg/rWtp5Aj)
- [Reference](https://pkg.go.dev/github.com/vmihailenco/msgpack/v5)
- [Examples](https://pkg.go.dev/github.com/vmihailenco/msgpack/v5#pkg-examples)

## Features

- Primitives, arrays, maps, structs, time.Time and interface{}.
- Appengine \*datastore.Key and datastore.Cursor.
- [CustomEncoder]/[CustomDecoder] interfaces for custom encoding.
- [Extensions](https://pkg.go.dev/github.com/vmihailenco/msgpack/v5#example-RegisterExt) to encode
  type information.
- Renaming fields via `msgpack:"my_field_name"` and alias via `msgpack:"alias:another_name"`.
- Omitting individual empty fields via `msgpack:",omitempty"` tag or all
  [empty fields in a struct](https://pkg.go.dev/github.com/vmihailenco/msgpack/v5#example-Marshal-OmitEmpty).
- [Map keys sorting](https://pkg.go.dev/github.com/vmihailenco/msgpack/v5#Encoder.SetSortMapKeys).
- Encoding/decoding all
  [structs as arrays](https://pkg.go.dev/github.com/vmihailenco/msgpack/v5#Encoder.UseArrayEncodedStructs)
  or
  [individual structs](https://pkg.go.dev/github.com/vmihailenco/msgpack/v5#example-Marshal-AsArray).
- [Encoder.SetCustomStructTag] with [Decoder.SetCustomStructTag] can turn msgpack into drop-in
  replacement for any tag.
- Simple but very fast and efficient
  [queries](https://pkg.go.dev/github.com/vmihailenco/msgpack/v5#example-Decoder.Query).

[customencoder]: https://pkg.go.dev/github.com/vmihailenco/msgpack/v5#CustomEncoder
[customdecoder]: https://pkg.go.dev/github.com/vmihailenco/msgpack/v5#CustomDecoder
[encoder.setcustomstructtag]:
  https://pkg.go.dev/github.com/vmihailenco/msgpack/v5#Encoder.SetCustomStructTag
[decoder.setcustomstructtag]:
  https://pkg.go.dev/github.com/vmihailenco/msgpack/v5#Decoder.SetCustomStructTag

## Installation

msgpack supports 2 last Go versions and requires support for
[Go modules](https://github.com/golang/go/wiki/Modules). So make sure to initialize a Go module:

```shell
go mod init github.com/my/repo
```

And then install msgpack/v5 (note _v5_ in the import; omitting it is a popular mistake):

```shell
go get github.com/vmihailenco/msgpack/v5
```

## Quickstart

```go
import "github.com/vmihailenco/msgpack/v5"

func ExampleMarshal() {
    type Item struct {
        Foo string
    }

    b, err := msgpack.Marshal(&Item{Foo: "bar"})
    if err != nil {
        panic(err)
    }

    var item Item
    err = msgpack.Unmarshal(b, &item)
    if err != nil {
        panic(err)
    }
    fmt.Println(item.Foo)
    // Output: bar
}
```

## Batch columnar decoding

When an analytics job reads many MessagePack records but needs only a few
fields per record, decoding each record into `map[string]interface{}` wastes
allocations. `Decoder.DecodeColumns` (and the `UnmarshalColumns` convenience
wrapper) projects a stream of records directly into typed column arrays:

```go
var (
    ids    []int64
    scores []float64
)
res, err := msgpack.UnmarshalColumns(data, []msgpack.ColSpec{
    {Path: "id", Kind: msgpack.ColInt, Dst: &ids},
    {Path: "attrs.score", Kind: msgpack.ColFloat, Dst: &scores, TrackNull: true},
}, &msgpack.ColumnOptions{ErrorPolicy: msgpack.SkipBadRows, RowsHint: n})
```

- Paths are dotted map keys with optional array indexes (`items.0.id`);
  unselected fields are structurally skipped.
- Every record appends exactly one aligned slot to every column, so a bad row
  never shifts later columns.
- `Column.Present[i]` is true when the path exists; with `TrackNull`,
  `Column.Null[i]` additionally marks an explicit MessagePack nil, so a
  missing field (`Present=false`) is distinguishable from `nil`
  (`Present=true, Null=true`).
- `ColumnOptions.ErrorPolicy` selects between stopping at the first bad row
  (`StopOnBadRow`) and collecting them in `ColumnResult.BadRows`
  (`SkipBadRows`). In both cases `ColumnResult.ResumeOffset` is the byte
  offset of the first record not consumed; feed `data[ResumeOffset:]` to a
  fresh decoder to continue. Framing errors return a `FrameError` carrying
  the same offset (MessagePack has no in-stream resync marker).
- `MaxRows` caps a batch; call the decoder repeatedly to page through a
  stream.

## See also

- [Golang ORM](https://github.com/uptrace/bun) for PostgreSQL, MySQL, MSSQL, and SQLite
- [Golang PostgreSQL](https://bun.uptrace.dev/postgres/)
- [Golang HTTP router](https://github.com/uptrace/bunrouter)
- [Golang ClickHouse ORM](https://github.com/uptrace/go-clickhouse)

## Contributors

Thanks to all the people who already contributed!

<a href="https://github.com/vmihailenco/msgpack/graphs/contributors">
  <img src="https://contributors-img.web.app/image?repo=vmihailenco/msgpack" />
</a>
