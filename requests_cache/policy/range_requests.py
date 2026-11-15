"""Utilities for handling ``Range`` requests and ``206 Partial Content`` responses.

A ``206`` response must never be stored under (or served from) the same cache key as a complete
``200`` response. Two policies are supported via :py:attr:`.CacheSettings.range_policy`:

* ``'isolate'`` (default): Range requests bypass the cache entirely, and ``206`` responses are
  never written to the cache. A ``200`` response to a range request (e.g., the server ignored
  ``Range``, or ``If-Range`` didn't match) is treated as a complete response.
* ``'cache'``: Exact single-range ``206`` responses are cached under a range-qualified key. A
  cached partial response may only be reused for a request with the exact same byte range, and
  only if the cached entry carries an entity validator (``ETag`` or ``Last-Modified``) that
  matches the request's ``If-Range`` header, if one was sent.

Multi-range requests, responses with an unknown total length (``Content-Range: bytes 0-99/*``),
and invalid ``Content-Range`` headers are always rejected. Unverified fragments are never
spliced together to synthesize a complete response.
"""

from __future__ import annotations

import re
from email.utils import parsedate_to_datetime
from hashlib import blake2b, sha256
from logging import getLogger
from typing import TYPE_CHECKING, Optional, Tuple

from .._utils import encode

if TYPE_CHECKING:
    from requests import PreparedRequest, Response
    from requests.structures import CaseInsensitiveDict

__all__ = [
    'get_range_cache_key',
    'if_range_matches',
    'is_cacheable_range_response',
    'parse_content_range',
    'parse_range_header',
]

# A single byte-range spec: 'bytes=0-99', 'bytes=100-', or 'bytes=-100'
ByteRange = Tuple[Optional[int], Optional[int]]

RANGE_HEADER_RE = re.compile(r'^\s*bytes\s*=\s*(.+?)\s*$', re.IGNORECASE)
RANGE_SPEC_RE = re.compile(r'^(\d*)-(\d*)$')
CONTENT_RANGE_RE = re.compile(r'^\s*bytes\s+(\d+)-(\d+)/(\d+|\*)\s*$', re.IGNORECASE)

logger = getLogger(__name__)


def parse_range_header(value: Optional[str]) -> Optional[list]:
    """Parse a ``Range`` request header into a list of ``(start, end)`` tuples. Only the ``bytes``
    unit is supported. ``start`` is ``None`` for a suffix range (``bytes=-100``), and ``end`` is
    ``None`` for an open range (``bytes=100-``).

    Returns ``None`` if the header is missing, invalid, or uses a non-bytes unit.
    """
    if not value:
        return None
    match = RANGE_HEADER_RE.match(value)
    if not match:
        return None

    ranges = []
    for spec in match.group(1).split(','):
        spec_match = RANGE_SPEC_RE.match(spec.strip())
        if not spec_match:
            return None
        start, end = spec_match.groups()
        if not start and not end:
            return None
        ranges.append((int(start) if start else None, int(end) if end else None))
    return ranges


def parse_content_range(value: Optional[str]) -> Optional[Tuple[int, int, Optional[int]]]:
    """Parse a ``Content-Range`` response header into ``(start, end, total)``. ``total`` is
    ``None`` if the complete length is unknown (``*``).

    Returns ``None`` if the header is missing or invalid.
    """
    if not value:
        return None
    match = CONTENT_RANGE_RE.match(value)
    if not match:
        return None
    start, end, total = match.groups()
    return (int(start), int(end), int(total) if total != '*' else None)


def get_range_cache_key(cache_key: str, byte_range: ByteRange) -> str:
    """Derive a cache key for a single-range request, qualified by both the base (full-entity)
    cache key and the normalized byte range. This guarantees that partial responses can never
    collide with complete responses or with different ranges of the same resource.
    """
    start, end = byte_range
    range_spec = f'bytes={start if start is not None else ""}-{end if end is not None else ""}'
    try:
        key = blake2b(digest_size=8)
    except (TypeError, ValueError):  # pragma: no cover
        key = sha256()  # type: ignore
    key.update(encode(cache_key))
    key.update(encode(range_spec))
    return key.hexdigest()


def is_cacheable_range_response(request: 'PreparedRequest', response: 'Response') -> bool:
    """Determine if a ``206`` response is safe to cache as an exact single-range response.
    Rejects multi-range requests/responses, unknown complete lengths, invalid ``Content-Range``
    headers, ranges that don't match the request, and responses without an entity validator.
    """
    if response.status_code != 206:
        return False

    # Multipart responses (multiple ranges) are never cached
    content_type = response.headers.get('Content-Type', '')
    if content_type.lower().startswith('multipart/'):
        logger.debug('Not caching multipart range response')
        return False

    # Only exact single ranges are cacheable
    request_ranges = parse_range_header(request.headers.get('Range'))
    if not request_ranges or len(request_ranges) != 1:
        logger.debug('Not caching response to a multi-range or invalid range request')
        return False

    # The Content-Range header must be valid and contain a known complete length
    content_range = parse_content_range(response.headers.get('Content-Range'))
    if content_range is None:
        logger.debug('Not caching range response with missing or invalid Content-Range header')
        return False
    start, end, total = content_range
    if total is None:
        logger.debug('Not caching range response with unknown complete length')
        return False
    if not 0 <= start <= end < total:
        logger.debug(f'Not caching range response with invalid Content-Range: {content_range}')
        return False

    # The response range must exactly match the requested range
    if _resolve_range(request_ranges[0], total) != (start, end):
        logger.debug('Not caching range response that does not match the requested range')
        return False

    # An entity validator is required so a cached partial response can be verified on reuse
    if not (response.headers.get('ETag') or response.headers.get('Last-Modified')):
        logger.debug('Not caching range response without an entity validator')
        return False

    return True


def if_range_matches(if_range: str, headers: 'CaseInsensitiveDict') -> bool:
    """Check an ``If-Range`` request header against cached response headers. The value is either
    an entity tag (compared against ``ETag`` with strong comparison) or an HTTP date (compared
    against ``Last-Modified``).
    """
    if_range = if_range.strip()
    etag = headers.get('ETag')
    last_modified = headers.get('Last-Modified')

    # Weak entity tags never match in a range context (RFC 9110, section 13.1.5)
    if if_range.startswith('W/'):
        return False
    if if_range.startswith('"'):
        return etag is not None and if_range == etag

    # Otherwise, treat the value as an HTTP date
    if last_modified is None:
        return False
    try:
        return parsedate_to_datetime(if_range) == parsedate_to_datetime(last_modified)
    except (TypeError, ValueError):
        return False


def _resolve_range(byte_range: ByteRange, total: int) -> Tuple[int, int]:
    """Resolve a request byte range to absolute ``(start, end)`` positions, given the complete
    length of the resource.
    """
    start, end = byte_range
    if start is None:
        # Suffix range: last `end` bytes
        return (max(total - end, 0), total - 1)  # type: ignore[operator]
    if end is None:
        # Open range: from `start` to the end of the resource
        return (start, total - 1)
    return (start, end)
