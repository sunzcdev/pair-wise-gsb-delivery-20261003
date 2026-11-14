from collections import OrderedDict

import pytest

from w3lib.http import (
    ControlCharacterError,
    EmptyHeaderNameError,
    HeadersDictInput,
    HeadersParseError,
    InvalidHeaderNameError,
    LoneCarriageReturnError,
    MissingColonError,
    ObsFoldError,
    RawHeader,
    RawHeaders,
    TrailingDataError,
    basic_auth_header,
    headers_dict_to_raw,
    headers_raw_to_dict,
    parse_raw_headers,
)


class TestHttp:
    def test_basic_auth_header(self):
        assert (
            basic_auth_header("someuser", "somepass")
            == b"Basic c29tZXVzZXI6c29tZXBhc3M="
        )
        # Check url unsafe encoded header
        assert (
            basic_auth_header("someuser", "@<yu9>&o?Q")
            == b"Basic c29tZXVzZXI6QDx5dTk+Jm8/UQ=="
        )

    def test_basic_auth_header_encoding(self):
        assert (
            basic_auth_header("somæusèr", "sømepäss", encoding="utf8")
            == b"Basic c29tw6Z1c8Oocjpzw7htZXDDpHNz"
        )
        # default encoding (ISO-8859-1)
        assert (
            basic_auth_header("somæusèr", "sømepäss")
            == b"Basic c29t5nVz6HI6c/htZXDkc3M="
        )

    def test_headers_raw_dict_none(self):
        assert headers_raw_to_dict(None) is None
        assert headers_dict_to_raw(None) is None

    def test_headers_raw_dict_empty(self):
        assert headers_raw_to_dict(b"") == {}
        assert headers_dict_to_raw({}) == b""

    def test_headers_raw_to_dict(self):
        raw = b"Content-type: text/html\n\rAccept: gzip\n\r\
                Cache-Control: no-cache\n\rCache-Control: no-store\n\n"
        dct = {
            b"Content-type": [b"text/html"],
            b"Accept": [b"gzip"],
            b"Cache-Control": [b"no-cache", b"no-store"],
        }
        assert headers_raw_to_dict(raw) == dct

    def test_headers_dict_to_raw(self):
        dct = OrderedDict([(b"Content-type", b"text/html"), (b"Accept", b"gzip")])
        assert headers_dict_to_raw(dct) == b"Content-type: text/html\r\nAccept: gzip"

    def test_headers_dict_to_raw_listtuple(self):
        dct: HeadersDictInput = OrderedDict(
            [(b"Content-type", [b"text/html"]), (b"Accept", [b"gzip"])]
        )
        assert headers_dict_to_raw(dct) == b"Content-type: text/html\r\nAccept: gzip"

        dct = OrderedDict([(b"Content-type", (b"text/html",)), (b"Accept", (b"gzip",))])
        assert headers_dict_to_raw(dct) == b"Content-type: text/html\r\nAccept: gzip"

        dct = OrderedDict([(b"Cookie", (b"val001", b"val002")), (b"Accept", b"gzip")])
        assert (
            headers_dict_to_raw(dct)
            == b"Cookie: val001\r\nCookie: val002\r\nAccept: gzip"
        )

        dct = OrderedDict([(b"Cookie", [b"val001", b"val002"]), (b"Accept", b"gzip")])
        assert (
            headers_dict_to_raw(dct)
            == b"Cookie: val001\r\nCookie: val002\r\nAccept: gzip"
        )

    def test_headers_dict_to_raw_wrong_values(self):
        dct: HeadersDictInput = OrderedDict(
            [
                (b"Content-type", 0),
            ]
        )
        assert headers_dict_to_raw(dct) == b""

        dct = OrderedDict([(b"Content-type", 1), (b"Accept", [b"gzip"])])
        assert headers_dict_to_raw(dct) == b"Accept: gzip"


