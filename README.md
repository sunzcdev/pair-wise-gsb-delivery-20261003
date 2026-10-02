# go-mbox

[![GoDoc](https://godoc.org/github.com/emersion/go-mbox?status.svg)](https://godoc.org/github.com/emersion/go-mbox)

Package mbox parses the mbox file format into messages and formats messages into
mbox files.

# Reading

`Reader.NextMessage` returns the message text only. `Reader.NextMessageWithEnvelope`
additionally returns the `From ` separator line's information: the raw line, the
sender and the parsed date. The envelope always belongs to the message it
precedes, even when calls are interleaved with `NextMessage`.

By default the reader is lenient: an unreadable date yields the zero time and the
message is still returned. Set `Reader.Strict` before reading to make an
unrecognized date or unexpected trailing text return an `*EnvelopeError` (which
wraps `ErrUnknownDate` or `ErrTrailingText` and carries the offending line
number and raw line). The offending message is skipped, so the next call resumes
with the following message.

# License

MIT
