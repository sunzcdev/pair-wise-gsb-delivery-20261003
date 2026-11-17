"""Internal shared utilities for chardet."""

from __future__ import annotations

import codecs
from collections.abc import Callable
from dataclasses import dataclass

#: Default maximum number of bytes to examine during detection.
DEFAULT_MAX_BYTES: int = 200_000

#: Evidence cap: how much of the examination window the candidate-filtering,
#: validation, and probing stages consume before their answer is considered
#: converged (ADR-0006).  Exhaustive checks (BOM, magic, UTF-8, ASCII,
#: binary, escape presence) take no cap.  Must stay >= DEFAULT_MAX_BYTES so
#: every call using the default window is provably unaffected; a test
#: asserts the invariant.
EVIDENCE_CAP_BYTES: int = 256 * 1024

#: Chunk size for whole-window validity decodes.  Large enough that
#: per-chunk overhead vanishes, small enough that the transient decoded
#: ``str`` stays bounded regardless of input size.
_DECODE_CHUNK_SIZE: int = 1 << 20

#: Default minimum confidence threshold for filtering results.
MINIMUM_THRESHOLD: float = 0.20

#: One whole-window decode rejects a byte the codec cannot read.
DECODE_INVALID: int = 0
#: One whole-window decode consumes every byte, nothing deferred.
DECODE_COMPLETE: int = 1
#: One whole-window decode decodes up to a final sequence that a later byte
#: could still finish -- a character cut by the end of the data, not a bad
#: byte.
DECODE_TAIL_INCOMPLETE: int = 2


@dataclass(frozen=True, slots=True)
class DecodeAssessment:
    """One candidate's whole-window decode verdict.

    :ivar status: One of :data:`DECODE_INVALID`, :data:`DECODE_COMPLETE`, or
        :data:`DECODE_TAIL_INCOMPLETE`.
    :ivar pending: The bytes the decoder deferred, empty unless *status* is
        :data:`DECODE_TAIL_INCOMPLETE`.  They are a prefix of a sequence
        that a later byte could finish, so they are the entire difference
        between a cut character and a complete one.
    :ivar tail_is_all_evidence: ``True`` when the deferred tail is the only
        non-ASCII the decode produced -- the window was ASCII up to the
        cut, so the multi-byte character the candidate claims is the one
        the window was cut through and nothing else.  Meaningless (and
        always ``False``) unless *status* is :data:`DECODE_TAIL_INCOMPLETE`.
    """

    status: int
    pending: bytes
    tail_is_all_evidence: bool = False

    @property
    def is_complete(self) -> bool:
        """Whether the data ended on a character boundary."""
        return self.status == DECODE_COMPLETE

    @property
    def is_valid(self) -> bool:
        """Whether the data decodes, accepting a cut character at the end."""
        return self.status != DECODE_INVALID

    def is_acceptable(self, *, window_complete: bool) -> bool:
        """Whether this verdict may stand as evidence for a candidate.

        Invalid is never acceptable.  A cut character is acceptable unless
        it is *all* the candidate has on a window known to be the caller's
        whole input: nothing can complete it there, and it is the only
        thing supporting the candidate, so the candidate rests on a byte
        that does not exist.  Real multi-byte text ahead of the cut is
        evidence the candidate did earn, and a prefix window may yet hold
        the missing bytes, so both stay.

        :param window_complete: ``True`` when the data examined is the
            caller's whole input.
        :returns: ``True`` if the candidate survives this verdict.
        """
        if self.status == DECODE_INVALID:
            return False
        if self.status == DECODE_TAIL_INCOMPLETE:
            return not (window_complete and self.tail_is_all_evidence)
        return True


#: Cache of incremental-decoder classes, keyed by encoding name.
#:
#: :func:`decodes_without_error` is called once per candidate encoding per
#: detection -- roughly 86 times for :attr:`~chardet.enums.EncodingEra.ALL` --
#: and the factory lookup is a sixth of each call.  The class is immutable and
#: shared; only the per-call *instance* carries decoder state, so caching the
#: class is thread-safe while caching an instance would not be.
#:
#: Only successful lookups are cached, which bounds the cache by the number of
#: installed codecs.  A failed lookup returns ``False`` without storing
#: anything, so an unbounded stream of bogus names (a charset declaration in
#: markup is attacker-controlled) cannot grow it.
_INCREMENTAL_DECODERS: dict[str, Callable[..., codecs.IncrementalDecoder]] = {}


