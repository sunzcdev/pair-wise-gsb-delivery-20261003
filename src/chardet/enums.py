"""Enumerations for chardet."""

from __future__ import annotations

import enum


class EncodingEra(enum.IntFlag):
    """Bit flags representing encoding eras for filtering detection candidates."""

    MODERN_WEB = 1
    LEGACY_ISO = 2
    LEGACY_MAC = 4
    LEGACY_REGIONAL = 8
    DOS = 16
    MAINFRAME = 32
    ALL = MODERN_WEB | LEGACY_ISO | LEGACY_MAC | LEGACY_REGIONAL | DOS | MAINFRAME


class DeclarationPolicy(enum.Enum):
    """How a charset declaration found in the data influences detection.

    A ``<meta charset>``, ``<?xml encoding>``, or PEP 263 cookie is a
    claim about the bytes, not a fact: pages carry stale, defaulted, or
    copied declarations.  The policy says how much weight that claim gets.

    - ``STRICT`` (default) — a declaration is honored only when its
      encoding (or its Windows superset) decodes the whole examination
      window and passes the caller's filters; such a declaration is the
      answer and detection stops.  A declaration that fails the window is
      skipped, so a stale prolog cannot hide a correct meta tag.
    - ``PREFER`` — like ``STRICT`` a compatible declaration is the answer,
      but detection does not stop: every other candidate is still checked
      and ranked behind it, so ``detect_all`` shows what the bytes alone
      would have said.  Deterministic early-stage answers (BOM, UTF-16/32
      patterns, escape sequences, magic numbers, binary) still stand on
      their own — a declaration never pre-empts them under this policy.
    - ``HINT`` — a declaration never commits.  It only lifts its encoding
      to a moderate confidence when the pipeline's own byte checks already
      kept that encoding in the ranking; a candidate those checks
      eliminated stays eliminated, so a declaration that lies about the
      bytes has no effect at all.
    """

    STRICT = "strict"
    PREFER = "prefer"
    HINT = "hint"


class DeclarationStatus(enum.Enum):
    """What the markup stage made of one charset declaration.

    Reported by :func:`chardet.pipeline.markup.explain_declarations`,
    which keeps the outcomes distinct that the detection result itself
    collapses: a declaration no codec answers to is not the same failure
    as a legal declaration whose bytes do not match.
    """

    #: The declaration is the answer (``answer`` holds the reported name).
    ACCEPTED = "accepted"
    #: The encoding decodes the window but the caller's
    #: ``include_encodings``/``exclude_encodings``/``encoding_era`` filters
    #: rule it out.  Ends the scan: the page named an encoding that fits
    #: its bytes, so a later declaration is not more trustworthy for it.
    FILTERED = "filtered"
    #: A legal label, but the encoding cannot decode the window — the
    #: declaration lies about the bytes.  The scan moves on to the next
    #: declaration.
    DECODE_MISMATCH = "decode-mismatch"
    #: No codec answers to the label, or the label is one a prescan never
    #: honors (``utf-7``, ``hz-gb-2312``).  The scan moves on.
    INVALID_LABEL = "invalid-label"


class LanguageFilter(enum.IntFlag):
    """Language filter flags for UniversalDetector (chardet 6.x API compat).

    Accepted but not used — our pipeline does not filter by language group.

    .. deprecated::
        Retained only for backward compatibility with chardet 6.x callers.
        Will be removed in a future major version.
    """

    CHINESE_SIMPLIFIED = 0x01
    CHINESE_TRADITIONAL = 0x02
    JAPANESE = 0x04
    KOREAN = 0x08
    NON_CJK = 0x10
    ALL = 0x1F
    CHINESE = CHINESE_SIMPLIFIED | CHINESE_TRADITIONAL
    CJK = CHINESE | JAPANESE | KOREAN
