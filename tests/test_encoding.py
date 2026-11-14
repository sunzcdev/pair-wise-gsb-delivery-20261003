from __future__ import annotations

import codecs
import random
from io import BytesIO
from typing import Any

import pytest

import w3lib.encoding
from w3lib.encoding import (
    EncodingCandidate,
    EncodingCandidateStatus,
    EncodingDiagnosis,
    html_body_declared_encoding,
    html_encoding,
    html_to_unicode,
    http_content_type_encoding,
    read_bom,
    resolve_encoding,
    to_unicode,
)


class TestRequestEncoding:
    utf8_fragments = [
        # Content-Type as meta http-equiv
        b"""<meta http-equiv="content-type" content="text/html;charset=UTF-8" />""",
        b"""\n<meta http-equiv="Content-Type"\ncontent="text/html; charset=utf-8">""",
        b"""<meta http-equiv="Content-Type" content="text/html" charset="utf-8">""",
        b"""<meta http-equiv=Content-Type content="text/html" charset='utf-8'>""",
        b"""<meta http-equiv="Content-Type" content\t=\n"text/html" charset\t="utf-8">""",
        b"""<meta content="text/html; charset=utf-8"\n http-equiv='Content-Type'>""",
        # html5 meta charset
        b"""<meta charset="utf-8">""",
        b"""<meta charset =\n"utf-8">""",
        # xml encoding
        b"""<?xml version="1.0" encoding="utf-8"?>""",
    ]

    def test_bom(self):
        # cjk water character in unicode
        water_unicode = "\u6c34"
        # BOM + water character encoded
        utf16be = b"\xfe\xff\x6c\x34"
        utf16le = b"\xff\xfe\x34\x6c"
        for string in (utf16be, utf16le):
            bom_encoding, bom = read_bom(string)
            assert bom_encoding is not None
            assert bom is not None
            decoded = string[len(bom) :].decode(bom_encoding)
            assert water_unicode == decoded
        # UTF-32 is not a web encoding, and the UTF-32LE BOM starts with the
        # UTF-16LE one
        assert read_bom(b"\x00\x00\xfe\xff\x00\x00\x6c\x34") == (None, None)
        assert read_bom(b"\xff\xfe\x00\x00\x34\x6c\x00\x00") == (
            "utf-16-le",
            b"\xff\xfe",
        )
        # UTF-16 XML without BOM
        assert read_bom('<?xml version="1.0"?>'.encode("utf-16-le")) == (
            "utf-16-le",
            b"",
        )
        # Body without BOM
        enc, bom = read_bom(b"foo")
        assert enc is None
        assert bom is None
        # Empty body
        enc, bom = read_bom(b"")
        assert enc is None
        assert bom is None

    def test_http_encoding_header(self):
        header_value = "Content-Type: text/html; charset=ISO-8859-4"
        extracted = http_content_type_encoding(header_value)
        assert extracted == "iso8859-4"
        assert http_content_type_encoding("something else") is None
        # valid quoted charset values
        # (https://mimesniff.spec.whatwg.org/#parse-a-mime-type)
        assert (
            http_content_type_encoding('Content-Type: text/html; charset="utf-8"')
            == "utf-8"
        )
        assert http_content_type_encoding('text/html;charset="UTF-8"') == "utf-8"
        assert (
            http_content_type_encoding('text/html; Charset="ISO-8859-4"') == "iso8859-4"
        )
        assert http_content_type_encoding("text/html;charset=utf-8") == "utf-8"
        # invalid: whitespace around "=" is not allowed
        assert http_content_type_encoding("text/html; charset = utf-8") is None
        assert http_content_type_encoding('text/html; charset= "utf-8"') is None
        # invalid: single quotes are not HTTP quoted-string
        assert http_content_type_encoding("text/html; charset='utf-8'") is None
        # invalid: incomplete quotes
        assert http_content_type_encoding('text/html; charset="utf-8') is None
        assert http_content_type_encoding('text/html; charset=utf-8"') is None
        # valid: with additional parameters
        assert (
            http_content_type_encoding('text/html; charset="utf-8"; foo=bar') == "utf-8"
        )
        assert (
            http_content_type_encoding('text/html; charset="utf-8" ; foo=bar')
            == "utf-8"
        )
        assert (
            http_content_type_encoding("text/html; charset=utf-8; foo=bar") == "utf-8"
        )
        assert http_content_type_encoding('foo=bar; charset="utf-8"') == "utf-8"
        # invalid: trailing junk after closing quote
        assert http_content_type_encoding('text/html; charset="utf-8"x') is None
        assert http_content_type_encoding('text/html; charset="utf-8"extra') is None
        # invalid: parameter name substring match
        assert http_content_type_encoding('text/html; xcharset="utf-8"') is None
        assert http_content_type_encoding("text/html; mycharset=utf-8") is None
        # invalid: orphan quote in unquoted value
        assert http_content_type_encoding('text/html; charset=utf-8x"') is None
        # a quoted-string is opaque: a charset written inside another
        # parameter's value belongs to that value
        assert (
            http_content_type_encoding(
                'text/html; foo="bar; charset=utf-7;"; charset=utf-8'
            )
            == "utf-8"
        )
        assert http_content_type_encoding('text/html; foo="bar; charset=utf-7"') is None
        assert (
            http_content_type_encoding('text/html; foo="bar; charset=utf-7;"') is None
        )
        assert (
            http_content_type_encoding(
                'text/html; foo="bar; charset=utf-7;"; charset="utf-8"'
            )
            == "utf-8"
        )
        # an invalid charset value is skipped and the scan continues
        assert (
            http_content_type_encoding(
                'text/html; foo="bar; charset=utf-7;"; charset=""; charset=utf-8'
            )
            == "utf-8"
        )
        # the parameter is still found when the header does not end right
        # after it
        assert (
            http_content_type_encoding("text/html;charset=utf-8, text/html") == "utf-8"
        )
        assert (
            http_content_type_encoding('text/html; charset="utf-8", text/html')
            == "utf-8"
        )
        assert (
            http_content_type_encoding("Content-Type: text/html; charset=utf-8\r\n")
            == "utf-8"
        )
        # comma-joined headers with differing essences: the first charset
        # wins, unlike Fetch's extract-a-MIME-type where the last valid MIME
        # type's would
        assert (
            http_content_type_encoding(
                "text/html; charset=utf-8, text/plain; charset=utf-16"
            )
            == "utf-8"
        )

    def test_html_body_declared_encoding(self):
        for fragment in self.utf8_fragments:
            encoding = html_body_declared_encoding(fragment)
            assert encoding == "utf-8", fragment
        assert None is html_body_declared_encoding(b"something else")
        assert (
            html_body_declared_encoding(
                b"""
            <head></head><body>
            <meta charset="utf-8">
        """
            )
            == "utf-8"
        )
        assert (
            html_body_declared_encoding(
                b"""<meta http-equiv="Fake-Content-Type-Header" content="text/html; charset=utf-8">"""
            )
            is None
        )
        assert (
            html_body_declared_encoding(
                b"""<meta httpequiv="ContentType" content="text/html; charset=gbk" />"""
            )
            is None
        )
        # a charset declared inside a comment is not honored, and a commented
        # body tag does not stop the scan
        assert html_body_declared_encoding(b'<!-- <meta charset="utf-7"> -->') is None
        assert (
            html_body_declared_encoding(b'<!-- <body> --><meta charset="utf-8">')
            == "utf-8"
        )

    @pytest.mark.parametrize(
        ("body", "expected"),
        [
            # a "charset=" inside an unrelated attribute value is not a
            # declaration, and neither is the tail of another attribute's name
            (
                b'<meta name="description" content="Set charset=big5 in your editor">',
                None,
            ),
            (b'<meta data-charset="big5">', None),
            (
                b'<meta name="description" content="use charset=big5"><meta charset="utf-8">',
                "utf-8",
            ),
            (
                b'<meta property="og:description" content="How to set charset=windows-1251 on your site"><meta charset="utf-8">',
                "utf-8",
            ),
            # a charset inside a content attribute needs the
            # http-equiv="content-type" pragma
            (b'<meta content="text/html; charset=gbk">', None),
            (b'<meta http-equiv content="text/html; charset=gbk">', None),
            (
                b'<meta http_equiv="content-type" content="text/html; charset=gbk">',
                None,
            ),
            # another pragma is not the content-type one, however its own
            # content value spells "charset="
            (b'<meta http-equiv="refresh" content="0; url=/x?charset=big5">', None),
            (
                b'<meta http-equiv="X-UA-Compatible" content="IE=edge; charset=big5">',
                None,
            ),
            (
                b'<meta http-equiv="Content-Security-Policy" content="default-src /x?charset=big5">',
                None,
            ),
            # only the first content attribute of a tag is read, and an empty
            # charset attribute does not hide it
            (
                b'<meta http-equiv="content-type" content="text/html; charset=gbk" content="text/html; charset=big5">',
                "gbk",
            ),
            (
                b'<meta http-equiv="content-type" charset="" content="text/html; charset=gbk">',
                "gbk",
            ),
            # a charset attribute wins over a content one, wherever it is
            (
                b'<meta http-equiv="content-type" content="text/html; charset=gbk" charset="utf-8">',
                "utf-8",
            ),
            # a quoted ">" does not end the tag, and a quoted tag written
            # inside one neither ends nor stops the scan
            (b'<meta content="a>b" charset="utf-8">', "utf-8"),
            (b'<meta content="<body>" charset="utf-8">', "utf-8"),
            # an empty charset attribute is not a declaration
            (b'<meta charset="">', None),
            (b'<meta charset=""><meta charset="utf-8">', "utf-8"),
            # a quoted "<!--" does not start a comment
            (b'<meta name="a" content="<!--"><meta charset="utf-8">', "utf-8"),
            # a meta tag inside an attribute value of another tag is not one
            (b'<img alt="<meta charset=big5>">', None),
            (b'<img alt="<meta charset=big5>"><meta charset="utf-8">', "utf-8"),
            (b"</p title='<meta charset=big5>'>", None),
            (b'<p title=<meta charset="big5">', None),
            # the text around a comment is not spliced into a tag
            (b'<met<!-- -->a charset="big5">', None),
            (b'<meta charset="big5"<!-- -->>', "big5hkscs"),
            # a comment ends at the first "-->" or "--!>", whose dashes can be
            # those of its "<!--"
            (b'<!--><meta charset="big5">', "big5hkscs"),
            (b'<!---><meta charset="big5">', "big5hkscs"),
            (b'<!----><meta charset="big5">', "big5hkscs"),
            (b'<!-- x --><meta charset="big5">', "big5hkscs"),
            (b'<!-- x --!><meta charset="big5">', "big5hkscs"),
            (b'<!----!><meta charset="big5">', "big5hkscs"),
            (b'<!--!><meta charset="big5">', None),
            (b'<!-- <meta charset="utf-8"> --><meta charset="big5">', "big5hkscs"),
            # an unterminated comment hides everything after it
            (b'<!-- <meta charset="big5">', None),
            # an xml declaration without a usable encoding does not stop the
            # scan
            (b'<?xml version="1.0"?><meta charset="utf-8">', "utf-8"),
            (b'<?xml version="1.0" encoding=""?><meta charset="utf-8">', "utf-8"),
        ],
    )
    def test_html_body_declared_encoding_attributes(
        self, body: bytes, expected: str | None
    ) -> None:
        assert html_body_declared_encoding(body) == expected
        assert html_body_declared_encoding(body.decode("ascii")) == expected

    def test_html_body_declared_encoding_unicode(self):
        # html_body_declared_encoding should work when unicode body is passed
        assert html_body_declared_encoding("something else") is None

        for fragment in self.utf8_fragments:
            encoding = html_body_declared_encoding(fragment.decode("utf8"))
            assert encoding == "utf-8", fragment

        assert (
            html_body_declared_encoding(
                """
            <head></head><body>
            <meta charset="utf-8">
        """
            )
            == "utf-8"
        )
        assert (
            html_body_declared_encoding(
                """<meta http-equiv="Fake-Content-Type-Header" content="text/html; charset=utf-8">"""
            )
            is None
        )


