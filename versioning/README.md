# versioning

Optional schema versioning for [MessagePack](https://msgpack.uptrace.dev/).
It keeps historical records readable when Go structs are renamed, split, or
retyped.

- Field identity on the wire is a **stable integer id**, not the Go field name
  (`version:"3"`).
- The **schema version lives in the encoded data**, as the first element of a
  fixed envelope. It is never read from deployment configuration.
- Migrations between versions are **functions the caller registers explicitly**.
  Nothing is guessed from name similarity.
- Types that never opt in are encoded with plain MessagePack; the old
  `msgpack.Marshal` / `msgpack.Unmarshal` bytes are unchanged.

## Wire format

A registered value is encoded as a two-element MessagePack array:

```
[ version:int, fields: map<uint64 fieldID, value> ]
```

Non-registered values are passed through to the underlying encoder unchanged.

## Usage

```go
type PersonV1 struct {
    FullName string `version:"1"`
}

type PersonV2 struct {
    FirstName string `version:"2"`
    LastName  string `version:"3"`
}

r := versioning.NewRegistry()
r.MustRegister(1, PersonV1{})
r.MustRegister(2, PersonV2{})

// The split is stated explicitly; it is never inferred from the names.
r.MustConversion(1, 2, func(in versioning.Fields) (versioning.Fields, error) {
    var full string
    if _, err := in.Get(1, &full); err != nil {
        return nil, err
    }
    first, last, _ := strings.Cut(full, " ")
    out := versioning.Fields{}
    if err := out.Set(2, first); err != nil {
        return nil, err
    }
    return out, out.Set(3, last)
})

var person PersonV2
err := r.Unmarshal(historicalBytes, &person) // upgrades v1 -> v2
```

### Lossless type changes

When a type change cannot preserve information (e.g. a fractional `float64`
becoming an `int64`), return an error wrapping `versioning.ErrLossy`, or use
`versioning.LossyError`:

```go
if f != float64(int64(f)) {
    return nil, versioning.LossyError(1, "fractional value cannot be an int")
}
```
