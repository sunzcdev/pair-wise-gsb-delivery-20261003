"""Stage 2a: Byte sequence validity filtering.

Two things travel out of this stage, because every later stage needs both:
*which* candidates decode the window and *how* -- completely, or up to a
character cut off at the end.  The distinction is the whole point.  Input is
usually a prefix of a larger whole, so a character cut by the end of the
window is not evidence against an encoding.  An illegal sequence -- a byte
the codec cannot read, or a lead no character begins with -- is evidence
against it wherever it sits, the tail included.  Tolerance for the end of
the window must not become tolerance for corruption at the end of the
window.

Three rules, in order:

* A candidate that fails anywhere in the window is dropped.  A deferred tail
  only counts as a cut when some continuation could finish it
  (:func:`~chardet._utils.completes_deferred_tail`); a tail nothing can
  complete (``EB`` for CP932, ``ED A0`` for UTF-8) is corruption.
* The cut is credited as evidence only when real multi-byte text decoded
  ahead of it.  A candidate whose *only* non-ASCII is the cut rests on a byte
  that is not there.
* That last shape is dropped outright once the window is confirmed to be the
  caller's whole file.  Nothing can complete the cut there, so the candidate
  is ASCII plus a fragment -- and the caller's own ``.decode()`` will hit
  the fragment.  A cut *after* real multi-byte text is kept even then: the
  evidence is present, and a genuine file ending mid-character cannot be
  decoded by anyone, so choosing among the candidates that can is the
  ranking's job (the decode-safety tiebreak), not this filter's.

Note: ``from __future__ import annotations`` is intentionally omitted because
this module is compiled with mypyc, which does not support PEP 563 string
annotations.
"""

from chardet._utils import DECODE_COMPLETE, DecodeAssessment, assess_decode
from chardet.registry import EncodingInfo


def assess_candidates(
    data: bytes,
    candidates: tuple[EncodingInfo, ...],
    *,
    window_complete: bool,
) -> tuple[tuple[EncodingInfo, DecodeAssessment], ...]:
    """Validate *candidates* against *data* and keep their decode verdicts.

    One whole-window decode per candidate, with the verdict carried forward
    rather than thrown away: later stages ask whether an answer decodes the
    window, whether it ended on a character boundary, and *which* tail it
    deferred.  Re-deriving any of that means decoding again, so it is done
    once here.

    A candidate survives when
    :meth:`~chardet._utils.DecodeAssessment.is_acceptable` allows its
    verdict, which is where the rules in the module docstring live -- a
    cut-character-only candidate on a confirmed-complete window is dropped
    there, which is exactly what the caller's own decode of it would do.

    The verdicts are returned rather than discarded.  A caller that only
    wants the names uses :func:`filter_by_validity`; the orchestrator keeps
    the pairs so the headers it threads to the ranking carry the same
    judgment validity made, instead of each later stage re-decoding the
    window to reconstruct it.

    :param data: The raw byte data to test.
    :param candidates: Encoding candidates to validate.
    :param window_complete: ``True`` when *data* is a confirmed-complete
        window, so a cut that is a candidate's only evidence is not
        tolerated.
    :returns: ``(encoding, assessment)`` pairs for the candidates that
        decode *data*, in the order they were given.
    """
    if not data:
        return tuple(
            (enc, DecodeAssessment(status=DECODE_COMPLETE, pending=b""))
            for enc in candidates
        )
    validated: list[tuple[EncodingInfo, DecodeAssessment]] = []
    for enc in candidates:
        assessment = assess_decode(data, enc.name)
        if assessment.is_acceptable(window_complete=window_complete):
            validated.append((enc, assessment))
    return tuple(validated)


def filter_by_validity(
    data: bytes,
    candidates: tuple[EncodingInfo, ...],
    *,
    window_complete: bool = False,
) -> tuple[EncodingInfo, ...]:
    """Filter candidates to only those where *data* decodes without errors.

    The name-and-drop view of :func:`assess_candidates`; see there and the
    module docstring for the tolerance rule.

    :param data: The raw byte data to test.
    :param candidates: Encoding candidates to validate.
    :param window_complete: ``True`` when *data* is the caller's whole input.
    :returns: The subset of *candidates* that can decode *data*.
    """
    return tuple(
        enc
        for enc, _ in assess_candidates(
            data, candidates, window_complete=window_complete
        )
    )
