import httpcore
from httpcore._utils import parse_keep_alive_header


def test_parse_keep_alive_header_timeout_and_max():
    headers = [(b"Keep-Alive", b"timeout=5, max=100")]
    assert parse_keep_alive_header(headers) == httpcore.KeepAliveHint(
        timeout=5.0, max_requests=100
    )


def test_parse_keep_alive_header_each_parameter_independently():
    assert parse_keep_alive_header([(b"Keep-Alive", b"timeout=15")]) == (
        httpcore.KeepAliveHint(timeout=15.0, max_requests=None)
    )
    assert parse_keep_alive_header([(b"Keep-Alive", b"max=2")]) == (
        httpcore.KeepAliveHint(timeout=None, max_requests=2)
    )


def test_parse_keep_alive_header_is_case_insensitive():
    headers = [(b"keep-alive", b"Timeout=5"), (b"KEEP-ALIVE", b"MAX=10")]
    assert parse_keep_alive_header(headers) == httpcore.KeepAliveHint(
        timeout=5.0, max_requests=10
    )


def test_parse_keep_alive_header_ignores_unknown_parameters():
    headers = [(b"Keep-Alive", b"timeout=5, foo=bar, max=3")]
    assert parse_keep_alive_header(headers) == httpcore.KeepAliveHint(
        timeout=5.0, max_requests=3
    )


def test_parse_keep_alive_header_tolerates_whitespace():
    headers = [(b"Keep-Alive", b" timeout = 7 , max = 4 ")]
    assert parse_keep_alive_header(headers) == httpcore.KeepAliveHint(
        timeout=7.0, max_requests=4
    )


def test_parse_keep_alive_header_unparseable_falls_back():
    # A malformed hint must not raise; it is simply dropped.
    headers = [(b"Keep-Alive", b"timeout=soon, max=lots")]
    assert parse_keep_alive_header(headers) == httpcore.KeepAliveHint()


def test_parse_keep_alive_header_partially_unparseable():
    headers = [(b"Keep-Alive", b"timeout=soon, max=4")]
    assert parse_keep_alive_header(headers) == httpcore.KeepAliveHint(
        timeout=None, max_requests=4
    )


def test_parse_keep_alive_header_missing_or_empty():
    assert parse_keep_alive_header([(b"Keep-Alive", b"")]) == httpcore.KeepAliveHint()
    assert parse_keep_alive_header([(b"Content-Length", b"0")]) == (
        httpcore.KeepAliveHint()
    )


def test_parse_keep_alive_header_multiple_headers_use_strictest():
    headers = [
        (b"Keep-Alive", b"timeout=10, max=5"),
        (b"Keep-Alive", b"timeout=3, max=8"),
    ]
    assert parse_keep_alive_header(headers) == httpcore.KeepAliveHint(
        timeout=3.0, max_requests=5
    )


def test_parse_keep_alive_header_rejects_negative_and_non_finite():
    assert parse_keep_alive_header([(b"Keep-Alive", b"timeout=-1, max=-2")]) == (
        httpcore.KeepAliveHint()
    )
    for raw in (b"nan", b"inf", b"-inf", b"infinity"):
        headers = [(b"Keep-Alive", b"timeout=" + raw)]
        assert parse_keep_alive_header(headers) == httpcore.KeepAliveHint()


def test_parse_keep_alive_header_timeout_accepts_floats():
    headers = [(b"Keep-Alive", b"timeout=2.5")]
    assert parse_keep_alive_header(headers) == httpcore.KeepAliveHint(
        timeout=2.5, max_requests=None
    )
