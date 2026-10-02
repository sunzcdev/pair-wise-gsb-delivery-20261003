# go-mbox

[![GoDoc](https://godoc.org/github.com/emersion/go-mbox?status.svg)](https://godoc.org/github.com/emersion/go-mbox)

Package mbox parses the mbox file format into messages and formats messages into
mbox files.

The default `NewReader`/`NewWriter` use the least common denominator mboxo
rules. Pass `mbox.Mboxrd` to `NewReaderFormat`/`NewWriterFormat` for the mboxrd
variant, where any line beginning with zero or more `>` followed by `From ` is
quoted with one additional `>` on write and unquoted on read. Recognition only
happens at the start of a line; a `From ` in the middle of a line is never
touched.

# License

MIT
