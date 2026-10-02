# go-mbox

[![GoDoc](https://godoc.org/github.com/emersion/go-mbox?status.svg)](https://godoc.org/github.com/emersion/go-mbox)

Package mbox parses the mbox file format into messages and formats messages into
mbox files.

# Recovery

The default `Reader` is strict and stops at the first format error. For
importing possibly damaged archives, `NewRecoveringReader` scans forward for
the next message whose separator line passes validation (line prefix, envelope
structure and a bounded probe of the subsequent header fields), bounded by
`MaxScanBytes`. Returned messages carry a `Recovered` flag, their offset and
the raw `Skipped` regions, so callers know the data is incomplete.

# License

MIT
