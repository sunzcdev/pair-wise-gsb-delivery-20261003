"""
Functions for handling encoding of web pages
"""

from __future__ import annotations

import re
import warnings
from dataclasses import dataclass, field
from enum import Enum
from functools import lru_cache
from typing import TYPE_CHECKING, Any, Literal, TypeAlias, overload, cast

from frostwork.encoding import (
    autodetect_encoding,
    bom_encoding,
    codec_name,
    declared_encoding,
    decode,
    detect_encoding,
)

if TYPE_CHECKING:
    from collections.abc import Callable

EncodingSource: TypeAlias = Literal["bom", "http", "meta", "auto_detect", "default"]


class EncodingCandidateStatus(str, Enum):
    """What happened to an encoding candidate while resolving the encoding."""

    ADOPTED = "adopted"
    """The candidate supplied the encoding the body was decoded with."""

    SKIPPED = "skipped"
    """The candidate was not usable, e.g. its label named no web encoding."""

    UNUSED = "unused"
    """The candidate was usable but an earlier source had already been
    adopted, so it was not needed."""


@dataclass(frozen=True)
class EncodingCandidate:
    """One encoding source considered by :func:`html_to_unicode`.

    *label* is the value the source gave before normalization, and
    *encoding* is the Python codec name :func:`resolve_encoding` normalized
    it to, or ``None`` when the label named no encoding of the Encoding
    Standard. *reason* always states, in plain words, why *status* is what
    it is.
    """

    source: EncodingSource
    label: str | None
    encoding: str | None
    status: EncodingCandidateStatus
    reason: str


@dataclass(frozen=True)
class EncodingDiagnosis:
    """The full trail of the encoding choice of :func:`html_to_unicode`.

    :attr:`candidates` are ordered by precedence: BOM, HTTP charset, body
    declaration, the detection callback and the default encoding.
    :attr:`conflicts` is ``True`` when two usable candidates normalized to
    different codecs; two aliases of the same codec never conflict.
    :attr:`notes` explains input-level situations that no single candidate
    carries, e.g. an already-decoded body, an empty body or a truncated BOM.
    """

    encoding: str
    """The encoding the body was decoded with, as a Python codec name."""

    candidates: tuple[EncodingCandidate, ...]
    """Every source that was considered, in precedence order."""

    conflicts: bool = False
    """Whether usable candidates named different encodings."""

    bom: tuple[str, bytes] | None = None
    """The BOM that was read as ``(encoding, bom_bytes)``, or ``None``."""

    notes: tuple[str, ...] = field(default_factory=tuple)
    """Explanations of input-level situations, in discovery order."""

# The value ends at whitespace, ";", "," or the end of the header. Comma is
# not parameter syntax; stopping the value at "," approximates Fetch's
# "extract a MIME type" for comma-joined duplicate headers, keeping the first
# charset rather than the last valid MIME type's.
_HEADER_ENCODING_RE = re.compile(
    r"(?:^|;)[ \t]*charset="
    r'(?:"([\w-]+)"|([\w-]+))'
    r"(?![^\s;,])",
    re.IGNORECASE,
)
# https://mimesniff.spec.whatwg.org/commit-snapshots/39aa53511b13953d84fef8d4131d6f61d0ccbde6/#parse-a-mime-type
# Parameters are ";"-separated name=value pairs whose value is either a token
# or a quoted-string, and a quoted-string is opaque
# (https://fetch.spec.whatwg.org/commit-snapshots/586cd2a44c2a865b37c166dc0740f3fb8bb220d6/#collect-an-http-quoted-string),
# so it has to be consumed as a whole: a "charset=" written inside one belongs
# to that value and is not a parameter of its own.
_HEADER_PARAMETER_RE = re.compile(
    r"(?:^|;)[ \t]*(?P<name>[^\s;=]+)="
    r'(?:"(?P<quoted>[^"\\]*(?:\\.[^"\\]*)*)"(?![^\s;,])'
    r"|(?P<token>[^;,\s]*))"
)
_ENCODING_LABEL_RE = re.compile(r"[\w-]+")


