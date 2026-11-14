"""Explicit resolution of WARC ``revisit`` records.

A ``revisit`` record records the HTTP metadata of a capture without storing
the payload a second time.  The payload is expected to be byte-identical to
the payload of an earlier record, referenced either by record id
(``WARC-Refers-To``) or by target URI and date
(``WARC-Refers-To-Target-URI`` + ``WARC-Refers-To-Date``).

This module turns such a record into readable content.  Nothing here fetches
data on its own: the caller supplies a *lookup* callback which returns the
record a reference points at.  The library never reads from the network and
never opens archive files by itself.

Example::

    from warcio.revisit import RevisitResolver

    def lookup(reference):
        # caller-defined: return an ArcWarcRecord or None
        return index.get_record(reference.record_id)

    with RevisitResolver(lookup) as resolver:
        content = resolver.resolve(record)
        print(content.statusline)          # from the revisit record
        print(content.read_content())      # payload from the original record

Stream ownership
----------------
* The lookup callback owns every record and stream it returns.  The resolver
  copies the payload it needs into its own buffer and never closes the
  callback's stream, so the callback may close it as soon as ``resolve()``
  returns.
* Every :class:`RevisitPayload` owns that buffer.  Cached payloads are
  released by :meth:`RevisitResolver.clear_cache` / ``close()``; uncached ones
  belong to the returned :class:`RevisitContent` and are released by
  :meth:`RevisitContent.close`.
* Readers returned by :meth:`RevisitContent.open_payload` and
  :meth:`RevisitContent.open_content` are independent cursors over the
  copied bytes, and every ``resolve()`` call hands back a fresh payload
  buffer rather than the callback's stream, so no cursor is shared between
  resolutions.
* The payload copy is read from the callback's stream exactly once and then
  kept in the resolver's cache (keyed by ``WARC-Record-ID`` when present).
  Resolving the same record again therefore does not need to read the
  callback's stream a second time.  With ``cache_payloads=False`` the
  callback must return a record whose stream is still at the start of the
  payload for every resolution; a stream that was already consumed is
  reported as :class:`~warcio.exceptions.RevisitPayloadError` rather than
  silently yielding empty content.

HTTP metadata provenance
------------------------
``RevisitContent`` deliberately keeps its sources apart.  Its
``rec_headers`` and ``http_headers`` are the revisit record's own WARC and
HTTP metadata, ``payload_record`` is the earlier record whose payload bytes
were used, and only the payload bytes are borrowed from that record.  A
revisit is never replaced by the whole referenced record.

Payload vs. content
-------------------
``WARC-Payload-Digest`` covers the payload *as stored*, i.e. the bytes after
the record's HTTP headers and including any ``Transfer-Encoding`` /
``Content-Encoding``.  Digest verification and ``read_payload()`` therefore
work on ``raw_stream``; ``read_content()`` applies the original record's HTTP
decoding on top and yields the decoded entity body.

Callbacks must therefore return records whose ``raw_stream`` is positioned at
the start of the payload, which is the case for records produced by
``ArchiveIterator`` without ``no_record_parse``.

Errors
------
:class:`~warcio.exceptions.RevisitResolveError` and its subclasses are raised
when a chain is too long, loops, points at a missing record, points at a
record type which can not supply a payload, or when the payload found does not
match the digest declared by the revisit.  A digest mismatch never yields
content.
"""

import tempfile
import threading

from collections import OrderedDict

from warcio.exceptions import (
    RevisitResolveError,
    RevisitReferenceError,
    RevisitCycleError,
    RevisitChainLengthError,
    RevisitProfileError,
    RevisitTargetTypeError,
    RevisitDigestError,
    RevisitPayloadError,
)

from warcio.recordloader import content_stream_for
from warcio.utils import BUFF_SIZE, Digester
from warcio.digestverifyingreader import _parse_digest, _to_b32


