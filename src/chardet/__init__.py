"""Universal character encoding detector — 0BSD-licensed rewrite."""

from __future__ import annotations

from collections.abc import Iterable

from chardet._request import _DEFAULT_CHUNK_SIZE, DetectionRequest
from chardet._utils import DEFAULT_MAX_BYTES, MINIMUM_THRESHOLD
from chardet._version import __version__
from chardet.detector import UniversalDetector
from chardet.enums import EncodingEra, LanguageFilter
from chardet.pipeline import DetectionDict, DetectionResult
from chardet.segments import (
    DEFAULT_WINDOW_SIZE,
    EncodingChange,
    Segment,
    SegmentReport,
    _validate_window,
    run_segmented_detection,
)

__all__ = [
    "DEFAULT_MAX_BYTES",
    "DEFAULT_WINDOW_SIZE",
    "MINIMUM_THRESHOLD",
    "DetectionDict",
    "DetectionResult",
    "EncodingChange",
    "EncodingEra",
    "LanguageFilter",
    "Segment",
    "SegmentReport",
    "UniversalDetector",
    "__version__",
    "detect",
    "detect_all",
    "detect_segments",
]


def __getattr__(name: str) -> object:
    """Resolve the deprecated ``chardet.equivalences`` submodule on demand.

    Before the 7.5 split, ``chardet/__init__.py`` imported that module for
    its own use, which as a side effect made ``chardet.equivalences`` work
    without an explicit ``import chardet.equivalences``.  This package now
    imports :mod:`chardet.output_names` instead, so the attribute is bound
    lazily here: importing the shim eagerly would fire its deprecation
    warning on every ``import chardet``.
    """
    if name == "equivalences":
        # ``from chardet import equivalences`` would re-enter this function:
        # the import machinery probes the parent package with ``hasattr``
        # first.  Kept local so the shim stays off the eager import path.
        import importlib  # noqa: PLC0415

        return importlib.import_module("chardet.equivalences")
    msg = f"module {__name__!r} has no attribute {name!r}"
    raise AttributeError(msg)


def detect(  # noqa: PLR0913
    byte_str: bytes | bytearray,
    should_rename_legacy: bool = False,
    encoding_era: EncodingEra = EncodingEra.ALL,
    chunk_size: int = _DEFAULT_CHUNK_SIZE,
    max_bytes: int = DEFAULT_MAX_BYTES,
    *,
    prefer_superset: bool = False,
    compat_names: bool = True,
    include_encodings: Iterable[str] | None = None,
    exclude_encodings: Iterable[str] | None = None,
    no_match_encoding: str = "cp1252",
    empty_input_encoding: str = "utf-8",
) -> DetectionDict:
    """Detect the encoding of the given byte string.

    :param byte_str: The byte sequence to detect encoding for.
    :param should_rename_legacy: Deprecated alias for *prefer_superset*.
    :param encoding_era: Restrict candidate encodings to the given era.
    :param chunk_size: Deprecated -- accepted for backward compatibility but
        has no effect.
    :param max_bytes: Maximum number of bytes to examine from *byte_str*.
    :param prefer_superset: If ``True``, remap subset encodings in the result
        to their decode-safe Windows/CP superset equivalents (e.g.,
        ISO-8859-1 -> Windows-1252, EUC-KR -> CP949).  Recommended when the
        result will be used to decode: detection examines at most
        *max_bytes* of input, and only the superset is guaranteed to decode
        bytes beyond that window.  The remap is skipped when the superset
        cannot decode the examined window itself (the Windows code pages
        leave a few C1 positions undefined that the ISO subsets map), so
        the reported name always decodes what was examined.  It is also
        skipped when *encoding_era*, *include_encodings*, or
        *exclude_encodings* rule the superset out, so the result never
        names an encoding those filters exclude.  If ``False``
        (default), the detected
        encoding is reported under its own name --- note this only skips
        the renaming step; it is not a promise of the *smallest* matching
        encoding, since detection may natively choose a superset that fits
        the data better.  The default will change to ``True`` in chardet
        8.0; pass ``False`` explicitly if you depend on subset names.
    :param compat_names: If ``True`` (default), return encoding names
        compatible with chardet 5.x/6.x.  If ``False``, return raw Python
        codec names.
    :param include_encodings: If given, restrict detection to only these
        encodings (names or aliases).
    :param exclude_encodings: If given, remove these encodings from the
        candidate set.
    :param no_match_encoding: Encoding to return when no candidate survives
        the pipeline.  Defaults to ``"cp1252"``.  It is returned as given,
        at confidence 0.10, and is the one answer that is not checked
        against the data: it is the caller's sentinel for "nothing fit".
    :param empty_input_encoding: Encoding to return for empty input.  Defaults
        to ``"utf-8"``.
    :returns: A dictionary with keys ``"encoding"``, ``"confidence"``,
        ``"language"``, and ``"mime_type"``.
    """
    request = DetectionRequest.resolve(
        should_rename_legacy=should_rename_legacy,
        encoding_era=encoding_era,
        chunk_size=chunk_size,
        max_bytes=max_bytes,
        prefer_superset=prefer_superset,
        compat_names=compat_names,
        include_encodings=include_encodings,
        exclude_encodings=exclude_encodings,
        no_match_encoding=no_match_encoding,
        empty_input_encoding=empty_input_encoding,
        stacklevel=3,
    )
    data = byte_str if isinstance(byte_str, bytes) else bytes(byte_str)
    return request.render(request.run(data), data[:max_bytes])[0]