def _quoted_aware_charset(content_type: str) -> str | None:
    for match in _HEADER_PARAMETER_RE.finditer(content_type):
        if match.group("name").lower() != "charset":
            continue
        label = match.group("quoted")
        if label is None:
            label = match.group("token")
        if _ENCODING_LABEL_RE.fullmatch(label):
            return resolve_encoding(label)
    return None


def http_content_type_encoding(content_type: str | None) -> str | None:
    """Extract the encoding in the content-type header

    >>> import w3lib.encoding
    >>> w3lib.encoding.http_content_type_encoding("Content-Type: text/html; charset=ISO-8859-4")
    'iso8859-4'

    """

    if content_type:
        match = _HEADER_ENCODING_RE.search(content_type)
        if match:
            # A match inside a quoted-string must be preceded by that string's
            # opening quote, so if no '"' precedes it the fast answer is
            # correct; only otherwise walk the parameters.
            if content_type.find('"', 0, match.start()) < 0:
                return resolve_encoding(match.group(1) or match.group(2))
            return _quoted_aware_charset(content_type)

    return None


def html_body_declared_encoding(html_body_str: str | bytes) -> str | None:
    '''Return the encoding specified in meta tags in the html body,
    or ``None`` if no suitable encoding was found

    >>> import w3lib.encoding
    >>> w3lib.encoding.html_body_declared_encoding(
    ... """<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Strict//EN"
    ...      "http://www.w3.org/TR/xhtml1/DTD/xhtml1-strict.dtd">
    ... <html xmlns="http://www.w3.org/1999/xhtml" xml:lang="en" lang="en">
    ... <head>
    ...     <title>Some title</title>
    ...     <meta http-equiv="content-type" content="text/html;charset=utf-8" />
    ... </head>
    ... <body>
    ... ...
    ... </body>
    ... </html>""")
    'utf-8'
    >>>

    '''

    if isinstance(html_body_str, str):
        html_body_str = html_body_str.encode("utf-8", "replace")
    name = declared_encoding(html_body_str)
    return None if name is None else codec_name(name)


_DEPRECATED_ENCODING_TRANSLATION = {
    "ascii": "cp1252",
    "big5": "big5hkscs",
    "euc_kr": "cp949",
    "gb2312": "gb18030",
    "gb_2312_80": "gb18030",
    "gbk": "gb18030",
    "iso8859_11": "cp874",
    "iso8859_9": "cp1254",
    "latin_1": "cp1252",
    "macintosh": "mac_roman",
    "shift_jis": "cp932",
    "tis_620": "cp874",
    "win_1251": "cp1251",
    "windows_31j": "cp932",
    "win_31j": "cp932",
    "windows_874": "cp874",
    "win_874": "cp874",
    "x_sjis": "cp932",
    "zh_cn": "gb18030",
}


def __getattr__(name: str) -> Any:
    if name == "DEFAULT_ENCODING_TRANSLATION":
        warnings.warn(
            "w3lib.encoding.DEFAULT_ENCODING_TRANSLATION is deprecated.",
            DeprecationWarning,
            stacklevel=2,
        )
        return _DEPRECATED_ENCODING_TRANSLATION
    raise AttributeError(f"module {__name__!r} has no attribute {name!r}")


@lru_cache(maxsize=256)
def resolve_encoding(encoding_alias: str) -> str | None:
    """Return the Python codec name of the encoding of the `Encoding
    Standard`_ that `encoding_alias` maps to, or ``None`` if there is none,
    e.g. for ``utf-32``.

    .. _Encoding Standard: https://encoding.spec.whatwg.org/

    >>> import w3lib.encoding
    >>> w3lib.encoding.resolve_encoding('latin1')
    'cp1252'
    >>> w3lib.encoding.resolve_encoding('gb_2312-80')
    'gbk'
    >>> w3lib.encoding.resolve_encoding('base64')
    >>>

    """
    return codec_name(encoding_alias)


