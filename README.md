# go-mbox

[![GoDoc](https://godoc.org/github.com/emersion/go-mbox?status.svg)](https://godoc.org/github.com/emersion/go-mbox)

Package mbox parses the mbox file format into messages and formats messages into
mbox files.

# Recovery

The default `Reader` is strict: it stops at the first format error. For
importing damaged archives, `NewRecoveringReader` skips malformed data and
searches for the next valid message separator instead. A separator candidate
must start with `From `, have a plausible envelope structure and be followed
by message headers (a bounded probe). The search is bounded by a maximum
number of scanned bytes and stops with `ErrRecoveryLimit` when there is not
enough evidence to continue. Each returned message is accompanied by a
`RecoveryInfo` carrying the message offset and the raw regions that were
skipped, so callers know when data is incomplete.

# License

MIT
