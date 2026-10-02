# go-mbox

[![GoDoc](https://godoc.org/github.com/emersion/go-mbox?status.svg)](https://godoc.org/github.com/emersion/go-mbox)

Package mbox parses the mbox file format into messages and formats messages into
mbox files.

# Limits

`Reader.Limits` caps the resources a single message may consume:

- `MaxRawBytes` bounds the raw bytes consumed by one message.
- `MaxOutputBytes` bounds the unescaped bytes delivered to the caller.
- `MaxLineBytes` bounds the length of a single raw line.
- `MaxSkipScanBytes` bounds the scan performed while skipping a message.

Exceeding a budget returns a `*LimitError` carrying the message index, the raw
stream offset and the exceeded budget. `Reader.SkipMessage` explicitly
discards the remainder of the current message within the skip budget; the next
call to `NextMessage` continues at the next boundary actually found.

# License

MIT
