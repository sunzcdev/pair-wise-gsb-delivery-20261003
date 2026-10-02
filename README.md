# go-sasl

[![Go Reference](https://pkg.go.dev/badge/github.com/emersion/go-sasl.svg)](https://pkg.go.dev/github.com/emersion/go-sasl)

A [SASL](https://tools.ietf.org/html/rfc4422) library written in Go.

Implemented mechanisms:

* [ANONYMOUS](https://tools.ietf.org/html/rfc4505)
* [EXTERNAL](https://tools.ietf.org/html/rfc4422#appendix-A)
* [LOGIN](https://tools.ietf.org/html/draft-murchison-sasl-login-00) (obsolete, use PLAIN instead)
* [PLAIN](https://tools.ietf.org/html/rfc4616)
* [OAUTHBEARER](https://tools.ietf.org/html/rfc7628)

## Exchange budgets

Any `Client` or `Server` can be wrapped with `NewBudgetClient` or
`NewBudgetServer` to restrict a single authentication exchange:

* the total number of exchange steps (`MaxSteps`),
* the size of each message received from the peer (`MaxMessageBytes`),
* the cumulative number of bytes received (`MaxInputBytes`),
* and the cumulative number of bytes sent (`MaxOutputBytes`).

The wrapped exchange can additionally be canceled with a
`context.Context`. Once a budget is exhausted or the context is canceled,
subsequent calls return the same stable terminal error without entering the
underlying mechanism. Output from the underlying mechanism that exceeds the
remaining budget is truncated before delivery and reported in the error.
Budget errors only carry numeric accounting information and never include
exchanged data such as passwords or bearer tokens.

## License

MIT