def _incremental_decoder(
    encoding: str,
) -> codecs.IncrementalDecoder | None:
    """Return a fresh incremental decoder for *encoding*, or ``None``.

    The class lookup is cached; ``None`` means the codec does not exist.
    Every decode below catches ``UnicodeError`` rather than
    ``UnicodeDecodeError`` because the utf-16/utf-32 incremental decoders
    raise a bare ``UnicodeError`` when the stream does not start with a BOM.
    """
    decoder_class = _INCREMENTAL_DECODERS.get(encoding)
    if decoder_class is None:
        try:
            decoder_class = codecs.getincrementaldecoder(encoding)
        except LookupError:
            return None
        _INCREMENTAL_DECODERS[encoding] = decoder_class
    return decoder_class()


def count_deleted(data: bytes, table: bytes) -> int:
    """Count how many bytes of *data* a deletion *table* would remove.

    ``bytes.translate`` with a deletion table allocates an output buffer
    proportional to the input, so counting a whole large window through one
    call spikes memory by roughly the input size.  Chunking keeps the
    transient bounded at one C call per chunk, and the count is identical:
    deletion is per-byte and carries no state across the split.

    :param data: The raw byte data to scan.
    :param table: Byte values to delete.
    :returns: The number of bytes of *data* present in *table*.
    """
    if len(data) <= _DECODE_CHUNK_SIZE:
        return len(data) - len(data.translate(None, table))
    count = 0
    for pos in range(0, len(data), _DECODE_CHUNK_SIZE):
        chunk = data[pos : pos + _DECODE_CHUNK_SIZE]
        count += len(chunk) - len(chunk.translate(None, table))
    return count


#: Upper bound on the byte appends one tail-completability check may try.
#: The shapes that matter settle far below it: a lead that begins no
#: character is refused on the very first pass (256 probes, 0 accepted
#: states), so CP932's ``EB``, EUC-KR's ``FF`` and ``80``, Big5's ``80``,
#: JOHAB's ``D4`` and UTF-8's ``ED A0`` are all proven dead in about a
#: millisecond.  A real prefix opens at least one accepted state, and the
#: search finds its completion well inside the bound too (UTF-8's slowest
#: lead needs 403).
#:
#: What the bound does not settle is a codec whose *grammar* branches: a
#: GB18030 lead Python defers but no character begins with (``80``, ``FF``)
#: fans out to 658,000 probes before it can be shown dead, and a stateful
#: shift (UTF-7's base64) has no fixed reach at all.  Proving those would
#: cost hundreds of milliseconds for a byte a real stream rarely ends on.
#: Running out therefore answers *completable* -- the pre-existing
#: ``final=False`` tolerance -- so no candidate that used to survive is
#: lost; the exact answers below the bound are what remove the corruption
#: this check exists for.
_MAX_TAIL_PROBES: int = 1024

#: How many bytes past the deferred tail a completion may need.  Four is a
#: whole four-byte sequence (GB18030, UTF-8), the longest any registry codec
#: reads as one character; a tail that is already `n` bytes long needs at
#: most `4 - n` more.
_MAX_TAIL_GROWTH: int = 4

#: Verdicts per ``(encoding, deferred tail)``.  A decode of a truncated
#: window asks the same question for the same short tail several times: once
#: per candidate during validity and again from the past-cap check.  The
#: keys are short byte strings, so a lookup is far cheaper than the search.
#: Capped so a long-lived process reading many distinct tails cannot grow it
#: without bound; past the cap the table is simply emptied and refilled.
_TAIL_COMPLETABLE: dict[tuple[str, bytes], bool] = {}
_TAIL_COMPLETABLE_MAX: int = 4096


