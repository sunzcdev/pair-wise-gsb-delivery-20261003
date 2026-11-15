"""Internal utilities for generating cache keys that are used for request matching

.. automodsumm:: requests_cache.cache_keys
   :functions-only:
   :nosignatures:
"""

from __future__ import annotations

import json
from contextlib import nullcontext
from hashlib import blake2b, sha256
from logging import getLogger
from typing import (
    TYPE_CHECKING,
    Any,
    Dict,
    Iterable,
    List,
    Mapping,
    MutableMapping,
    Optional,
    Tuple,
    Union,
)
from urllib.parse import parse_qsl, urlencode, urlparse, urlunparse

from requests import Request, Session
from requests.structures import CaseInsensitiveDict
from url_normalize import url_normalize

from ._utils import decode, encode, patch_form_boundary, is_json_content_type

__all__ = [
    'create_key',
    'JSON_BODY_MATCH_FALLBACK',
    'JSON_BODY_MATCH_OFF',
    'JSON_BODY_MATCH_REJECT',
    'JSONBodyNormalizationError',
    'normalize_body',
    'normalize_headers',
    'normalize_json_body_canonical',
    'normalize_request',
    'normalize_params',
    'normalize_url',
]
if TYPE_CHECKING:
    from .models import AnyPreparedRequest, AnyRequest, CachedResponse

# Maximum JSON request body size that will be filtered and normalized
MAX_NORM_BODY_SIZE = 10 * 1024 * 1024

# Explicit JSON body matching modes
JSON_BODY_MATCH_OFF = 'off'
JSON_BODY_MATCH_FALLBACK = 'fallback'
JSON_BODY_MATCH_REJECT = 'reject'
JSON_BODY_MATCH_MODES = (
    JSON_BODY_MATCH_OFF,
    JSON_BODY_MATCH_FALLBACK,
    JSON_BODY_MATCH_REJECT,
)

KVList = List[Tuple[str, str]]
ParamList = Optional[Iterable[str]]
RequestContent = Union[Mapping, str, bytes]

logger = getLogger(__name__)


class JSONBodyNormalizationError(ValueError):
    """Raised when a request body declared as JSON cannot be normalized into an unambiguous
    canonical cache key (e.g. invalid JSON, an unsupported charset, or duplicate object keys).
    """


def _reject_duplicate_keys(pairs: List[Tuple[str, Any]]) -> Dict[str, Any]:
    """``object_pairs_hook`` that rejects objects containing duplicate keys, since their meaning
    is ambiguous and different JSON parsers resolve them differently."""
    result: Dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise JSONBodyNormalizationError(f'Duplicate JSON object key: {key!r}')
        result[key] = value
    return result


def _reject_non_finite(value: str) -> Any:
    """``parse_constant`` hook that rejects the non-standard ``NaN``/``Infinity`` tokens so only
    fully parseable, standards-compliant JSON is used for canonical matching."""
    raise JSONBodyNormalizationError(f'Non-finite JSON number is not supported: {value}')


def _get_content_charset(request: AnyPreparedRequest) -> str:
    """Determine the charset used to decode a JSON request body.

    JSON defaults to UTF-8 (RFC 8259). A charset explicitly declared on the Content-Type header
    takes precedence, and an unrecognized/unsupported charset is treated as a normalization
    failure rather than guessed.
    """
    content_type = request.headers.get('Content-Type', '') or ''
    for param in content_type.split(';')[1:]:
        name, sep, value = param.strip().partition('=')
        if sep and name.lower() == 'charset' and value:
            return value.strip('"').strip("'").lower()
    return 'utf-8'


def _decode_json_body(body: Union[str, bytes], charset: str) -> str:
    """Decode a raw JSON body using a definite charset, rejecting undecodable bytes."""
    if isinstance(body, str):
        return body
    try:
        return body.decode(charset)
    except (LookupError, UnicodeDecodeError) as e:
        raise JSONBodyNormalizationError(
            f'Unable to decode JSON body using charset {charset!r}: {e}'
        ) from e


def _parse_canonical_json(decoded_body: str) -> Any:
    """Parse JSON while preserving integer precision (no float coercion) and rejecting ambiguous
    duplicate keys or non-standard tokens."""
    try:
        return json.loads(
            decoded_body,
            parse_int=int,
            parse_float=float,
            parse_constant=_reject_non_finite,
            object_pairs_hook=_reject_duplicate_keys,
        )
    except JSONBodyNormalizationError:
        raise
    except (TypeError, ValueError) as e:
        raise JSONBodyNormalizationError(f'Invalid JSON request body: {e}') from e


