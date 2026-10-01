# Data Connection ↔ Control Connection Binding with TLS Session Resumption

## Problem

Historically the only thing tying an FTPS data connection to the FTP control
connection that requested it is the peer IP address
(`ActiveConnectionsCheck` / `PasvConnectionsCheck`). That check is necessary
but not sufficient: several users (NAT gateways, corporate proxies, shared
VPN exits, container SNAT) routinely share one public IP. Nothing in the IP
check stops user B from connecting to the passive listener (or answering the
active-mode callback) created for user A's transfer.

Client certificates do not solve this either: the FTP protocol never
negotiates a *second* authentication on the data channel, and accepting "any
certificate we issued" would still let every legitimate user impersonate
every other one. What is needed is a cryptographic proof that the data
connection is a continuation of **this specific** control TLS connection.

## Mechanism

TLS already provides exactly such a proof: session resumption. A resumed
handshake can only be performed by whoever holds the session ticket/PSK the
server handed to that client on the control connection. We bind every ticket
to the control connection that created it, then require the data connection
to resume it.

The binding is implemented with Go 1.21+ `tls.Config` hooks
(`WrapSession` / `UnwrapSession`), one random 32-byte **binding tag** per
control connection, and a server-wide AES-256-GCM key that authenticates the
ticket envelope:

```
ticket = 0x01 || nonce(12) || AEAD_SEAL(SessionState.Bytes())
```

The binding tag is carried inside the ticket in `SessionState.Extra` (and is
therefore covered by the AEAD), so a client cannot read or forge it.

* The control connection uses a **lenient** `UnwrapSession`: a foreign,
  stale, or malformed ticket is ignored (`nil, nil`) and the client simply
  performs a full handshake and receives a fresh, correctly bound ticket.
* A data connection uses a **strict** `UnwrapSession`: a foreign or
  malformed ticket returns an error and aborts the TLS handshake. After the
  handshake we additionally require `ConnectionState.DidResume == true`, so a
  plain full handshake (no usable ticket) is rejected as well.

### TLS 1.2 (RFC 5077)

* On the full `AUTH TLS` handshake the server sends a **NewSessionTicket**
  record containing the encrypted envelope; the Go client stores it in its
  `ClientSessionCache`.
* On the data connection the client puts the ticket in the
  `SessionTicket` extension of the ClientHello. The server calls
  `UnwrapSession` with it, verifies the AEAD and the embedded binding tag,
  and performs the abbreviated handshake (master secret reuse). A
  cryptographic binding is derived from the fact that only the holder of the
  ticket can produce the `verify_data` for the resumed Finished messages.
* A resumed connection still gets a freshly minted ticket, so **ticket
  rotation** works transparently: every new ticket carries the same binding
  tag and remains tied to the same control connection.

### TLS 1.3 (RFC 8446)

* There are no standalone tickets: the equivalent is the **pre-shared key**
  (PSK) mode. After the full `AUTH TLS` handshake the server sends one or
  more post-handshake `NewSessionTicket` messages; the opaque PSK identity is
  our encrypted envelope and the resumption PSK is derived from the
  resumption master secret.
* On the data connection the client offers the identity in the
  `pre_shared_key` extension of its ClientHello. The server calls
  `UnwrapSession` with the identity, checks the binding tag and the PSK
  binder, and resumes. The binder itself is a MAC over the ClientHello with
  the resumption key, which only the original client can compute.
* As with TLS 1.2, every resumed handshake issues a new ticket derived from
  the new resumption master secret, which is TLS 1.3 ticket rotation and
  keeps the binding alive.

In both versions the data connection is therefore proved to be a continuation
of the control session at the TLS layer; the source IP is no longer load
bearing for this decision.

## Settings

```go
type TLSDataSessionRequirement int8

const (
    TLSDataSessionDisabled TLSDataSessionRequirement = iota // default
    TLSDataSessionRequired
)

type Settings struct {
    // ...
    TLSDataSessionRequirement TLSDataSessionRequirement
}
```

* `TLSDataSessionDisabled` (default): historical behavior, only the IP check
  (if configured) applies. Full upgrade path, no wire change.
* `TLSDataSessionRequired`: every TLS-protected data connection on an
  explicit FTPS (`AUTH TLS` + `PROT P`) session must resume the control
  session. Failure to do so closes the data connection and the transfer
  command receives `425 Can't open data connection`.

Scope and compatibility notes:

* Only **explicit FTPS** is covered. Implicit TLS (`TLSRequired ==
  ImplicitEncryption`) does not use the `AUTH` upgrade and is left untouched.
* Clear data connections (`PROT C`) are not affected.
* Tickets must be enabled; the server forces `SessionTicketsDisabled = false`
  on the cloned config regardless of the driver-provided setting.
* The Go crypto/tls server does not keep a stateful session-ID cache, so
  clients must use ticket/PSK resumption (every mainstream client does).
* Because each data connection requires the control ticket, clients must
  share the session cache between the control and data TLS sockets.

## Code Layout

* `tls_session_binding.go`: envelope AEAD, per-connection binding tag,
  `WrapSession`/`UnwrapSession` hooks and the post-handshake
  `DidResume` enforcement (`verifyDataTLSConnection`).
* `server.go`: server-wide ticket-encryption key (lazily generated,
  AES-256-GCM) and the binding factory.
* `handle_misc.go`: `AUTH TLS` installs the lenient, binding-enabled config.
* `transfer_pasv.go` / `transfer_active.go`: data connections use the strict
  config and are verified on open, for both passive and active modes.
* `driver.go`: `TLSDataSessionRequirement` type, constants and `Settings`
  field.

## Tests

`data_session_binding_test.go` drives a minimal explicit FTPS client with a
selectable per-control-session ticket cache, in both TLS 1.2 and TLS 1.3:

* `TestTLSDataSessionRequirementNormalResume`: two successive transfers both
  resume, and each rotated ticket is distinct (rotation actually happens).
* `TestTLSDataSessionRequirementSwappedDataConnectionRejected`: two control
  sessions from the same IP open/answer each other's data connection; the
  misattributed connection is refused with `425`, while the legitimate
  client still works. Covers passive mode.
* `TestTLSDataSessionRequirementActiveMode`: the same swap attack over
  active mode (`EPRT`) is rejected, and the legitimate active transfer works.
* `TestTLSDataSessionRequirementFullHandshakeRejected`: a data connection
  with an empty cache cannot fall back to a full handshake.
* `TestTLSDataSessionRequirementDisabledCompatibility`: with the policy off,
  both a swapped/resumed connection and a full-handshake data connection are
  accepted, reproducing the pre-feature behavior.
