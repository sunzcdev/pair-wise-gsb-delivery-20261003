"""Normalization- and case-folding-aware substring search.

This module is intentionally independent from JMESPath's ``contains``
function: it does not change any existing semantics.  It provides a
*new* search that compares text under Unicode normalization plus case
folding, while every reported position still refers to the *original*
(unnormalized) haystack string, expressed as half-open Python string
indices ``(start, end)`` such that ``haystack[start:end]`` is the
matched surface text.

Rules (all deterministic):

1.  Key transform.  Both haystack and needle are turned into a
    comparable "key" with, per normalization segment::

        key(seg) = normalize(form, normalize(form, seg).casefold())

    ``form`` defaults to ``'NFC'``.  A normalization segment is a
    maximal run of characters that normalization cannot reorder or
    compose across: the string is split before every starter
    (``unicodedata.combining(ch) == 0``), except that Hangul
    ``L + V`` and ``LV + T`` jamo sequences are kept in one segment
    so they still compose into syllables.  Splitting this way makes
    per-segment normalization identical to whole-string
    normalization, while preserving an exact offset mapping.

2.  Position mapping.  Every character of the key carries the
    ``(start, end)`` span of the original segment it came from:

    * one character folding to many (``'ß' -> 'ss'``): every produced
      key character maps back to the single original character, so a
      match on either ``s`` reports the ``'ß'`` interval;
    * many characters composing to one (``'e' + U+0301 -> 'é'``,
      Hangul jamo): the composed key character maps back to the whole
      original cluster, so the reported interval covers every
      original character that participated;
    * a key-space match ``[i, j)`` maps to the original interval
      ``(span[i].start, span[j-1].end)``.  Normalized positions are
    never reported directly.

3.  Overlap.  With ``overlap=False`` (default) the search continues
    after the end of each key-space match; with ``overlap=True`` it
    continues one key character after the match start.  Because
    folding can collapse several key characters onto the same
    original span, overlapping matches may report duplicate original
    intervals; they are reported as-is, in key-space order.

4.  Empty query.  An empty needle matches at every gap of the
    original string, i.e. ``[(i, i) for i in range(len(haystack)+1)]``
    (mirroring how ``'' in s`` and ``s.find('')`` behave).

5.  No match.  An empty list is returned.  ``contains_normalized``
    returns ``True`` exactly when ``find_normalized`` is non-empty,
    so an empty needle is always contained (even in an empty
    haystack), matching Python's ``in`` semantics.

Example (pure strings)::

    >>> find_normalized('Straße', 'STRASSE')
    [(0, 6)]
    >>> find_normalized('Cafe\u0301', '\u00e9')  # 'e' + combining acute
    [(3, 5)]
    >>> find_normalized('aaaa', 'aa', overlap=True)
    [(0, 2), (1, 3), (2, 4)]
"""

import unicodedata

__all__ = ['find_normalized', 'contains_normalized']

_VALID_FORMS = ('NFC', 'NFD', 'NFKC', 'NFKD')


def _is_hangul_l(ch):
    return '\u1100' <= ch <= '\u1112'


def _is_hangul_v(ch):
    return '\u1161' <= ch <= '\u1175'


def _is_hangul_t(ch):
    return '\u11a8' <= ch <= '\u11c2'


def _is_hangul_lv(ch):
    # Precomposed syllable without a trailing consonant.
    o = ord(ch)
    return 0xAC00 <= o <= 0xD7A3 and (o - 0xAC00) % 28 == 0


def _is_boundary(prev, ch):
    """True if normalization cannot make ``prev`` and ``ch`` interact."""
    if unicodedata.combining(ch):
        # A non-starter may compose with / reorder around the previous
        # character; keep it in the same segment.
        return False
    if _is_hangul_v(ch) and _is_hangul_l(prev):
        return False  # L + V composes into an LV syllable
    if _is_hangul_t(ch) and _is_hangul_lv(prev):
        return False  # LV + T composes into an LVT syllable
    return True


def _segments(s):
    """Yield ``(start, segment)`` normalization-independent chunks."""
    start = 0
    prev = None
    for i, ch in enumerate(s):
        if i and _is_boundary(prev, ch):
            yield start, s[start:i]
            start = i
        prev = ch
    if s:
        yield start, s[start:]


def _transform_segment(seg, form):
    # casefold() can produce a non-normalized result (and can turn a
    # combining mark into a starter, e.g. U+0345 -> U+03B9), so the
    # output is normalized again.  Segment boundaries stay valid for
    # this second pass: a boundary always starts with a starter, and
    # case folding never produces Hangul jamo, so nothing new can
    # compose across segments.
    return unicodedata.normalize(form, unicodedata.normalize(form, seg).casefold())


def _key_and_spans(s, form):
    """Return ``(key, spans)`` for ``s``.

    ``spans[k]`` is the ``(start, end)`` interval in the *original*
    string that key character ``k`` was produced from.
    """
    parts = []
    spans = []
    for start, seg in _segments(s):
        key = _transform_segment(seg, form)
        parts.append(key)
        spans.extend([(start, start + len(seg))] * len(key))
    return ''.join(parts), spans


def _transform(s, form):
    return ''.join(_transform_segment(seg, form) for _, seg in _segments(s))


def _check_args(haystack, needle, form):
    if not isinstance(haystack, str):
        raise TypeError('haystack must be str, got %s'
                        % type(haystack).__name__)
    if not isinstance(needle, str):
        raise TypeError('needle must be str, got %s'
                        % type(needle).__name__)
    if form not in _VALID_FORMS:
        raise ValueError('form must be one of %r, got %r'
                         % (_VALID_FORMS, form))


def find_normalized(haystack, needle, form='NFC', overlap=False):
    """Find ``needle`` in ``haystack`` under normalization + case folding.

    Returns a list of half-open ``(start, end)`` intervals indexing
    into the *original* ``haystack`` (Python character indices), in
    order of occurrence.  See the module docstring for the exact
    mapping, overlap, empty-needle and no-match rules.
    """
    _check_args(haystack, needle, form)
    if needle == '':
        # Defined directly on original-string gaps; mirrors str.find.
        return [(i, i) for i in range(len(haystack) + 1)]
    key, spans = _key_and_spans(haystack, form)
    nkey = _transform(needle, form)
    matches = []
    pos = 0
    while True:
        i = key.find(nkey, pos)
        if i < 0:
            break
        j = i + len(nkey)
        matches.append((spans[i][0], spans[j - 1][1]))
        pos = i + 1 if overlap else j
    return matches


def contains_normalized(haystack, needle, form='NFC'):
    """True if ``needle`` occurs in ``haystack`` under the same rules.

    This is a new, separate predicate; JMESPath's standard ``contains``
    (and Python's ``in``) are untouched.  An empty needle is always
    contained, mirroring ``'' in s``.
    """
    return bool(find_normalized(haystack, needle, form=form))