def completes_deferred_tail(
    pending: bytes, encoding: str, state_flag: int = 0
) -> bool:
    """Return ``True`` if *pending* can start a character *encoding* decodes.

    The question a flush failure leaves open: are the bytes the decoder
    buffered a prefix of a sequence later bytes could finish (a character
    cut by the end of the data), or a sequence nothing can complete (a bad
    byte at the end of the window)?  Applied to the whole *pending* buffer,
    which is everything the decoder held when the data ran out.

    The search feeds fresh decoders real bytes -- the deferred tail plus
    candidate continuations -- and never installs a partial buffer behind a
    decoder's back.  That matters twice over.  Installed states are not
    validated: the stdlib decoders will carry a buffer no real stream could
    have produced, and the charmap codecs crash the interpreter on some of
    them, so ``setstate`` is not a safe way to ask this question at all.  A
    real feed keeps the answer to what the codec itself accepts.

    The reach is the codec's, not a hand-written lead/trail table's: CP932's
    ``EB`` leads no character and dies with nothing accepted, while
    EUC-KR's ``A1`` reaches a pair in one byte.  Duplicate decoder states
    collapse, so the frontier stays small where a codec's grammar branches.
    The bytes are only ever *appended*; nothing is trimmed off the data to
    make it decode.

    The search is bounded by :data:`_MAX_TAIL_PROBES`.  Running out answers
    *completable* -- the same verdict the plain ``final=False`` tolerance
    gave -- so no candidate that used to survive is lost; the exact answers
    below the bound are what remove corruption.

    :param pending: The decoder's buffered tail (``getstate()[0]``).
    :param encoding: Name of the codec that deferred it.
    :param state_flag: The rest of the decoder's state
        (``getstate()[1]``).  Used only to check that a fresh decoder fed
        *pending* lands in the same state: a codec that remembers a leading
        mark (``utf-16``/``utf-32`` sniff a BOM) does not, and its tail
        cannot be replayed, so the answer is *completable* rather than a
        guess.  For every other codec the flag is the same zero a fresh
        decoder starts at.
    :returns: ``True`` if some continuation could complete the tail.
    """
    key = (encoding, pending)
    cached = _TAIL_COMPLETABLE.get(key)
    if cached is not None:
        return cached
    result = _tail_reaches_a_character(pending, encoding, state_flag)
    if len(_TAIL_COMPLETABLE) >= _TAIL_COMPLETABLE_MAX:
        _TAIL_COMPLETABLE.clear()
    _TAIL_COMPLETABLE[key] = result
    return result


def _tail_reaches_a_character(
    pending: bytes, encoding: str, state_flag: int
) -> bool:
    """Run :func:`completes_deferred_tail`'s search over real byte feeds.

    Every candidate is decoded from a fresh decoder fed *pending* plus the
    appended bytes, so a state the search reaches is one the codec would
    reach.  Duplicate decoder states collapse to the first continuation that
    reached them, which keeps the frontier from exploding where a codec's
    grammar branches (GB18030's four-byte runs fan out to thousands of
    states within three appends).

    A tail cannot always be replayed from a fresh decoder: a codec whose
    stream begins with a mark (``utf-16``/``utf-32`` sniff a BOM and record
    it in ``getstate()[1]``) produces a *different* state when fed the tail
    alone, because the mark is gone.  The search detects that up front and
    answers *completable*: the deferred bytes are a prefix the original
    stream was in the middle of, and nothing here can prove otherwise.  That
    is the verdict the plain ``final=False`` tolerance gave, so no legitimate
    cut is newly rejected.
    """
    try:
        decoder_class = codecs.getincrementaldecoder(encoding)
    except LookupError:
        return False
    if not _replays_from_the_tail(pending, state_flag, decoder_class):
        return True
    # The continuation that produced each live decoder state, so a repeat of
    # that state costs nothing to re-explore.
    frontier: dict[tuple[bytes, int], bytes] = {(pending, 0): b""}
    probes = 0
    for _ in range(_MAX_TAIL_GROWTH):
        next_frontier: dict[tuple[bytes, int], bytes] = {}
        for appended in frontier.values():
            base = pending + appended
            for byte in range(256):
                probes += 1
                if probes > _MAX_TAIL_PROBES:
                    return True
                decoder = decoder_class()
                try:
                    decoder.decode(base + bytes((byte,)), final=False)
                except UnicodeError:
                    # The codec rejected this continuation; it reaches no
                    # character.
                    continue
                tail, flag = decoder.getstate()
                if not tail:
                    try:
                        decoder.decode(b"", final=True)
                    except UnicodeError:
                        continue
                    return True
                state = (tail, flag)
                if state not in next_frontier:
                    next_frontier[state] = appended + bytes((byte,))
        if not next_frontier:
            return False
        frontier = next_frontier
    return False


