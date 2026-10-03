# go-msgauth

[![Go Reference](https://pkg.go.dev/badge/github.com/emersion/go-msgauth.svg)](https://pkg.go.dev/github.com/emersion/go-msgauth)

A Go library and tools to authenticate e-mails.

## Libraries

* [`dkim`]: create and verify [DKIM signatures][DKIM]
* [`arc`]: create and verify [ARC] sets (AAR, AMS and ARC-Seal), preserving
  authentication results across relays
* [`authres`]: create and parse [Authentication-Results header fields][Authentication-Results]
* [`dmarc`]: fetch [DMARC] records

## Tools

A few tools are included in go-msgauth:

- `dkim-keygen`: generate a DKIM key
- `dkim-milter`: a mail filter to sign and verify DKIM signatures
- `dkim-verify`: verify a DKIM-signed email
- `dmarc-lookup`: lookup the DMARC policy of a domain
- `arc-relay-demo`: build a two-hop ARC flow and show where the chain breaks
  when a seal, a header set or an instance number is tampered with

## License

MIT

[DKIM]: https://tools.ietf.org/html/rfc6376
[ARC]: https://www.rfc-editor.org/rfc/rfc8617
[Authentication-Results]: https://tools.ietf.org/html/rfc7601
[DMARC]: https://tools.ietf.org/html/rfc7489
[`dkim`]: https://pkg.go.dev/github.com/emersion/go-msgauth/dkim
[`authres`]: https://pkg.go.dev/github.com/emersion/go-msgauth/authres
[`arc`]: https://pkg.go.dev/github.com/emersion/go-msgauth/arc
[`dmarc`]: https://pkg.go.dev/github.com/emersion/go-msgauth/dmarc