def read_bom(data: bytes) -> tuple[None, None] | tuple[str, bytes]:
    r"""Read the byte order mark in the text, if present, and
    return the encoding represented by the BOM and the BOM.

    If no BOM can be detected, ``(None, None)`` is returned.

    As in browsers, a UTF-32LE BOM reads as a UTF-16LE one, and a UTF-16 body
    that starts with an XML declaration and no BOM gives an empty BOM.

    >>> import w3lib.encoding
    >>> w3lib.encoding.read_bom(b'\xfe\xff\x6c\x34')
    ('utf-16-be', b'\xfe\xff')
    >>> w3lib.encoding.read_bom(b'\xff\xfe\x34\x6c')
    ('utf-16-le', b'\xff\xfe')
    >>> w3lib.encoding.read_bom(b'\x01\x02\x03\x04')
    (None, None)
    >>>

    """
    bom = bom_encoding(data)
    if bom is None:
        return None, None
    name, length = bom
    return cast("str", codec_name(name)), data[:length]


def to_unicode(data_str: bytes, encoding: str) -> str:
    r"""Convert a str object to unicode using the encoding given

    If *encoding* names an encoding of the `Encoding Standard`_, *data_str* is
    decoded the way browsers decode it, e.g. ``utf-16`` is UTF-16LE.

    Characters that cannot be converted will be converted to ``\ufffd`` (the
    unicode replacement character).

    .. _Encoding Standard: https://encoding.spec.whatwg.org/
    """
    try:
        return decode(data_str, encoding)
    except LookupError:
        return data_str.decode(encoding, "replace")


def html_encoding(
    content_type_header: str | None, html_body_str: bytes, *, url: str | None = None
) -> str:
    """Return the encoding :func:`html_to_unicode` would decode
    *html_body_str* with, given the same *url*, without decoding it.

    .. versionadded:: 2.6.0

    >>> import w3lib.encoding
    >>> w3lib.encoding.html_encoding(None, b'<meta charset="latin1">')
    'cp1252'
    """
    enc = http_content_type_encoding(content_type_header)
    return cast("str", codec_name(detect_encoding(html_body_str, enc, url=url)))


# The byte sequences that open a recognized BOM, kept shorter than the full
# BOM on purpose: a body that ends inside one of them is a truncated BOM.
_BOM_PREFIXES = (
    (b"\xef\xbb\xbf", "utf-8"),
    (b"\xfe\xff", "utf-16-be"),
    (b"\xff\xfe", "utf-16-le"),
    (b"\x00\x00\xfe\xff", "utf-32-be"),
)


def _truncated_bom_note(body: bytes) -> str | None:
    """Explain *body* when it ends inside a byte sequence that begins like a
    BOM but is too short to be one."""
    for bom_bytes, name in _BOM_PREFIXES:
        limit = min(len(body), len(bom_bytes) - 1)
        for length in range(limit, 0, -1):
            if body[:length] == bom_bytes[:length]:
                return (
                    f"the body is only {length} byte(s) long and its first "
                    f"{length} byte(s) {body[:length]!r} are the beginning of a "
                    f"{name} BOM {bom_bytes!r}, but the BOM is truncated, so no "
                    "BOM was recognized"
                )
    return None


def _candidate(
    source: EncodingSource,
    label: str | None,
    status: EncodingCandidateStatus,
    reason: str,
) -> EncodingCandidate:
    return EncodingCandidate(
        source=source,
        label=label,
        encoding=None if label is None else resolve_encoding(label),
        status=status,
        reason=reason,
    )


