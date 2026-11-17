[![Build Status](https://github.com/alexdalitz/dnsruby/actions/workflows/ci.yml/badge.svg)](https://github.com/alexdalitz/dnsruby/actions/workflows/ci.yml)
[![Coverage Status](https://img.shields.io/coveralls/alexdalitz/dnsruby.svg)](https://coveralls.io/r/alexdalitz/dnsruby?branch=master)

<img src="http://caerkettontech.com/dnsruby/DNSRuby-colour-mid.png" width="200" height="200" />

# Dnsruby

Dnsruby is a pure Ruby DNS client library. Its stub resolver sends queries to
configured DNS servers, with an API based on Ruby's `Resolv` library and extended
to support DNSSEC, dynamic updates and zone transfers. It aims to comply with
the DNS RFCs.

**Upgrading from 1.74.0:** [Read the v2 upgrade guide](#upgrading-to-v2) for changed
defaults and compatibility notes.

## Getting started

Install the gem:

```sh
gem install dnsruby
```

Or add it to your Gemfile and run `bundle install`:

```ruby
gem 'dnsruby'
```

To query the nameservers configured on your system:

```ruby
require 'dnsruby'

resolver = Dnsruby::Resolver.new
begin
  response = resolver.query('example.com', 'A')
  response.answer.each { |record| puts record }
ensure
  resolver.close
end
```

The second argument selects the record type: for example, `'AAAA'`, `'MX'` or
`'TXT'`. If you leave it out, the query asks for an A record. Reuse a resolver
for multiple queries, then close it when you have finished.

See the [API documentation](https://www.rubydoc.info/gems/dnsruby/),
[usage articles in the wiki](https://github.com/alexdalitz/dnsruby/wiki), and
[demo programs](demo) for more examples.

### Dependencies

Dnsruby requires **Ruby 3.0 or later**. RubyGems installs the `base64`, `logger`
and `simpleidn` dependencies. TSIG signing and DNSSEC validation also require
Ruby's OpenSSL library.

### Demo code

The [demo folder](demo) includes `rubydig`, a basic dig-style command-line tool,
and examples of resolving many names concurrently.

## Usage help

### IPv6 nameservers and address formatting

An IPv6 link-local nameserver needs an interface name or index after `%`, so
the operating system knows which network interface to use:

```ruby
resolver = Dnsruby::Resolver.new(nameserver: 'fe80::1%en0')
begin
  response = resolver.query('example.com')
ensure
  resolver.close
end
```

Replace the address and `en0` with your DNS server's address and local interface.
The same format works in `resolv.conf`, for example `nameserver fe80::1%en0`.
The interface suffix is only for local routing; do not put it in an AAAA record.

IPv6 text output is unchanged from 1.74.0. To request the standard RFC 5952
format explicitly, use:

```ruby
Dnsruby::IPv6.create('2001:DB8::1').to_s_canonical # => '2001:db8::1'
```

## Features

- DNSSEC validation.
- Zone transfers (AXFR and IXFR), including TSIG-signed transfers.
- Dynamic updates, including TSIG-signed updates.
- Unknown record types through the generic record API (RFC 3597).
- Record types including A, AAAA, AFSDB, ANY, CAA, CERT, CNAME, DNAME, GPOS,
  HINFO, ISDN, LOC, MB, MG, MINFO, MR, MX, NAPTR, NS, NSAP, NXT, OPT, PTR, PX,
  RP, RT, SOA, SPF, SRV, SVCB, HTTPS, TKEY, TSIG, TXT, WKS, X25, DNSKEY, RRSIG,
  NSEC, NSEC3, NSEC3PARAM, DS and DLV.

## Upgrading to v2

Version 2 adds SVCB and HTTPS records and improves concurrent queries, TCP
connections, DNSSEC validation, caching and zone-file parsing. Start with the
checks below; the [release notes](RELEASE_NOTES.md#compatibility-with-1740)
describe the compatibility changes in full.

### Check these before upgrading

1. **Use Ruby 3.0 or later.** Older Ruby versions are no longer supported.
2. **Check TSIG keys if you sign updates or transfers.** An omitted algorithm
   now means HMAC-SHA256, rather than HMAC-MD5. The application and server must
   agree. See [updating TSIG keys](#updating-tsig-keys).
3. **Check saved records of unknown types.** Automatically generated record
   classes no longer have permanent Ruby names, so saving them with Marshal or
   YAML is unsupported. Ordinary named classes such as `Dnsruby::RR::IN::A`
   are unaffected. For a small, fixed set of application-defined types,
   `Dnsruby::RR::Generic.create(type, klass)` can register permanent classes
   before records are created. See the [persistence notes](RELEASE_NOTES.md#compatibility-with-1740)
   for details.
4. **Review DNSSEC results and DS creation if you use them.** Legacy SHA-1
   signing algorithms are no longer accepted by resolver validation, and new
   DS records use SHA-256 by default. See [DNSSEC algorithm policy](#dnssec-algorithm-policy).
5. **Review any custom retry or transfer settings.** Retries are capped at
   three per question, server address and transport. A zone transfer now has a
   30-second limit for reading or writing each complete DNS message. See
   [retries](#response-caching-and-retries) and [transfer deadlines](#zone-transfer-deadlines).

The default UDP payload size is also smaller: **1232 bytes, down from 4096**.
This reduces fragmentation but can cause more large replies to retry over TCP.
An explicit `udp_size` setting still works. Existing IPv6 text output is preserved.

### Updating TSIG keys

The key name, secret and algorithm must match the DNS server's configuration.
SHA-256 is the default in v2. If the server currently uses MD5, migrate both
sides to SHA-256 or explicitly name the old algorithm during the transition.

For BIND, generate a SHA-256 key with
[tsig-keygen](https://bind9.readthedocs.io/en/stable/manpages.html#tsig-keygen-tsig-key-generation-tool):

```sh
tsig-keygen -a hmac-sha256 dnsruby-key.example.
```

Install the key on the server, grant the required update or transfer permissions,
and reload its configuration. Update any scripts that generate keys as well.
Then configure the application with the same name and base64-encoded secret:

```ruby
resolver.tsig = ['dnsruby-key.example.', secret, 'hmac-sha256']
```

Or set the key on an individual message:

```ruby
message.set_tsig(name: 'dnsruby-key.example.', key: secret,
                 algorithm: 'hmac-sha256')
```

These explicit SHA-256 settings also work in 1.74.0, so you can migrate the keys
before upgrading Dnsruby. Keep the generated secret private.

If you must keep an existing MD5 key temporarily, add the algorithm explicitly:

```ruby
resolver.tsig = [key_name, secret, 'hmac-md5']
```

In particular, replace existing two-element settings such as
`resolver.tsig = [key_name, secret]`: they now select SHA-256. For a message,
use `algorithm: 'hmac-md5'` in the `set_tsig` hash.

Explicit MD5 remains available for compatibility, although
[RFC 8945 section 6](https://www.rfc-editor.org/rfc/rfc8945.html#section-6)
prohibits its use. Dnsruby does not fall back to MD5 automatically. Selecting it
logs one warning per process; set `Dnsruby.log.level = Logger::WARN` before
configuring TSIG to see it. The default log level is FATAL.

### DNSSEC algorithm policy

Dnsruby validates signatures made with RSA/SHA-256 (8), RSA/SHA-512 (10),
ECDSA P-256 (13), ECDSA P-384 (14), Ed25519 (15), and Ed448 (16). Ed25519 and Ed448 require version
3.0 or later of the `openssl` gem and an OpenSSL provider that supports it
(normally OpenSSL 1.1.1 or later). On older Ruby releases, including Ruby 3.0,
you may need to install a newer compatible `openssl` gem. An unavailable
algorithm is treated as unsupported.

Resolver validation treats RSA/SHA-1 signing algorithms 5 and 7 as unsupported,
as required by [RFC 9905](https://www.rfc-editor.org/rfc/rfc9905.html#section-2).
An answer whose authenticated chain relies only on these algorithms is
`INSECURE`: it is not protected by an accepted DNSSEC chain. A signature using
a supported algorithm must still validate where one is required; a failed
check does not become an insecure result.

Explicit `Dnsruby::Dnssec.verify` calls retain the ability to verify historical
RSA/SHA-1 signatures. Within an authenticated set of DS records, SHA-256 digests
take preference over SHA-1.

`Dnsruby::RR::DS.from_key(key)` now creates a SHA-256 DS record. New DS creation
rejects SHA-1 digests and signing algorithms 5/7. Existing records remain
readable, and `digest_key` and `check_key` still support historical verification.

### DNSSEC validation limits

Dnsruby limits the time and work spent validating an answer, including extra
lookups needed to establish trust. This prevents an unusually expensive reply
from occupying a validator indefinitely.

The defaults for one validation are:

- **Five seconds** of elapsed time, including supporting lookups.
- **64 signature verification attempts.**
- **100,000 work units**, counting operations such as key/signature comparisons
  and NSEC3 hashes used to check that a name or record is absent.

Reaching a limit raises `Dnsruby::ValidationLimitError`, a subclass of
`Dnsruby::VerifyError`. Validation fails; the answer is not accepted as insecure.
If you need to validate unusually large amounts of trusted data, set the limits
before starting queries:

```ruby
Dnsruby::Dnssec.validation_limits = {
  max_signature_verifications: 128,
  max_work: 200_000,
  timeout: 10.0
}
```

Omitted settings use their defaults, and all values must be positive. Validation
also stops when the query times out or the resolver closes. Cancellation is
checked between operations and while waiting on the network; an OpenSSL
operation already in progress is allowed to finish.

At most four workers validate asynchronous responses at once, with 64 more
responses waiting. Further responses fail with `ValidationLimitError`.

### Response caching and retries

Dnsruby now caches negative answers: **NXDOMAIN** means the name does not exist,
and **NODATA** means there is no record of the requested type. It uses the shorter
of the authority's SOA TTL and SOA MINIMUM as the cache lifetime, shortened
further when record or DNSSEC lifetimes require it. Completed lookup failures
are cached for one second, so repeated requests do not immediately repeat the
same failed lookup.

These caches have size limits and keep results separate for different resolver
settings and DNSSEC trust policies. Cancelled queries, raw messages, dynamic
updates and TSIG-signed transactions do not populate them. To disable them, set
`resolver.do_caching = false`; a Recursor uses the setting on its supplied
Resolver. `PacketSender.clear_caches` and `Recursor.clear_caches` also invalidate
these entries.

Positive answers are kept separate for each upstream server, port, source
configuration and query options, including EDNS. Truncated replies are never
reused from the cache, so a later query can still retry over TCP when needed.
Each send attempt uses one snapshot of its transport configuration; subsequent
attempts can use newer settings. Adding an explicit source port replaces the
default `[0]` pool (random source ports). A pool cannot mix zero and explicit ports.

For each question, Dnsruby makes at most **three attempts per server address per
transport**, including retries without optional EDNS support. UDP and TCP have
separate limits. Setting `retry_times` above three is still accepted, but does
not increase this limit.

Recursor caches also have size limits. Cached delegations expire when their NS
records expire, even if the associated nameserver addresses have longer TTLs.
Their reusable lifetimes are also bounded by the parent route through which
they were learned, so a child cannot keep that route alive indefinitely. An
unrelated parent-cache eviction does not discard an already admitted child.
Root NS TTLs govern bootstrap reuse, and missing addresses are resolved even
when a priming reply includes addresses for only some root nameservers.
Fresh nameserver addresses with a zero TTL remain usable for the current
lookup, but are not retained in shared caches.
When you supply a Resolver to `Recursor.new`, its transport and source-address
settings apply to the recursive queries. Its `query_timeout` limits the whole
lookup, including nameserver discovery and validation. TSIG keys remain tied
to the configured peer; they are not passed to arbitrary authoritative servers.

Learned DNSKEY and DS records also have size limits. Configured trust anchors
are kept until you remove them or their requested expiration time arrives;
this applies to both DNSKEY and DS anchors.

### Recursive lookup limits

Each `Recursor` lookup has a 30-second deadline, including root bootstrap,
nameserver address discovery and DNSSEC lookups. A shorter Resolver
`query_timeout` or inherited deadline takes precedence. The recursive deadline
also applies when the Resolver's `query_timeout` is zero.

For ordinary questions, `Recursor` follows CNAME and DNAME aliases and returns
the alias chain together with the target answer under the original question.
Alias lookups share the same deadline and work limits. Questions explicitly
requesting CNAME, DNAME or ANY return the received records directly. When
validation is enabled, the DNSSEC limits above are shared across the entire
recursive lookup, including parallel address discovery and the final answer.

The lookup shares limits of 128 transmission attempts and 256 discovery work
units across its supporting queries. Work units count recursive steps and
nameserver candidates. At most eight extra threads discover nameserver
addresses; further discovery runs within an existing thread. Root address
queries run in batches of at most eight. Repeated address dependencies on the
same discovery path are skipped.

Set these limits before starting queries when your workload needs more time
or discovery work:

```ruby
recursor.recursion_limits = {
  timeout: 60.0,
  max_queries: 256,
  max_steps: 512,
  max_workers: 8
}
```

Omitted settings use their defaults. The timeout must be positive and finite;
other limits must be positive integers. Exhausted work or transmission limits
raise `Dnsruby::RecursionLimitError`, a subclass of `Dnsruby::ResolvError`, and
are not cached as failures of the individual question. An expired recursive
deadline raises `Dnsruby::ResolvTimeout`.

### Zone transfer deadlines

Zone transfers now have a deadline for each complete DNS message read or
written. This is a total time limit for that message, not a timer that restarts
each time more bytes arrive.

| Setting | Default | What it limits |
| --- | --- | --- |
| `connect_timeout` | 5 seconds | Opening a connection to a server. |
| `io_timeout` | 30 seconds | Reading or writing one complete DNS message. |
| `transfer_timeout` | `nil` (no overall limit) | One complete attempt against a server, including the connection and an IXFR-to-AXFR fallback. |

Increase `io_timeout` for a slow connection, or set `transfer_timeout` to bound
the complete attempt. Both accept positive durations in seconds; use `nil` for
`transfer_timeout` to remove the overall limit. If an attempt fails, the next
configured server may be tried.

Use an authoritative server that allows transfers from your application. If it
requires TSIG, configure `transfer.tsig` with the matching key before transferring.

```ruby
transfer = Dnsruby::ZoneTransfer.new
transfer.server = '192.0.2.53' # Replace with your zone's authoritative server
transfer.io_timeout = 60
transfer.transfer_timeout = 300
records = transfer.transfer('example.org')
```

Transfers handle partial reads and writes, check that replies match the request,
and require matching opening and closing SOA records. Multi-message TSIG
verification remains supported.

## Unit tests

From the project root, install the development dependencies and run the tests:

```sh
bundle install
bundle exec rake test_offline  # Tests that do not need public DNS access
bundle exec rake test_online   # Tests that need network access
bundle exec rake test          # Both suites
```

Using `bundle exec` selects the dependency versions required by the project,
including minitest. Tests that need OpenSSL are skipped if it is unavailable.

### Longer concurrency tests

The separate `soak` task exercises concurrent UDP and TCP queries against its
own local test server. It needs no public DNS access, privileged port or
separately running server, and is not included in the standard suites.

Start with a small workload:

```sh
SOAK_THREADS=4 SOAK_ROUNDS=3 SOAK_ASYNC_ROUNDS=3 SOAK_TIMEOUT=30 bundle exec rake soak
```

For the default workload of 81,512 queries across eight cases, allow enough open
files and a longer per-case deadline on a busy machine:

```sh
ulimit -n 4096
SOAK_TIMEOUT=300 bundle exec rake soak
```

Increase `SOAK_ROUNDS` and `SOAK_ASYNC_ROUNDS` for longer runs. The tests check
errors, wrong responses and duplicate completions. They do not measure memory
or file-descriptor growth over a multi-day run. The older RubyDNS/NIO helpers
remain available but are not used by this task.

## Contact/Links

| Link | Address |
| --- | --- |
| Author | alex@caerkettontech.com |
| GitHub | https://github.com/alexdalitz/dnsruby |
| Google Group | https://groups.google.com/forum/#!forum/dnsruby |
| RubyGems | https://rubygems.org/gems/dnsruby/ |
| Documentation | https://www.rubydoc.info/gems/dnsruby/ |