# ============================================================================
# WARC-Profile values understood by this resolver
IDENTICAL_PAYLOAD_DIGEST_PROFILES = frozenset([
    'http://netpreserve.org/warc/1.0/revisit/identical-payload-digest',
    'http://netpreserve.org/warc/1.1/revisit/identical-payload-digest',
])

SERVER_NOT_AVAILABLE_PROFILE_SUFFIX = 'revisit/server-not-available'

#: Default maximum number of revisit records followed in one chain
DEFAULT_MAX_CHAIN_LENGTH = 10

#: WARC types whose payload can stand in for a revisit payload
DEFAULT_PAYLOAD_RECORD_TYPES = ('response', 'resource')

#: Payloads larger than this are spooled to a temporary file
DEFAULT_SPOOL_MAX_SIZE = 8 * 1024 * 1024

#: Maximum number of payloads kept in a resolver's cache
DEFAULT_MAX_CACHED_PAYLOADS = 32


# ============================================================================
def _close_quietly(stream):
    """ Close ``stream`` if it supports closing, ignoring errors. """
    close = getattr(stream, 'close', None)
    if close is None:
        return
    try:
        close()
    except Exception:  # pragma: no cover - defensive
        pass


# ============================================================================
def _digest_matches(actual, expected):
    """ Compare two ``'algo:value'`` digests.

    Returns ``True``/``False``, or ``None`` if the algorithms differ.  The
    same RFC 3548 encodings accepted by ``warcio check`` are accepted here.
    """
    actual_algo, actual_value = _parse_digest(actual)
    expected_algo, expected_value = _parse_digest(expected)

    if actual_algo.lower() != expected_algo.lower():
        return None

    try:
        expected_b32 = _to_b32(len(actual_value), expected_value)
    except (ValueError, TypeError):
        return False

    return actual_value == expected_b32


# ============================================================================
class RevisitReference(object):
    """ A single reference from a revisit record to the record it refers to.

    ``kind`` is :attr:`RECORD_ID` when the reference is a WARC record id, or
    :attr:`URI_DATE` when it is a target URI plus date.  ``revisit`` is the
    record that carries the reference, so a callback can inspect it (for
    example to compare ``WARC-Profile`` values).

    A ``URI_DATE`` reference with ``via_profile`` set was derived from the
    revisit's own ``WARC-Target-URI`` and ``WARC-Date`` because the
    identical-payload-digest profile allows that fallback.
    """

    RECORD_ID = 'record-id'
    URI_DATE = 'uri-date'

    def __init__(self, kind, revisit, record_id=None, uri=None, date=None,
                 via_profile=False):
        self.kind = kind
        self.revisit = revisit
        self.record_id = record_id
        self.uri = uri
        self.date = date
        self.via_profile = via_profile

    @property
    def key(self):
        if self.kind == self.RECORD_ID:
            return self.record_id
        return (self.uri, self.date)

    def __repr__(self):
        return ('RevisitReference(kind={0!r}, record_id={1!r}, uri={2!r}, '
                'date={3!r}, via_profile={4!r})').format(
                    self.kind, self.record_id, self.uri, self.date,
                    self.via_profile)

    def __str__(self):
        if self.kind == self.RECORD_ID:
            return 'WARC-Refers-To {0}'.format(self.record_id)
        return ('WARC-Refers-To-Target-URI {0} + WARC-Refers-To-Date '
                '{1}').format(self.uri, self.date)


