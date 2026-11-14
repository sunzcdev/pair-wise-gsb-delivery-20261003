# Content selection for entries with multiple content representations.
# Copyright 2010-2025 Kurt McKee <contactme@kurtmckee.org>
# Copyright 2002-2008 Mark Pilgrim
# All rights reserved.
#
# This file is a part of feedparser.
#
# Redistribution and use in source and binary forms, with or without modification,
# are permitted provided that the following conditions are met:
#
# * Redistributions of source code must retain the above copyright notice,
#   this list of conditions and the following disclaimer in the documentation
#   and/or other materials provided with the distribution.
#
# THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS 'AS IS'
# AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
# IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE
# ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT OWNER OR CONTRIBUTORS BE
# LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR
# CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF
# SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
# INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN
# CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE)
# ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
# POSSIBILITY OF SUCH DAMAGE.

"""Select a single content representation from a parsed entry.

Entries may carry several ``content`` representations (different media
types, different languages, or external ``src`` pointers).  This module
provides :func:`select_content`, a single place that implements the
selection rules so callers don't each hand-roll their own:

*   Media types are compared after normalization: lowercased, with
    parameters (``; charset=...``) stripped, and shorthand aliases
    (``text``, ``html``, ``xhtml``) mapped to their canonical types.
    The same rules apply to the stored types and to the requested
    preference list.
*   Language preferences are matched exactly first, then by language
    family fallback (``zh`` matches ``zh-CN`` and vice versa).
*   Representations without a language tag are only used as a fallback
    when ``allow_untagged_language`` is true.
*   Representations that only point to external content (``src`` with
    no inline body) are excluded unless ``allow_external`` is true.
    This module never performs network access.
*   Ties are broken by source order: the earliest representation in
    the parsed ``content`` list wins.
*   When nothing matches, the result is a distinguishable "no result";
    the entry's ``summary`` is never substituted for ``content``.

The parsed entry is never modified; all raw representations collected
by :func:`feedparser.parse` remain available on the result.
"""

import typing

__all__ = (
    "ContentSelection",
    "select_content",
)

# Shorthand media type aliases, mirroring the parser's mapping rules.
_MEDIA_TYPE_ALIASES = {
    "text": "text/plain",
    "plain": "text/plain",
    "html": "text/html",
    "xhtml": "application/xhtml+xml",
}

# Machine-readable reasons attached to every selection result.
NO_REPRESENTATIONS = "no-representations"
NO_MATCHING_MEDIA_TYPE = "no-matching-media-type"
NO_MATCHING_LANGUAGE = "no-matching-language"
EXTERNAL_ONLY = "external-only"
SELECTED = "selected"


def _normalize_media_type(value: typing.Optional[str]) -> str:
    """Normalize a media type for comparison.

    The value is lowercased, parameters are stripped, and shorthand
    aliases are mapped to their canonical types.
    """

    if not value:
        return ""
    base = value.split(";", 1)[0].strip().lower()
    return _MEDIA_TYPE_ALIASES.get(base, base)


def _normalize_language(value: typing.Optional[str]) -> str:
    """Normalize a language tag for comparison."""

    if not value:
        return ""
    return value.strip().lower().replace("_", "-")


def _language_match(requested: str, available: str) -> typing.Optional[str]:
    """Compare a requested language tag with an available one.

    Returns ``"exact"``, ``"family"``, or ``None``.
    """

    if requested == available:
        return "exact"
    if available.startswith(requested + "-") or requested.startswith(available + "-"):
        return "family"
    return None


def _is_external(representation: typing.Mapping[str, typing.Any]) -> bool:
    """Return True if the representation only points to external content."""

    return bool(representation.get("src")) and not representation.get("value")


class ContentSelection:
    """The outcome of :func:`select_content`.

    ``found`` distinguishes a successful selection from a "no result"
    outcome; when it is false, ``reason`` explains why and
    ``representation``/``value`` are ``None``.  ``representations``
    always holds every raw representation collected by the parser,
    in source order.
    """

    __slots__ = (
        "found",
        "reason",
        "representation",
        "representations",
        "index",
        "value",
        "src",
        "external",
        "media_type",
        "language",
        "language_match",
    )

    def __init__(
        self,
        *,
        found: bool,
        reason: str,
        representations: typing.Tuple[typing.Mapping[str, typing.Any], ...],
        representation: typing.Optional[typing.Mapping[str, typing.Any]] = None,
        index: typing.Optional[int] = None,
        external: bool = False,
        language_match: typing.Optional[str] = None,
    ) -> None:
        self.found = found
        self.reason = reason
        self.representations = representations
        self.representation = representation
        self.index = index
        self.external = external
        self.language_match = language_match
        if representation is not None:
            self.value = representation.get("value")
            self.src = representation.get("src")
            self.media_type = _normalize_media_type(representation.get("type"))
            self.language = representation.get("language")
        else:
            self.value = None
            self.src = None
            self.media_type = None
            self.language = None

    def __bool__(self) -> bool:
        return self.found

    def __repr__(self) -> str:
        if self.found:
            return (
                f"{type(self).__name__}(found=True, index={self.index}, "
                f"media_type={self.media_type!r}, language={self.language!r}, "
                f"language_match={self.language_match!r}, external={self.external})"
            )
        return f"{type(self).__name__}(found=False, reason={self.reason!r})"


