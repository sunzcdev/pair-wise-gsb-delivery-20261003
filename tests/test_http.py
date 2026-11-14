import gzip
import io
import zlib

import pytest
import requests
import responses

import feedparser.http
from feedparser.exceptions import BodyTooLargeError, ContentDecodingError

FEED_XML = b"""<?xml version="1.0" encoding="utf-8"?>
<rss version="2.0">
    <channel>
        <title>Example feed</title>
        <item><title>Example item</title></item>
    </channel>
</rss>
"""


def gzip_compress(data):
    return gzip.compress(data)


def zlib_compress(data):
    return zlib.compress(data)


def raw_deflate_compress(data):
    compressor = zlib.compressobj(9, zlib.DEFLATED, -zlib.MAX_WBITS)
    return compressor.compress(data) + compressor.flush()


def test_get_default():
    responses.get("http://example.com/default", body=FEED_XML)
    result = {}
    data = feedparser.http.get("http://example.com/default", result)
    assert data == FEED_XML
    assert result["status"] == 200
    assert result["href"] == "http://example.com/default"
    assert "bozo" not in result


def test_get_default_gzip():
    responses.get(
        "http://example.com/default-gzip",
        body=gzip_compress(FEED_XML),
        headers={"Content-Encoding": "gzip"},
    )
    result = {}
    data = feedparser.http.get("http://example.com/default-gzip", result)
    assert data == FEED_XML
    assert "bozo" not in result


@pytest.mark.parametrize(
    "compress",
    (gzip_compress, zlib_compress, raw_deflate_compress),
    ids=("gzip", "zlib", "raw-deflate"),
)
def test_get_size_limit_ok(compress):
    content_encoding = "gzip" if compress is gzip_compress else "deflate"
    responses.get(
        "http://example.com/limited-ok",
        body=compress(FEED_XML),
        headers={"Content-Encoding": content_encoding},
    )
    result = {}
    data = feedparser.http.get(
        "http://example.com/limited-ok", result, size_limit=len(FEED_XML)
    )
    assert data == FEED_XML
    assert "bozo" not in result


def test_get_size_limit_exceeded():
    # A tiny compressed response decodes to a body larger than the limit.
    responses.get(
        "http://example.com/limited-exceeded",
        body=gzip_compress(FEED_XML * 100),
        headers={"Content-Encoding": "gzip"},
    )
    result = {}
    data = feedparser.http.get(
        "http://example.com/limited-exceeded", result, size_limit=len(FEED_XML)
    )
    # The partially read body is not returned as if it were complete.
    assert data == b""
    assert result["bozo"] is True
    assert isinstance(result["bozo_exception"], BodyTooLargeError)


def test_get_size_limit_chunked_without_content_length():
    # The limit is enforced on actual bytes, not on response headers.
    responses.get(
        "http://example.com/limited-chunked",
        body=gzip_compress(FEED_XML * 100),
        headers={
            "Content-Encoding": "gzip",
            "Transfer-Encoding": "chunked",
        },
    )
    result = {}
    data = feedparser.http.get(
        "http://example.com/limited-chunked", result, size_limit=len(FEED_XML)
    )
    assert data == b""
    assert result["bozo"] is True
    assert isinstance(result["bozo_exception"], BodyTooLargeError)


def test_get_size_limit_without_content_length():
    responses.get("http://example.com/limited-no-length", body=FEED_XML * 100)
    result = {}
    data = feedparser.http.get(
        "http://example.com/limited-no-length", result, size_limit=len(FEED_XML)
    )
    assert data == b""
    assert result["bozo"] is True
    assert isinstance(result["bozo_exception"], BodyTooLargeError)


def test_get_corrupt_gzip():
    responses.get(
        "http://example.com/corrupt-gzip",
        body=b"this is not gzip data",
        headers={"Content-Encoding": "gzip"},
    )
    result = {}
    data = feedparser.http.get(
        "http://example.com/corrupt-gzip", result, size_limit=len(FEED_XML)
    )
    assert data == b""
    assert result["bozo"] is True
    assert isinstance(result["bozo_exception"], ContentDecodingError)


def test_get_truncated_gzip():
    responses.get(
        "http://example.com/truncated-gzip",
        body=gzip_compress(FEED_XML)[:10],
        headers={"Content-Encoding": "gzip"},
    )
    result = {}
    data = feedparser.http.get(
        "http://example.com/truncated-gzip", result, size_limit=len(FEED_XML)
    )
    assert data == b""
    assert result["bozo"] is True
    assert isinstance(result["bozo_exception"], ContentDecodingError)