# ============================================================================
class RevisitPayload(object):
    """ A read-only, re-readable buffer of resolved payload bytes.

    Bytes are held in a :class:`tempfile.SpooledTemporaryFile`, so large
    payloads spill to disk instead of memory.  :meth:`open` returns a reader
    with its own cursor; the shared buffer is never handed out directly.
    """

    def __init__(self, spool_max_size=DEFAULT_SPOOL_MAX_SIZE):
        self._spool = tempfile.SpooledTemporaryFile(max_size=spool_max_size,
                                                    mode='w+b')
        self._lock = threading.RLock()
        self._digests = {}
        self.length = 0
        self.closed = False

    # -- writing (used while the payload is being loaded) --------------
    def _write(self, data):
        self._spool.write(data)
        self.length += len(data)

    def _store_digest(self, algo, digester):
        self._digests[algo.lower()] = str(digester)

    # -- reading -------------------------------------------------------
    def digest(self, algo):
        """ Return the digest of the whole payload as ``'algo:value'``. """
        key = algo.lower()
        if key in self._digests:
            return self._digests[key]

        with self._lock:
            if key in self._digests:
                return self._digests[key]

            if self.closed:
                raise ValueError('payload buffer is closed')

            digester = Digester(key)
            with self.open() as reader:
                while True:
                    data = reader.read(BUFF_SIZE)
                    if not data:
                        break
                    digester.update(data)

            value = str(digester)
            self._digests[key] = value
            return value

    def ensure_digests(self, algorithms):
        for algo in algorithms:
            self.digest(algo)

    def open(self):
        """ Return a new independent reader positioned at the start. """
        if self.closed:
            raise ValueError('payload buffer is closed')
        return RevisitPayloadReader(self)

    def read(self):
        """ Return the whole payload as bytes. """
        with self.open() as reader:
            return reader.read()

    def close(self):
        with self._lock:
            if not self.closed:
                self.closed = True
                self._spool.close()

    def __del__(self):  # pragma: no cover - interpreter shutdown
        try:
            self.close()
        except Exception:
            pass

    def __repr__(self):
        return 'RevisitPayload(length={0}, closed={1})'.format(
            self.length, self.closed)


# ============================================================================
class RevisitPayloadReader(object):
    """ A streaming reader over a :class:`RevisitPayload`.

    Each instance keeps its own offset into the shared buffer, so several
    readers may exist at once and iteration of one never moves another.
    """

    def __init__(self, payload):
        self._payload = payload
        self._offset = 0
        self.closed = False

    def _readable(self):
        if self.closed:
            return False
        if self._payload.closed:
            raise ValueError('payload buffer is closed')
        return True

    def read(self, size=-1):
        if not self._readable():
            return b''

        payload = self._payload
        with payload._lock:
            spool = payload._spool
            spool.seek(self._offset)

            if size is None or size < 0:
                data = spool.read()
            else:
                chunks = []
                remaining = size
                while remaining > 0:
                    chunk = spool.read(remaining)
                    if not chunk:
                        break
                    chunks.append(chunk)
                    remaining -= len(chunk)
                data = b''.join(chunks)

            self._offset += len(data)
            return data

    def readline(self, size=-1):
        if not self._readable():
            return b''

        payload = self._payload
        with payload._lock:
            spool = payload._spool
            spool.seek(self._offset)
            data = spool.readline(size)
            self._offset += len(data)
            return data

    def tell(self):
        return self._offset

    def readable(self):
        return True

    def close(self):
        self.closed = True

    def __iter__(self):
        return self

    def __next__(self):
        line = self.readline()
        if not line:
            raise StopIteration
        return line

    # python 2 style alias, mirroring the rest of the library
    next = __next__

    def __enter__(self):
        return self

    def __exit__(self, *exc_info):
        self.close()


