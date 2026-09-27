# CCC Command (Clear Command Channel) Implementation

## Overview

Implement the `CCC` command as specified in [RFC 4217 section 5](https://tools.ietf.org/html/rfc4217#section-5).
It reverts the control connection to clear text after a TLS session has been established, which some
legacy clients and firewall/NAT inspection setups require. Data connections keep the protection level
set by the `PROT` command: `CCC` never touches `transferTLS`.

## Enabling

`CCC` is rejected unless the server explicitly opts in with the `EnableCCC` setting (default: disabled).
When enabled and TLS is configured, `CCC` is advertised in the `FEAT` response.

## Accepted session states

`CCC` is only accepted when all of the following hold:

- `EnableCCC` is set, otherwise `502 CCC is disabled`.
- The connection is not an implicit TLS connection (`503`), there is no clear-text fallback on such ports.
- The control connection is currently protected by TLS (`503` otherwise, including a second `CCC`).
- Neither the server (`TLSRequired`) nor the per-client requirement mandates encryption (`534`).
- The client is logged in (enforced by the command table: `CCC` is not an open command).

## TLS shutdown and clear-text resynchronization

The hard part is the boundary between the TLS shutdown and the clear-text command stream
(`clearControlConnection` in `handle_auth.go`):

1. The `200` reply is sent and flushed through the TLS layer.
2. The server sends its `close_notify`. `tls.Conn.Close()` is the only way to emit it, and it also
   closes the underlying connection, so `handleAUTH` builds the TLS layer on a `noCloseConn` wrapper
   and keeps the raw connection in `clientHandler.rawConn` (closed by `disconnect`).
   `tls.Conn.Close()` also leaves an expired write deadline on the underlying connection, which must
   be cleared or no clear-text answer could ever be sent.
3. The server waits for the client `close_notify` on the raw connection with a deadline
   (`cccShutdownTimeout`, 30s). The record is consumed at the TLS record layer so it is never
   interpreted as a clear-text command. TLS 1.2 and below send it as a plain alert record; TLS 1.3
   encrypts it, so the first record after `CCC` is consumed. If the first bytes don't look like a
   TLS record, the client switched to clear text without a `close_notify` and those bytes are kept.
4. The reader/writer are rebuilt on the raw connection. Already-received commands are carried over:
   decrypted data buffered by the previous `bufio.Reader` and any clear-text bytes read while
   looking for the `close_notify`.

## Dialogs

### Successful close_notify exchange

```
C: AUTH TLS
S: 234 AUTH command ok. Expecting TLS Negotiation.
    === TLS handshake ===
C: USER test                      (TLS)
S: 331 OK                         (TLS)
C: PASS test                      (TLS)
S: 230 Password ok, continue      (TLS)
C: PROT P                         (TLS)
S: 200 OK                         (TLS)
C: CCC                            (TLS)
S: 200 Control connection will now switch to clear text (TLS)
S: --- TLS close_notify --->
C: <--- TLS close_notify ---
C: NOOP                           (clear text)
S: 200 OK                         (clear text)
```

Final state: control connection in clear text, session (login, cwd) preserved,
data connections still protected by `PROT P`.

### Client never sends its close_notify

```
C: CCC                            (TLS)
S: 200 Control connection will now switch to clear text (TLS)
S: --- TLS close_notify --->
    ... 30s of silence ...
S: 421 CCC shutdown failed: closing control connection (clear text, best effort)
S: --- TCP FIN --->
```

Final state: connection closed. The channel state cannot be resynchronized safely
(any late `close_notify` would be parsed as a command), so the server gives up on it.

### CCC disabled (default)

```
C: CCC                            (TLS)
S: 502 CCC is disabled            (TLS)
C: NOOP                           (TLS)
S: 200 OK                         (TLS)
```

Final state: connection unchanged, still fully protected by TLS.