class _Recorder:
    """Collects the encoding candidates in precedence order and tracks which
    codec, if any, has already been adopted."""

    def __init__(self) -> None:
        self.candidates: list[EncodingCandidate] = []
        self.adopted: str | None = None

    def record(self, source: EncodingSource, label: str | None) -> str | None:
        """Normalize *label* and explain why it is adopted, skipped or left
        unused. Return its codec name when it is usable."""
        if label is None:
            self.candidates.append(
                _candidate(
                    source,
                    None,
                    EncodingCandidateStatus.SKIPPED,
                    "no encoding was declared by this source",
                )
            )
            return None
        normalized = resolve_encoding(label)
        if normalized is None:
            self.candidates.append(
                _candidate(
                    source,
                    label,
                    EncodingCandidateStatus.SKIPPED,
                    f"{label!r} names no encoding of the Encoding Standard and "
                    "is ignored the way browsers ignore an invalid label",
                )
            )
            return None
        if self.adopted is None:
            self.candidates.append(
                EncodingCandidate(
                    source=source,
                    label=label,
                    encoding=normalized,
                    status=EncodingCandidateStatus.ADOPTED,
                    reason="the first usable source in precedence order was adopted",
                )
            )
            self.adopted = normalized
            return normalized
        self.candidates.append(
            EncodingCandidate(
                source=source,
                label=label,
                encoding=normalized,
                status=EncodingCandidateStatus.UNUSED,
                reason=(
                    "an earlier source had already supplied an encoding, so "
                    "this usable candidate was not needed"
                ),
            )
        )
        return normalized

    def append_adopted(
        self, source: EncodingSource, label: str, reason: str
    ) -> None:
        """Record an implicit candidate (the UTF-8 check or the byte-based
        guess) that is adopted without going through label resolution."""
        encoding = cast("str", resolve_encoding(label))
        self.candidates.append(
            EncodingCandidate(
                source=source,
                label=label,
                encoding=encoding,
                status=EncodingCandidateStatus.ADOPTED,
                reason=reason,
            )
        )
        self.adopted = encoding

    def append_unused(
        self, source: EncodingSource, label: str, reason: str
    ) -> None:
        """Record an implicit candidate that was computed but not needed."""
        self.candidates.append(
            EncodingCandidate(
                source=source,
                label=label,
                encoding=cast("str", resolve_encoding(label)),
                status=EncodingCandidateStatus.UNUSED,
                reason=reason,
            )
        )

    def skip(
        self, source: EncodingSource, label: str | None, reason: str
    ) -> None:
        """Record a source that was never consulted, e.g. a callback that an
        earlier adoption made unnecessary."""
        self.candidates.append(
            _candidate(source, label, EncodingCandidateStatus.SKIPPED, reason)
        )

    def leave_unused(
        self, source: EncodingSource, label: str | None, reason: str
    ) -> None:
        """Record a source that had a value but was never consulted because an
        earlier source was already adopted."""
        self.candidates.append(
            _candidate(source, label, EncodingCandidateStatus.UNUSED, reason)
        )

    def has_conflicts(
        self,
        explicit_sources: frozenset[EncodingSource] = frozenset(
            ("bom", "http", "meta")
        ),
    ) -> bool:
        """Whether two explicit sources that were actually consulted
        normalized to different codecs.

        Aliases of the same codec never conflict. The implicit browser
        fallbacks (the valid-UTF-8 check, the byte-based guess and the
        built-in UTF-8 default) are not declarations a page made, so they do
        not turn a single explicit source into a conflict.
        """
        usable = {
            candidate.encoding
            for candidate in self.candidates
            if candidate.status
            in (EncodingCandidateStatus.ADOPTED, EncodingCandidateStatus.UNUSED)
            and candidate.source in explicit_sources
        }
        return len(usable) > 1


def _record_http(recorder: _Recorder, content_type_header: str | None) -> None:
    """Record the HTTP charset, keeping both the raw label and its normalized
    codec and never letting an invalid label hide a later source."""
    if not content_type_header:
        recorder.record("http", None)
        return
    match = _HEADER_ENCODING_RE.search(content_type_header)
    if match is None:
        recorder.record("http", None)
        return
    raw_label = match.group(1) or match.group(2)
    if content_type_header.find('"', 0, match.start()) >= 0:
        # A charset seen through a quoted-string needs the quoted-aware walk,
        # which returns the normalized codec; record that label when the raw
        # one is unresolvable on its own.
        resolved = _quoted_aware_charset(content_type_header)
        recorder.record("http", resolved or raw_label)
        return
    recorder.record("http", raw_label)


