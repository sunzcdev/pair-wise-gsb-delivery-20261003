"""One detection request: resolved options, the pipeline run, and rendering.

:func:`chardet.detect`, :func:`chardet.detect_all`,
:class:`chardet.UniversalDetector` and the ``chardetect`` CLI all ask the
same question with the same options.  :class:`DetectionRequest` owns that
sequence once: validate and resolve the public keyword arguments
(:meth:`DetectionRequest.resolve`), run the pipeline with them
(:meth:`DetectionRequest.run`), and turn the ranked results into the dicts
callers see (:meth:`DetectionRequest.render`).  The public functions are
thin adapters over it.
"""

from __future__ import annotations

import dataclasses
import warnings
from collections.abc import Iterable

from chardet._utils import DEFAULT_MAX_BYTES, MINIMUM_THRESHOLD
from chardet.enums import EncodingEra
from chardet.output_names import apply_compat_names, apply_preferred_superset
from chardet.pipeline import DetectionDict, DetectionResult
from chardet.pipeline.orchestrator import run_pipeline
from chardet.registry import _validate_encoding, normalize_encodings

#: Default chunk_size value (deprecated, kept for backward-compat signatures).
_DEFAULT_CHUNK_SIZE: int = 65_536


def _warn_deprecated_chunk_size(chunk_size: int, stacklevel: int = 3) -> None:
    """Emit a deprecation warning if *chunk_size* differs from the default."""
    if chunk_size != _DEFAULT_CHUNK_SIZE:
        warnings.warn(
            "chunk_size is not used in this version of chardet and will be ignored",
            DeprecationWarning,
            stacklevel=stacklevel,
        )


def _validate_max_bytes(max_bytes: int) -> None:
    """Raise ValueError if *max_bytes* is not a positive integer."""
    if isinstance(max_bytes, bool) or not isinstance(max_bytes, int) or max_bytes < 1:
        msg = "max_bytes must be a positive integer"
        raise ValueError(msg)


def _validate_confidence_threshold(value: float | None, name: str) -> float | None:
    """Validate one optional confidence threshold, or pass ``None`` through."""
    if value is None:
        return None
    if (
        isinstance(value, bool)
        or not isinstance(value, (int, float))
        or not 0.0 <= value <= 1.0
    ):
        msg = f"{name} must be a number between 0.0 and 1.0"
        raise ValueError(msg)
    return float(value)


def _decode_window(window: bytes, encoding: str | None) -> str | None:
    """Decode *window* under *encoding*, or return ``None`` when that fails."""
    if encoding is None:
        return None
    try:
        return window.decode(encoding)
    except (LookupError, UnicodeDecodeError):
        return None


def _group_equivalent(
    results: list[DetectionResult], window: bytes
) -> list[list[DetectionResult]]:
    """Group ranked candidates that are the same encoding for *window*.

    Two candidates share a group when they decode the examined window to
    the same text — aliases of one codec trivially so, and near-identical
    pairs such as ISO-8859-1 and Windows-1252 whenever the window holds
    none of the bytes they map differently.  A candidate whose name
    cannot decode the window groups only with an exact name match.  The
    first member of each group is its highest-confidence one, since
    *results* arrives ranked; the groups are ordered the same way.
    """
    groups: list[list[DetectionResult]] = []
    texts: list[str | None] = []  # each group's leader's decode of the window
    for result in results:
        text = _decode_window(window, result.encoding)
        for group, leader_text in zip(groups, texts, strict=True):
            same_text = text is not None and text == leader_text
            same_failure = (
                text is None
                and leader_text is None
                and result.encoding == group[0].encoding
            )
            if same_text or same_failure:
                group.append(result)
                break
        else:
            groups.append([result])
            texts.append(text)
    return groups


def _resolve_prefer_superset(
    *, should_rename_legacy: bool, prefer_superset: bool, stacklevel: int = 3
) -> bool:
    """Resolve the deprecated *should_rename_legacy* into *prefer_superset*."""
    if should_rename_legacy:
        warnings.warn(
            "should_rename_legacy is deprecated, use prefer_superset instead",
            DeprecationWarning,
            stacklevel=stacklevel,
        )
        return True
    return prefer_superset