class TestCodecsEncoding:
    def test_resolve_encoding(self):
        assert resolve_encoding("latin1") == "cp1252"
        assert resolve_encoding(" Latin-1") == "cp1252"
        assert resolve_encoding("gb_2312-80") == "gbk"
        assert resolve_encoding("unknown encoding") is None
        # utf-7 is not in the Encoding Standard and enables charset-smuggling,
        # so it must not resolve, however it is spelled.
        for alias in ("utf-7", "utf7", "U7", "unicode-1-1-utf-7", "UTF_7"):
            assert resolve_encoding(alias) is None, alias

    def test_resolve_encoding_utf7_not_smuggled(self):
        # a utf-7 charset from the header or a meta tag is ignored, so the
        # "+ADw-script+AD4-" payload stays inert instead of decoding to markup
        assert http_content_type_encoding("text/html; charset=utf-7") is None
        assert html_body_declared_encoding(b'<meta charset="utf-7">') is None
        enc, body = html_to_unicode("text/html; charset=utf-7", b"+ADw-script+AD4-")
        assert enc != "utf-7"
        assert "<script>" not in body

    @pytest.mark.parametrize(
        "alias",
        [
            # bytes-to-bytes codecs
            "base64",
            "base_64",
            "bz2",
            "hex",
            "quopri",
            "quoted-printable",
            "rot13",
            "ROT_13",
            "uu",
            "zip",
            "zlib",
            # text codecs that cannot decode a page
            "idna",
            "punycode",
            "undefined",
            "unicode-escape",
            "unicode_escape",
            "raw_unicode_escape",
        ],
    )
    def test_resolve_encoding_unsuitable_codec(self, alias: str) -> None:
        """Python codecs that aren't web encodings must not resolve."""
        assert resolve_encoding(alias) is None
        assert http_content_type_encoding(f"text/html; charset={alias}") is None
        assert html_body_declared_encoding(f"<meta charset={alias}>".encode()) is None
        for content_type, body in (
            (f"text/html; charset={alias}", b"abc"),
            (None, f"<meta charset={alias}>abc".encode()),
            (None, f'<?xml version="1.0" encoding="{alias}"?><a/>'.encode()),
        ):
            enc, text = html_to_unicode(content_type, body)
            assert enc == "utf-8"
            assert text.endswith(("abc", "<a/>"))

    def test_resolve_encoding_escape_codec_not_smuggled(self) -> None:
        # the escape codecs would turn the inert "\x3c" text into markup
        for alias in ("unicode-escape", "raw-unicode-escape"):
            enc, body = html_to_unicode(
                f"text/html; charset={alias}", rb"\x3cscript\x3e\u003cscript\u003e"
            )
            assert enc == "utf-8"
            assert "<script>" not in body

    def test_default_encoding_translation(self) -> None:
        with pytest.warns(DeprecationWarning, match="DEFAULT_ENCODING_TRANSLATION"):
            assert w3lib.encoding.DEFAULT_ENCODING_TRANSLATION["latin_1"] == "cp1252"