def _adopted_label(recorder: _Recorder, source: EncodingSource) -> str | None:
    """The raw label of the adopted candidate of *source*, if any."""
    for candidate in recorder.candidates:
        if (
            candidate.source == source
            and candidate.status is EncodingCandidateStatus.ADOPTED
        ):
            return candidate.label
    return None


def _modern_candidates(
    recorder: _Recorder, body: bytes, *, url: str | None
) -> str:
    """Reproduce the browser-style pipeline of frostwork's
    ``detect_encoding`` while recording every implicit candidate, invoking the
    underlying byte-based guess at most once."""
    # The browser prescan reads the head for a declaration even when the
    # transport layer gave a charset; it simply loses to the earlier source.
    declared_name = declared_encoding(body) if body else None
    declared = (
        None if declared_name is None else cast("str", codec_name(declared_name))
    )
    recorder.record("meta", declared)

    if not body:
        recorder.record("default", "utf-8")
        return cast("str", recorder.adopted)

    try:
        body.decode("utf-8")
    except UnicodeDecodeError:
        guessed = autodetect_encoding(body, url=url)
        if recorder.adopted is None:
            recorder.append_adopted(
                "auto_detect",
                guessed,
                "the body is not valid UTF-8, so the encoding was guessed "
                "from its bytes"
                + (" and the top-level domain of the URL" if url else ""),
            )
            recorder.skip(
                "default",
                "utf-8",
                "the guessed encoding was used instead of the UTF-8 fallback",
            )
        else:
            recorder.append_unused(
                "auto_detect",
                guessed,
                "an earlier source had already supplied an encoding, so the "
                "byte-based guess was not needed",
            )
            recorder.skip(
                "default",
                "utf-8",
                "an earlier source had already supplied an encoding, so the "
                "UTF-8 fallback was not needed",
            )
        return cast("str", recorder.adopted)

    if recorder.adopted is None:
        recorder.append_adopted(
            "auto_detect",
            "utf-8",
            "the body is valid UTF-8, so UTF-8 was detected and adopted",
        )
        recorder.candidates.append(
            EncodingCandidate(
                source="default",
                label="utf-8",
                encoding="utf-8",
                status=EncodingCandidateStatus.ADOPTED,
                reason="the valid UTF-8 body was decoded with the UTF-8 default",
            )
        )
    else:
        recorder.append_unused(
            "auto_detect",
            "utf-8",
            "an earlier source had already supplied an encoding, so the valid "
            "UTF-8 check was not needed",
        )
        recorder.skip(
            "default",
            "utf-8",
            "the detected encoding had already been adopted",
        )
    return cast("str", recorder.adopted)