def _replays_from_the_tail(
    pending: bytes, state_flag: int, decoder_class: type
) -> bool:
    """Whether a fresh decoder fed *pending* lands in the original state.

    False for a codec that remembers a leading mark the tail no longer
    carries: ``utf-16`` and ``utf-32`` sniff a BOM and record it in the
    state flag, so feeding them only the trailing bytes produces a different
    state (or refuses outright).  Such a tail cannot be judged by replay, so
    the caller abstains rather than guess.
    """
    decoder = decoder_class()
    try:
        decoder.decode(pending, final=False)
    except UnicodeError:
        return False
    return decoder.getstate() == (pending, state_flag)


def decode_status(data: bytes, encoding: str) -> int:
    """Classify how *data* decodes under *encoding* in one incremental pass.

    The one judgment the pipeline makes about a candidate: a whole-window
    incremental decode with ``final=False``, then a flush.  A decoder fed
    chunk by chunk defers a sequence that is still open at the end of every
    chunk, so a character cut by the end of *data* costs nothing here; an
    invalid byte anywhere before the tail raises at that byte.  The flush
    then settles which of the two valid outcomes the deferred tail is:

    * :data:`DECODE_COMPLETE` -- the data ended on a character boundary.
    * :data:`DECODE_TAIL_INCOMPLETE` -- the flush raises: the decoder holds
      a prefix of a sequence that later bytes could still finish.  Every
      other byte decoded cleanly.
    * :data:`DECODE_INVALID` -- the decode raised before the end, or the
      deferred prefix no continuation could complete.

    The last case is why the flush is not the whole answer.  CPython's
    incremental decoders differ from codec to codec about how early they
    reject a prefix nothing can complete: some raise on the feed, some
    defer it and raise only on the flush, and UTF-8's defers ``ED A0`` and
    friends.  A tail is only *incomplete* when some continuation could
    finish it, so a flush failure is checked against the codec's own reach
    -- :func:`completes_deferred_tail` -- rather than trusted.

    Large inputs are fed in chunks and the decoded text is discarded, so
    the pass never allocates a matching ``str``.  Chunking is transparent:
    the decoder carries its state across calls.

    :param data: The raw byte data to classify.
    :param encoding: Name of the codec to test *data* against.
    :returns: One of :data:`DECODE_INVALID`, :data:`DECODE_COMPLETE`, or
        :data:`DECODE_TAIL_INCOMPLETE`.
    """
    return assess_decode(data, encoding).status


def assess_decode(data: bytes, encoding: str) -> DecodeAssessment:
    """Return the :func:`decode_status` verdict and the tail it deferred.

    :func:`decode_status` answers the yes/no questions; this is the same
    single decode pass for callers that also need to know *which* bytes the
    candidate deferred, which is how the validity stage tells a cut
    character at the end of real multi-byte text from a tail that is a
    candidate's only evidence.

    :param data: The raw byte data to classify.
    :param encoding: Name of the codec to test *data* against.
    :returns: A :class:`DecodeAssessment`; ``pending`` is empty unless the
        status is :data:`DECODE_TAIL_INCOMPLETE`, and
        ``tail_is_all_evidence`` is set when the text before the cut was
        pure ASCII.
    """
    decoder = _incremental_decoder(encoding)
    if decoder is None:
        return DecodeAssessment(DECODE_INVALID, b"")
    saw_non_ascii_text = False
    try:
        if len(data) <= _DECODE_CHUNK_SIZE:
            chunks = (data,)
        else:
            chunks = (
                data[pos : pos + _DECODE_CHUNK_SIZE]
                for pos in range(0, len(data), _DECODE_CHUNK_SIZE)
            )
        for chunk in chunks:
            text = decoder.decode(chunk, final=False)
            if not saw_non_ascii_text and not text.isascii():
                saw_non_ascii_text = True
    except UnicodeError:
        return DecodeAssessment(DECODE_INVALID, b"")
    try:
        decoder.decode(b"", final=True)
    except UnicodeError:
        pending, state_flag = decoder.getstate()
        if not pending or not completes_deferred_tail(pending, encoding, state_flag):
            return DecodeAssessment(DECODE_INVALID, b"")
        # "Only the tail stands out from ASCII": nothing decoded before it
        # carried a non-ASCII character, so the cut is this candidate's sole
        # multi-byte evidence.
        return DecodeAssessment(
            DECODE_TAIL_INCOMPLETE, pending, tail_is_all_evidence=not saw_non_ascii_text
        )
    return DecodeAssessment(DECODE_COMPLETE, b"")