def detect_all(  # noqa: PLR0913
    byte_str: bytes | bytearray,
    ignore_threshold: bool = False,
    should_rename_legacy: bool = False,
    encoding_era: EncodingEra = EncodingEra.ALL,
    chunk_size: int = _DEFAULT_CHUNK_SIZE,
    max_bytes: int = DEFAULT_MAX_BYTES,
    *,
    prefer_superset: bool = False,
    compat_names: bool = True,
    include_encodings: Iterable[str] | None = None,
    exclude_encodings: Iterable[str] | None = None,
    no_match_encoding: str = "cp1252",
    empty_input_encoding: str = "utf-8",
) -> list[DetectionDict]:
    """Detect all possible encodings of the given byte string.

    When *ignore_threshold* is False (the default), results with confidence
    <= MINIMUM_THRESHOLD (0.20) are filtered out.  If all results are below
    the threshold, the full unfiltered list is returned as a fallback so the
    caller always receives at least one result.

    :param byte_str: The byte sequence to detect encoding for.
    :param ignore_threshold: If ``True``, return all candidate encodings
        regardless of confidence score.
    :param should_rename_legacy: Deprecated alias for *prefer_superset*.
    :param encoding_era: Restrict candidate encodings to the given era.
    :param chunk_size: Deprecated -- accepted for backward compatibility but
        has no effect.
    :param max_bytes: Maximum number of bytes to examine from *byte_str*.
    :param prefer_superset: If ``True``, remap subset encodings in the
        results to their decode-safe Windows/CP superset equivalents.
        If ``False`` (default), skip the renaming --- not a promise of the
        smallest matching encoding.  The default will change to ``True``
        in chardet 8.0.  See :func:`detect` for details.
    :param compat_names: If ``True`` (default), return encoding names
        compatible with chardet 5.x/6.x.  If ``False``, return raw Python
        codec names.
    :param include_encodings: If given, restrict detection to only these
        encodings (names or aliases).
    :param exclude_encodings: If given, remove these encodings from the
        candidate set.
    :param no_match_encoding: Encoding to return when no candidate survives
        the pipeline.  Defaults to ``"cp1252"``.
    :param empty_input_encoding: Encoding to return for empty input.  Defaults
        to ``"utf-8"``.
    :returns: A list of dictionaries, sorted by descending confidence, one
        per encoding name.
    """
    request = DetectionRequest.resolve(
        should_rename_legacy=should_rename_legacy,
        encoding_era=encoding_era,
        chunk_size=chunk_size,
        max_bytes=max_bytes,
        prefer_superset=prefer_superset,
        compat_names=compat_names,
        include_encodings=include_encodings,
        exclude_encodings=exclude_encodings,
        no_match_encoding=no_match_encoding,
        empty_input_encoding=empty_input_encoding,
        stacklevel=3,
    )
    data = byte_str if isinstance(byte_str, bytes) else bytes(byte_str)
    return request.render(
        request.run(data, full_ranking=True),
        data[:max_bytes],
        all_results=True,
        ignore_threshold=ignore_threshold,
    )