# ============================================================================
class RevisitContent(object):
    """ The readable content produced by resolving a revisit record.

    The revisit's own metadata stays authoritative for the current capture:
    ``rec_headers`` and ``http_headers`` come from ``revisit``.  The payload
    bytes come from ``payload_record``, which is kept as a separate object
    rather than substituted for the revisit.  ``chain`` lists the revisit
    records walked through, outermost first.

    ``open_payload()`` yields the payload exactly as stored (the bytes covered
    by ``WARC-Payload-Digest``), while ``open_content()`` yields the payload
    with the original record's HTTP transfer/content decoding applied.
    """

    def __init__(self, revisit, chain, payload_record, payload,
                 actual_digest=None, owns_payload=False):
        self.revisit = revisit
        self.chain = tuple(chain)
        self.payload_record = payload_record
        self.payload = payload
        self.actual_digest = actual_digest
        self._owns_payload = owns_payload
        self._closed = False

    # -- metadata ------------------------------------------------------
    @property
    def rec_headers(self):
        """ The revisit record's own WARC headers. """
        return self.revisit.rec_headers

    @property
    def http_headers(self):
        """ The revisit record's own HTTP status + headers (may be ``None``). """
        return self.revisit.http_headers

    @property
    def statusline(self):
        if self.http_headers and self.http_headers.statusline:
            return self.http_headers.statusline
        return None

    @property
    def target_uri(self):
        return self.rec_headers.get_header('WARC-Target-URI')

    @property
    def length(self):
        """ Length of the resolved payload, in bytes. """
        return self.payload.length

    @property
    def digest_verified(self):
        """ True when the payload was checked against a declared digest. """
        return self.actual_digest is not None

    @property
    def closed(self):
        return self._closed

    # -- payload access ------------------------------------------------
    def _check_open(self):
        if self._closed:
            raise ValueError('revisit content is closed')

    def open_payload(self):
        """ Open a new reader over the payload as stored, positioned at 0. """
        self._check_open()
        return self.payload.open()

    def read_payload(self):
        """ Read the whole payload as stored and return it as bytes. """
        reader = self.open_payload()
        try:
            return reader.read()
        finally:
            _close_quietly(reader)

    def iter_payload(self, size=BUFF_SIZE):
        """ Yield the payload as stored in chunks, starting at the beginning. """
        reader = self.open_payload()
        try:
            while True:
                data = reader.read(size)
                if not data:
                    break
                yield data
        finally:
            _close_quietly(reader)

    def open_content(self, http_headers=None):
        """ Open a reader over the decoded payload, positioned at 0.

        Decoding follows the HTTP headers of the record that stored the
        payload unless ``http_headers`` is given explicitly.  The reader
        should be closed by the caller; closing it never closes the shared
        payload buffer.
        """
        self._check_open()
        if http_headers is None:
            http_headers = getattr(self.payload_record, 'http_headers', None)
        return content_stream_for(http_headers, self.payload.open())

    def read_content(self, http_headers=None):
        """ Read the whole decoded payload and return it as bytes. """
        reader = self.open_content(http_headers)
        try:
            return reader.read()
        finally:
            _close_quietly(reader)

    def iter_content(self, size=BUFF_SIZE, http_headers=None):
        """ Yield the decoded payload in chunks, starting at the beginning. """
        reader = self.open_content(http_headers)
        try:
            while True:
                data = reader.read(size)
                if not data:
                    break
                yield data
        finally:
            _close_quietly(reader)

    # -- lifetime ------------------------------------------------------
    def close(self):
        """ Release this content.

        A payload cached by the resolver stays alive for later resolutions;
        the resolver releases it from ``clear_cache()`` or ``close()``.
        """
        if self._closed:
            return
        self._closed = True
        if self._owns_payload:
            self.payload.close()

    def __enter__(self):
        return self

    def __exit__(self, *exc_info):
        self.close()

    def __repr__(self):
        return ('RevisitContent(uri={0!r}, chain_length={1}, '
                'payload_record_type={2!r}, length={3})').format(
                    self.target_uri, len(self.chain),
                    getattr(self.payload_record, 'rec_type', None),
                    self.payload.length)


