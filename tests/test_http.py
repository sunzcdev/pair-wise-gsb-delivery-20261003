from collections import OrderedDict

from w3lib.http import (
    HeadersDictInput,
    Link,
    LinkHeaderError,
    basic_auth_header,
    headers_dict_to_raw,
    headers_raw_to_dict,
    parse_link_headers,
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


class TestParseLinkHeaders:
    def test_empty(self):
        result = parse_link_headers([])
        assert result.links == []
        assert result.errors == []

    def test_single_link(self):
        result = parse_link_headers(
            [b'<https://example.com/page/2>; rel=next; title="Page 2"']
        )
        assert result.errors == []
        assert result.links == [
            Link(
                url="https://example.com/page/2",
                params={"rel": "next", "title": "Page 2"},
            )
        ]
        assert result.links[0].rels == ("next",)

    def test_multiple_headers_keep_order(self):
        result = parse_link_headers(
            [
                b"<https://example.com/1>; rel=next",
                b"<https://example.com/0>; rel=prev",
                b"<https://example.com/fr>; rel=alternate",
            ]
        )
        assert result.errors == []
        assert [link.url for link in result.links] == [
            "https://example.com/1",
            "https://example.com/0",
            "https://example.com/fr",
        ]
        assert [link.rels for link in result.links] == [
            ("next",),
            ("prev",),
            ("alternate",),
        ]

    def test_multiple_links_in_one_header(self):
        result = parse_link_headers(
            [b"<https://example.com/2>; rel=next, <https://example.com/0>; rel=prev"]
        )
        assert result.errors == []
        assert [link.url for link in result.links] == [
            "https://example.com/2",
            "https://example.com/0",
        ]

    def test_multiple_rel_values(self):
        result = parse_link_headers([b'<https://example.com/>; rel="next alternate"'])
        assert result.errors == []
        assert result.links[0].params["rel"] == "next alternate"
        assert result.links[0].rels == ("next", "alternate")

    def test_quoted_commas_semicolons_and_escapes(self):
        result = parse_link_headers(
            [
                b'<https://example.com/>; rel=next; title="a, b; c", '
                b'<https://example.com/2>; rel=prev; title="say \\"hi\\""'
            ]
        )
        assert result.errors == []
        assert [link.url for link in result.links] == [
            "https://example.com/",
            "https://example.com/2",
        ]
        assert result.links[0].params["title"] == "a, b; c"
        assert result.links[1].params["title"] == 'say "hi"'

    def test_escaped_backslash(self):
        result = parse_link_headers([b'<https://example.com/>; rel=next; title="a\\\\b"'])
        assert result.errors == []
        assert result.links[0].params["title"] == "a\\b"

    def test_valueless_and_uppercase_params(self):
        result = parse_link_headers([b"<https://example.com/>; REL=next; FLAG"])
        assert result.errors == []
        assert result.links[0].params == {"rel": "next", "flag": ""}
        assert result.links[0].rels == ("next",)

    def test_relative_target_with_base_url(self):
        result = parse_link_headers(
            [b"</page/2>; rel=next, <../0>; rel=prev"],
            base_url="https://example.com/page/1",
        )
        assert result.errors == []
        assert [link.url for link in result.links] == [
            "https://example.com/page/2",
            "https://example.com/0",
        ]

    def test_relative_target_without_base_url(self):
        result = parse_link_headers([b"</page/2>; rel=next"])
        assert result.errors == []
        assert result.links[0].url == "/page/2"

    def test_base_url_as_bytes(self):
        result = parse_link_headers(
            [b"</page/2>; rel=next"], base_url=b"https://example.com/page/1"
        )
        assert result.links[0].url == "https://example.com/page/2"

    def test_str_headers(self):
        result = parse_link_headers(["<https://example.com/>; rel=next"])
        assert result.errors == []
        assert result.links[0].url == "https://example.com/"

    def test_duplicate_param_is_an_error(self):
        result = parse_link_headers(
            [
                b"<https://example.com/dup>; rel=next; rel=prev, "
                b"<https://example.com/ok>; rel=next"
            ]
        )
        # The broken link-value is reported and excluded, the valid one is
        # still returned.
        assert [link.url for link in result.links] == ["https://example.com/ok"]
        assert len(result.errors) == 1
        assert isinstance(result.errors[0], LinkHeaderError)
        assert "duplicate link parameter 'rel'" in str(result.errors[0])

    def test_broken_items_are_reported_and_skipped(self):
        result = parse_link_headers(
            [
                b"not-a-link, "
                b"<https://example.com/unclosed; rel=next, "
                b"<https://example.com/ok>; rel=next"
            ]
        )
        assert [link.url for link in result.links] == ["https://example.com/ok"]
        assert len(result.errors) == 2
        assert all(isinstance(error, LinkHeaderError) for error in result.errors)

    def test_unterminated_quoted_string(self):
        result = parse_link_headers(
            [b'<https://example.com/ok>; rel=next, <https://example.com/x>; title="oops']
        )
        assert [link.url for link in result.links] == ["https://example.com/ok"]
        assert len(result.errors) == 1
        assert "unterminated quoted string" in str(result.errors[0])

    def test_invalid_param_value(self):
        result = parse_link_headers([b"<https://example.com/>; rel=next; title=not a token"])
        assert result.links == []
        assert len(result.errors) == 1

    def test_empty_list_elements_are_ignored(self):
        result = parse_link_headers([b"<https://example.com/>; rel=next, ,"])
        assert result.errors == []
        assert len(result.links) == 1