def detect_segments(  # noqa: PLR0913
    byte_str: bytes | bytearray,
    window_size: int = DEFAULT_WINDOW_SIZE,
    step: int | None = None,
    encoding_era: EncodingEra = EncodingEra.ALL,
    max_bytes: int = DEFAULT_MAX_BYTES,
    *,
    prefer_superset: bool = False,
    compat_names: bool = True,
    include_encodings: Iterable[str] | None = None,
    exclude_encodings: Iterable[str] | None = None,
    no_match_encoding: str = "cp1252",
    empty_input_encoding: str = "utf-8",
) -> SegmentReport:
    """Detect encodings window by window, for concatenated inputs.

    A harvested file is sometimes several texts in different encodings
    concatenated into one byte stream, and a single whole-input verdict
    hides that: the dominant region wins and the rest is mislabeled.
    This function is the opt-in alternative to :func:`detect`: it runs
    the same detection pipeline independently over bounded windows of
    the input and reports each window's candidate ranking plus the byte
    offsets where the winning encoding changes.  It never decodes the
    input into one string --- which spans to convert, and with which
    candidate, remains the caller's decision.

    The conclusions cover only the sampled windows: at most *max_bytes*
    of *byte_str* is examined, and :attr:`SegmentReport.truncated` says
    whether the input was longer.  Windows start at multiples of *step*
    and span up to *window_size* bytes; both default to
    :data:`DEFAULT_WINDOW_SIZE` (non-overlapping).

    Two rules keep window-boundary artifacts from reading as switches:
    a multi-byte sequence clipped by a window edge is not, on its own,
    evidence of an encoding change (weak verdicts and isolated
    one-window deviations ground no change), and a pure-ASCII window
    grounds no change either, since ASCII decodes identically under
    every candidate.

    :param byte_str: The byte sequence to detect encodings for.
    :param window_size: Maximum bytes examined per window.
    :param step: Distance between window starts.  Defaults to
        *window_size* (non-overlapping windows).  A smaller *step*
        overlaps windows, which localizes changes more precisely at
        proportionally higher cost.
    :param encoding_era: Restrict candidate encodings to the given era.
    :param max_bytes: Maximum number of bytes to sample from *byte_str*.
    :param prefer_superset: If ``True``, remap subset encodings in the
        per-window candidates to their decode-safe Windows/CP superset
        equivalents.  If ``False`` (default), skip the renaming.  See
        :func:`detect` for details.
    :param compat_names: If ``True`` (default), report encoding names
        compatible with chardet 5.x/6.x.  If ``False``, report raw
        Python codec names.
    :param include_encodings: If given, restrict detection to only these
        encodings (names or aliases).
    :param exclude_encodings: If given, remove these encodings from the
        candidate set.
    :param no_match_encoding: Encoding to report for a window where no
        candidate survives the pipeline.  Defaults to ``"cp1252"``.
    :param empty_input_encoding: Encoding to report for empty windows
        (only reachable for empty input, which yields no windows).
    :returns: A :class:`SegmentReport` with one :class:`Segment` per
        window and the :class:`EncodingChange` offsets derived from
        their verdicts.
    """
    effective_step = _validate_window(window_size, step)
    request = DetectionRequest.resolve(
        encoding_era=encoding_era,
        max_bytes=max_bytes,
        prefer_superset=prefer_superset,
        compat_names=compat_names,
        include_encodings=include_encodings,
        exclude_encodings=exclude_encodings,
        no_match_encoding=no_match_encoding,
        empty_input_encoding=empty_input_encoding,
        stacklevel=3,
    )
    data = byte_str if isinstance(byte_str, bytes) else bytes(byte_str)
    return run_segmented_detection(request, data, window_size, effective_step)