def test_get_interrupted_request():
    responses.get(
        "http://example.com/interrupted-request",
        body=requests.ConnectionError("connection refused"),
    )
    result = {}
    data = feedparser.http.get("http://example.com/interrupted-request", result)
    assert data == b""
    assert result["bozo"] is True
    assert isinstance(result["bozo_exception"], requests.ConnectionError)


def test_get_interrupted_body():
    # The server promises more bytes than it sends.
    responses.get(
        "http://example.com/interrupted-body",
        body=FEED_XML[:50],
        headers={"Content-Length": str(len(FEED_XML))},
    )
    result = {}
    data = feedparser.http.get(
        "http://example.com/interrupted-body", result, size_limit=len(FEED_XML)
    )
    assert data == b""
    assert result["bozo"] is True
    assert isinstance(result["bozo_exception"], requests.RequestException)
    assert not isinstance(
        result["bozo_exception"], (BodyTooLargeError, ContentDecodingError)
    )


@pytest.fixture
def track_responses():
    """Wrap a session's get() to record the responses it returns."""

    def track(session):
        responses_seen = []
        original_get = session.get

        def tracking_get(*args, **kwargs):
            response = original_get(*args, **kwargs)
            responses_seen.append(response)
            return response

        session.get = tracking_get
        return responses_seen

    return track


def test_get_custom_session_not_closed(track_responses):
    responses.get("http://example.com/session-1", body=FEED_XML)
    responses.get("http://example.com/session-2", body=FEED_XML)
    session = requests.Session()
    try:
        responses_seen = track_responses(session)
        result = {}
        data = feedparser.http.get(
            "http://example.com/session-1",
            result,
            session=session,
            size_limit=len(FEED_XML),
        )
        assert data == FEED_XML
        # The response is closed...
        assert responses_seen[0].raw.closed
        # ...but the caller-held session remains usable.
        result = {}
        data = feedparser.http.get(
            "http://example.com/session-2", result, session=session
        )
        assert data == FEED_XML
    finally:
        session.close()


def test_get_response_closed_on_success(track_responses):
    responses.get("http://example.com/closed-success", body=FEED_XML)
    session = requests.Session()
    try:
        responses_seen = track_responses(session)
        feedparser.http.get(
            "http://example.com/closed-success",
            {},
            session=session,
            size_limit=len(FEED_XML),
        )
        assert responses_seen[0].raw.closed
    finally:
        session.close()


def test_get_response_closed_on_limit_exceeded(track_responses):
    responses.get(
        "http://example.com/closed-exceeded",
        body=gzip_compress(FEED_XML * 100),
        headers={"Content-Encoding": "gzip"},
    )
    session = requests.Session()
    try:
        responses_seen = track_responses(session)
        feedparser.http.get(
            "http://example.com/closed-exceeded",
            {},
            session=session,
            size_limit=len(FEED_XML),
        )
        assert responses_seen[0].raw.closed
    finally:
        session.close()


def test_get_response_closed_on_corrupt_body(track_responses):
    responses.get(
        "http://example.com/closed-corrupt",
        body=b"this is not gzip data",
        headers={"Content-Encoding": "gzip"},
    )
    session = requests.Session()
    try:
        responses_seen = track_responses(session)
        feedparser.http.get(
            "http://example.com/closed-corrupt",
            {},
            session=session,
            size_limit=len(FEED_XML),
        )
        assert responses_seen[0].raw.closed
    finally:
        session.close()


def test_get_response_closed_on_interrupted_body(track_responses):
    responses.get(
        "http://example.com/closed-interrupted",
        body=FEED_XML[:50],
        headers={"Content-Length": str(len(FEED_XML))},
    )
    session = requests.Session()
    try:
        responses_seen = track_responses(session)
        feedparser.http.get(
            "http://example.com/closed-interrupted",
            {},
            session=session,
            size_limit=len(FEED_XML),
        )
        assert responses_seen[0].raw.closed
    finally:
        session.close()


class FakeRaw:
    def __init__(self, data):
        self.file = io.BytesIO(data)

    def read(self, amt=-1, decode_content=False):
        return self.file.read(amt)


class FakeResponse:
    def __init__(self, data, content_encoding=""):
        self.headers = {"Content-Encoding": content_encoding}
        self.raw = FakeRaw(data)


def test_body_budget_records_received_and_decoded():
    compressed = gzip_compress(FEED_XML)
    budget = feedparser.http.BodyBudget()
    data = feedparser.http._read_body(
        FakeResponse(compressed, "gzip"),
        budget,
    )
    assert data == FEED_XML
    assert budget.received == len(compressed)
    assert budget.decoded == len(FEED_XML)


def test_body_budget_unlimited():
    budget = feedparser.http.BodyBudget()
    budget.add_received(10)
    budget.add_decoded(10**9)
    assert budget.received == 10
    assert budget.decoded == 10**9
