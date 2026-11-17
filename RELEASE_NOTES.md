# Release Notes

## v2.0.0

Dnsruby 2.0 adds SVCB and HTTPS records and support for Ed25519 and Ed448 DNSSEC
signatures. It makes queries, TCP connections, zone transfers and DNSSEC
validation more reliable. It also bounds caches and validation work, updates
the root trust anchors, and improves performance when many queries are in
progress.

Most existing query code can stay as it is. Before upgrading, check the Ruby
requirement, TSIG settings and record persistence described below, followed by
any DNSSEC or timeout settings your application uses.

### Compatibility with 1.74.0

* **Ruby 3.0 or later is required.** Upgrade older Ruby installations before
  installing Dnsruby 2.0.
* **TSIG now defaults to HMAC-SHA256.** If you previously omitted the algorithm
  and your server uses HMAC-MD5, update the key on both sides or select MD5
  explicitly: `resolver.tsig = key_name, secret, 'hmac-md5'`. Explicit MD5
  settings still work, but MD5 is prohibited by RFC 8945 section 6. Selecting it
  logs one warning per process at WARN level; the default log level is FATAL.
  SHA-1 remains supported, and SHA-256 is recommended. There is no automatic
  fallback to another algorithm. See [updating TSIG keys](README.md#updating-tsig-keys)
  for SHA-256 settings that also work with 1.74.0.
* **Automatically generated record classes no longer support Marshal/YAML
  persistence.** Unknown type/class pairs now use a cache of at most 256 classes,
  and those classes are no longer permanent Ruby constants. Ordinary named
  classes such as `Dnsruby::RR::IN::A` are unaffected. Records and classes your
  application holds remain usable even after their automatic `ClassHash` entry
  is evicted. To customise a generated class, keep the result of
  `Dnsruby::RR.get_class(type, klass)` and use `class_eval`. For a fixed set of
  application-defined types that needs permanent names and normal Marshal
  behavior, call `Dnsruby::RR::Generic.create(type, klass)` before creating records. These
  explicit registrations stay in memory: do not use them for arbitrary types
  received from the network. The 256-class limit excludes explicit registrations
  and objects retained by your application or the response cache.
* **DNSSEC uses current algorithm policy.** Resolver validation treats SHA-1
  signing algorithms 5 and 7 as unsupported under RFC 9905. Answers relying
  solely on them become `INSECURE`, meaning they are not protected by an
  accepted DNSSEC chain. Explicit cryptographic verification and historical
  record decoding remain available. DSA algorithms 3 and 6 are also
  unsupported for validation, as required by RFC 8624. An authenticated
  delegation with only unsupported DS algorithms or digests is insecure; a
  mixed set still requires validation through a supported DS. Within an
  authenticated DS set, SHA-256 takes precedence over SHA-1. `DS.from_key` now
  defaults to SHA-256 and rejects new DS records using SHA-1 digests or signing
  algorithms 5 or 7. RSA keys with noncanonical encodings or sizes outside the
  protocol limits are rejected. See [DNSSEC algorithm policy](README.md#dnssec-algorithm-policy).
  Ed25519 and Ed448 validation require version 3.0 or later of the `openssl` gem and
  an OpenSSL provider that supports it. Otherwise it follows the same rules
  as other unsupported algorithms.
* **DNSSEC validation has work and time limits.** The defaults are 64 signature
  attempts, 100,000 work units and five seconds per answer, including queries
  needed to follow the chain of trust. A limit raises `ValidationLimitError`,
  a subclass of `VerifyError`; it never turns a failed validation into an
  insecure answer. Up to four workers validate responses at once, with another
  64 responses waiting. Applications with unusual validation workloads can
  [adjust these limits](README.md#dnssec-validation-limits).
* **Recursive lookups have shared discovery limits.** A complete lookup now
  defaults to 30 seconds, 128 transmission attempts, 256 discovery work units
  and eight address-discovery worker threads. Supporting lookups share these
  limits, and repeated dependencies on the same path are skipped. A shorter
  Resolver timeout still takes precedence. Applications can configure
  `Recursor#recursion_limits`; work or transmission exhaustion raises
  `RecursionLimitError`. See [recursive lookup limits](README.md#recursive-lookup-limits).
* **Retries, failures and transfers have bounded lifetimes.** For each question,
  RFC 9520 limits attempts to three per server address and transport, including
  protocol retries; `retry_times` values above three are capped. Completed
  failures are cached for one second, so an immediate repeat may return the
  same error without a new exchange. The total `query_timeout` now includes
  local DNSSEC validation. Reusable TCP connections close after ten idle
  seconds by default; set `tcp_idle_timeout` to a positive number of seconds
  to change this. Connections with outstanding queries stay open. ZoneTransfer
  allows 30 seconds by default to read or write each complete frame; increase
  `io_timeout` for slower transfers. Its optional `transfer_timeout` limits a
  complete attempt with one server. See [response caching and retries](README.md#response-caching-and-retries)
  and [zone transfer deadlines](README.md#zone-transfer-deadlines).
* **The default EDNS UDP payload size is 1232 bytes, down from 4096.** This
  reduces fragmentation risk but can cause more TCP retries. Explicit sizes
  such as 4096 still work. DNSSEC raises smaller limits to 1220 bytes, the
  minimum in RFC 4035 section 4.1. The 1232-byte default is permitted by
  RFC 10001 section 3.2.
* **SVCB and HTTPS are now parsed records.** Types 64 and 65 use their registered
  names in text output and expose `priority`, `target` and `params`. Their
  `data` and `rdata` readers return binary RDATA snapshots reflecting those
  fields, and `rdlength` reports the length. To replace the data, use `data=`,
  `rdata=` or `RR.create` with `rdata:`. The original decoded `@rdata` remains
  available for existing callers such as JSONDNS, but this private field does
  not track later edits. Malformed escapes are rejected. ALPN and DoC path
  lists permit only escaped commas and backslashes at their second escape
  layer; the `mandatory`, `port`, `ipv4hint`, `ech` and `ipv6hint` mnemonics
  do not permit presentation escapes. Numeric `keyNNNNN` forms still accept
  escaped wire octets. [PR #222](https://github.com/alexdalitz/dnsruby/pull/222).
* **Some invalid names and numeric values are now rejected.** DNS names longer
  than 255 wire octets raise `ResolvError`, including some 256- and 257-octet
  names previously accepted. Numeric type/class constructors and RRSIG
  type-covered setters require values in 0–65535.
  [PR #223](https://github.com/alexdalitz/dnsruby/pull/223).
* **Numeric and IPv6 text remain predictable.** Numeric spellings retain their
  presentation on each object but no longer register global aliases or change
  a code's preferred name. Explicit `Types.add_pair` and `Classes.add_pair`
  aliases still work. Existing `IPv6#to_s`, `#inspect` and record presentations
  keep their 1.74.0 output; use `IPv6#to_s_canonical` for RFC 5952 formatting.
* **The simpleidn range is now `>= 0.2.1`, `< 0.4.0`.** Updating from 0.2.x can
  change Unicode-to-DNS name conversion. Check the results if your application
  depends on exact converted names.
  [PR #224](https://github.com/alexdalitz/dnsruby/pull/224).

Zone files now resolve relative `$ORIGIN` directives and SOA names against the
current origin, including names with an escaped final dot. TXT records preserve
UTF-8 and escaped bytes through wire encoding, and their 255-octet string limit
is checked before writing. ZoneReader treats nonnumeric escapes literally under
DNS zone-file rules, including `\n` as `n`; direct `RR::TXT.parse` and
`RR.create` retain their legacy C-style escapes for compatibility. Escaped
whitespace also works in `$ORIGIN`, MX and NSEC names.
DS, CDS and DLV records preserve private algorithm
numbers and unknown digest numbers. Name compression also keeps literal dots
inside labels distinct from the dots that separate labels.

### More reliable queries and transfers

* TCP pipelining handles concurrent sends, immediate replies, partial writes
  and disconnected servers. Connection setup, writes and waiting for a pipeline
  slot all respect the controlling query's deadline and cancellation. Malformed
  replies no longer interrupt other queries or cause endless TCP retries.
  UDP reads cannot block after a readiness notification, and changes to TCP
  pipelining take effect consistently on subsequent attempts. Updating a source
  port pool no longer briefly exposes an empty pool to concurrent sends.
  Invalid replacements leave the previous configuration intact, and an occupied
  port no longer prevents a send from trying the other configured ports.
  Each attempt retains one transport configuration snapshot through connection
  setup, reply matching and caching. Adding an explicit source port replaces
  the default `[0]` pool; zero cannot be mixed with explicit ports.
  A zero retry delay keeps every scheduled nameserver attempt.
  [#155](https://github.com/alexdalitz/dnsruby/issues/155).
* Cancellation and timeouts wake waiting queries promptly, including while
  validation or nameserver discovery is running. Late replies or callbacks
  cannot complete an expired query or affect a later query that reuses its ID.
  Once validation starts, another nameserver's reply cannot replace the accepted
  answer. Closing a client's completion Queue no longer interrupts other
  resolvers. After `fork`, the child uses fresh connections and query state;
  the parent's pending work remains with the parent.
* Ordinary queries reject replies with the wrong ID, opcode or query/response
  flag; `send_raw` retains its existing behavior. UDP-to-TCP fallback still
  respects `no_tcp` and `ignore_truncation`. Retrying without optional EDNS keeps
  the current transport, and TCP can handle a FORMERR reply without a question;
  UDP still requires a matching question. Queries requiring local DNSSEC
  validation keep EDNS enabled. Setting DO on an existing OPT preserves its
  payload size, options and other flags.
* Hostname nameservers no longer cause recursive configuration hangs. Existing
  queries continue while replacement nameservers are resolved, and concurrent
  configuration changes are preserved. IPv6 link-local nameservers accept
  interface zones such as `fe80::1%eth0`, including in `resolv.conf`. Recursive
  discovery tries other nameservers and address families after ordinary lookup
  failures, within the same cancellation and validation limits. A Recursor
  honors the transport and source settings of its supplied Resolver and applies
  its query timeout to the complete lookup. Changes to DNSSEC root hints take
  effect on subsequent lookups. Root hints are also refreshed when all their
  cached addresses expire, including during a lookup. Root NS TTLs separately
  bound bootstrap reuse, partial priming replies trigger discovery of missing
  root addresses, and zero-TTL priming addresses remain usable by their current
  lookup. Ancestor lookup preserves escaped label boundaries and relative names.
  [#179](https://github.com/alexdalitz/dnsruby/issues/179),
  [#184](https://github.com/alexdalitz/dnsruby/issues/184),
  [PR #221](https://github.com/alexdalitz/dnsruby/pull/221).
* A SingleResolver keeps only its selected server after initialization and
  later `server=` changes. A Resolver with `retry_times = 0` completes with
  a timeout immediately instead of leaving an empty retry plan waiting.
* Zone transfers complete partial writes, check response envelopes and require
  matching opening and closing SOAs. TSIG transfers authenticate the whole
  sequence, permit up to 99 unsigned messages between signatures and reject
  decreasing signed timestamps. Reusing a ZoneTransfer gives each request a
  fresh signing time unless one was set explicitly.

### Authentication, DNSSEC and caching

* Positive answers are kept separate for different upstream servers, ports,
  source settings and query options. Truncated replies are not cached. A
  recursor follows one delegation that encloses the question and still accepts
  valid sibling glue from the parent zone.

* TSIG queries always require a fresh authenticated exchange and bypass the
  shared response cache. Unsigned replies and invalid MACs are ignored while
  waiting for a valid reply; if none arrives, the timeout reports the
  authentication error. Authenticated BADTIME and BADTRUNC replies report the
  server's error. Queries are prepared before signing, including when the key
  is set on a Message; already signed messages retain their headers and OPT
  record. Verification checks the MAC before timestamps and truncation policy,
  and uses the Original ID as required by RFC 8945. Valid truncated MACs are
  supported; `mac_length` sets the signing length and minimum accepted length
  in octets. Malformed TSIG records are rejected, and key secrets are no longer
  written to the log.
* Root trust anchors include KSK-2024 (38696) and remove revoked KSK-2010
  (19036). `Dnssec.reset` restores the current anchors.
  `Dnssec.clear_trusted_keys` clears learned keys and DS/DLV anchors while
  retaining configured DNSKEY anchors. Removing or expiring a configured anchor
  also removes keys learned from it. Keys sharing a tag can coexist, and an
  unusable matching key does not prevent another from validating the answer.
  DS matching accepts the same owner name with or without a trailing dot,
  including when the DS is a configured trust anchor.
  Learned DNSKEY and DS records have size limits, and validation looks up only
  matching candidates. DS anchors now honor their requested expiration times,
  just as DNSKEY anchors do. Direct RRset verification can try another trusted
  verifier when the first has no suitable key.
  [#225](https://github.com/alexdalitz/dnsruby/issues/225).
* Signed aliases can lead to authenticated unsigned zones and correctly return
  `INSECURE`. CNAME and DNAME chains validate through to the terminal answer,
  including negative answers, wildcard NODATA and NSEC3 Opt-Out delegations.
  A synthesized CNAME must match its signed DNAME; valid responses that omit
  the synthesized CNAME also work. Cross-zone aliases discover the target's
  chain within the same validation budget. An unsigned delegation requires
  authenticated proof: lookup failures cannot become insecure results.
  A provisional insecure classification never suppresses a required record's
  verification error; unsigned alias responses check every required owner and
  the terminal name. Signed records discover their signer zones without extra
  DS queries for each record owner.
  Signer zones, DNSKEY Zone Key flags and DS owner/class bindings are checked.
  A key authenticating a DNSKEY RRset must also be present in that set.
  Missing NSEC or RRSIG bitmap bits do not prove those types absent.
  NSEC3 records with unsupported flags are ignored as denial proofs while
  supported records remain usable.
* Signed Additional records are verified when their keys are available.
  Unverifiable signed extras are removed without rejecting an independently
  authenticated answer. Unsigned glue remains available without acquiring
  authentication metadata from records in another section; unverified extras
  do not enter the trusted-key cache. Validation and cancellation limits still
  apply to optional data.
* Validated answers and cached copies expire according to their original
  receipt time, signatures and chain of trust. Revalidation does not renew
  those lifetimes, and failed or BOGUS validation is never cached as successful.
  Trying another verifier preserves a successful insecure result's authenticated
  lifetime, so negative answers can still be cached under the default policy.
  Bounded negative caches use the SOA lifetime required by RFC 2308; failure
  caches follow RFC 9520. Entries respect the question class, configuration and
  trust changes. Cancellation, raw messages, UPDATE and TSIG bypass these new
  negative and failure caches. Recursive delegation caches are also bounded,
  and delegation and glue-address lifetimes are tracked separately. Cached
  child routes cannot outlive the parent route through which they were learned;
  parent-cache eviction alone does not discard an already admitted child.
* Changing a DNSKEY's key or algorithm clears its cached public key.
  `verify_rrset` accepts a single DS/DLV anchor as well as an RRSet. NSEC3 and
  NSEC3PARAM correctly handle unsigned length octets and retain salt lengths.

### Records, parsing and performance

* SVCB and HTTPS work in DNS messages and zone files, including relative targets,
  numeric `keyNNNNN` parameters and escaped text. IPv6 hints use RFC 5952 text.
* CAA accepts extensible alphanumeric tags and empty or binary values, including
  an empty `issue` value. Wire decoding preserves the value independently of
  presentation parsing. Private RRSIG algorithm numbers round-trip unchanged.
  Decoded CAA records retain the historical String form of `rdata`, including
  comparisons with text-created records and existing JSONDNS serialization.
  Received ordinary TTLs with their high bit set become zero under RFC 2181;
  OPT fields and TSIG's zero-TTL rule retain their special handling. Mandatory
  names require their wire encoding, while valid empty UPDATE markers still
  round-trip. DS, CDS and DLV equality and hashing agree for digest case variants.
* Zone-file TTLs and SOA intervals accept combined time suffixes such as
  `1h30m`. Both class-before-TTL and TTL-before-class formats work. Quoted,
  escaped and multiline values preserve their punctuation, Unicode and binary
  character strings, including single-quoted TXT and SPF values. An explicit
  record TTL leaves the `$TTL` default in place for later records. Relative
  names in KX, NSEC, NXT, RRSIG, IPSECKEY and HIP data now use the zone origin.
  Zone files also accept RFC 3597 hexadecimal data for known record types
  without changing its bytes.
* Query scheduling, retries and cancellation avoid repeatedly scanning every
  pending query. TCP reply matching is faster when many queries share a
  connection. DNSSEC reuses canonical data and verifies reusable DS, DNSKEY
  and wildcard proof sets once per message. Cache hits spend less time holding
  the cache lock, and record parsing and numeric lookups allocate fewer
  temporary objects. Unknown-record class handling and replacement no longer
  accumulate memory or slow garbage collection on Ruby 3.0–3.2.
  Name compression shares immutable label copies between suffixes, reducing
  allocations while preserving case and escaped label boundaries.
  RRset grouping uses an index and avoids cloning unrelated records. It
  preserves section order and TTL normalization, and charges validation work
  while grouping records and checking duplicates.
  Canonical RRset sorting also avoids repeated duplicate scans, and binary
  message decoding avoids temporary one-byte strings while retaining bounds
  checks, compression safeguards and legacy nonbinary input behavior.
* Recursive answers now complete CNAME and DNAME chains within the original
  lookup's deadline and work limits. Zero-TTL nameserver addresses are usable
  during the lookup that obtained them without entering shared caches. DNSSEC
  work and time limits are shared across the lookup's network hops, parallel
  address discovery and final answer validation.
* DNSSEC validation evaluates applicable trust anchors before accepting a
  provisional insecure result. RRSIG validity and learned-key expiration use
  the serial-number arithmetic required for timestamps that cross the
  32-bit boundary.
* SRV targets, DNAME targets, RRSIG signer names and NSEC successor names are
  encoded without name compression. Valid empty UPDATE prerequisites and
  deletions work for all applicable record types. Zone-file owner names retain
  escaped spaces and quotes. Decoding reuses validated compression suffixes
  within each packet while keeping returned names independently mutable.
* `RRSet#add` accepts another RRSet, and deleting signatures keeps the signature
  count correct. Bulk duplicate checks avoid repeatedly encoding every record
  already in a set while preserving mutable record behavior.
* Replacing a SingleResolver's server preserves concurrent configuration
  changes. TCP replies report their byte size. Zone transfers retain verified
  BADTIME and BADTRUNC diagnostics and no longer allocate unused IXFR state
  for each received record.


## v1.74.0

* Resolve all configured nameservers in parallel
* Only raise ArgumentError from Resolver if _all_ configured nameservers are invalid - otherwise, just log the bad one(s) and continue
* print out extra OPT data
*  Fix wakeup socket deletion on stopping of select thread
* Make sure to add nameservers if using config_info in Resolver
* Fix for RRSET pruning and potential cache poisoning - thanks Alexander Tan
* Fix WKS record protocol field encoding and decoding size according to RFC 1035 Section 3.4.2 where PROTOCOL is an 8 bit (#220)
* Updating workflows to include Ruby 4, and get rdoc working for it

## v1.73.1

* fix(caa): add support for issuevmc (#214) - thanks Mark McDonnell!

## v1.73.0

* Ruby 3.4 updates - thanks @zarqman!
* Enable setting up a custom UDP size when DNSSEC validation is disabled
* EDNS OPT RR for udp_size > 512 - thanks @mrideout!
* Test code improvements - thanks @mrideout!
* base64 gem version fixes
* Fix for file descriptor leak on truncated UDP messages

## v1.72.4

* allow parsing case insensitive property tags for CAA records - thanks ruby-dev!
* Add logger as a dependency for Ruby 3.5.0+ support - thanks Alexandre ZANNI!

## v1.72.3

* Fix cached answer response time - thanks Andre Meij!
* Add base64 as a dependency for Ruby 3.4.0+ support - thanks Alexandre ZANNI!

## v1.72.2

* Invalid CAA handling fixes - thanks Noah Smethwick and Ryan Doherty!

## v.1.72.1

* Add issuemail CAA record support - thanks Ryan Doherty!

## v.1.72.0

* Fix compatibility with the `--enable-string-literal` Ruby option - thanks Jean byroot Boussier!

## v.1.71.0

* Fix assigning public_key in ECDSA - thanks Jiří Kubíček!

## v.1.70.0

* OpenSSL 3 support
* Resource record hashes no longer include the rdata
* Removed requite to title-cased Digest - thanks jhawthron

## v.1.61.9

* Remove unused net-ftp dependency

## v.1.61.8

* Fix compatability with Ruby 3.1 - thanks @byroot Jean Boussier
* Get the digdlv demo to enable dnssec

## v1.61.7

* Fix namespace issue from v1.61.6

## v1.61.6

* RD bit not set on Recursor queries (thanks Sean Dilda!)
* Handle quoted strings followed by unquoted strings in TXT records
* do not worry about rogue repeated Ids - just log an error and get on with life
* Clear Ruby warnings
* Use GitHub Actions rather than Travis

## v1.61.5

* CAA record fixes - add contactphone and contactemail
* Fix eprecation warnings

## v1.61.4

* Dnsruby::Name : document .punycode
* gemspec enhancement
* add yard build file
* fix create name include url special characters
* Fix uninitialized constant error when using via Rails
* Implement ECDSAP256SHA256 (13) / ECDSAP384SHA384 (14) algorithms for DNSKEY
* Reinitialize all IANA TAR keys with Dnssec.reset

## v1.61.3

* TCP timeout and port changes

## v1.61.2

* Add new root key

## v1.61.1

* Add Addressable as a gem runtime dependency

## v1.61.0

* Add URI, CDS and CDNSKEY records
* Supply port to DNS.new as optiona parameter
* Supply timeout to zone transfer connect
* Fix multi-line strings
* Try absolute name as candidate in DNS even if not dot supplied
* Do not try to generate candidates if no domain is given
* Handle new OpenSSL interface as well as old
* Handle new DSA interface
* fix encode error select thread issue
* handle encoding errors
* add punycode support
* Make sure dnssec is enabled in verifier and also in digroot demo
* Other minor fixes and changes to test code and infrastructure

## v1.60.2

* Fix deletion of TXT records with spaces in dynamic updates (thanks Sean Dilda)
* Fix use of non-default ports in Dnsruby::Resolver (thanks Thomas Morgan)
* Fix NAPTR encoding for null rdata dynamic update packets
* Fix CAA resource record encoding
* Avoid changing ruby global thread abort behavior (thanks Brent Cook)

## v1.60.1

* DNSSEC validation switched OFF by default (but can still be switched on)
* Add APL RR support (thanks Manabu Sonoda)
* Various test fixes (thanks Keith Bennett)
* 'include' issues fixed (thanks Keith Bennett!)
* Fixnum replacement (thanks Keith Bennett)
* Zone transfer fixes (thanks Manabu Sonoda)
* Name decoding fix
* MX record passing error now raised
* CAA RR support (thanks Richard Luther)
* TLSA RR support (thanks Manabu Sonoda)


## v1.60.0

* TCP multi-packet support fixed
* Response 'Message' now included with exception.
* Docs added
* CNAME dynamic update fix

## v1.59.3

* Output TXT record multiple strings correctly
* NONE class encoding fix
* only add name labels if there are any

## v1.59.2

* Timeout error fix

## v1.59.1

* Support for HMAC SHA512 TSIG keys
* Fix TCP pipelining tests
* IDN encoding error returned as Dnsruby::OtherResolvError

## v1.59.0

* Add LICENSE file
* Add Cache max_size (gihub issue 64)
* Disable caching for SOA lookups in demo check_soa.rb
* Fix for invalid nameserver in config
* Fix encoding for OPT data (thanks Craig Despeaux)
* Various test system fixes
* OPT fixes
* DNSSEC verification failure handling wrt lack of DS chain
* DNSSEC validation policy name constants
* Fix for BOGUS DLV chains
* demo upgrades
* Resolver hints improvements 


## v1.58.0

* Add TCP pipelining (reusing a single TCP connection for multiple requests).
* Enhance zone reading, including reading data from a string.
* Add add_answer! method for adding duplicate answers, as needed for an AXFR response.
* Add support for GPOS and NXT resource records.
* Test cleanup, including removal of use of Nominet servers, soak_test cleanup.
* Refactorings: MessageDecoder, Resolv, Resolver (part).
* Fix zone reader adding unwanted dot to relative hostnames being converted to absolute.
* Fix default access for tsig options in Resolver.
* Fix ZoneTransfer not to use deprecated SingleResolver.
* Fix Resolver bug in parameter to create_tsig_options.
* Fix tests to always use working copy and not gem.


## v1.57.0

* Add query_raw method as alias for send_plain_message, with option to raise or return error.
* Fixed a bug in RR hash calculation where TTL should have been ignored but wasn't.
* Add support for (obsolete) GPOS resource record type.
* Tweak Travis CI configuration.
* Fix zone reader for case where a line contains whitespace preceding a comment.
* Add post install message.
* Improve README.
* Moved content of NEWS to RELEASE_NOTES.md.
* Use git ls-files now to determine files for inclusion in gem.


## v1.56.0

* Drop support for Ruby 1.8, using lambda -> and hash 'key: value' notations.
* First release since the move from Rubyforge to Github (https://github.com/alexdalitz/dnsruby).
* Add EDNS client subnet support.
* Relocate CodeMapper subclasses, Resolv, RR, and RRSet classes.
* Add Travis CI and coveralls integration.
* Improve Google IPV6 support.
* Convert some file names to snake case.
* Remove trailing whitespace from lines, and ensure that comments have space between '#' and text.
* Restore test success when running under JRuby.
* Disabled attempt to connect to Nominet servers, which are no longer available.
* Convert from test/unit to minitest/autorun to support Ruby 2.1+.
* Remove setup.rb.
* Other minor refactoring and improvements to production code, test code, and documentation.


## v1.53

* Validation routine fixes
* Ruby 1.9 fixes
* Recursor fixes
* IPv4 Regex fixes
* Fixes for A/PTR lookups with IP-like domain name
* TXT and SSHFP processing fixes
* Default retry parameters in Resolver more sensible


## v1.48

* Fixed deadlock/performance issue seen on some platforms
* DNSSEC validation now disabled by default
* Signed root DS record can be added to validator
* ITAR support removed
* multi-line DS/RRSIG reading bug fixed (thanks Marco Davids!)
* DS algorithms of more than one digit can now be read from string
* LOC records now parsed correctly
* HINFO records now parsed correctly


## v1.42

* Complicated TXT and NAPTR records now handled correctly
* ZoneReader now handles odd escape characters correctly
* Warns when immediate timeout occurs because no nameservers are configured
* Easy hmac-sha1/256 options to Resolver#tsig=
* ZoneReader fixed for "IN CNAME @" notations
* ZoneReader supports wildcards
* Dnsruby.version method added - currently returns 1.42


## v1.41

* RFC3597 unknown classes (e.g. CLASS32) now handled correctly
    in RRSIGs
* Resolver#do_caching flag added for Resolver-level caching
* DNSKEY#key_tag now cached - only recalculated when key data
    changes
* Bugfix where Resolver would not time queries out if no 
    nameservers were configured
* Recursor now performs A and AAAA queries in parallel
* Fix for zero length salt
* Fixing priming for signed root
* Fixes for DLV verification
* Other minor fixes


## v1.40

* Zone file reading support added (Dnsruby::ZoneReader)
* Name and Label speed-ups
* CodeMapper speed-ups
* DHCID RR added
* LOC presentation format parsing fixed
* KX RR added
* Quotations now allowed in text representation for ISDN, X25 and HINFO
* AFSDB from_string fixes
* Fixing CERT types and from_string
* CERT now allows algorithm 0
* Fix for DS record comparison
* HIP RR added
* Minor bug fixes
* IPSECKEY RR added
* Clients can now manipulate Name::Labels