class TestUnicodeDecoding:
    def test_utf8(self):
        assert to_unicode(b"\xc2\xa3", "utf-8") == "\xa3"

    def test_invalid_utf8(self):
        assert to_unicode(b"\xc2\xc2\xa3", "utf-8") == "\ufffd\xa3"

    @pytest.mark.parametrize("encoding", ["gb18030", "GB18030", "gb18030_2000"])
    @pytest.mark.parametrize(
        ("data", "expected"),
        [
            (b"\x80", "\u20ac"),
            (b"100\x80", "100\u20ac"),
            (b"\xff\x80", "\ufffd\u20ac"),
            (b"\x81\x80", "\u4e90"),
            (b"\xa2\xe3", "\u20ac"),
            (b"\xff", "\ufffd"),
        ],
    )
    def test_gb18030(self, data: bytes, expected: str, encoding: str) -> None:
        assert to_unicode(data, encoding) == expected


def ct(charset: str | None) -> str | None:
    if charset is None:
        return None
    return "Content-Type: text/html; charset=" + charset


def norm_encoding(enc: str) -> str:
    return codecs.lookup(enc).name


class TestHtmlConversion:
    def test_unicode_body(self):
        unicode_string = "\u043a\u0438\u0440\u0438\u043b\u043b\u0438\u0447\u0435\u0441\u043a\u0438\u0439 \u0442\u0435\u043a\u0441\u0442"
        original_string = unicode_string.encode("cp1251")
        _, body_unicode = html_to_unicode(ct("cp1251"), original_string)
        # check body_as_unicode
        assert isinstance(body_unicode, str)
        assert body_unicode == unicode_string

    @staticmethod
    def _assert_encoding(
        content_type: str | None,
        body: bytes,
        expected_encoding: str,
        expected_unicode: str,
    ) -> None:
        assert not isinstance(body, str)
        encoding, body_unicode = html_to_unicode(ct(content_type), body)
        assert isinstance(body_unicode, str)
        assert norm_encoding(encoding) == norm_encoding(expected_encoding)
        assert html_encoding(ct(content_type), body) == encoding

        assert body_unicode == expected_unicode

    def test_content_type_and_conversion(self):
        """Test content type header is interpreted and text converted as
        expected
        """
        self._assert_encoding("utf-8", b"\xc2\xa3", "utf-8", "\xa3")
        # iso-8859-1 is overridden to cp1252
        self._assert_encoding("iso-8859-1", b"\xa3", "cp1252", "\xa3")
        self._assert_encoding("", b"\xc2\xa3", "utf-8", "\xa3")
        self._assert_encoding("none", b"\xc2\xa3", "utf-8", "\xa3")
        self._assert_encoding("gb2312", b"\xa8D", "gbk", "\u2015")
        self._assert_encoding("gbk", b"\xa8D", "gbk", "\u2015")
        self._assert_encoding("big5", b"\xf9\xda", "big5hkscs", "\u6052")

    def test_invalid_utf8_encoded_body_with_valid_utf8_BOM(self):
        # unlike scrapy, the BOM is stripped
        self._assert_encoding(
            "utf-8", b"\xef\xbb\xbfWORD\xe3\xabWORD2", "utf-8", "WORD\ufffdWORD2"
        )
        self._assert_encoding(
            None, b"\xef\xbb\xbfWORD\xe3\xabWORD2", "utf-8", "WORD\ufffdWORD2"
        )

    def test_utf8_unexpected_end_of_data_with_valid_utf8_BOM(self):
        # unlike scrapy, the BOM is stripped
        self._assert_encoding(
            "utf-8", b"\xef\xbb\xbfWORD\xe3\xab", "utf-8", "WORD\ufffd"
        )
        self._assert_encoding(None, b"\xef\xbb\xbfWORD\xe3\xab", "utf-8", "WORD\ufffd")

    def test_replace_wrong_encoding(self):
        """Test invalid chars are replaced properly"""
        _, body_unicode = html_to_unicode(ct("utf-8"), b"PREFIX\xe3\xabSUFFIX")
        # Policy for replacing invalid chars may suffer minor variations but
        # it should always contain the unicode replacement char ('\ufffd')
        assert "\ufffd" in body_unicode, repr(body_unicode)
        assert "PREFIX" in body_unicode, repr(body_unicode)
        assert "SUFFIX" in body_unicode, repr(body_unicode)

        # Do not destroy html tags due to encoding bugs
        _, body_unicode = html_to_unicode(ct("utf-8"), b"\xf0<span>value</span>")
        assert "<span>value</span>" in body_unicode, repr(body_unicode)

    def _assert_encoding_detected(
        self,
        content_type: str | None,
        expected_encoding: str,
        body: bytes,
        **kwargs: Any,
    ) -> None:
        assert not isinstance(body, str)
        encoding, body_unicode = html_to_unicode(ct(content_type), body, **kwargs)
        assert isinstance(body_unicode, str)
        assert norm_encoding(encoding) == norm_encoding(expected_encoding)
        if not kwargs:
            assert html_encoding(ct(content_type), body) == encoding

    def test_BOM(self):
        # utf-16 cases already tested, as is the BOM detection function

        # BOM takes precedence, ahead of the http header
        bom_be_str = codecs.BOM_UTF16_BE + "hi".encode("utf-16-be")
        expected = "hi"
        self._assert_encoding("utf-8", bom_be_str, "utf-16-be", expected)

        # BOM is stripped when present
        bom_utf8_str = codecs.BOM_UTF8 + b"hi"
        self._assert_encoding("utf-8", bom_utf8_str, "utf-8", "hi")
        self._assert_encoding(None, bom_utf8_str, "utf-8", "hi")

    def test_utf16_32(self):
        # tools.ietf.org/html/rfc2781 section 4.3

        # USE BOM and strip it
        bom_be_str = codecs.BOM_UTF16_BE + "hi".encode("utf-16-be")
        self._assert_encoding("utf-16", bom_be_str, "utf-16-be", "hi")
        self._assert_encoding(None, bom_be_str, "utf-16-be", "hi")

        bom_le_str = codecs.BOM_UTF16_LE + "hi".encode("utf-16-le")
        self._assert_encoding("utf-16", bom_le_str, "utf-16-le", "hi")
        self._assert_encoding(None, bom_le_str, "utf-16-le", "hi")

        # UTF-32 is not a web encoding, and the UTF-32LE BOM starts with the
        # UTF-16LE one
        bom_le_str = codecs.BOM_UTF32_LE + "hi".encode("utf-32-le")
        self._assert_encoding("utf-32", bom_le_str, "utf-16-le", "\x00h\x00i\x00")

        # if there is no BOM, utf-16 is little endian
        self._assert_encoding("utf-16", "hi".encode("utf-16-le"), "utf-16-le", "hi")

        # a utf-16 declaration in the body could only be read as ASCII
        encoding, _ = html_to_unicode(None, b'<meta charset="utf-16">')
        assert encoding == "utf-8"

    def test_python_crash(self):
        random.seed(42)
        buf = BytesIO()
        for _ in range(150000):
            buf.write(bytes([random.randint(0, 255)]))  # noqa: S311
        to_unicode(buf.getvalue(), "utf-16-le")
        to_unicode(buf.getvalue(), "utf-16-be")
        to_unicode(buf.getvalue(), "utf-32-le")
        to_unicode(buf.getvalue(), "utf-32-be")

    def test_html_encoding(self):
        # extracting the encoding from raw html is tested elsewhere
        body = b"""blah blah <meta   http-equiv="Content-Type"
            content="text/html; charset=iso-8859-1"> other stuff"""
        self._assert_encoding_detected(None, "cp1252", body)

        # header encoding takes precedence
        self._assert_encoding_detected("utf-8", "utf-8", body)
        # BOM encoding takes precedence
        self._assert_encoding_detected(None, "utf-8", codecs.BOM_UTF8 + body)

    def test_autodetect(self):
        # an undeclared body is UTF-8 if valid, else guessed from its bytes
        self._assert_encoding_detected(None, "utf-8", "日本語のテキスト".encode())
        self._assert_encoding(
            None,
            "日本語のテキストです。".encode("shift_jis"),
            "cp932",
            "日本語のテキストです。",
        )

    @pytest.mark.parametrize(
        ("url", "encoding", "text"),
        [
            (None, "cp866", "\u0412\u0430"),
            ("https://example.com/", "cp866", "\u0412\u0430"),
            ("https://example.jp/", "cp932", "あ"),
        ],
    )
    def test_autodetect_url(self, url, encoding, text):
        body = b"\x82\xa0"
        assert html_encoding(None, body, url=url) == encoding
        assert html_to_unicode(None, body, url=url) == (encoding, text)

    def test_whatwg_decoders(self):
        # EUC-JP as browsers decode it, which Python's codec does not
        self._assert_encoding("euc-jp", b"\xa1\xc1 \xad\xa1", "euc_jp", "～ ①")
        self._assert_encoding(
            "x-user-defined", b"caf\xe9 ok", "x-user-defined", "caf\uf7e9 ok"
        )
        # in the body, x-user-defined declares windows-1252
        self._assert_encoding(
            None,
            b"<meta charset=x-user-defined>caf\xe9 ok",
            "cp1252",
            "<meta charset=x-user-defined>caf\xe9 ok",
        )
        self._assert_encoding("iso-2022-kr", b"caf\xe9 ok", "replacement", "\ufffd")

    def test_deprecated_autodetect(self):
        def asciif(x):
            return "ascii"

        body = b"""<meta charset="utf-8">"""
        with pytest.warns(DeprecationWarning, match="auto_detect_fun"):
            # header encoding takes precedence
            self._assert_encoding_detected(
                "koi8-r", "koi8-r", body, auto_detect_fun=asciif
            )
        with pytest.warns(DeprecationWarning, match="auto_detect_fun"):
            # then body encoding
            self._assert_encoding_detected(None, "utf-8", body, auto_detect_fun=asciif)
        with pytest.warns(DeprecationWarning, match="auto_detect_fun"):
            # if no other encoding, the auto detect encoding is used.
            self._assert_encoding_detected(
                None, "ascii", b"no encoding info", auto_detect_fun=asciif
            )

    def test_default_encoding(self):
        # if no other method available, the default encoding of utf-8 is used
        self._assert_encoding_detected(None, "utf-8", b"no encoding info")
        # this can be overridden
        with pytest.warns(DeprecationWarning, match="default_encoding"):
            self._assert_encoding_detected(
                None, "ascii", b"no encoding info", default_encoding="ascii"
            )

    def test_empty_body(self):
        # if no other method available, the default encoding of utf-8 is used
        self._assert_encoding_detected(None, "utf-8", b"")