def _diagnose(
    content_type_header: str | None,
    html_body_str: bytes | str,
    default_encoding: str | None,
    auto_detect_fun: Callable[[bytes], str | None] | None,
    *,
    url: str | None,
) -> tuple[str, str, EncodingDiagnosis]:
    """The single pipeline behind both return forms of
    :func:`html_to_unicode`, so a diagnosis can never diverge from the
    decoding it explains."""
    recorder = _Recorder()
    notes: list[str] = []
    legacy = auto_detect_fun is not None or default_encoding is not None

    if isinstance(html_body_str, str):
        notes.append(
            "the body was already decoded text, so it was returned unchanged "
            "and UTF-8 was reported as its encoding"
        )
        body = html_body_str.encode("utf-8", "replace")
    else:
        body = html_body_str

    bom = bom_encoding(body)
    if bom is not None:
        name, length = bom
        encoding = cast("str", codec_name(name))
        recorder.candidates.append(
            EncodingCandidate(
                source="bom",
                label=name,
                encoding=encoding,
                status=EncodingCandidateStatus.ADOPTED,
                reason=(
                    "a BOM was found and takes precedence over every other "
                    "source; it is stripped before decoding"
                    if length
                    else "a UTF-16 document starting with an XML declaration "
                    "was recognized without a BOM and takes precedence"
                ),
            )
        )
        text = decode(body[length:], name)
        if isinstance(html_body_str, str):
            text = html_body_str
        return encoding, text, EncodingDiagnosis(
            encoding=encoding,
            candidates=tuple(recorder.candidates),
            bom=(encoding, body[:length]),
            notes=tuple(notes),
        )

    truncated = _truncated_bom_note(body)
    if truncated is not None:
        notes.append(truncated)
    if not body:
        notes.append("the body is empty, so the decoded text is empty")

    _record_http(recorder, content_type_header)

    if legacy:
        # Drive the decoding from the raw label exactly the way the legacy
        # pipeline did; the recorder only explains how each label was handled.
        chosen_label: str | None = _adopted_label(recorder, "http")
        # The legacy pipeline only consulted the body when the header gave no
        # usable charset, so mirror that precedence in the diagnosis.
        if chosen_label is None:
            meta_label = html_body_declared_encoding(body)
            recorder.record("meta", meta_label)
            if meta_label is not None and chosen_label is None:
                chosen_label = meta_label
        else:
            # The value is still read for the diagnosis, but the legacy
            # pipeline itself never consulted it.
            meta_label = html_body_declared_encoding(body)
            if meta_label is None:
                recorder.skip(
                    "meta",
                    None,
                    "the HTTP charset had already been adopted and the body "
                    "declares no encoding",
                )
            else:
                recorder.leave_unused(
                    "meta",
                    meta_label,
                    "the HTTP charset had already been adopted, so the body "
                    "declaration was not consulted",
                )

        if auto_detect_fun is not None:
            if chosen_label is None:
                detected_label = auto_detect_fun(body)
                recorder.record("auto_detect", detected_label)
                # The legacy pipeline adopted the callback's raw label even
                # when it names no web encoding, and let to_unicode() handle
                # it, so the returned label is the raw one.
                if detected_label is not None:
                    chosen_label = detected_label
            else:
                recorder.skip(
                    "auto_detect",
                    None,
                    "an earlier source had already supplied an encoding, so "
                    "the detection callback was not called",
                )
        else:
            recorder.skip(
                "auto_detect",
                None,
                "no detection callback was provided",
            )

        # The legacy fallback literally spells the label "utf8"; the
        # diagnosis normalizes it to the "utf-8" codec.
        fallback_label = default_encoding or "utf8"
        if chosen_label is None:
            recorder.record("default", fallback_label)
            chosen_label = fallback_label
        else:
            recorder.leave_unused(
                "default",
                fallback_label,
                "an earlier source had already supplied an encoding, so the "
                "default encoding was not needed",
            )
        encoding = cast("str", chosen_label)
        text = to_unicode(body, encoding)
    else:
        encoding = _modern_candidates(recorder, body, url=url)
        text = "" if not body else decode(body, encoding)

    if isinstance(html_body_str, str):
        text = html_body_str

    explicit_sources: frozenset[EncodingSource] = (
        frozenset(("bom", "http", "meta", "auto_detect", "default"))
        if legacy
        else frozenset(("bom", "http", "meta"))
    )
    diagnosis = EncodingDiagnosis(
        encoding=encoding,
        candidates=tuple(recorder.candidates),
        conflicts=recorder.has_conflicts(explicit_sources),
        notes=tuple(notes),
    )
    return encoding, text, diagnosis


@overload
def html_to_unicode(
    content_type_header: str | None,
    html_body_str: bytes,
    default_encoding: str | None = None,
    auto_detect_fun: Callable[[bytes], str | None] | None = None,
    *,
    url: str | None = None,
    diagnostic: Literal[False] = False,
) -> tuple[str, str]: ...


@overload
def html_to_unicode(
    content_type_header: str | None,
    html_body_str: bytes,
    default_encoding: str | None = None,
    auto_detect_fun: Callable[[bytes], str | None] | None = None,
    *,
    url: str | None = None,
    diagnostic: Literal[True],
) -> tuple[str, str, EncodingDiagnosis]: ...


