# PRET (Pre-Transfer) Command

## Context

With a passive data connection the client sends `PASV`/`EPSV` before the
command (`STOR`, `RETR`, ...) that names the file. When the server picks a data
node based on the file path, at `PASV` time it still does not know which file
is coming.

`PRET` (popularised by drftpd's pre-transfer framework) lets the client
announce the upcoming transfer command and its argument ahead of time, so the
driver can reserve resources for that single transfer.

## Driver API

The feature is opt-in through a client driver extension:

```go
type ClientDriverExtensionPreTransfer interface {
    PrepareTransfer(cc ClientContext, info PreTransferInfo) (TransferReservation, error)
}

type PreTransferInfo struct {
    Command      string       // RETR, STOR, APPE, LIST, NLST or MLSD
    Path         string       // absolute path (empty for a path-less listing)
    Restart      int64        // REST offset in effect at PRET time
    TransferType TransferType // type in effect at PRET time
}
```

`PrepareTransfer` is called on every accepted `PRET`. Returning an error
rejects the command (mapped with `getErrorCode`, default 550) and creates no
reservation.

The returned `TransferReservation` observes the reservation lifecycle:

```go
type TransferReservation interface {
    Consumed()
}

type TransferReservationLifecycle interface {
    Cancel(info PreTransferInfo, reservation TransferReservation)
    Release(info PreTransferInfo, reservation TransferReservation, err error)
}
```

## Lifecycle

```text
PRET STOR f
  PrepareTransfer --> reservation stored on the session (preTransfer)

PASV

STOR f
  consumePreTransfer: snapshot matches --> Consumed()
  transfer runs
  releaseConsumedPreTransfer --> Release(err)
```

`Consumed` is called at most once; the reservation is then finalized exactly
once:

- `Release` after a consumed transfer attempt (successful or not),
- `Cancel` when a pending reservation is discarded unused.

Pending reservations are canceled when:

- a new `PRET` supersedes them (old one is canceled before the new
  `PrepareTransfer` call),
- the announced transfer no longer matches the snapshot,
- the client disconnects before the transfer starts.

## Matching rules

When a transfer command arrives the server compares its snapshot against the
session state:

- command (`PRET STOR` then `RETR` mismatches),
- resolved absolute path (`PRET STOR a` then `STOR b` mismatches, including a
  path resolved against a changed working directory),
- REST offset (a new `REST` after `PRET` mismatches),
- transfer type (a new `TYPE` after `PRET` mismatches).

On mismatch the pending reservation is canceled and the command is rejected
with `503 Bad sequence of commands`; the client has to re-`PRET`. A consumed
reservation is never reusable, so the next transfer requires a new `PRET` too.

When the extension is not implemented, `PRET` answers `502`, it is not
advertised in `FEAT`, and transfers behave exactly as before.

## Concurrency

The reservation pointer lives on `clientHandler.preTransfer` and is guarded by
`transferMu`, the same lock that serializes transfers with `STAT`/`ABOR`.
Driver callbacks run outside the lock. A reservation consumed by a transfer
goroutine is left in place until that goroutine releases it, so a disconnect
during a transfer cannot steal the reservation out from under it.