def _by_source(diagnosis: EncodingDiagnosis) -> dict[str, EncodingCandidate]:
    return {candidate.source: candidate for candidate in diagnosis.candidates}


class TestHtmlToUnicodeDiagnosis:
    def test_two_tuple_return_is_unchanged(self) -> None:
        result = html_to_unicode(None, b"plain body")
        assert isinstance(result, tuple)
        assert len(result) == 2
        encoding, text = result
        assert encoding == "utf-8"
        assert text == "plain body"
        # the flag can also be passed explicitly
        assert html_to_unicode(None, b"plain body", diagnostic=False) == result

    def test_diagnostic_return_shape(self) -> None:
        result = html_to_unicode(None, b"plain body", diagnostic=True)
        assert len(result) == 3
        encoding, text, diagnosis = result
        assert isinstance(diagnosis, EncodingDiagnosis)
        assert diagnosis.encoding == encoding
        assert text == "plain body"
        # frozen and fully populated
        with pytest.raises((AttributeError, Exception)):
            diagnosis.encoding = "gbk"  # type: ignore[misc]
        assert tuple(c.source for c in diagnosis.candidates) == (
            "http",
            "meta",
            "auto_detect",
            "default",
        )

    def test_every_candidate_has_label_normalized_value_and_reason(self) -> None:
        _, _, diagnosis = html_to_unicode(
            "text/html; charset=ISO-8859-1",
            b'<meta charset="latin1">caf\xe9',
            diagnostic=True,
        )
        for candidate in diagnosis.candidates:
            assert isinstance(candidate, EncodingCandidate)
            assert candidate.reason
            if candidate.label is not None:
                assert candidate.encoding is not None
                assert candidate.encoding == resolve_encoding(candidate.label)
            if candidate.status is EncodingCandidateStatus.SKIPPED:
                assert candidate.status.value == "skipped"
        http = _by_source(diagnosis)["http"]
        assert http.label == "ISO-8859-1"
        assert http.encoding == "cp1252"
        assert http.status is EncodingCandidateStatus.ADOPTED

    def test_aliases_of_same_codec_are_not_conflicts(self) -> None:
        _, _, diagnosis = html_to_unicode(
            "text/html; charset=latin1",
            b'<meta charset="iso-8859-1">caf\xe9',
            diagnostic=True,
        )
        assert diagnosis.conflicts is False
        sources = _by_source(diagnosis)
        assert sources["http"].encoding == sources["meta"].encoding == "cp1252"
        assert sources["http"].status is EncodingCandidateStatus.ADOPTED
        assert sources["meta"].status is EncodingCandidateStatus.UNUSED

    def test_conflicting_sources_are_flagged_and_explained(self) -> None:
        body = b'<meta charset="big5">\xa4\xa4'
        encoding, text, diagnosis = html_to_unicode(
            "text/html; charset=utf-8", body, diagnostic=True
        )
        assert diagnosis.conflicts is True
        sources = _by_source(diagnosis)
        assert sources["http"].encoding == "utf-8"
        assert sources["meta"].encoding == "big5hkscs"
        # the actual choice and the adopted candidate always agree
        adopted = [
            c
            for c in diagnosis.candidates
            if c.status is EncodingCandidateStatus.ADOPTED
        ]
        assert adopted[0].source == "http"
        assert diagnosis.encoding == encoding == adopted[0].encoding == "utf-8"
        # the same body decoded without the diagnostic flag is identical
        plain_encoding, plain_text = html_to_unicode(
            "text/html; charset=utf-8", body
        )
        assert (plain_encoding, plain_text) == (encoding, text)

    def test_invalid_label_does_not_hide_later_valid_source(self) -> None:
        body = b'<meta charset="big5">\xa4\xa4'
        encoding, _, diagnosis = html_to_unicode(
            "text/html; charset=utf-7", body, diagnostic=True
        )
        assert encoding == "big5hkscs"
        assert diagnosis.conflicts is False
        sources = _by_source(diagnosis)
        assert sources["http"].status is EncodingCandidateStatus.SKIPPED
        assert sources["http"].encoding is None
        assert sources["meta"].status is EncodingCandidateStatus.ADOPTED
        assert sources["meta"].encoding == "big5hkscs"

    def test_diagnostic_does_not_change_output(self) -> None:
        cases = [
            (None, b"\xef\xbb\xbfWORD\xe3\xab"),
            ("text/html; charset=utf-8", b"PREFIX\xe3\xabSUFFIX"),
            (None, "日本語のテキスト".encode("shift_jis")),
            ("text/html; charset=iso-2022-kr", b"caf\xe9 ok"),
            (None, b""),
        ]
        for content_type, body in cases:
            plain = html_to_unicode(content_type, body)
            _, text, diagnosis = html_to_unicode(
                content_type, body, diagnostic=True
            )
            assert diagnosis.encoding == plain[0]
            assert text == plain[1]

    def test_diagnostic_calls_callback_once(self) -> None:
        calls = []

        def detector(body: bytes) -> str:
            calls.append(body)
            return "koi8-r"

        with pytest.warns(DeprecationWarning, match="auto_detect_fun"):
            encoding, text, diagnosis = html_to_unicode(
                None,
                b"no encoding info",
                auto_detect_fun=detector,
                diagnostic=True,
            )
        assert len(calls) == 1
        candidate = _by_source(diagnosis)["auto_detect"]
        assert candidate.status is EncodingCandidateStatus.ADOPTED
        assert candidate.encoding == "koi8-r"
        assert encoding == "koi8-r"

        # the plain return form invokes the callback the same number of times
        calls.clear()
        with pytest.warns(DeprecationWarning, match="auto_detect_fun"):
            html_to_unicode(None, b"no encoding info", auto_detect_fun=detector)
        assert len(calls) == 1

    def test_callback_is_not_called_when_earlier_source_wins(self) -> None:
        def boom(body: bytes) -> str:
            raise AssertionError("the callback must not run")

        with pytest.warns(DeprecationWarning, match="auto_detect_fun"):
            _, _, diagnosis = html_to_unicode(
                "text/html; charset=utf-8",
                b"hi",
                auto_detect_fun=boom,
                diagnostic=True,
            )
        candidate = _by_source(diagnosis)["auto_detect"]
        assert candidate.status is EncodingCandidateStatus.SKIPPED
        assert "not called" in candidate.reason

    def test_legacy_callback_invalid_label_is_kept_as_raw_label(self) -> None:
        # The deprecated legacy pipeline adopted any non-None callback label,
        # even one that names no web encoding, and decoded it through
        # to_unicode(); the diagnosis keeps that raw label while showing its
        # normalized codec is unavailable. This preserves the historical
        # behavior rather than silently switching to the default encoding.
        with pytest.warns(DeprecationWarning):
            encoding, _, diagnosis = html_to_unicode(
                None,
                b"no encoding info",
                auto_detect_fun=lambda body: "utf-7",
                default_encoding="ascii",
                diagnostic=True,
            )
        sources = _by_source(diagnosis)
        assert encoding == "utf-7"
        assert diagnosis.encoding == "utf-7"
        assert sources["auto_detect"].label == "utf-7"
        assert sources["auto_detect"].encoding is None
        assert sources["auto_detect"].status is EncodingCandidateStatus.SKIPPED
        assert sources["default"].status is EncodingCandidateStatus.UNUSED

    def test_callback_returning_none_falls_through_to_default(self) -> None:
        with pytest.warns(DeprecationWarning):
            encoding, _, diagnosis = html_to_unicode(
                None,
                b"no encoding info",
                auto_detect_fun=lambda body: None,
                default_encoding="ascii",
                diagnostic=True,
            )
        sources = _by_source(diagnosis)
        assert encoding == "ascii"
        assert sources["auto_detect"].status is EncodingCandidateStatus.SKIPPED
        assert sources["default"].status is EncodingCandidateStatus.ADOPTED
        assert sources["default"].label == "ascii"
        assert sources["default"].encoding == "cp1252"

    def test_default_encoding_candidate_is_recorded(self) -> None:
        with pytest.warns(DeprecationWarning, match="default_encoding"):
            _, _, diagnosis = html_to_unicode(
                None,
                b"no encoding info",
                default_encoding="cp1251",
                diagnostic=True,
            )
        candidate = _by_source(diagnosis)["default"]
        assert candidate.status is EncodingCandidateStatus.ADOPTED
        assert candidate.label == "cp1251"
        assert candidate.encoding == "cp1251"

    def test_bom_candidate_is_adopted_and_recorded(self) -> None:
        body = codecs.BOM_UTF8 + b"hi"
        encoding, text, diagnosis = html_to_unicode(
            "text/html; charset=gbk", body, diagnostic=True
        )
        assert encoding == "utf-8"
        assert text == "hi"
        assert diagnosis.bom == ("utf-8", codecs.BOM_UTF8)
        bom_candidate = diagnosis.candidates[0]
        assert bom_candidate.source == "bom"
        assert bom_candidate.status is EncodingCandidateStatus.ADOPTED
        assert diagnosis.conflicts is False

    def test_bomless_utf16_xml_recognized_with_empty_bom_bytes(self) -> None:
        body = '<?xml version="1.0"?>'.encode("utf-16-le")
        encoding, _, diagnosis = html_to_unicode(None, body, diagnostic=True)
        assert encoding == "utf-16-le"
        assert diagnosis.bom == ("utf-16-le", b"")
        assert "without a BOM" in diagnosis.candidates[0].reason

    @pytest.mark.parametrize(
        "body",
        [
            b"\xef",
            b"\xef\xbb",
            b"\xfe",
            b"\xff",
            b"\x00\x00\xfe",
        ],
    )
    def test_half_bom_short_input_is_explained(self, body: bytes) -> None:
        _, _, diagnosis = html_to_unicode(None, body, diagnostic=True)
        assert diagnosis.bom is None
        assert any("BOM" in note and "truncated" in note for note in diagnosis.notes)

    def test_full_bom_does_not_get_truncation_note(self) -> None:
        _, _, diagnosis = html_to_unicode(
            None, b"\xef\xbb\xbfhi", diagnostic=True
        )
        assert all("truncated" not in note for note in diagnosis.notes)

    def test_empty_body_note_and_default(self) -> None:
        encoding, text, diagnosis = html_to_unicode(None, b"", diagnostic=True)
        assert encoding == "utf-8"
        assert text == ""
        assert any("empty" in note for note in diagnosis.notes)
        candidate = _by_source(diagnosis)["default"]
        assert candidate.status is EncodingCandidateStatus.ADOPTED
        assert candidate.encoding == "utf-8"

    def test_already_decoded_text_note_and_unchanged_output(self) -> None:
        text_in = "already decoded \u2603"
        encoding, text, diagnosis = html_to_unicode(
            None, text_in, diagnostic=True
        )
        assert text is text_in or text == text_in
        assert text == text_in
        assert encoding == "utf-8"
        assert any("already decoded text" in note for note in diagnosis.notes)

    def test_detection_candidate_for_valid_and_invalid_utf8(self) -> None:
        _, _, valid = html_to_unicode(None, b"plain ascii", diagnostic=True)
        valid_candidate = _by_source(valid)["auto_detect"]
        assert valid_candidate.encoding == "utf-8"
        assert valid_candidate.status is EncodingCandidateStatus.ADOPTED

        _, _, invalid = html_to_unicode(
            None, b"\x82\xa0", url="https://example.jp/", diagnostic=True
        )
        invalid_candidate = _by_source(invalid)["auto_detect"]
        assert invalid_candidate.status is EncodingCandidateStatus.ADOPTED
        assert invalid.encoding == "cp932"

    def test_conflicting_legacy_sources(self) -> None:
        with pytest.warns(DeprecationWarning):
            encoding, _, diagnosis = html_to_unicode(
                "text/html; charset=koi8-r",
                b'<meta charset="utf-8">x',
                auto_detect_fun=lambda body: "cp1251",
                diagnostic=True,
            )
        assert encoding == "koi8-r"
        assert diagnosis.conflicts is True
        sources = _by_source(diagnosis)
        assert sources["http"].status is EncodingCandidateStatus.ADOPTED
        assert sources["meta"].status is EncodingCandidateStatus.UNUSED
        assert sources["auto_detect"].status is EncodingCandidateStatus.SKIPPED

    def test_status_values_are_strings(self) -> None:
        _, _, diagnosis = html_to_unicode(None, b"x", diagnostic=True)
        for candidate in diagnosis.candidates:
            assert candidate.status.value in {"adopted", "skipped", "unused"}
