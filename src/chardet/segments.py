"""Segmented encoding detection for concatenated multi-encoding inputs.

A harvested file is sometimes several texts in different encodings
concatenated into one byte stream.  A single whole-file verdict hides
that: the highest-confidence answer describes the dominant region and
silently mislabels the rest.  This module offers the opt-in alternative
behind :func:`chardet.detect_segments`: run the normal detection pipeline
over bounded windows of the input and report, per window, the candidate
ranking, plus the byte offsets where the winning encoding changes.

The report is evidence, not a conversion.  Nothing here decodes the
input into one string --- picking the spans to convert, and the encoding
to convert each with, stays with the caller.

Two boundary rules keep window artifacts from masquerading as switches:

* A multi-byte sequence clipped by a window edge is not, on its own,
  evidence of an encoding change.  The pipeline already tolerates a
  dangling tail byte-for-byte; on top of that, a window whose verdict
  is weak (at or below
  :data:`~chardet.MINIMUM_THRESHOLD`) grounds no change, and an
  isolated one-window deviation between two windows that agree is
  treated as a boundary artifact and suppressed.
* A pure-ASCII window grounds no change either.  ASCII decodes
  identically under every candidate, so an ASCII run between two
  different encodings is reported as "no verdict", not as a switch to
  ASCII and back.
"""

from __future__ import annotations

import dataclasses

from chardet._request import DetectionRequest
from chardet._utils import MINIMUM_THRESHOLD
from chardet.pipeline import DetectionDict

#: Default window size for :func:`chardet.detect_segments`.
#: Large enough for the statistical stage to converge on CJK and
#: single-byte text alike, small enough that a concatenation of
#: article-sized texts still produces several windows.  A multiple of 4,
#: so default windows never cut a UTF-16/32 code unit off alignment.
DEFAULT_WINDOW_SIZE: int = 16_384


@dataclasses.dataclass(frozen=True, slots=True)
class Segment:
    """One examined window and its candidate ranking.

    ``start``/``end`` are byte offsets into the caller's input
    (``end`` exclusive).  ``candidates`` is the window's ranked results,
    best first, in the same dict shape :func:`chardet.detect_all`
    returns.
    """

    start: int
    end: int
    candidates: tuple[DetectionDict, ...]

    @property
    def encoding(self) -> str | None:
        """The window's top candidate encoding (``None`` for binary)."""
        return self.candidates[0]["encoding"]

    @property
    def confidence(self) -> float:
        """The window's top candidate confidence."""
        return self.candidates[0]["confidence"]


@dataclasses.dataclass(frozen=True, slots=True)
class EncodingChange:
    """A detected encoding switch at a byte offset.

    ``offset`` is the start of the first window whose verdict differs
    from the established one; the true switch point is somewhere in the
    preceding window.  ``from_encoding``/``to_encoding`` are the verdicts
    on either side.
    """

    offset: int
    from_encoding: str | None
    to_encoding: str | None


@dataclasses.dataclass(frozen=True, slots=True)
class SegmentReport:
    """The outcome of one :func:`chardet.detect_segments` call.

    The verdicts cover only the sampled windows: at most ``max_bytes``
    of the input is examined, so when ``truncated`` is true nothing here
    says anything about the bytes past ``sampled_bytes``.  Even within
    the sample the report is a per-window candidate list, not a decoded
    string --- how to convert the file remains the caller's decision.
    """

    segments: tuple[Segment, ...]
    changes: tuple[EncodingChange, ...]
    sampled_bytes: int
    total_bytes: int
    window_size: int

    @property
    def truncated(self) -> bool:
        """``True`` when ``max_bytes`` capped how much of the input was sampled."""
        return self.total_bytes > self.sampled_bytes


def _window_verdict(segment: Segment) -> str | None:
    """Return the verdict that may ground a change, or ``None``.

    ``None`` means "no verdict": the window is binary (no encoding at
    all), pure ASCII (decodes identically under every candidate, so it
    must not force a switch), or its top candidate is at or below
    :data:`~chardet.MINIMUM_THRESHOLD` (weak evidence, the typical shape
    of a window that opened on a clipped multi-byte fragment).
    """
    top = segment.candidates[0]
    encoding = top["encoding"]
    if encoding is None or encoding.lower() == "ascii":
        return None
    if top["confidence"] <= MINIMUM_THRESHOLD:
        return None
    return encoding


def _find_changes(segments: tuple[Segment, ...]) -> tuple[EncodingChange, ...]:
    """Derive the encoding-change offsets from the per-window verdicts.

    A change is recorded when a window's verdict differs from the
    established one, except when the deviation is isolated --- the next
    window with a verdict agrees with the established encoding again.
    A lone dissenting window between two that agree is boundary noise
    (a clipped multi-byte sequence, a short run of the other text), not
    a switch.
    """
    verdicts = [_window_verdict(s) for s in segments]
    # next_known[i]: the nearest verdict after position i, for the
    # isolated-deviation look-ahead.
    next_known: list[str | None] = [None] * len(verdicts)
    following: str | None = None
    for i in range(len(verdicts) - 1, -1, -1):
        next_known[i] = following
        if verdicts[i] is not None:
            following = verdicts[i]
    changes: list[EncodingChange] = []
    current: str | None = None
    for segment, verdict, look_ahead in zip(
        segments, verdicts, next_known, strict=True
    ):
        if verdict is None or verdict == current:
            continue
        if current is not None:
            if look_ahead == current:
                continue  # isolated one-window deviation: boundary noise
            changes.append(EncodingChange(segment.start, current, verdict))
        current = verdict
    return tuple(changes)


def _validate_window(window_size: int, step: int | None) -> int:
    """Validate *window_size* and *step*, returning the effective step."""
    for name, value in (("window_size", window_size), ("step", step)):
        if value is None:
            continue
        if isinstance(value, bool) or not isinstance(value, int) or value < 1:
            msg = f"{name} must be a positive integer"
            raise ValueError(msg)
    return window_size if step is None else step


def run_segmented_detection(
    request: DetectionRequest,
    data: bytes,
    window_size: int,
    step: int,
) -> SegmentReport:
    """Run *request*'s pipeline over bounded windows of *data*.

    This is the implementation behind :func:`chardet.detect_segments`;
    the request is already resolved and *window_size*/*step* already
    validated.  Windows start at multiples of *step* and span up to
    *window_size* bytes of the sampled prefix ``data[:max_bytes]``; each
    is ranked independently and the per-window verdicts are reduced to
    change offsets by :func:`_find_changes`.

    :param request: The resolved detection options.
    :param data: The caller's bytes; only the first ``max_bytes`` are
        sampled.
    :param window_size: Maximum bytes per window.
    :param step: Distance between window starts.
    :returns: The per-window candidates and the change offsets.
    """
    sampled = data[: request.max_bytes]
    sampled_len = len(sampled)
    input_truncated = sampled_len < len(data)
    segments: list[Segment] = []
    for start in range(0, sampled_len, step):
        end = min(start + window_size, sampled_len)
        window = sampled[start:end]
        # Every window but a truly final one is a cut view of a larger
        # input; the flag keeps the pipeline's truncation handling honest.
        results = request.run(
            window,
            full_ranking=True,
            input_truncated=input_truncated or end < sampled_len,
        )
        candidates = request.render(results, window, all_results=True)
        segments.append(Segment(start, end, tuple(candidates)))
    segment_tuple = tuple(segments)
    return SegmentReport(
        segments=segment_tuple,
        changes=_find_changes(segment_tuple),
        sampled_bytes=sampled_len,
        total_bytes=len(data),
        window_size=window_size,
    )