def _filter_json_canonical(data: Any, ignored_parameters: Optional[set]) -> Any:
    """Recursively replace ignored parameters while preserving value types and array order."""
    if isinstance(data, Mapping):
        result = {}
        for key, value in data.items():
            if key in ignored_parameters:
                result[key] = 'REDACTED'
            else:
                result[key] = _filter_json_canonical(value, ignored_parameters)
        return result
    if isinstance(data, list):
        return [_filter_json_canonical(item, ignored_parameters) for item in data]
    return data


def _canonical_json(value: Any) -> str:
    """Serialize parsed JSON into a deterministic, type-preserving canonical string.

    Object keys are sorted (key order is irrelevant), while arrays keep their order. Type tags
    keep values like ``1``, ``"1"`` and ``true`` distinct, and integers are emitted without any
    floating-point conversion.
    """
    if value is None:
        return 'null'
    if isinstance(value, bool):
        return f'bool:{value}'
    if isinstance(value, int):
        return f'int:{value}'
    if isinstance(value, float):
        return f'float:{json.dumps(value)}'
    if isinstance(value, str):
        return f'str:{json.dumps(value, ensure_ascii=False)}'
    if isinstance(value, Mapping):
        # Standard JSON object keys are always strings, so they can be sorted directly
        items = ','.join(
            f'{json.dumps(key, ensure_ascii=False)}:{_canonical_json(item)}'
            for key, item in sorted(value.items())
        )
        return f'obj:{{{items}}}'
    if isinstance(value, list):
        items = ','.join(_canonical_json(item) for item in value)
        return f'arr:[{items}]'
    raise JSONBodyNormalizationError(f'Unsupported JSON value type: {type(value).__name__}')


def normalize_json_body_canonical(
    original_body: Union[str, bytes],
    charset: str,
    ignored_parameters: ParamList = None,
) -> str:
    """Parse a JSON request body and return an unambiguous canonical representation for cache keys.

    Only bodies fully parseable as JSON are accepted. Object key order is ignored, but value
    types and array order are preserved, and duplicate object keys are rejected.
    """
    decoded_body = _decode_json_body(original_body, charset)
    parsed = _parse_canonical_json(decoded_body)
    parsed = _filter_json_canonical(parsed, set(ignored_parameters or []))
    return _canonical_json(parsed)


def create_key(
    request: AnyRequest,
    ignored_parameters: ParamList = None,
    match_headers: Union[ParamList, bool] = False,
    serializer: Any = None,
    content_root_key: Optional[str] = None,
    json_body_match: Optional[str] = JSON_BODY_MATCH_OFF,
    **request_kwargs,
) -> str:
    """Create a normalized cache key based on a request object

    Args:
        request: Request object to generate a cache key from
        ignored_parameters: Request parameters, headers, and/or JSON body params to exclude
        match_headers: Match only the specified headers, or ``True`` to match all headers
        serializer: Serializer name or instance
        content_root_key: root element in the request body to apply ignored_parameters to
        json_body_match: How to match request bodies declared as JSON; one of
            :data:`JSON_BODY_MATCH_OFF`, :data:`JSON_BODY_MATCH_FALLBACK`, or
            :data:`JSON_BODY_MATCH_REJECT`
        request_kwargs: Additional keyword arguments for :py:func:`~requests.request`
    """
    # Normalize and gather all relevant request info to match against
    request = normalize_request(
        request, ignored_parameters, content_root_key, json_body_match
    )
    key_parts = [
        request.method or '',
        request.url,
        request.body or '',
        bool(request_kwargs.get('verify', True)),
        *get_matched_headers(request.headers, match_headers),
        str(serializer),
    ]

    # Generate a hash based on this info
    try:
        key = blake2b(digest_size=8)
    except (TypeError, ValueError):
        # OpenSSL 1.1.0 doesn't support the digest_size parameter for blake2b, resulting in a TypeError
        # On FIPS-compliant systems, blake2b is not compliant algorithm, resulting in a ValueError
        # In both cases, fallback to SHA-256
        key = sha256()  # type: ignore
    for part in key_parts:
        key.update(encode(part))
    return key.hexdigest()