def _get_representations(
    entry: typing.Mapping[str, typing.Any]
) -> typing.List[typing.Mapping[str, typing.Any]]:
    """Collect the raw content representations of an entry, in source order."""

    raw = entry.get("content")
    if raw is None:
        return []
    if isinstance(raw, (list, tuple)):
        return list(raw)
    # JSON feeds store a single content dict rather than a list.
    return [raw]


def select_content(
    entry: typing.Mapping[str, typing.Any],
    media_types: typing.Optional[typing.Iterable[str]] = None,
    languages: typing.Optional[typing.Iterable[str]] = None,
    *,
    allow_untagged_language: bool = False,
    allow_external: bool = False,
) -> ContentSelection:
    """Select one content representation from a parsed entry.

    :param entry: A parsed entry (a :class:`feedparser.FeedParserDict`),
        or any mapping with an optional ``content`` key.
    :param media_types: Media type preference list, most preferred
        first.  Types are normalized before comparison (case and
        parameters are ignored, shorthand aliases are mapped).  If
        empty or ``None``, any media type is acceptable.
    :param languages: Language preference list, most preferred first.
        Exact matches win over same-family fallbacks.  If empty or
        ``None``, any language is acceptable.
    :param allow_untagged_language: Allow representations without a
        language tag as a last-resort fallback.
    :param allow_external: Allow representations that only carry an
        external ``src`` pointer and no inline body.  The external
        content is never fetched.
    :return: A :class:`ContentSelection`.  When no representation is
        usable, ``found`` is false and ``reason`` identifies why; the
        entry's ``summary`` is never used as a substitute.
    """

    representations = _get_representations(entry)
    all_representations = tuple(representations)

    wanted_types = [_normalize_media_type(t) for t in media_types or ()]
    wanted_languages = [_normalize_language(lang) for lang in languages or ()]

    best: typing.Optional[
        typing.Tuple[typing.Tuple, int, typing.Mapping[str, typing.Any], bool, str]
    ] = None
    saw_media_mismatch = False
    saw_language_mismatch = False
    saw_external_excluded = False

    for index, representation in enumerate(representations):
        external = _is_external(representation)
        if external and not allow_external:
            saw_external_excluded = True
            continue

        if wanted_types:
            media_type = _normalize_media_type(representation.get("type"))
            if media_type not in wanted_types:
                saw_media_mismatch = True
                continue
            media_rank = wanted_types.index(media_type)
        else:
            media_rank = 0

        language = _normalize_language(representation.get("language"))
        language_match: typing.Optional[str] = None
        if wanted_languages:
            if language:
                language_key: typing.Optional[typing.Tuple[int, int]] = None
                for language_rank, wanted in enumerate(wanted_languages):
                    match = _language_match(wanted, language)
                    if match is None:
                        continue
                    exactness = 0 if match == "exact" else 1
                    candidate = (language_rank, exactness)
                    if language_key is None or candidate < language_key:
                        language_key = candidate
                        language_match = match
                if language_key is None:
                    saw_language_mismatch = True
                    continue
            elif allow_untagged_language:
                language_key = (len(wanted_languages), 2)
                language_match = "untagged"
            else:
                saw_language_mismatch = True
                continue
        else:
            language_key = (0, 0)

        key = (language_key, media_rank, index)
        if best is None or key < best[0]:
            best = (key, index, representation, external, language_match or "")

    if best is not None:
        _, index, representation, external, language_match = best
        return ContentSelection(
            found=True,
            reason=SELECTED,
            representations=all_representations,
            representation=representation,
            index=index,
            external=external,
            language_match=language_match or None,
        )

    if not representations:
        reason = NO_REPRESENTATIONS
    elif saw_external_excluded and not (saw_media_mismatch or saw_language_mismatch):
        reason = EXTERNAL_ONLY
    elif saw_language_mismatch and not saw_media_mismatch:
        reason = NO_MATCHING_LANGUAGE
    else:
        reason = NO_MATCHING_MEDIA_TYPE
    return ContentSelection(
        found=False, reason=reason, representations=all_representations
    )