def decodes_without_error(data: bytes, encoding: str) -> bool:
    """Return ``True`` if *data* decodes cleanly under *encoding*.

    Equivalent to ``data.decode(encoding, errors="strict")`` except that an
    incomplete multi-byte sequence at the *end* of *data* is accepted instead of
    raising.

    Detection input is nearly always a prefix of a larger whole.  Callers pass
    the first N bytes of a file, and chardet slices further on its own.
    For a two-byte encoding any of those cuts lands mid-character roughly half
    the time.

    A one-shot strict decode cannot tell a truncated tail from corrupt data: it
    raises either way, so the candidate is discarded and every CJK encoding can
    disappear from the candidate set over a single dangling lead byte. An
    incremental decoder with ``final=False`` defers the partial tail instead,
    while still raising on genuine corruption anywhere before it.

    The deferred tail is held to :func:`decode_status`'s judgment: a prefix
    no continuation could complete (``EB`` for CP932, ``ED A0`` for UTF-8)
    is corruption at the end of the window, not a cut character, and is
    rejected here exactly as a one-shot decode would.  Without that, a
    candidate would stay in the set -- and could win -- for data its codec
    cannot decode even as a complete file.

    Large inputs are fed in chunks and the decoded text is discarded, so a
    whole-window check never allocates a matching ``str``.  Chunking is
    transparent: the decoder carries its state across calls, so a sequence
    straddling a chunk edge decodes exactly as it would in one call.

    :param data: The raw byte data to test.
    :param encoding: Name of the codec to test *data* against.
    :returns: ``True`` if *data* decodes without error, ``False`` otherwise.
    """
    return decode_status(data, encoding) != DECODE_INVALID


def decodes_completely(data: bytes, encoding: str) -> bool:
    """Return ``True`` if *data* decodes under *encoding* with nothing deferred.

    The strict sibling of :func:`decodes_without_error`: a cut tail counts as
    an error rather than a deferred one.  This is the question that matters
    when *data* is the caller's entire input --- ``data.decode(encoding)``
    will make exactly this judgment.

    :param data: The raw byte data to test.
    :param encoding: Name of the codec to test *data* against.
    :returns: ``True`` if *data* decodes completely, ``False`` otherwise.
    """
    return decode_status(data, encoding) == DECODE_COMPLETE


def dangling_tail_with_ascii_prefix(data: bytes, encoding: str) -> bool:
    """Return ``True`` if *data* is ASCII text plus a cut character.

    One decode pass answers both halves of the decode-safety question:
    the tolerant (``final=False``) decode yields the text before any
    deferred tail, and the *status* of the whole window says whether that
    deferred tail is a character the data was cut through.  True means the
    candidate decoded real ASCII characters and then hit an incomplete
    multi-byte sequence at the end --- its only *non-ASCII* evidence is the
    cut character itself.

    Deliberately False when the deferred tail no continuation could finish
    (``EB`` under CP932, ``ED A0`` under UTF-8): that is corruption at the
    end of the window, not a cut character, and the candidate should be
    judged on the strength of its answer rather than excused.  Also False
    when the tolerant decode yields nothing at all (the entire input is one
    dangling sequence): that candidate has zero decoded evidence, not ASCII
    evidence, and a clipped multi-byte fragment is better served by the
    ranking's own judgment.

    :param data: The raw byte data to test.
    :param encoding: Name of the codec to decode *data* with.
    :returns: ``True`` if *data* decodes to non-empty pure ASCII with a
        completable sequence deferred at the end.
    """
    decoder = _incremental_decoder(encoding)
    if decoder is None:
        return False
    try:
        text = decoder.decode(data, final=False)
    except UnicodeError:
        return False
    if not text or not text.isascii():
        return False
    return assess_decode(data, encoding).status == DECODE_TAIL_INCOMPLETE
