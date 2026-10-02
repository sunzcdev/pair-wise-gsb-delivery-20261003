# go-smtp

[![Go Reference](https://pkg.go.dev/badge/github.com/emersion/go-smtp.svg)](https://pkg.go.dev/github.com/emersion/go-smtp)

An ESMTP client and server library written in Go.

## Features

* ESMTP client & server implementing [RFC 5321]
* Support for additional SMTP extensions such as [AUTH] and [PIPELINING]
* UTF-8 support for subject and message
* [LMTP] support
* Concurrent connection limits per source IP and per authenticated user

## Relationship with net/smtp

The Go standard library provides a SMTP client implementation in `net/smtp`.
However `net/smtp` is frozen: it's not getting any new features. go-smtp
provides a server implementation and a number of client improvements.

## Connection limiting

Set `Server.Limiter` to cap concurrent connections. Connections are accounted
against their source IP until authentication succeeds, at which point they are
atomically moved to a per-user bucket: if the user quota is already full, the
authentication is rejected with a `454 4.7.0` reply without ever overbooking
it. Source-quota rejections are issued during the connection phase as
`421 4.4.5`.

`NewMemoryLimiter` provides an in-process implementation. Its counters are
local to the server process: it does not coordinate with other processes or
nodes, so it is not a cluster-wide guarantee. Deployments that need shared
quotas can implement the `Limiter` interface on top of an external store.

## Contributing

See [CONTRIBUTING.md] for contribution guidelines.

## Licence

MIT

[RFC 5321]: https://tools.ietf.org/html/rfc5321
[AUTH]: https://tools.ietf.org/html/rfc4954
[PIPELINING]: https://tools.ietf.org/html/rfc2920
[LMTP]: https://tools.ietf.org/html/rfc2033
[CONTRIBUTING.md]: https://github.com/emersion/.github/blob/main/CONTRIBUTING.md
