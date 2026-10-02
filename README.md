# go-mbox

[![GoDoc](https://godoc.org/github.com/emersion/go-mbox?status.svg)](https://godoc.org/github.com/emersion/go-mbox)

Package mbox parses the mbox file format into messages and formats messages into
mbox files.

An mbox archive can also be indexed with `BuildIndex`. The index is built in a
streaming pass and records, for every message, the raw byte offsets of its
separator line, body start and end. It never retains a whole message in
memory. A range-limited reader for any message can then be created on top of an
`io.ReaderAt` via `IndexEntry.Reader`; partially read messages can be skipped
simply by discarding that reader.

# License

MIT