class TestParseRawHeaders:
    def test_none(self):
        assert parse_raw_headers(None) is None

    def test_empty(self):
        result = parse_raw_headers(b"")
        assert isinstance(result, RawHeaders)
        assert len(result) == 0
        assert result.issues == ()

    def test_order_spelling_and_lines(self):
        raw = (
            b"Content-Type: text/html\r\n"
            b"CACHE-control: no-cache\r\n"
            b"Cache-Control: no-store\r\n"
            b"\r\n"
        )
        result = parse_raw_headers(raw)
        assert result.headers == (
            RawHeader(b"Content-Type", b"text/html", 1),
            RawHeader(b"CACHE-control", b"no-cache", 2),
            RawHeader(b"Cache-Control", b"no-store", 3),
        )
        assert result.issues == ()

    def test_lf_line_endings(self):
        result = parse_raw_headers(b"A: 1\nB: 2\n")
        assert result.headers == (RawHeader(b"A", b"1", 1), RawHeader(b"B", b"2", 2))
        assert result.issues == ()

    def test_bare_cr_line_ending(self):
        with pytest.raises(LoneCarriageReturnError):
            parse_raw_headers(b"A: 1\rB: 2")
        result = parse_raw_headers(b"A: 1\rB: 2", strict=False)
        assert result.headers == (RawHeader(b"A", b"1", 1), RawHeader(b"B", b"2", 2))
        assert len(result.issues) == 1
        assert isinstance(result.issues[0], LoneCarriageReturnError)
        assert result.issues[0].line == 1

    def test_missing_colon(self):
        with pytest.raises(MissingColonError) as excinfo:
            parse_raw_headers(b"A: 1\r\nnot a header\r\n")
        assert excinfo.value.line == 2
        assert excinfo.value.raw == b"not a header"
        result = parse_raw_headers(b"A: 1\r\nnot a header\r\n", strict=False)
        assert result.headers == (RawHeader(b"A", b"1", 1),)
        assert len(result.issues) == 1
        assert isinstance(result.issues[0], MissingColonError)

    def test_empty_name(self):
        with pytest.raises(EmptyHeaderNameError):
            parse_raw_headers(b": value")
        result = parse_raw_headers(b": value", strict=False)
        assert result.headers == ()
        assert len(result.issues) == 1
        assert isinstance(result.issues[0], EmptyHeaderNameError)

    def test_invalid_name(self):
        with pytest.raises(InvalidHeaderNameError):
            parse_raw_headers(b"Bad Name: value")
        result = parse_raw_headers(b"Bad Name: value", strict=False)
        # Lenient mode keeps the header with its original spelling.
        assert result.headers == (RawHeader(b"Bad Name", b"value", 1),)
        assert len(result.issues) == 1
        assert isinstance(result.issues[0], InvalidHeaderNameError)

    def test_control_character_in_value(self):
        with pytest.raises(ControlCharacterError):
            parse_raw_headers(b"A: va\x0blue")
        result = parse_raw_headers(b"A: va\x0blue", strict=False)
        assert result.headers == (RawHeader(b"A", b"va\x0blue", 1),)
        assert len(result.issues) == 1
        assert isinstance(result.issues[0], ControlCharacterError)

    def test_tab_in_value_is_allowed(self):
        result = parse_raw_headers(b"A: va\tlue")
        assert result.headers == (RawHeader(b"A", b"va\tlue", 1),)
        assert result.issues == ()

    def test_obs_fold_strict(self):
        with pytest.raises(ObsFoldError):
            parse_raw_headers(b"A: 1\r\n folded\r\n")

    def test_obs_fold_lenient_unfolds(self):
        result = parse_raw_headers(b"A: 1\r\n folded\r\nB: 2\r\n", strict=False)
        assert result.headers == (
            RawHeader(b"A", b"1 folded", 1),
            RawHeader(b"B", b"2", 3),
        )
        assert len(result.issues) == 1
        assert isinstance(result.issues[0], ObsFoldError)
        assert result.issues[0].line == 2

    def test_obs_fold_with_colon_is_not_a_new_header(self):
        result = parse_raw_headers(b"A: 1\r\n B: 2\r\n", strict=False)
        assert result.headers == (RawHeader(b"A", b"1 B: 2", 1),)
        assert result.get_all(b"B") == []

    def test_orphan_continuation_line(self):
        result = parse_raw_headers(b" folded\r\nA: 1\r\n", strict=False)
        assert result.headers == (RawHeader(b"A", b"1", 2),)
        assert len(result.issues) == 1
        assert isinstance(result.issues[0], ObsFoldError)

    def test_trailing_data_after_blank_line(self):
        raw = b"A: 1\r\n\r\n<body>\r\n"
        with pytest.raises(TrailingDataError) as excinfo:
            parse_raw_headers(raw)
        assert excinfo.value.line == 3
        result = parse_raw_headers(raw, strict=False)
        assert result.headers == (RawHeader(b"A", b"1", 1),)
        assert len(result.issues) == 1
        assert isinstance(result.issues[0], TrailingDataError)

    def test_blank_line_ends_headers(self):
        result = parse_raw_headers(b"A: 1\r\n\r\n")
        assert result.headers == (RawHeader(b"A", b"1", 1),)
        assert result.issues == ()

    def test_get_case_insensitive(self):
        result = parse_raw_headers(b"Content-Type: text/html\r\n\r\n")
        assert result.get(b"content-type") == b"text/html"
        assert result.get(b"CONTENT-TYPE") == b"text/html"
        assert result.get(b"accept") is None
        assert result.get(b"accept", b"default") == b"default"

    def test_get_all_multi_value_not_comma_joined(self):
        raw = (
            b"Set-Cookie: a=1; Path=/\r\n"
            b"set-cookie: b=2; Expires=Wed, 01 Jan 2025 00:00:00 GMT\r\n"
            b"\r\n"
        )
        result = parse_raw_headers(raw)
        assert result.get_all(b"SET-cookie") == [
            b"a=1; Path=/",
            b"b=2; Expires=Wed, 01 Jan 2025 00:00:00 GMT",
        ]
        assert result.get(b"set-cookie") == b"a=1; Path=/"
        assert result.get_all(b"missing") == []

    def test_sequence_protocol(self):
        result = parse_raw_headers(b"A: 1\r\nB: 2\r\n")
        assert len(result) == 2
        assert result[0] == RawHeader(b"A", b"1", 1)
        assert list(result) == [RawHeader(b"A", b"1", 1), RawHeader(b"B", b"2", 2)]
        assert result[0:1] == (RawHeader(b"A", b"1", 1),)

    def test_to_raw_round_trip(self):
        result = parse_raw_headers(b"A: 1\r\nB: 2\r\n\r\n")
        assert result.to_raw() == b"A: 1\r\nB: 2"

    def test_to_raw_no_trailing_newline(self):
        result = parse_raw_headers(b"A: 1")
        raw = result.to_raw()
        assert raw == b"A: 1"
        assert not raw.endswith(b"\r\n")

    def test_to_raw_rejects_injection(self):
        headers = RawHeaders([RawHeader(b"A", b"1\r\nInjected: x", 1)])
        with pytest.raises(ValueError, match="control characters"):
            headers.to_raw()
        headers = RawHeaders([RawHeader(b"Bad\r\nName", b"1", 1)])
        with pytest.raises(ValueError, match="invalid header field name"):
            headers.to_raw()

    def test_errors_are_headers_parse_errors(self):
        result = parse_raw_headers(
            b"Bad Name: v\r\nno colon\r\n: empty\r\nA: \x01\r\n cont\r\n\r\nrest",
            strict=False,
        )
        assert all(isinstance(issue, HeadersParseError) for issue in result.issues)
        assert [type(issue) for issue in result.issues] == [
            InvalidHeaderNameError,
            MissingColonError,
            EmptyHeaderNameError,
            ControlCharacterError,
            ObsFoldError,
            TrailingDataError,
        ]
