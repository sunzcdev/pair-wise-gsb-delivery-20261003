# tests/test_internal_utils.py
"""Tests for the internal shared utilities in ``chardet._utils``."""

from __future__ import annotations

import codecs

from chardet._utils import (
    DECODE_COMPLETE,
    DECODE_INVALID,
    DECODE_TAIL_INCOMPLETE,
    _DECODE_CHUNK_SIZE,
    assess_decode,
    completes_deferred_tail,
    count_deleted,
    dangling_tail_with_ascii_prefix,
    decode_status,
    decodes_completely,
    decodes_without_error,
)
from chardet.registry import REGISTRY


def test_assess_decode_classifies_complete_invalid_and_cut():
    """One whole-window pass answers all three questions at once."""
    assert assess_decode("héllo".encode(), "utf-8").status == DECODE_COMPLETE
    assert assess_decode(b"h\xc3\xa9llo caf\xc3", "utf-8").status == (
        DECODE_TAIL_INCOMPLETE
    )
    assert assess_decode(b"h\xc3\xa9llo \xed\xa0", "utf-8").status == DECODE_INVALID
    # A single-byte codec never defers, so it is only ever complete/invalid.
    assert assess_decode(b"caf\xe9", "iso8859-1").status == DECODE_COMPLETE


def test_assess_decode_reports_the_deferred_bytes():
    """The pending bytes are what later stages re-use instead of decoding."""
    assessment = assess_decode(b"h\xc3\xa9llo caf\xc3", "utf-8")
    assert assessment.pending == b"\xc3"
    assert assessment.status == DECODE_TAIL_INCOMPLETE

    cjk = "こんにちは".encode("shift_jis") + b"\x82"
    assessment = assess_decode(cjk, "shift_jis")
    assert assessment.pending == b"\x82"
    assert not assessment.tail_is_all_evidence


def test_assess_decode_marks_a_tail_that_is_the_only_evidence():
    """ASCII up to the cut means the cut is all the non-ASCII there is."""
    assert assess_decode(b"hello world caf\xc3", "utf-8").tail_is_all_evidence
    assert not assess_decode("héllo caf\xc3".encode("latin-1"), "utf-8").is_valid


def test_a_dead_lead_is_invalid_not_a_cut_character():
    """A prefix no continuation completes is corruption, at the end too."""
    assert decode_status(b"hello \xeb", "cp932") == DECODE_INVALID
    assert decode_status(b"hello \x85", "cp932") == DECODE_INVALID
    assert decode_status(b"hello \xed\xa0", "utf-8") == DECODE_INVALID
    # And the real prefixes stay tolerated.
    assert decode_status(b"hello \x82", "cp932") == DECODE_TAIL_INCOMPLETE
    assert decode_status(b"hello \xc3", "utf-8") == DECODE_TAIL_INCOMPLETE


def test_completes_deferred_tail_follows_the_codec():
    """The reach is the codec's, so a dead lead answers False."""
    assert completes_deferred_tail(b"\x82", "shift_jis")
    assert not completes_deferred_tail(b"\xeb", "cp932")
    assert completes_deferred_tail(b"\xa1", "euc_kr")
    assert not completes_deferred_tail(b"\xff", "euc_kr")
    assert completes_deferred_tail(b"\xc3", "utf-8")
    assert not completes_deferred_tail(b"\xed\xa0", "utf-8")


def test_completes_deferred_tail_unknown_codec_is_false():
    assert not completes_deferred_tail(b"\xc3", "not-a-codec")


def test_completes_deferred_tail_never_installs_a_partial_buffer():
    """The search feeds real bytes; ``setstate`` crashes charmap codecs.

    A state installed behind a decoder's back is not one a real stream
    reached: the charmap codecs fault on some of them, so the check must
    never take that route.  Every codec's deferred one-byte tails are
    exercised here, which is the population the search actually sees.
    """
    for encoding, enc_info in REGISTRY.items():
        try:
            decoder_class = codecs.getincrementaldecoder(encoding)
        except LookupError:
            continue
        for byte in range(256):
            tail = bytes([byte])
            decoder = decoder_class()
            try:
                decoder.decode(tail, final=False)
                decoder.decode(b"", final=True)
            except UnicodeError:
                pass
            else:
                continue
            # Deferred: ask the search.  It must answer, not crash.
            assert isinstance(
                completes_deferred_tail(tail, encoding), bool
            ), (encoding, tail, enc_info.name)


def test_completes_deferred_tail_cache_is_bounded():
    """The verdict cache cannot grow without bound in a long-lived process."""
    import chardet._utils as utils  # noqa: PLC0415

    original = utils._TAIL_COMPLETABLE_MAX
    try:
        utils._TAIL_COMPLETABLE_MAX = 2
        utils._TAIL_COMPLETABLE.clear()
        for encoding, tail in (("cp932", b"\x82"), ("euc_kr", b"\xa1"), ("utf-8", b"\xc3")):
            completes_deferred_tail(tail, encoding)
        # Distinct tails keep filling it, but it is emptied before it can
        # pass the cap, so the bound holds however many are asked about.
        assert len(utils._TAIL_COMPLETABLE) <= 2
    finally:
        utils._TAIL_COMPLETABLE_MAX = original
        utils._TAIL_COMPLETABLE.clear()


def test_decodes_without_error_unknown_codec_is_false():
    assert decodes_without_error(b"hello", "not-a-codec") is False


def test_decodes_completely_unknown_codec_is_false():
    assert decodes_completely(b"hello", "not-a-codec") is False


def test_dangling_tail_unknown_codec_is_false():
    assert dangling_tail_with_ascii_prefix(b"hello", "not-a-codec") is False


def test_dangling_tail_invalid_bytes_is_false():
    """Genuine corruption before the tail is an error, not a deferred tail."""
    assert dangling_tail_with_ascii_prefix(b"\xff", "utf-8") is False


def test_count_deleted_chunked_matches_whole_pass():
    """Counting through bounded chunks must equal the one-shot count.

    Deletion is per-byte and carries no state across the chunk split, so a
    window larger than the chunk size counts identically.
    """
    data = b"a\x00" * (_DECODE_CHUNK_SIZE // 2 + 17)
    assert len(data) > _DECODE_CHUNK_SIZE
    assert count_deleted(data, b"\x00") == data.count(0)


def test_decodes_without_error_chunked_sequence_straddles_chunk_edge():
    """A multi-byte sequence crossing the chunk boundary decodes cleanly.

    The incremental decoder carries its state across chunks, so the split
    must be invisible even when it lands mid-character.
    """
    data = b"a" * (_DECODE_CHUNK_SIZE - 1) + "é".encode() + b"tail"
    assert len(data) > _DECODE_CHUNK_SIZE
    assert decodes_without_error(data, "utf-8") is True