# ============================================================================
class RevisitResolver(object):
    """ Resolve revisit records with a caller-provided lookup.

    ``lookup`` is a callable invoked as ``lookup(reference)`` with a
    :class:`RevisitReference`; it returns the record the reference points at,
    or ``None`` when the record is not available.  As a convenience the two
    kinds of reference can be handled by separate callbacks instead:
    ``by_record_id(record_id)`` and ``by_uri_date(uri, date)``.  ``lookup``
    may also be a mapping keyed by record id or by ``(uri, date)``.

    The callback may return an :class:`~warcio.recordloader.ArcWarcRecord`
    read from an archive, a record built with
    :class:`~warcio.recordbuilder.RecordBuilder`, or any object with the same
    ``rec_type``, ``rec_headers``, ``http_headers`` and ``raw_stream``
    attributes.
    """

    DEFAULT_MAX_CHAIN_LENGTH = DEFAULT_MAX_CHAIN_LENGTH
    DEFAULT_PAYLOAD_RECORD_TYPES = DEFAULT_PAYLOAD_RECORD_TYPES
    DEFAULT_SPOOL_MAX_SIZE = DEFAULT_SPOOL_MAX_SIZE

    def __init__(self, lookup=None, by_record_id=None, by_uri_date=None,
                 max_chain_length=DEFAULT_MAX_CHAIN_LENGTH,
                 payload_record_types=DEFAULT_PAYLOAD_RECORD_TYPES,
                 require_digest=False,
                 verify_target_digest=False,
                 strict_profiles=True,
                 spool_max_size=DEFAULT_SPOOL_MAX_SIZE,
                 cache_payloads=True,
                 max_cached_payloads=DEFAULT_MAX_CACHED_PAYLOADS):
        if max_chain_length is None or int(max_chain_length) < 1:
            raise ValueError('max_chain_length must be at least 1')

        if max_cached_payloads is not None and int(max_cached_payloads) < 1:
            raise ValueError('max_cached_payloads must be at least 1, or None')

        if lookup is None and by_record_id is None and by_uri_date is None:
            raise ValueError('a lookup callback is required')

        self._lookup = self._wrap_lookup(lookup, by_record_id, by_uri_date)

        self.max_chain_length = int(max_chain_length)
        self.payload_record_types = tuple(payload_record_types)
        self.require_digest = require_digest
        self.verify_target_digest = verify_target_digest
        self.strict_profiles = strict_profiles
        self.spool_max_size = spool_max_size
        self.cache_payloads = cache_payloads
        self.max_cached_payloads = max_cached_payloads

        self._payload_cache = OrderedDict()
        self._cache_lock = threading.Lock()

    # ------------------------------------------------------------------
    @staticmethod
    def _wrap_lookup(lookup, by_record_id, by_uri_date):
        if lookup is not None and not callable(lookup):
            mapping = lookup
            if not hasattr(mapping, 'get'):
                raise TypeError('lookup must be callable or mapping-like')

            def mapping_lookup(reference, mapping=mapping):
                if reference.kind == RevisitReference.RECORD_ID:
                    keys = [reference.record_id]
                else:
                    keys = [(reference.uri, reference.date), reference.uri]

                for key in keys:
                    try:
                        value = mapping.get(key)
                    except TypeError:  # pragma: no cover - unhashable key
                        continue
                    if value is not None:
                        return value
                return None

            return mapping_lookup

        if lookup is not None:
            return lambda reference: lookup(reference)

        def split_lookup(reference):
            if reference.kind == RevisitReference.RECORD_ID:
                if by_record_id is None:
                    return None
                return by_record_id(reference.record_id)

            if by_uri_date is None:
                return None
            return by_uri_date(reference.uri, reference.date)

        return split_lookup

    # ------------------------------------------------------------------
    def resolve(self, revisit):
        """ Resolve ``revisit`` and return a :class:`RevisitContent`. """
        if getattr(revisit, 'rec_type', None) != 'revisit':
            raise RevisitResolveError(
                'record to resolve is not a revisit record '
                '(WARC-Type: {0})'.format(getattr(revisit, 'rec_type', None)),
                record=revisit)

        chain = []
        seen = set()
        current = revisit

        while True:
            chain.append(current)

            if len(chain) > self.max_chain_length:
                raise RevisitChainLengthError(
                    'revisit chain longer than the configured maximum of '
                    '{0}'.format(self.max_chain_length),
                    record=current, chain=chain)

            key = self._record_identity(current)
            if key in seen:
                rec_id = None
                if getattr(current, 'rec_headers', None) is not None:
                    rec_id = current.rec_headers.get_header('WARC-Record-ID')
                raise RevisitCycleError(
                    'revisit chain loops back to a record already in the '
                    'chain ({0})'.format(rec_id or 'no WARC-Record-ID'),
                    record=current, chain=chain)
            seen.add(key)

            self._check_profile(current, chain)

            target, reference, attempts = self._find_reference(current)
            if target is None:
                raise RevisitReferenceError(
                    'could not find the record referenced by revisit '
                    'record{0}'.format(
                        ': ' + str(attempts[0]) if attempts else
                        ' (no WARC-Refers-To, WARC-Refers-To-Target-URI or '
                        'usable WARC-Refers-To-Date)'),
                    record=current, chain=chain,
                    reference=attempts[0] if attempts else None)

            if getattr(target, 'rec_type', None) == 'revisit':
                current = target
                continue

            payload_record = target
            break

        target_type = getattr(payload_record, 'rec_type', None)
        if target_type not in self.payload_record_types:
            raise RevisitTargetTypeError(
                'referenced record has WARC-Type {0!r}, which can not supply '
                'a revisit payload; expected one of {1}'.format(
                    target_type, list(self.payload_record_types)),
                record=payload_record, chain=chain,
                target_record=payload_record)

        expected = self._expected_digests(chain, payload_record)
        algorithms = self._digest_algorithms(expected, chain)

        payload, is_cached = self._obtain_payload(payload_record, algorithms)

        try:
            actual_digest = self._verify_payload(payload, expected, chain)
        except Exception:
            # a failed payload must never be shared or returned
            if not is_cached:
                payload.close()
            raise

        owns_payload = not is_cached and not self.cache_payloads

        if not is_cached and self.cache_payloads:
            self._cache_payload(payload_record, payload)

        return RevisitContent(revisit, chain, payload_record, payload,
                              actual_digest=actual_digest,
                              owns_payload=owns_payload)

    # ------------------------------------------------------------------
    def clear_cache(self):
        """ Close and drop all payloads cached by this resolver. """
        with self._cache_lock:
            cached = list(self._payload_cache.values())
            self._payload_cache.clear()

        for _, payload, _ in cached:
            payload.close()

    def close(self):
        self.clear_cache()

    def __enter__(self):
        return self

    def __exit__(self, *exc_info):
        self.close()

    # ------------------------------------------------------------------
    def _find_reference(self, revisit):
        attempts = self._reference_attempts(revisit)

        for reference in attempts:
            target = self._lookup(reference)
            if target is not None:
                return target, reference, attempts

        return None, None, attempts

    def _reference_attempts(self, revisit):
        headers = getattr(revisit, 'rec_headers', None)
        if headers is None:
            return []

        record_id = headers.get_header('WARC-Refers-To')
        uri = headers.get_header('WARC-Refers-To-Target-URI')
        date = headers.get_header('WARC-Refers-To-Date')

        attempts = []

        if record_id:
            attempts.append(RevisitReference(RevisitReference.RECORD_ID,
                                             revisit, record_id=record_id))

        if uri and date:
            attempts.append(RevisitReference(RevisitReference.URI_DATE,
                                             revisit, uri=uri, date=date))

        # the identical-payload-digest profile allows the revisit's own
        # WARC-Target-URI and WARC-Date when no WARC-Refers-To-* headers exist
        if not attempts:
            profile = headers.get_header('WARC-Profile')
            if profile in IDENTICAL_PAYLOAD_DIGEST_PROFILES:
                uri = headers.get_header('WARC-Target-URI')
                date = headers.get_header('WARC-Date')
                if uri and date:
                    attempts.append(RevisitReference(
                        RevisitReference.URI_DATE, revisit,
                        uri=uri, date=date, via_profile=True))

        return attempts

    def _check_profile(self, revisit, chain):
        headers = getattr(revisit, 'rec_headers', None)
        if headers is None:
            return
        profile = headers.get_header('WARC-Profile')
        if not profile:
            return

        if profile.endswith(SERVER_NOT_AVAILABLE_PROFILE_SUFFIX):
            raise RevisitProfileError(
                'revisit declares that the payload is not available: '
                '{0}'.format(profile), record=revisit, chain=chain)

        if profile in IDENTICAL_PAYLOAD_DIGEST_PROFILES:
            return

        if self.strict_profiles:
            raise RevisitProfileError(
                'unsupported WARC-Profile, payload can not be interpreted: '
                '{0}'.format(profile), record=revisit, chain=chain)

    @staticmethod
    def _record_identity(record):
        """ Identity used to detect cycles in a revisit chain.

        ``WARC-Record-ID`` is authoritative when present.  Otherwise the
        object identity is used, which still catches a lookup that returns the
        very same record again.  ``WARC-Target-URI`` + ``WARC-Date`` is
        deliberately not used: a revisit and the response it refers to share
        them, and two distinct revisits may legitimately do so as well.
        """
        rec_headers = getattr(record, 'rec_headers', None)
        if rec_headers is not None:
            rec_id = rec_headers.get_header('WARC-Record-ID')
            if rec_id:
                return ('record-id', rec_id)

        return ('object', id(record))

    # ------------------------------------------------------------------
    def _expected_digests(self, chain, payload_record):
        expected = []

        for rec in chain:
            rec_headers = getattr(rec, 'rec_headers', None)
            digest = None
            if rec_headers is not None:
                digest = rec_headers.get_header('WARC-Payload-Digest')

            if digest:
                expected.append((rec, digest))
            elif self.require_digest:
                raise RevisitDigestError(
                    'revisit record has no WARC-Payload-Digest to verify the '
                    'referenced payload against', record=rec, chain=chain)

        if self.verify_target_digest:
            rec_headers = getattr(payload_record, 'rec_headers', None)
            digest = None
            if rec_headers is not None:
                digest = rec_headers.get_header('WARC-Payload-Digest')
            if digest:
                expected.append((payload_record, digest))

        return expected

    def _digest_algorithms(self, expected, chain):
        algorithms = []

        for rec, digest in expected:
            try:
                algo, _ = _parse_digest(digest)
            except ValueError:
                raise RevisitDigestError(
                    'could not parse digest algorithm out of {0!r}'.format(
                        digest), record=rec, chain=chain, expected=digest)

            algo = algo.lower()
            if algo in algorithms:
                continue

            try:
                Digester(algo)
            except ValueError:
                raise RevisitDigestError(
                    'unknown hash algorithm name in payload digest: '
                    '{0}'.format(digest), record=rec, chain=chain,
                    expected=digest)

            algorithms.append(algo)

        return algorithms

    # ------------------------------------------------------------------
    def _obtain_payload(self, payload_record, algorithms):
        """ Return ``(payload, is_cached)`` for the referenced record.

        A payload already cached for the same record is reused.  When
        ``cache_payloads`` is off no cache is consulted at all.  Records are
        keyed by ``WARC-Record-ID`` when available, so a later lookup
        returning a different object for the same record still hits the
        cache.  Only records without an id are retained in the cache, to keep
        their identity stable.
        """
        if self.cache_payloads:
            key = self._cache_key(payload_record)
            with self._cache_lock:
                entry = self._payload_cache.get(key)

            if entry is not None and self._cache_entry_matches(entry,
                                                              payload_record):
                payload = entry[1]
                if payload.closed:
                    with self._cache_lock:
                        self._payload_cache.pop(key, None)
                else:
                    payload.ensure_digests(algorithms)
                    with self._cache_lock:
                        if key in self._payload_cache:
                            self._payload_cache.move_to_end(key)
                    return payload, True

        return self._read_payload(payload_record, algorithms), False

    def _cache_key(self, payload_record):
        rec_headers = getattr(payload_record, 'rec_headers', None)
        if rec_headers is not None:
            rec_id = rec_headers.get_header('WARC-Record-ID')
            if rec_id:
                return ('record-id', rec_id)
        return ('object', id(payload_record))

    @staticmethod
    def _cache_entry_matches(entry, payload_record):
        if entry[0] != 'object':
            return True
        # id() is only stable while the object is referenced, so the cache
        # holds the record itself for this key
        return entry[2] is payload_record

    def _cache_payload(self, payload_record, payload):
        evicted = []
        key = self._cache_key(payload_record)
        held = payload_record if key[0] == 'object' else None

        with self._cache_lock:
            self._payload_cache[key] = (key[0], payload, held)
            self._payload_cache.move_to_end(key)

            if self.max_cached_payloads is not None:
                while len(self._payload_cache) > self.max_cached_payloads:
                    _, (_, old_payload, _) = self._payload_cache.popitem(
                        last=False)
                    evicted.append(old_payload)

        # closing outside the lock keeps payload I/O out of the critical section
        for old_payload in evicted:
            old_payload.close()

    def _read_payload(self, payload_record, algorithms):
        payload = RevisitPayload(spool_max_size=self.spool_max_size)

        digesters = dict((algo, Digester(algo)) for algo in algorithms)
        stream = payload_record.raw_stream

        try:
            while True:
                data = stream.read(BUFF_SIZE)
                if not data:
                    break

                payload._write(data)
                for digester in digesters.values():
                    digester.update(data)

            expected_length = self._payload_length(payload_record)
            if 0 <= expected_length and payload.length < expected_length:
                raise RevisitPayloadError(
                    'payload of the referenced record is short: {0} of {1} '
                    'bytes were read; the stream may already be consumed '
                    'elsewhere'.format(payload.length, expected_length),
                    record=payload_record)

        except Exception:
            payload.close()
            raise

        for algo, digester in digesters.items():
            payload._store_digest(algo, digester)

        return payload

    @staticmethod
    def _payload_length(payload_record):
        """ Payload length of the referenced record as stored, or -1.

        ``ArcWarcRecord.payload_length`` is the number of payload bytes
        remaining after the HTTP headers, i.e. exactly what ``raw_stream``
        yields, so it can be used to notice a stream that was already read.
        """
        length = getattr(payload_record, 'payload_length', -1)
        if isinstance(length, int) and not isinstance(length, bool) and length >= 0:
            return length
        return -1

    def _verify_payload(self, payload, expected, chain):
        actual_digest = None

        for rec, digest in expected:
            algo, _ = _parse_digest(digest)
            actual = payload.digest(algo)

            match = _digest_matches(actual, digest)

            if match is None:
                raise RevisitDigestError(
                    'payload digest uses a different algorithm than the '
                    'one declared by the revisit: expected {0}, computed '
                    '{1}'.format(digest, actual), record=rec, chain=chain,
                    expected=digest, actual=actual)

            if not match:
                raise RevisitDigestError(
                    'payload digest mismatch: payload found for the revisit '
                    'does not match the declared {0} (found {1})'.format(
                        digest, actual), record=rec, chain=chain,
                    expected=digest, actual=actual)

            if actual_digest is None:
                actual_digest = actual

        return actual_digest


# ============================================================================
def resolve_revisit(revisit, lookup=None, **kwargs):
    """ Resolve one revisit record, see :class:`RevisitResolver`.

    A throw-away resolver is used, so no payload is cached between calls:
    the payload copy belongs to the returned :class:`RevisitContent` and is
    released by ``close()`` (or when the content is discarded).  Pass
    ``cache_payloads=True`` explicitly if a cache is still wanted.
    """
    kwargs.setdefault('cache_payloads', False)
    return RevisitResolver(lookup, **kwargs).resolve(revisit)