def html_to_unicode(
    content_type_header: str | None,
    html_body_str: bytes | str,
    default_encoding: str | None = None,
    auto_detect_fun: Callable[[bytes], str | None] | None = None,
    *,
    url: str | None = None,
    diagnostic: bool = False,
) -> tuple[str, str] | tuple[str, str, EncodingDiagnosis]:
    r'''Convert raw html bytes to unicode

    .. versionadded:: 2.6.0
       The *url* parameter.

    This finds the encoding of the html body the way a web browser does,
    trying in order:

    * BOM (byte-order mark)
    * http content type header
    * meta or xml tag declarations
    * UTF-8, if the body is valid UTF-8
    * the encoding guessed from the bytes of the body and the top-level domain
      of *url*, the URL of the body, as Firefox does

    If a BOM is found, it will be stripped.

    If `content_type_header` is not present, ``None`` can be passed signifying
    that the header was not present.

    This method will not fail, if characters cannot be converted to unicode,
    ``\ufffd`` (the unicode replacement character) will be inserted instead.

    By default a tuple of ``(<encoding used>, <unicode_string>)`` is returned.
    Pass ``diagnostic=True`` to additionally get an
    :class:`EncodingDiagnosis` explaining the choice: it records, for the
    BOM, the HTTP charset, the body declaration, the detection callback and
    the default encoding, the raw candidate label, its normalized codec and
    the reason the candidate was adopted, skipped or left unused. Aliases of
    the same codec never count as a conflict, and an invalid label is skipped
    without hiding the sources that follow it. The diagnostic mode runs the
    same single pipeline, so it neither calls *auto_detect_fun* an extra time
    nor changes the returned encoding or text.

    Examples:

    >>> import w3lib.encoding
    >>> w3lib.encoding.html_to_unicode(None,
    ... b"""<!DOCTYPE html>
    ... <head>
    ... <meta charset="UTF-8" />
    ... <meta name="viewport" content="width=device-width" />
    ... <title>Creative Commons France</title>
    ... <link rel='canonical' href='http://creativecommons.fr/' />
    ... <body>
    ... <p>Creative Commons est une organisation \xc3\xa0 but non lucratif
    ... qui a pour dessein de faciliter la diffusion et le partage des oeuvres
    ... tout en accompagnant les nouvelles pratiques de cr\xc3\xa9ation \xc3\xa0 l\xe2\x80\x99\xc3\xa8re numerique.</p>
    ... </body>
    ... </html>""")
    ('utf-8', '<!DOCTYPE html>\n<head>\n<meta charset="UTF-8" />\n<meta name="viewport" content="width=device-width" />\n<title>Creative Commons France</title>\n<link rel=\'canonical\' href=\'http://creativecommons.fr/\' />\n<body>\n<p>Creative Commons est une organisation \xe0 but non lucratif\nqui a pour dessein de faciliter la diffusion et le partage des oeuvres\ntout en accompagnant les nouvelles pratiques de cr\xe9ation \xe0 l\u2019\xe8re numerique.</p>\n</body>\n</html>')
    >>>
    >>> encoding, text, diagnosis = html_to_unicode(
    ...     "text/html; charset=latin1",
    ...     b'<meta charset="utf-8">caf\xe9',
    ...     diagnostic=True,
    ... )
    >>> diagnosis.conflicts
    True
    >>> [(c.source, c.encoding, c.status.value) for c in diagnosis.candidates]
    [('http', 'cp1252', 'adopted'), ('meta', 'utf-8', 'unused'), ('auto_detect', 'utf-8', 'unused'), ('default', 'utf-8', 'skipped')]
    >>> encoding == diagnosis.encoding
    True

    '''
    if auto_detect_fun is not None or default_encoding is not None:
        warnings.warn(
            "The default_encoding and auto_detect_fun parameters of "
            "w3lib.encoding.html_to_unicode() are deprecated.",
            DeprecationWarning,
            stacklevel=2,
        )
    encoding, text, diagnosis = _diagnose(
        content_type_header,
        html_body_str,
        default_encoding,
        auto_detect_fun,
        url=url,
    )
    if diagnostic:
        return encoding, text, diagnosis
    return encoding, text

