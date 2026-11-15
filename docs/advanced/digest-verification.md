# Digest Verification

HTTPX can optionally verify the response body against a digest supplied by the
caller.  The digest covers the raw body bytes as they arrive from the
transport, before content decoding such as gzip or brotli.

This is useful when a download manifest, HTTP response header, or another
trusted source declares a digest and the caller wants to check that the
received representation matches it.

## Verifying a response

Pass an algorithm and the expected digest to a streaming request:

```python
import hashlib

import httpx

expected = hashlib.sha256(b"...").hexdigest()

with httpx.stream(
    "GET", "https://example.org/archive", digest=("sha256", expected)
) as response:
    for chunk in response.iter_bytes():
        ...

assert response.is_digest_verified
```

`client.get()`, `client.stream()`, and the other client request methods also
accept the same `digest=` option.  For responses that are already being
constructed, the option can be passed to `Response`, or configured later with
`response.verify_digest()` before the stream is consumed:

```python
response = httpx.Response(
    200,
    stream=transport_stream,
    digest=("sha256", expected),
)
assert response.read() == expected_body
assert response.digest_status is httpx.DigestStatus.VERIFIED
```

`read()`, `iter_raw()`, `iter_bytes()`, and their async counterparts all use
the same verifier once it is configured.  Calling `read()` after an iteration
that already consumed the stream still respects the existing verifier rather
than starting a second calculation.

## Supported digest values

The algorithm names accepted by `hashlib` are supported, with or without
separators, so `sha256`, `sha-256`, and `SHA256` are equivalent.  The expected
value may be supplied as:

* raw digest bytes of the algorithm's digest size;
* a hexadecimal digest;
* a base64-encoded digest;
* an RFC 9530 structured-field value such as `sha-256=:base64:`;
* a complete field such as `sha-256=base64`.

An unsupported algorithm, a value with the wrong length, or a malformed
structured-field value raises `httpx.InvalidDigest` before the body is
consumed.  This is distinct from a digest that was parsed successfully but did
not match the downloaded bytes.

## Completion and failure states

Verification is incremental.  The verifier accumulates each raw chunk as it is
delivered and can be inspected through `response.digest_verifier`:

* `UNVERIFIED` - the body has not reached EOF.  This includes responses that
  were closed early and transfers interrupted by a network error.  A partially
  read prefix is never reported as verified.
* `VERIFIED` - the body reached EOF and the computed digest matched.
* `FAILED` - the body reached EOF but the computed digest did not match.

An early close leaves the status as `UNVERIFIED`.  Calling
`verifier.verify()` before EOF raises `httpx.IncompleteDigest`; it does not
accept the bytes read so far as a successful check.

A transport failure is not converted into a digest error.  The original
transport exception, such as `httpx.ReadError`, is propagated.  The verifier's
status remains `UNVERIFIED`, and `verifier.is_interrupted` is true with the
cause available as `verifier.interrupted`.

A successful parse followed by a mismatch raises `httpx.DigestMismatch` when
the stream reaches EOF.  The exception exposes `algorithm`, `expected`,
`actual`, and `bytes_read`.  `read()` also raises this exception because it
consumes the entire stream.

## Streaming is not an atomic download

Normal reading still delivers chunks as they arrive.  There is no buffering
and no rollback: **bytes that were already yielded before a digest mismatch
are not withdrawn.**  A mismatch means the consumer must discard or quarantine
the data it has already received.  Digest verification is therefore not an
atomic download facility and should not be described as one.

If the caller needs all-or-nothing storage, it should write to a temporary
location while reading and only publish the file after EOF and
`digest_status == httpx.DigestStatus.VERIFIED`.  The temporary bytes still
exist until the caller removes them.

## Wrapping the transport stream directly

For transport-level integrations, the wrapper can be used without a
`Response`:

```python
import httpx

verified_stream = httpx.DigestVerificationStream(
    raw_stream,
    "sha256",
    expected,
)

for chunk in verified_stream:
    ...

assert verified_stream.verified
```

The same wrapper works for sync and async byte streams.  Closing it before
EOF marks the verifier as incomplete:

```python
verified_stream.close()  # or await verified_stream.aclose()

assert verified_stream.status is httpx.DigestStatus.UNVERIFIED
```

`raise_on_mismatch=False` is available when a caller wants to inspect
`status` and `error` instead of catching `DigestMismatch` on the final
iteration.
