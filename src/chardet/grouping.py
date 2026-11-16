"""Group detection candidates by the text they decode the examined bytes to.

On short ASCII input, or on input whose non-ASCII bytes all sit in a range
the candidate encodings share, several encodings decode the examined window
to the exact same text and no amount of scoring can tell them apart from
this input alone.  This module makes that equivalence visible: it decodes
the window under each candidate and groups the ones that produce identical
text, keeping every member's own encoding name and confidence.

A group is a fact about *this* window only.  Two encodings that agree here
may part ways on the next byte --- ``cp1252`` and ``iso8859-1`` decode
``"Café"`` alike and disagree at ``0x80`` --- so membership never means the
encodings are interchangeable in general.  No static alias or superset
table decides membership either: only the decoded text of the bytes that
were actually examined does.
"""

from __future__ import annotations

import codecs
import dataclasses
from types import MappingProxyType
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from collections.abc import Iterable

    from chardet.pipeline import DetectionResult


@dataclasses.dataclass(frozen=True, slots=True)
class CandidateGroup:
    """Candidates that decode the examined window to one identical text.

    The equivalence holds for the bytes examined in this detection, not
    globally: members may decode other inputs differently.  Members keep
    the ranking order and the confidence scores the pipeline gave them.
    """

    #: The text every member decodes the examined window to.
    decoded_text: str
    #: The tied candidates, best-ranked first, scores untouched.
    members: tuple[DetectionResult, ...]


@dataclasses.dataclass(frozen=True, slots=True)
class DetectionGroups:
    """Detection candidates grouped by decoded-text equivalence.

    Returned by :func:`chardet.detect_groups`.  Every ranked candidate
    appears exactly once, either in one of :attr:`groups` or in
    :attr:`undecodable`, and exactly once in :attr:`output_names`.  A
    result with ``encoding=None`` (the binary verdict) names no codec and
    is omitted from all three.
    """

    #: Groups of candidates that decode the window to the same text,
    #: ordered by their best member's rank.
    groups: tuple[CandidateGroup, ...]
    #: Candidates whose encoding cannot decode the examined window.
    undecodable: tuple[DetectionResult, ...]
    #: Display name -> internal names of every member reported under it.
    #: Inverted so that a rename collapsing several candidates onto one
    #: output name (``prefer_superset`` mapping ISO-8859-1 and Windows-1252
    #: both to ``Windows-1252``) lists them all instead of dropping some.
    output_names: MappingProxyType[str, tuple[str, ...]]
    #: Number of bytes the grouping decoded --- the examined window,
    #: ``data[:max_bytes]``.  Equivalence claims extend no further.
    window_size: int


def _decode_window(window: bytes, encoding: str) -> str | None:
    """Decode *window* under *encoding*, or return ``None`` if it does not decode.

    Uses the same tolerant judgment as the pipeline's validity filter: an
    incomplete multi-byte sequence at the very end of the window is
    deferred rather than an error, because the window is usually a prefix
    of the caller's input.
    """
    try:
        decoder = codecs.getincrementaldecoder(encoding)()
    except LookupError:
        return None
    try:
        return decoder.decode(window, final=False)
    except UnicodeError:
        return None


def group_candidates(
    results: Iterable[DetectionResult], window: bytes
) -> tuple[list[CandidateGroup], list[DetectionResult]]:
    """Group *results* by the text each candidate decodes *window* to.

    :param results: Ranked detection results (internal encoding names).
        Results with ``encoding=None`` (the binary verdict) name no codec
        and are left out of both return values.
    :param window: The bytes the verdict is about --- the same window the
        detection examined.  Equivalence is claimed for these bytes only.
    :returns: ``(groups, undecodable)``: one :class:`CandidateGroup` per
        distinct decoded text, ordered by first (best-ranked) appearance,
        and the candidates whose encoding cannot decode *window*.
    """
    by_text: dict[str, list[DetectionResult]] = {}
    undecodable: list[DetectionResult] = []
    for result in results:
        if result.encoding is None:
            continue
        text = _decode_window(window, result.encoding)
        if text is None:
            undecodable.append(result)
        else:
            by_text.setdefault(text, []).append(result)
    groups = [
        CandidateGroup(decoded_text=text, members=tuple(members))
        for text, members in by_text.items()
    ]
    return groups, undecodable
