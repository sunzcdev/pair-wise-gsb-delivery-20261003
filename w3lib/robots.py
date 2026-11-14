"""
Combine the robots directives of a page into the rules that apply to one
crawler.

The directives come from the ``robots`` (or crawler-named) ``<meta>`` tags of
the page text and from the ``X-Robots-Tag`` HTTP response headers. This
module only reads what it is given: it never performs any network access.

The rules of the generic declarations (``robots``) and of the declarations
named after the requested crawler apply together; declarations named after
any other crawler are ignored.
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from datetime import UTC, datetime
from email.utils import parsedate_to_datetime
from typing import TYPE_CHECKING, Literal, TypeAlias

from w3lib._util import iter_tag_attributes, to_unicode
from w3lib.html import _build_meta_scan_pattern, replace_entities

if TYPE_CHECKING:
    from collections.abc import Mapping, Sequence

SourceLocation: TypeAlias = Literal["meta", "header"]

# Boolean directives, mapped to the family of behavior they control and to
# how strict they are in that family: a larger rank is stricter.
_BOOLEAN_DIRECTIVES: dict[str, tuple[str, int]] = {
    "index": ("index", 0),
    "noindex": ("index", 1),
    "follow": ("follow", 0),
    "nofollow": ("follow", 1),
    "noarchive": ("archive", 1),
    "nosnippet": ("snippet", 1),
    "notranslate": ("translate", 1),
    "noimageindex": ("imageindex", 1),
}
_NUMERIC_DIRECTIVES = frozenset(("max-snippet", "max-video-preview"))
_IMAGE_PREVIEW_RANKS = {"none": 0, "standard": 1, "large": 2}
_UNAVAILABLE_AFTER = "unavailable-after"
_VALUE_DIRECTIVES = frozenset(
    (
        "max-snippet",
        "max-image-preview",
        "max-video-preview",
        _UNAVAILABLE_AFTER,
    )
)
_SHORTHANDS = frozenset(("all", "none"))
_KNOWN_TOKENS = frozenset(_BOOLEAN_DIRECTIVES) | _VALUE_DIRECTIVES | _SHORTHANDS

# Families in the order their winning directive is reported.
_FAMILY_ORDER = (
    "index",
    "follow",
    "archive",
    "snippet",
    "translate",
    "imageindex",
    "max-snippet",
    "max-image-preview",
    "max-video-preview",
    _UNAVAILABLE_AFTER,
)
_FAMILY_RANK = {family: index for index, family in enumerate(_FAMILY_ORDER)}

# Comments and <script>/<noscript> content are consumed by this scanner, so a
# <meta> written there never matches; whole other tags are consumed too, so a
# <meta> inside an attribute value is not read as a declaration.
_meta_scan_re = _build_meta_scan_pattern(("script", "noscript"))
_INTEGER_RE = re.compile(r"[0-9]+", re.ASCII)


@dataclass(frozen=True)
class RobotsSource:
    """Where a directive was declared."""

    #: ``"meta"`` for a ``<meta>`` tag, ``"header"`` for an ``X-Robots-Tag``.
    location: SourceLocation
    #: ``"robots"`` for a generic declaration, otherwise the crawler name as
    #: written in the declaration.
    target: str
    #: The directive token as written, e.g. ``"NOINDEX"`` or
    #: ``"max-snippet: 0"``.
    raw: str
    #: The whole declaration the token was part of: the ``content`` attribute
    #: value or the full ``X-Robots-Tag`` header value.
    declaration: str
    #: Zero-based position of the ``<meta>`` tag among the ``<meta>`` tags of
    #: the document, or ``None`` for a header directive.
    meta_index: int | None = None
    #: Zero-based position of the ``X-Robots-Tag`` header among the response
    #: headers of that name, or ``None`` for a meta directive.
    header_index: int | None = None


@dataclass(frozen=True)
class RobotsDirective:
    """An effective directive, after shorthand expansion and merging."""

    #: Lowercase canonical directive name, e.g. ``"noindex"``.
    name: str
    #: The directive value for valued directives, otherwise ``None``.
    value: str | None
    #: The declaration that won for the family of the directive.
    source: RobotsSource


@dataclass(frozen=True)
class UnknownDirective:
    """A token that is not a recognized directive, kept for diagnostics."""

    #: The token as written; its meaning is never guessed.
    token: str
    #: Where the token was declared.
    source: RobotsSource


@dataclass(frozen=True)
class RobotsRules:
    """The rules that apply to the requested crawler."""

    #: The crawler name the rules were computed for.
    crawler: str
    #: The winning directive of every directive family, in a fixed order.
    directives: tuple[RobotsDirective, ...]
    #: Unrecognized tokens encountered in applicable declarations.
    unknown: tuple[UnknownDirective, ...]

    def get(self, name: str) -> RobotsDirective | None:
        """Return the effective directive called *name*, if any."""
        name = name.lower()
        for directive in self.directives:
            if directive.name == name:
                return directive
        return None

    @property
    def indexable(self) -> bool:
        """Whether the crawler may index (no effective ``noindex``)."""
        return self.get("noindex") is None

    @property
    def followable(self) -> bool:
        """Whether the crawler may follow links (no effective ``nofollow``)."""
        return self.get("nofollow") is None


@dataclass(frozen=True)
class _Occurrence:
    family: str
    name: str
    value: str | None
    weight: int | datetime
    source: RobotsSource


def _attribute_value(attrs: str, wanted: str) -> str | None:
    """Return the first value of the *wanted* attribute of a tag body."""
    for name, raw_value in iter_tag_attributes(attrs):
        if name == wanted:
            return replace_entities(raw_value) if "&" in raw_value else raw_value
    return None


def _parse_unavailable_after(value: str) -> datetime | None:
    try:
        parsed = parsedate_to_datetime(value)
    except (TypeError, ValueError, IndexError):
        parsed = None
    if parsed is None:
        try:
            parsed = datetime.fromisoformat(value)
        except ValueError:
            return None
    # Normalize every value to naive UTC so values from a header (RFC 2822,
    # usually timezone-aware) and from a meta tag (often naive) stay
    # comparable: the strictest value is the earliest point in time.
    if parsed.tzinfo is not None:
        parsed = parsed.astimezone(UTC).replace(tzinfo=None)
    return parsed

def _parse_token(
    token: str, source: RobotsSource
) -> tuple[list[_Occurrence], list[UnknownDirective]]:
    """Parse one comma-separated directive token."""
    head, has_value, raw_value = token.partition(":")
    key = head.strip().lower()

    if not has_value:
        if key == "all":
            return (
                [
                    _Occurrence("index", "index", None, 0, source),
                    _Occurrence("follow", "follow", None, 0, source),
                ],
                [],
            )
        if key == "none":
            return (
                [
                    _Occurrence("index", "noindex", None, 1, source),
                    _Occurrence("follow", "nofollow", None, 1, source),
                ],
                [],
            )
        if key in _BOOLEAN_DIRECTIVES:
            family, rank = _BOOLEAN_DIRECTIVES[key]
            return [_Occurrence(family, key, None, rank, source)], []
        return [], [UnknownDirective(token.strip(), source)]

    value = raw_value.strip()
    if key in _NUMERIC_DIRECTIVES:
        if _INTEGER_RE.fullmatch(value) is None:
            return [], [UnknownDirective(token.strip(), source)]
        number = int(value)
        return [_Occurrence(key, key, str(number), number, source)], []
    if key == "max-image-preview":
        rank = _IMAGE_PREVIEW_RANKS.get(value.lower())
        if rank is None:
            return [], [UnknownDirective(token.strip(), source)]
        return [_Occurrence(key, key, value.lower(), rank, source)], []
    if key == _UNAVAILABLE_AFTER:
        when = _parse_unavailable_after(value)
        if when is None:
            return [], [UnknownDirective(token.strip(), source)]
        return [_Occurrence(key, key, value, when, source)], []
    return [], [UnknownDirective(token.strip(), source)]


def _is_stricter(candidate: _Occurrence, current: _Occurrence) -> bool:
    """Return whether *candidate* restricts its family more than *current*."""
    if candidate.family in _NUMERIC_DIRECTIVES:
        return candidate.weight < current.weight  # type: ignore[operator]
    if candidate.family in ("max-image-preview", _UNAVAILABLE_AFTER):
        return candidate.weight < current.weight  # type: ignore[operator]
    return candidate.weight > current.weight  # type: ignore[operator]


def _split_directives(declaration: str) -> list[str]:
    """Split a directive list on commas, keeping commas inside a value.

    Most directives are comma-separated tokens. A valued directive carries
    its own value after a colon, which may contain commas, e.g. the RFC 2822
    date of ``unavailable-after``. Its value ends at the comma followed by a
    recognized next directive; anything before that comma belongs to the
    value, however many commas it contains.
    """
    raw_tokens = [token.strip() for token in declaration.split(",") if token.strip()]
    tokens: list[str] = []
    pending: str | None = None
    for raw_token in raw_tokens:
        if pending is not None:
            head = raw_token.split(":", 1)[0].strip().lower()
            if head in _KNOWN_TOKENS:
                tokens.append(pending)
                pending = raw_token
            else:
                pending = f"{pending}, {raw_token}"
            continue
        key = raw_token.split(":", 1)[0].strip().lower()
        if key in _VALUE_DIRECTIVES:
            pending = raw_token
        else:
            tokens.append(raw_token)
    if pending is not None:
        tokens.append(pending)
    return tokens


def _consume_declaration(
    declaration: str,
    target: str,
    location: SourceLocation,
    meta_index: int | None,
    header_index: int | None,
    occurrences: dict[str, _Occurrence],
    unknown: list[UnknownDirective],
) -> None:
    for token in _split_directives(declaration):
        source = RobotsSource(
            location=location,
            target=target,
            raw=token,
            declaration=declaration,
            meta_index=meta_index,
            header_index=header_index,
        )
        parsed, unknown_tokens = _parse_token(token, source)
        unknown.extend(unknown_tokens)
        for occurrence in parsed:
            current = occurrences.get(occurrence.family)
            if current is None or _is_stricter(occurrence, current):
                occurrences[occurrence.family] = occurrence


def _header_text(value: str | bytes) -> str:
    if isinstance(value, bytes):
        return value.decode("latin-1")
    return value


def _iter_meta(
    text: str | bytes, crawler: str, encoding: str
) -> list[tuple[str, str, int]]:
    """Return ``(target, content, meta_index)`` for every robots meta tag."""
    utext = to_unicode(text, encoding)
    wanted = crawler.strip().lower()
    declarations: list[tuple[str, str, int]] = []
    meta_index = -1
    for match in _meta_scan_re.finditer(utext):
        attrs = match.group("attrs")
        if attrs is None:
            continue
        meta_index += 1
        name = _attribute_value(attrs, "name")
        if name is None:
            continue
        label = name.strip().lower()
        if label == "robots":
            target = "robots"
        elif label == wanted and wanted:
            target = name.strip()
        else:
            continue
        content = _attribute_value(attrs, "content") or ""
        declarations.append((target, content, meta_index))
    return declarations


def _iter_headers(
    headers: Mapping[str | bytes, str | bytes | Sequence[str | bytes]] | None,
    crawler: str,
) -> list[tuple[str, str, int]]:
    """Return ``(target, body, header_index)`` for the applicable
    ``X-Robots-Tag`` headers."""
    if not headers:
        return []
    wanted = crawler.strip().lower()
    declarations: list[tuple[str, str, int]] = []
    header_index = -1
    for raw_key, raw_values in headers.items():
        if _header_text(raw_key).strip().lower() != "x-robots-tag":
            continue
        if isinstance(raw_values, (str, bytes)):
            values: Sequence[str | bytes] = (raw_values,)
        else:
            values = raw_values
        for raw_value in values:
            header_index += 1
            value = _header_text(raw_value).strip()
            head, separator, rest = value.partition(":")
            if separator and head.strip().lower() not in _KNOWN_TOKENS:
                prefix = head.strip()
                if prefix.lower() != wanted or not wanted:
                    continue
                target = prefix
                body = rest.strip()
            else:
                target = "robots"
                body = value
            declarations.append((target, body, header_index))
    return declarations


def robots_directives(
    text: str | bytes,
    headers: Mapping[str | bytes, str | bytes | Sequence[str | bytes]] | None,
    crawler: str,
    encoding: str = "utf-8",
) -> RobotsRules:
    """Return the robots rules that apply to *crawler*.

    *text* is the page text (HTML), *headers* the response headers as a
    mapping (keys and values may be ``str`` or ``bytes``; values may be
    sequences to represent repeated headers), and *crawler* the name of the
    crawler the rules are computed for, matched case-insensitively.

    Generic (``robots``) declarations and declarations named after *crawler*
    apply together; declarations named after other crawlers are ignored.
    Shorthands are expanded (``all`` to ``index, follow`` and ``none`` to
    ``noindex, nofollow``), the strictest declaration of every directive
    family wins, and unrecognized tokens are returned in
    :attr:`RobotsRules.unknown` without being interpreted.

    >>> from w3lib.robots import robots_directives
    >>> page = '<meta name="robots" content="noindex, nofollow">'
    >>> rules = robots_directives(page, None, "mybot")
    >>> rules.indexable, rules.followable
    (False, False)
    >>> rules.get("noindex").source.location
    'meta'
    """
    occurrences: dict[str, _Occurrence] = {}
    unknown: list[UnknownDirective] = []

    # Headers are declared before the body, so on an equal strictness tie the
    # earlier declaration, processed first, deterministically wins.
    for target, value, header_index in _iter_headers(headers, crawler):
        _consume_declaration(
            value,
            target,
            "header",
            None,
            header_index,
            occurrences,
            unknown,
        )

    for target, content, meta_index in _iter_meta(text, crawler, encoding):
        _consume_declaration(
            content,
            target,
            "meta",
            meta_index,
            None,
            occurrences,
            unknown,
        )

    directives = tuple(
        RobotsDirective(
            name=occurrence.name,
            value=occurrence.value,
            source=occurrence.source,
        )
        for _, occurrence in sorted(
            occurrences.items(), key=lambda item: _FAMILY_RANK[item[0]]
        )
    )
    return RobotsRules(
        crawler=crawler,
        directives=directives,
        unknown=tuple(unknown),
    )


__all__ = [
    "RobotsDirective",
    "RobotsRules",
    "RobotsSource",
    "UnknownDirective",
    "robots_directives",
]
