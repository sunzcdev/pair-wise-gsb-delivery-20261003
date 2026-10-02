# go-milter

[![GoDoc](https://godoc.org/github.com/emersion/go-milter?status.svg)](https://godoc.org/github.com/emersion/go-milter)
[![builds.sr.ht status](https://builds.sr.ht/~emersion/go-milter/commits.svg)](https://builds.sr.ht/~emersion/go-milter/commits?)

A Go library to write mail filters.

## Packet size limit

Every milter frame starts with a 4-byte big-endian length header followed by
one command byte and optional payload. Both the server (`Server.MaxPacketSize`)
and the client (`ClientOptions.MaxPacketSize`) reject frames whose advertised
length exceeds the configured limit before allocating or reading the payload.
An unset value defaults to `DefaultMaxPacketSize` (512 MiB). The limit applies
uniformly to option negotiation, macros and body packets; per-chunk body
sizes remain capped independently by `MaxBodyChunk` (65535 bytes).

Framing errors are reported as `*PacketError` values carrying the frame
direction (`DirectionMTA` or `DirectionMilter`), an error kind
(`PacketLengthZero`, `PacketTooLarge` or `PacketTruncated`) and the number of
bytes consumed before the error. Payload data (including message bodies) is
never included in the error.

## License

BSD 2-Clause