def get_matched_headers(
    headers: CaseInsensitiveDict, match_headers: Union[ParamList, bool]
) -> List[str]:
    """Get only the headers we should match against as a list of ``k=v`` strings, given an optional
    include list.
    """
    if not match_headers:
        return []
    if match_headers is True:
        match_headers = headers
    return [
        f'{k.lower()}={headers[k]}'
        for k in sorted(match_headers, key=lambda x: x.lower())
        if k in headers
    ]


def normalize_request(
    request: AnyRequest,
    ignored_parameters: ParamList = None,
    content_root_key: Optional[str] = None,
    json_body_match: Optional[str] = JSON_BODY_MATCH_OFF,
) -> AnyPreparedRequest:
    """Normalize and remove ignored parameters from request URL, body, and headers.
    This is used for both:

    * Increasing cache hits by generating more precise cache keys
    * Redacting potentially sensitive info from cached requests

    Args:
        request: Request object to normalize
        ignored_parameters: Request parameters, headers, and/or JSON body params to exclude
        content_root_key: root element in the request body to apply ignored_parameters to
        json_body_match: How to match request bodies declared as JSON; see :func:`create_key`
    """
    if isinstance(request, Request):
        # For a multipart POST request that hasn't been prepared, we need to patch the form boundary
        # so the request body will have a consistent hash
        with patch_form_boundary() if request.files else nullcontext():
            norm_request: AnyPreparedRequest = Session().prepare_request(request)
    else:
        norm_request = request.copy()

    norm_request.method = (norm_request.method or '').upper()
    norm_request.url = normalize_url(norm_request.url or '', ignored_parameters)
    norm_request.headers = normalize_headers(norm_request.headers, ignored_parameters)
    norm_request.body = normalize_body(
        norm_request, ignored_parameters, content_root_key, json_body_match
    )
    return norm_request


def normalize_headers(
    headers: MutableMapping[str, str],
    ignored_parameters: ParamList = None,
) -> CaseInsensitiveDict:
    """Sort and filter request headers, and normalize minor variations in multi-value headers"""
    headers = {k: decode(v) for (k, v) in headers.items()}
    if ignored_parameters:
        headers = filter_sort_dict(headers, ignored_parameters)
    for k, v in headers.items():
        if ',' in v:
            values = [v.strip() for v in v.lower().split(',') if v.strip()]
            headers[k] = ', '.join(sorted(values))
    return CaseInsensitiveDict(headers)


def normalize_url(url: str, ignored_parameters: ParamList) -> str:
    """Normalize and filter a URL. This includes request parameters, IDN domains, scheme, host,
    port, etc.
    """
    url = filter_url(url, ignored_parameters)
    return url_normalize(url) or ''


def normalize_body(
    request: AnyPreparedRequest,
    ignored_parameters: ParamList,
    content_root_key: Optional[str] = None,
    json_body_match: Optional[str] = JSON_BODY_MATCH_OFF,
) -> bytes:
    """Normalize and filter a request body if possible, depending on Content-Type"""
    if not request.body:
        return b''
    json_body_match = json_body_match or JSON_BODY_MATCH_OFF

    norm_body: Union[str, bytes] = request.body

    # Handle the case where the request body is a file-like object
    if hasattr(request.body, 'read'):
        norm_body = request.body.read() or b''
        try:
            request.body.seek(0)  # type: ignore[union-attr]
        except AttributeError as e:
            logger.warning(f'Unable to reset original request body: {e}', exc_info=True)

    try:
        content_type = request.headers['Content-Type'].split(';')[0].lower()
    except (AttributeError, KeyError):
        content_type = ''

    if is_json_content_type(content_type):
        if json_body_match == JSON_BODY_MATCH_OFF:
            # Legacy behavior: best-effort normalization that ignores ordering/type distinctions
            norm_body = normalize_json_body(norm_body, ignored_parameters, content_root_key)
        else:
            # Explicit strict JSON equivalence matching
            if len(norm_body) > MAX_NORM_BODY_SIZE:
                error = JSONBodyNormalizationError(
                    f'JSON body exceeds maximum normalized size of {MAX_NORM_BODY_SIZE} bytes'
                )
                if json_body_match == JSON_BODY_MATCH_REJECT:
                    raise error
                logger.debug('Falling back to raw byte body: %s', error)
            else:
                charset = _get_content_charset(request)
                try:
                    norm_body = normalize_json_body_canonical(
                        norm_body, charset, ignored_parameters
                    )
                except JSONBodyNormalizationError as error:
                    if json_body_match == JSON_BODY_MATCH_REJECT:
                        raise
                    logger.debug('Falling back to raw byte body: %s', error)
    elif content_type == 'application/x-www-form-urlencoded':
        norm_body = normalize_params(norm_body, ignored_parameters)

    return encode(norm_body)


