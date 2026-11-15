"""Tests for explicit, type- and order-preserving JSON request body matching.

These tests do not touch the filesystem; cache keys are generated directly from
:class:`requests.Request` objects.
"""

import pytest
from requests import Request

from requests_cache.cache_keys import (
    JSON_BODY_MATCH_FALLBACK,
    JSON_BODY_MATCH_OFF,
    JSON_BODY_MATCH_REJECT,
    JSONBodyNormalizationError,
    create_key,
    normalize_json_body_canonical,
    normalize_request,
)


def _post(body, content_type="application/json", mode=JSON_BODY_MATCH_REJECT):
    request = Request(
        "POST",
        "https://example.com/api",
        data=body,
        headers={"Content-Type": content_type},
    )
    return create_key(request, json_body_match=mode)


def test_object_key_order_is_ignored():
    assert _post(b'{"a":1,"b":2}') == _post(b'{"b":2,"a":1}')


@pytest.mark.parametrize(
    "left,right",
    [
        (b"1", b'"1"'),
        (b"1", b"true"),
        (b"false", b'""'),
        (b"null", b"0"),
        (b"1.0", b"1"),
    ],
)
def test_value_types_are_distinct(left, right):
    assert _post(left) != _post(right)


def test_array_order_is_preserved():
    assert _post(b"[1,2,3]") != _post(b"[3,2,1]")
    # But equivalent arrays still match
    assert _post(b'{"items":[1,2,3]}') == _post(b'{"items":[1,2,3]}')


def test_nested_structures_preserve_order_and_types():
    left = b'{"users":[{"id":1,"active":true},{"id":2,"active":false}]}'
    reordered = b'{"users":[{"active":true,"id":1},{"active":false,"id":2}]}'
    assert _post(left) == _post(reordered)
    swapped = b'{"users":[{"id":2,"active":false},{"id":1,"active":true}]}'
    assert _post(left) != _post(swapped)


def test_large_integers_keep_full_precision():
    big = 123456789012345678901234567890
    canonical = normalize_json_body_canonical(str(big).encode(), "utf-8")
    assert canonical == f"int:{big}"
    # A large int must never equal its float approximation
    assert _post(str(big).encode()) != _post(str(float(big)).encode())


def test_duplicate_object_keys_are_rejected():
    with pytest.raises(JSONBodyNormalizationError):
        _post(b'{"a":1,"a":2}', mode=JSON_BODY_MATCH_REJECT)
    with pytest.raises(JSONBodyNormalizationError):
        normalize_json_body_canonical(b'{"nested":{"x":1,"x":2}}', "utf-8")


def test_non_finite_numbers_are_rejected():
    with pytest.raises(JSONBodyNormalizationError):
        normalize_json_body_canonical(b"NaN", "utf-8")
    with pytest.raises(JSONBodyNormalizationError):
        normalize_json_body_canonical(b'{"v":Infinity}', "utf-8")


def test_floats_and_unicode_are_preserved():
    canonical = normalize_json_body_canonical(
        '{"f":1.0,"u":"é"}'.encode("utf-8"), "utf-8"
    )
    assert "float:1.0" in canonical
    assert "é" in canonical


def test_invalid_json_reject_mode_raises():
    with pytest.raises(JSONBodyNormalizationError):
        _post(b"not json", mode=JSON_BODY_MATCH_REJECT)


def test_invalid_json_fallback_uses_raw_bytes():
    invalid = b"not json"
    fallback_key = _post(invalid, mode=JSON_BODY_MATCH_FALLBACK)
    raw_key = create_key(
        Request(
            "POST",
            "https://example.com/api",
            data=invalid,
            headers={"Content-Type": "application/octet-stream"},
        )
    )
    assert fallback_key == raw_key


def test_fallback_still_canonicalizes_valid_json():
    assert _post(b'{"a":1}', mode=JSON_BODY_MATCH_FALLBACK) == _post(
        b'{"a": 1}', mode=JSON_BODY_MATCH_FALLBACK
    )


def test_fallback_uses_raw_bytes_for_duplicate_keys():
    duplicate = b'{"a":1,"a":2}'
    assert _post(duplicate, mode=JSON_BODY_MATCH_FALLBACK) == _post(
        duplicate, mode=JSON_BODY_MATCH_FALLBACK
    )
    assert _post(duplicate, mode=JSON_BODY_MATCH_FALLBACK) != _post(
        b'{"a":2,"a":1}', mode=JSON_BODY_MATCH_FALLBACK
    )


