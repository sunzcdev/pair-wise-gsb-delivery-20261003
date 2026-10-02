[![GoDoc](https://godoc.org/github.com/beevik/ntp?status.svg)](https://godoc.org/github.com/beevik/ntp)
[![Go](https://github.com/beevik/ntp/actions/workflows/go.yml/badge.svg)](https://github.com/beevik/ntp/actions/workflows/go.yml)

ntp
===

The ntp package is an implementation of a Simple NTP (SNTP) client based on
[RFC 5905](https://tools.ietf.org/html/rfc5905). It allows you to connect to
a remote NTP server and request information about the current time.


## Querying the current time

If all you care about is the current time according to a remote NTP server,
simply use the `Time` function:
```go
time, err := ntp.Time("0.beevik-ntp.pool.ntp.org")
```


## Querying time synchronization data

To obtain the current time as well as some additional synchronization data,
use the [`Query`](https://godoc.org/github.com/beevik/ntp#Query) function:
```go
response, err := ntp.Query("0.beevik-ntp.pool.ntp.org")
time := time.Now().Add(response.ClockOffset)
```

The [`Response`](https://godoc.org/github.com/beevik/ntp#Response) structure
returned by `Query` includes the following information:
* `ClockOffset`: The estimated offset of the local system clock relative to
  the server's clock. For a more accurate time reading, you may add this
  offset to any subsequent system clock reading.
* `Time`: The time the server transmitted its response, according to its own
  clock.
* `RTT`: An estimate of the round-trip-time delay between the client and the
  server.
* `Precision`: The precision of the server's clock reading.
* `Stratum`: The server's stratum, which indicates the number of hops from the
  server to the reference clock. A stratum 1 server is directly attached to
  the reference clock. If the stratum is zero, the server has responded with
  the "kiss of death" and you should examine the `KissCode`.
* `ReferenceID`: A unique identifier for the consulted reference clock.
* `ReferenceTime`: The time at which the server last updated its local clock setting.
* `RootDelay`: The server's aggregate round-trip-time delay to the stratum 1 server.
* `RootDispersion`: The server's estimated maximum measurement error relative
  to the reference clock.
* `RootDistance`: An estimate of the root synchronization distance between the
  client and the stratum 1 server.
* `Leap`: The leap second indicator, indicating whether a second should be
  added to or removed from the current month's last minute.
* `MinError`: A lower bound on the clock error between the client and the
  server.
* `KissCode`: A 4-character string describing the reason for a "kiss of death"
  response (stratum=0).
* `Poll`: The maximum polling interval between successive messages to the
  server.

The `Response` structure's [`Validate`](https://godoc.org/github.com/beevik/ntp#Response.Validate)
function performs additional sanity checks to determine whether the response
is suitable for time synchronization purposes.
```go
err := response.Validate()
if err == nil {
    // response data is suitable for synchronization purposes
}
```

If you wish to customize the behavior of the NTP query, use the
[`QueryWithOptions`](https://godoc.org/github.com/beevik/ntp#QueryWithOptions)
function:
```go
options := ntp.QueryOptions{ Timeout: 30*time.Second, TTL: 5 }
response, err := ntp.QueryWithOptions("0.beevik-ntp.pool.ntp.org", options)
time := time.Now().Add(response.ClockOffset)
```

Configurable [`QueryOptions`](https://godoc.org/github.com/beevik/ntp#QueryOptions)
include:
* `Timeout`: How long to wait before giving up on a response from the NTP
  server.
* `Version`: Which version of the NTP protocol to use (2, 3 or 4).
* `TTL`: The maximum number of IP hops before the request packet is discarded.
* `LocalAddress`: The local IP address to use when sending the query. Useful
  when the host has multiple network interfaces.
* `Auth`: The symmetric authentication key and algorithm used by the server to
  authenticate the query. The same information is used by the client to
  authenticate the server's response.
* `Extensions`: Extensions may be added to modify NTP queries before they are
	transmitted and to process NTP responses after they arrive.
* `GetSystemTime`: A custom function to obtain the current system time, used
  to override the platform's default system clock API call.
* `Dialer`: A custom network connection "dialer" function used to override the
  default UDP dialer function.


## Selecting an offset from multiple sources

When responses have already been collected from several servers, the
pure-computation `SelectClockOffset` function can be used to choose a single
clock offset without querying the network or touching the system clock. Each
input pairs a `Response` with a source ID.

Every sample is first checked with `Response.Validate`. Each distinct source
ID then casts at most one vote, using the first valid sample supplied for it;
repeated samples from the same source are reported but never gain extra
votes.

Each voting source defines the closed synchronization interval
`[ClockOffset - RootDistance, ClockOffset + RootDistance]`. Using the
intersection algorithm from [RFC 5905 section
12.1](https://tools.ietf.org/html/rfc5905#section-12), the lowest offset
window covered by at least `MinSources` (by default a simple majority)
distinct sources is selected. Sources covering the window are the
"truechimers" and determine the suggested offset (the median of their clock
offsets); the rest are "falsetickers".

```go
sources := []ntp.TimeSource{
    {SourceID: "a.example", Response: responseA},
    {SourceID: "b.example", Response: responseB},
    {SourceID: "c.example", Response: responseC},
}

result, err := ntp.SelectClockOffset(sources, ntp.SelectOptions{})
if errors.Is(err, ntp.ErrNoConsistentIntersection) {
    // No majority of sources agrees; the sources' offsets are deliberately
    // not averaged together. Inspect result.Sources for the reasons.
} else if err != nil {
    log.Fatal(err)
}

correctedNow := time.Now().Add(result.Offset)
for _, s := range result.Sources {
    if s.Included {
        fmt.Printf("%s included: %s\n", s.SourceID, s.Reason)
    } else {
        fmt.Printf("%s excluded (%s): %s\n", s.SourceID, s.Reason, s.Detail)
    }
}
```

Deterministic rules apply to every edge case:
* Intervals are closed, so two intervals that merely touch at an endpoint
  intersect, producing a zero-width window.
* If more than one window has quorum coverage, the lowest one wins.
* When two offsets compare equal, the earlier input sample orders first.
* The median of an even number of offsets is their midpoint, with a
  half-nanosecond rounded toward zero.
* A sample whose interval endpoints cannot be represented as a
  `time.Duration` is excluded with an `overflow` reason rather than clamped.

Set `MinSources` to require a specific number of agreeing distinct sources;
the zero value requests a simple majority (`n/2 + 1`) of the valid sources.

## Logging responses

To log an NTP query's response, use the response's `Log` function:
```go
options := ntp.QueryOptions{ Version: 4, RequestSupportedVersions: true }
response, err := ntp.QueryWithOptions("0.beevik-ntp.pool.ntp.org", options)
if err != nil {
    response.Log(os.Stdout)
}
```

Output:
```
    Version: 4
ClockOffset: -374.811087ms
        RTT: 67.989037ms
 SystemTime: Sun Sep 20 2026  18:36:41.96194660 (PDT)
  ~TrueTime: Sun Sep 20 2026  18:36:41.58713551 (PDT)
 ClientXmit: Mon Sep 21 2026  01:36:41.89013280 (UTC)
 ServerRecv: Mon Sep 21 2026  01:36:41.54931623 (UTC)
 ServerXmit: Mon Sep 21 2026  01:36:41.54937899 (UTC)
 ClientRecv: Mon Sep 21 2026  01:36:41.95818460 (UTC)
    Stratum: 3
       Leap: No Warning
      Flags: [Synchronized] (0x00000001)
        Era: 0
  Timescale: UTC
       Poll: 1s
  Precision: 29ns
  RootDelay: 20.111084ms
   RootDisp: 366.211µs
   RootDist: 44.416271ms
  Supported: [4]
   MinError: 340.816569ms
    RefTime: Mon Sep 21 2026  01:35:21.44779491 (UTC)
      RefID: 158.51.99.19 (0x9e336313)
   KissCode: <empty>
```

## Experimental NTPv5 support

This package supports the [current draft of NTP version 5
(NTPv5)](https://datatracker.ietf.org/doc/html/draft-ietf-ntp-ntpv5). The
implementation provided by this package is considered experimental and is
subject to change as the draft specification evolves. For this reason, this
package's NTPv5 features are not recommended for use in production
environments.


## Using the NTP pool

The NTP pool is a shared resource provided by the [NTP Pool
Project](https://www.pool.ntp.org/en/) and used by people and services all
over the world. To prevent it from becoming overloaded, please avoid querying
the standard `pool.ntp.org` zone names in your applications. Instead, consider
requesting your own [vendor zone](http://www.pool.ntp.org/en/vendors.html) or
[joining the pool](http://www.pool.ntp.org/join.html).


## Network Time Security (NTS)

Network Time Security (NTS) is a recent enhancement of NTP, designed to add
better authentication and message integrity to the protocol. It is defined by
[RFC 8915](https://tools.ietf.org/html/rfc8915). If you wish to use NTS, see
the [nts package](https://github.com/beevik/nts). (The nts package is
implemented as an extension to this package.)