def normalize_json_body(
    original_body: Union[str, bytes],
    ignored_parameters: ParamList,
    content_root_key: Optional[str] = None,
) -> Union[str, bytes]:
    """Normalize and filter a request body with serialized JSON data"""
    if len(original_body) <= 2 or len(original_body) > MAX_NORM_BODY_SIZE:
        return original_body

    try:
        body = json.loads(decode(original_body))
        if content_root_key and isinstance(body, dict) and content_root_key in body:
            body[content_root_key] = filter_sort_json(body[content_root_key], ignored_parameters)
        else:
            body = filter_sort_json(body, ignored_parameters)
        return json.dumps(body)
    # If it's invalid JSON, then don't mess with it
    except (AttributeError, TypeError, ValueError):
        logger.debug('Invalid JSON body')
        return original_body


def normalize_params(value: Union[str, bytes], ignored_parameters: ParamList = None) -> str:
    """Normalize and filter urlencoded params from either a URL or request body with form data"""
    value = decode(value)
    params = parse_qsl(value)
    params = filter_sort_multidict(params, ignored_parameters)
    query_str = urlencode(params)

    # parse_qsl doesn't handle key-only params, so add those here
    key_only_params = [k for k in value.split('&') if k and '=' not in k]
    if key_only_params:
        key_only_param_str = '&'.join(sorted(key_only_params))
        query_str = f'{query_str}&{key_only_param_str}' if query_str else key_only_param_str

    return query_str


def redact_response(
    response: CachedResponse,
    ignored_parameters: ParamList,
    json_body_match: Optional[str] = JSON_BODY_MATCH_OFF,
) -> CachedResponse:
    """Redact any ignored parameters (potentially containing sensitive info) from a cached request"""
    if ignored_parameters:
        response.url = filter_url(response.url, ignored_parameters)
        response.request.url = filter_url(response.request.url, ignored_parameters)
        response.headers = CaseInsensitiveDict(
            filter_sort_dict(response.headers, ignored_parameters)
        )
        response.request.headers = CaseInsensitiveDict(
            filter_sort_dict(response.request.headers, ignored_parameters)
        )
        response.request.body = normalize_body(
            response.request, ignored_parameters, json_body_match=json_body_match
        )
    return response


def filter_sort_json(data: Union[List, Mapping], ignored_parameters: ParamList):
    if isinstance(data, Mapping):
        return filter_sort_dict(data, ignored_parameters)
    else:
        return filter_sort_list(data, ignored_parameters)


def filter_sort_dict(
    data: Mapping[str, str],
    ignored_parameters: ParamList = None,
) -> Dict[str, str]:
    # Note: Any ignored_parameters present will have their values replaced instead of removing the
    # parameter, so the cache key will still match whether the parameter was present or not.
    ignored_parameters = set(ignored_parameters or [])
    return {k: ('REDACTED' if k in ignored_parameters else v) for k, v in sorted(data.items())}


def filter_sort_multidict(
    data: KVList,
    ignored_parameters: ParamList = None,
) -> KVList:
    ignored_parameters = set(ignored_parameters or [])
    return [(k, 'REDACTED' if k in ignored_parameters else v) for k, v in sorted(data)]


def filter_sort_list(data: List, ignored_parameters: ParamList = None) -> List:
    if not ignored_parameters:
        return sorted(data)
    return [k for k in sorted(data) if k not in set(ignored_parameters)]


def filter_url(url: str, ignored_parameters: ParamList) -> str:
    """Filter ignored parameters out of a URL"""
    # Strip query params from URL, sort and filter, and reassemble into a complete URL
    url_tokens = urlparse(url)
    return urlunparse(
        (
            url_tokens.scheme,
            url_tokens.netloc,
            url_tokens.path,
            url_tokens.params,
            normalize_params(url_tokens.query, ignored_parameters),
            url_tokens.fragment,
        )
    )