def test_only_declared_json_content_types_are_normalized():
    plain = create_key(
        Request(
            "POST",
            "https://example.com/api",
            data=b'{"a":1,"b":2}',
            headers={"Content-Type": "text/plain"},
        ),
        json_body_match=JSON_BODY_MATCH_REJECT,
    )
    reordered = create_key(
        Request(
            "POST",
            "https://example.com/api",
            data=b'{"b":2,"a":1}',
            headers={"Content-Type": "text/plain"},
        ),
        json_body_match=JSON_BODY_MATCH_REJECT,
    )
    assert plain != reordered


@pytest.mark.parametrize(
    "content_type",
    [
        "application/json",
        "application/json; charset=utf-8",
        "application/vnd.api+json; charset=utf-8",
    ],
)
def test_json_content_type_variants_are_supported(content_type):
    assert _post(b'{"a":1}', content_type=content_type) == _post(
        b'{"a":1}', content_type=content_type
    )


def test_charset_is_respected_and_undecodable_bytes_fail():
    encoded = '{"a":"é"}'.encode("utf-16")
    request = Request(
        "POST",
        "https://example.com/api",
        data=encoded,
        headers={"Content-Type": "application/json; charset=utf-16"},
    )
    key_utf16 = create_key(request, json_body_match=JSON_BODY_MATCH_REJECT)

    utf8_request = Request(
        "POST",
        "https://example.com/api",
        data='{"a":"é"}'.encode("utf-8"),
        headers={"Content-Type": "application/json; charset=utf-8"},
    )
    key_utf8 = create_key(utf8_request, json_body_match=JSON_BODY_MATCH_REJECT)
    assert key_utf16 == key_utf8

    with pytest.raises(JSONBodyNormalizationError):
        normalize_json_body_canonical(encoded, "utf-8")
    with pytest.raises(JSONBodyNormalizationError):
        normalize_json_body_canonical(b'{"a":1}', "not-a-real-charset")


def test_ignored_parameters_are_redacted_recursively():
    request = Request(
        "POST",
        "https://example.com/api",
        data=b'{"token":"a","nested":{"token":"b"},"arr":[1,2]}',
        headers={"Content-Type": "application/json"},
    )
    normalized = normalize_request(
        request, ignored_parameters=["token"], json_body_match=JSON_BODY_MATCH_REJECT
    )
    assert b"str:%22REDACTED%22" in normalized.body or b'REDACTED' in normalized.body
    assert b'"a"' not in normalized.body and b'"b"' not in normalized.body
    # Array order remains intact
    assert b"arr:[int:1,int:2]" in normalized.body


def test_default_off_mode_keeps_legacy_behavior():
    # Legacy normalization sorts arrays, so reordered arrays still match
    request_a = Request(
        "POST",
        "https://example.com/api",
        json=[3, 1, 2],
        headers={"Content-Type": "application/json"},
    )
    request_b = Request(
        "POST",
        "https://example.com/api",
        json=[2, 1, 3],
        headers={"Content-Type": "application/json"},
    )
    assert create_key(request_a, json_body_match=JSON_BODY_MATCH_OFF) == create_key(
        request_b, json_body_match=JSON_BODY_MATCH_OFF
    )


def test_oversized_body_reject_mode_raises():
    from requests_cache.cache_keys import MAX_NORM_BODY_SIZE

    oversized = b'{"pad":"' + b"0" * MAX_NORM_BODY_SIZE + b'"}'
    with pytest.raises(JSONBodyNormalizationError):
        _post(oversized, mode=JSON_BODY_MATCH_REJECT)


def test_oversized_body_fallback_keeps_raw_bytes():
    from requests_cache.cache_keys import MAX_NORM_BODY_SIZE

    oversized = b'{"pad":"' + b"0" * MAX_NORM_BODY_SIZE + b'"}'
    request = Request(
        "POST",
        "https://example.com/api",
        data=oversized,
        headers={"Content-Type": "application/json"},
    )
    normalized = normalize_request(request, json_body_match=JSON_BODY_MATCH_FALLBACK)
    assert normalized.body == oversized