@dataclasses.dataclass(frozen=True, slots=True)
class DetectionRequest:
    """The resolved options of one detection, and the work they drive.

    Build one with :meth:`resolve`, which validates the public keyword
    arguments; the fields hold canonical values only.
    """

    encoding_era: EncodingEra = EncodingEra.ALL
    max_bytes: int = DEFAULT_MAX_BYTES
    prefer_superset: bool = False
    compat_names: bool = True
    include_encodings: frozenset[str] | None = None
    exclude_encodings: frozenset[str] | None = None
    no_match_encoding: str = "cp1252"
    empty_input_encoding: str = "utf-8"
    min_confidence: float | None = None
    min_confidence_gap: float | None = None

    @property
    def _has_confidence_thresholds(self) -> bool:
        """Whether either confidence threshold option is set."""
        return self.min_confidence is not None or self.min_confidence_gap is not None

    @classmethod
    def resolve(  # noqa: PLR0913
        cls,
        *,
        should_rename_legacy: bool = False,
        encoding_era: EncodingEra = EncodingEra.ALL,
        chunk_size: int = _DEFAULT_CHUNK_SIZE,
        max_bytes: int = DEFAULT_MAX_BYTES,
        prefer_superset: bool = False,
        compat_names: bool = True,
        include_encodings: Iterable[str] | None = None,
        exclude_encodings: Iterable[str] | None = None,
        no_match_encoding: str = "cp1252",
        empty_input_encoding: str = "utf-8",
        min_confidence: float | None = None,
        min_confidence_gap: float | None = None,
        stacklevel: int = 2,
    ) -> DetectionRequest:
        """Validate the public options and resolve them into a request.

        Takes the keyword arguments exactly as :func:`chardet.detect`
        receives them, deprecated ones included, and owns their validation
        and deprecation warnings.

        :param stacklevel: Where the deprecation warnings point, counted as
            :func:`warnings.warn` counts it from the caller of this method:
            ``2`` is that caller, ``3`` the caller's caller (what a public
            function passes so the warning names the user's line).
        :raises ValueError: If *max_bytes* is not a positive integer, an
            encoding name is unknown, or a confidence threshold is not a
            number between 0.0 and 1.0.
        """
        # Each helper adds its own frame, hence the +1.
        _warn_deprecated_chunk_size(chunk_size, stacklevel=stacklevel + 1)
        _validate_max_bytes(max_bytes)
        prefer_superset = _resolve_prefer_superset(
            should_rename_legacy=should_rename_legacy,
            prefer_superset=prefer_superset,
            stacklevel=stacklevel + 1,
        )
        return cls(
            encoding_era=encoding_era,
            max_bytes=max_bytes,
            prefer_superset=prefer_superset,
            compat_names=compat_names,
            include_encodings=normalize_encodings(
                include_encodings, "include_encodings"
            ),
            exclude_encodings=normalize_encodings(
                exclude_encodings, "exclude_encodings"
            ),
            no_match_encoding=_validate_encoding(
                no_match_encoding, "no_match_encoding"
            ),
            empty_input_encoding=_validate_encoding(
                empty_input_encoding, "empty_input_encoding"
            ),
            min_confidence=_validate_confidence_threshold(
                min_confidence, "min_confidence"
            ),
            min_confidence_gap=_validate_confidence_threshold(
                min_confidence_gap, "min_confidence_gap"
            ),
        )

    def run(
        self,
        data: bytes,
        *,
        full_ranking: bool = False,
        input_truncated: bool = False,
    ) -> list[DetectionResult]:
        """Run the detection pipeline on *data* with this request's options.

        :param data: The caller's bytes; the pipeline examines at most
            :attr:`max_bytes` of them.
        :param full_ranking: Rank every candidate, as ``detect_all`` needs.
            The confidence thresholds need it too: the gap is measured
            against the second *distinct* encoding, which the pruned
            ranking does not guarantee, and a refused verdict lists the
            full candidate set.
        :param input_truncated: *data* is already a cut-off view of the
            caller's input (``UniversalDetector`` caps its buffer).
        :returns: The ranked results, best first.
        """
        return run_pipeline(
            data,
            self.encoding_era,
            max_bytes=self.max_bytes,
            include_encodings=self.include_encodings,
            exclude_encodings=self.exclude_encodings,
            no_match_encoding=self.no_match_encoding,
            empty_input_encoding=self.empty_input_encoding,
            full_ranking=full_ranking or self._has_confidence_thresholds,
            input_truncated=input_truncated,
        )

    def render(
        self,
        results: list[DetectionResult],
        window: bytes,
        *,
        all_results: bool = False,
        ignore_threshold: bool = False,
    ) -> list[DetectionDict]:
        """Turn ranked results into the dicts callers see.

        Applies the ``prefer_superset`` remap and then the display names,
        each checked against *window* so the reported name decodes it.
        That costs one whole-window decode for a remapped superset and one
        for a display name in
        :data:`~chardet.output_names.NARROWER_DISPLAY_NAMES`, none
        otherwise.

        :param results: Ranked results from :meth:`run`.
        :param window: The bytes the verdict is about (``data[:max_bytes]``).
        :param all_results: Render the whole ranking the way
            :func:`chardet.detect_all` reports it: drop results at or below
            :data:`~chardet.MINIMUM_THRESHOLD` unless that leaves nothing,
            sort by confidence, and list each name once.  When ``False``,
            render only the top result, subject to the confidence
            thresholds when they are set.
        :param ignore_threshold: With *all_results*, keep the results at or
            below the threshold too.
        :returns: The rendered dicts, best first.
        """
        if not all_results:
            if self._has_confidence_thresholds and results[0].statistical:
                return [self._render_verdict(results, window)]
            return [self._render_one(results[0].to_dict(), window)]
        dicts = [r.to_dict() for r in results]
        if not ignore_threshold:
            filtered = [d for d in dicts if d["confidence"] > MINIMUM_THRESHOLD]
            if filtered:
                dicts = filtered
        return self._rank_unique([self._render_one(d, window) for d in dicts])

    def _render_verdict(
        self, results: list[DetectionResult], window: bytes
    ) -> DetectionDict:
        """Apply the confidence thresholds to a statistical ranking.

        The top answer stands when it clears every threshold the request
        sets.  Otherwise the verdict is uncertain: ``encoding=None`` at
        confidence 0.0, with the grouped candidates attached under
        ``"candidates"`` — chardet declines to pick rather than dress a
        guess up as an answer.  The gap is measured between groups of
        equivalent candidates (see :func:`_group_equivalent`), so one
        encoding under two names never counts as its own runner-up.
        """
        groups = _group_equivalent(results, window)
        top = groups[0][0].confidence
        if self.min_confidence is not None and top < self.min_confidence:
            uncertain = True
        elif self.min_confidence_gap is not None:
            second = groups[1][0].confidence if len(groups) > 1 else 0.0
            uncertain = top - second < self.min_confidence_gap
        else:
            uncertain = False
        if not uncertain:
            return self._render_one(results[0].to_dict(), window)
        candidates = self._rank_unique(
            [self._render_one(group[0].to_dict(), window) for group in groups]
        )
        return {
            "encoding": None,
            "confidence": 0.0,
            "language": None,
            "mime_type": None,
            "candidates": candidates,
        }

    def _render_one(self, d: DetectionDict, window: bytes) -> DetectionDict:
        """Apply the output remaps to one result dict, in place."""
        if self.prefer_superset:
            apply_preferred_superset(
                d,
                window,
                include_encodings=self.include_encodings,
                exclude_encodings=self.exclude_encodings,
                encoding_era=self.encoding_era,
            )
        if self.compat_names:
            apply_compat_names(d, window)
        return d

    @staticmethod
    def _rank_unique(dicts: list[DetectionDict]) -> list[DetectionDict]:
        """Sort by confidence and keep the first dict for each name."""
        dicts.sort(key=lambda d: d["confidence"], reverse=True)
        # The superset remap can land two candidates on one name (ISO-8859-7
        # and Windows-1253 both become Windows-1253).  Keep the first, which
        # carries the higher confidence; the sort is stable, so ties keep the
        # pipeline's order.
        seen: set[str | None] = set()
        unique: list[DetectionDict] = []
        for d in dicts:
            if d["encoding"] not in seen:
                seen.add(d["encoding"])
                unique.append(d)
        return unique
