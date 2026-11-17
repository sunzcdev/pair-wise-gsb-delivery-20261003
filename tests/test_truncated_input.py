# tests/test_truncated_input.py
"""Detection must be stable when the input is a prefix of a larger whole.

Callers routinely detect on a slice -- an editor reads the first 4 kB or 64 kB
of a file and hands over the buffer -- and chardet slices further on its own,
``data[:max_bytes]`` in the orchestrator and ``data[:_SCAN_LIMIT]`` in
:func:`chardet.pipeline.markup.detect_markup_charset`.  For a two-byte encoding
any of those cuts lands mid-character about half the time.

Validity filtering used to decode with a one-shot strict decode, which cannot
tell a truncated tail from corrupt data, so a single dangling lead byte dropped
every CJK candidate and the answer came down to input-length parity.
"""

from __future__ import annotations

import pytest

import chardet
from chardet import UniversalDetector
from chardet._utils import (
    DECODE_TAIL_INCOMPLETE,
    DEFAULT_MAX_BYTES,
    assess_decode,
    dangling_tail_with_ascii_prefix,
    decodes_completely,
    decodes_without_error,
)
from chardet.enums import EncodingEra
from chardet.output_names import decodes_under_public_name
from chardet.pipeline.markup import detect_markup_charset
from chardet.pipeline.validity import assess_candidates, filter_by_validity
from chardet.registry import REGISTRY, get_candidates

_ZH = "這是一個用於測試編碼偵測的中文句子，內容足夠長以便統計模型能夠正確判斷。"
_JA = "これは文字コード判定のテストに用いる日本語の文章です。統計モデルが正しく判定できる長さにしています。"
_KO = "이것은 문자 인코딩 감지를 테스트하기 위한 한국어 문장입니다. 통계 모델이 올바르게 판단할 수 있을 만큼 충분히 깁니다."

# One entry per multi-byte family.  The expected value is a prefix match rather
# than an exact name because a superset (gb18030 for gbk, cp932 for shift_jis,
# cp949 for euc_kr) is an equally correct answer -- the point is that the family
# survives, not which member wins.
_MULTIBYTE_SAMPLES = [
    ("gbk", (_ZH * 4).encode("gbk"), ("gb",)),
    ("big5", (_ZH * 4).encode("big5"), ("big5",)),
    ("shift_jis", (_JA * 4).encode("shift_jis"), ("shift_jis", "cp932")),
    ("euc_jp", (_JA * 4).encode("euc_jp"), ("euc-jp", "euc_jp")),
    ("euc_kr", (_KO * 4).encode("euc_kr"), ("euc-kr", "euc_kr", "cp949")),
    ("utf-8", (_ZH * 4).encode(), ("utf-8",)),
]

# Four consecutive prefixes cover every byte-boundary offset for encodings up to
# four bytes per character, so at least one prefix per run cuts mid-character.
_PREFIX_OFFSETS = (0, 1, 2, 3, 4)


@pytest.mark.parametrize(("name", "data", "expected"), _MULTIBYTE_SAMPLES)
def test_detection_stable_across_truncated_prefixes(
    name: str, data: bytes, expected: tuple[str, ...]
) -> None:
    for offset in _PREFIX_OFFSETS:
        prefix = data[: len(data) - offset]
        result = chardet.detect(prefix, encoding_era=EncodingEra.ALL)
        encoding = (result["encoding"] or "").lower()
        assert encoding.startswith(expected), (
            f"{name} minus {offset} byte(s) detected as {result['encoding']}"
        )


def test_truncated_tail_keeps_encoding_as_candidate():
    data = (_ZH * 4).encode("gbk")[:-1]
    candidates = tuple(
        e for e in get_candidates(EncodingEra.ALL) if e.name.startswith("gb")
    )
    assert candidates
    assert filter_by_validity(data, candidates)


def test_tail_tolerance_is_only_for_a_character_the_data_could_finish():
    """A cut sequence at the end is tolerated; a dead one is not.

    The window really was cut through the next character: the trailing byte
    is a lead and one more byte completes it.  A lead no character begins
    with is corruption at the end of the window exactly as it would be in
    the middle -- the tolerance is for a prefix of a valid sequence, not for
    whatever byte happens to be last.
    """
    cp932_text = "これはテストです".encode("cp932")
    assert assess_decode(cp932_text[:-1], "cp932").is_valid
    # 0xEB leads no CP932 character under any second byte.
    assert not assess_decode(cp932_text + b"\xeb", "cp932").is_valid
    cp932_only = tuple(e for e in get_candidates(EncodingEra.ALL) if e.name == "cp932")
    assert filter_by_validity(cp932_text + b"\xeb", cp932_only) == ()


def test_dead_lead_bytes_eliminate_the_candidate():
    """Leads no character begins with are corruption, not cut characters.

    CPython's incremental decoders defer these prefixes instead of raising,
    so a ``final=False`` decode alone used to accept them -- and the codec
    would still reject the same bytes as a complete file.
    """
    for data, encoding, tail in (
        (b"hello ", "cp932", b"\xeb"),
        (b"hello ", "cp932", b"\x85"),
        (b"\xbf\xc8\xb3\xe7", "euc_kr", b"\xff"),
        (b"\xbf\xc8\xb3\xe7", "euc_kr", b"\x80"),
        (b"\xd6\xd0\xce\xc4", "gbk", b"\x80"),
        (b"h\xc3\xa9llo ", "utf-8", b"\xed\xa0"),
    ):
        candidate = data + tail
        assert not decodes_without_error(candidate, encoding), (encoding, tail)
        assert not decodes_completely(candidate, encoding), (encoding, tail)


def test_a_cut_character_is_deferred_not_invalid():
    """The same path must still keep a real prefix of a character."""
    for data, encoding in (
        (b"\x82\xb1\x82\xf1\x82", "shift_jis"),
        (b"\xbf\xc8\xb3\xe7\xa1", "euc_kr"),
        (b"\xd6\xd0\xce\xc4\xd6", "gbk"),
        (b"h\xc3\xa9llo caf\xc3", "utf-8"),
        (b"h\xc3\xa9llo caf\xe3\x81", "utf-8"),
    ):
        assert decodes_without_error(data, encoding), (encoding, data)
        assert not decodes_completely(data, encoding), (encoding, data)


def test_cut_character_after_real_text_survives_a_complete_window():
    """Real multi-byte text is evidence; validity keeps the candidate for it.

    A whole file genuinely ending mid-character cannot be decoded by anybody,
    so validity does not pretend the candidate is bad -- the ranking's own
    decode-safety tiebreak picks among the candidates that can decode it.
    """
    body = (_ZH * 4).encode("gbk")[:-1]
    gbk_only = tuple(e for e in get_candidates(EncodingEra.ALL) if e.name == "gbk")
    assert filter_by_validity(body, gbk_only, window_complete=True) == gbk_only


def test_tail_only_evidence_does_not_survive_a_complete_window():
    """ASCII plus a cut sequence is not evidence on the caller's whole file.

    Nothing follows the file, so the missing character can never arrive; the
    candidate's only non-ASCII was the byte that is not there.
    """
    data = b"hello world caf\xc3"
    utf8_only = tuple(e for e in get_candidates(EncodingEra.ALL) if e.name == "utf-8")
    assert filter_by_validity(data, utf8_only) == utf8_only
    assert filter_by_validity(data, utf8_only, window_complete=True) == ()


def test_assess_candidates_carries_the_decode_state():
    """Candidates leave validity with their verdict, not just their names."""
    cut = "これはテストです".encode("cp932")[:-1]
    cp932_only = tuple(e for e in get_candidates(EncodingEra.ALL) if e.name == "cp932")
    assessed = assess_candidates(cut, cp932_only, window_complete=False)
    assert len(assessed) == 1
    encoding, assessment = assessed[0]
    assert encoding.name == "cp932"
    assert assessment.status == DECODE_TAIL_INCOMPLETE
    assert assessment.pending
    assert not assessment.tail_is_all_evidence

    ascii_cut = b"hello world caf\xc3"
    utf8_only = tuple(e for e in get_candidates(EncodingEra.ALL) if e.name == "utf-8")
    (_, assessment) = assess_candidates(ascii_cut, utf8_only, window_complete=False)[0]
    assert assessment.tail_is_all_evidence


def test_illegal_trail_byte_still_eliminates_encoding():
    # 0xD6 is a valid gbk lead byte; 0x20 is not a valid trail byte.
    assert not decodes_without_error(b"\xd5\xe2\xca\xc7\xd6\x20", "gbk")


def test_unmapped_bytes_still_eliminate_encoding():
    assert not decodes_without_error(b"\xd5\xe2\xca\xc7\xff\xff", "gbk")


def test_corruption_before_a_truncated_tail_is_still_caught():
    # Corrupt pair mid-buffer, then a dangling lead byte at the end.  Tolerating
    # the tail must not tolerate the corruption ahead of it.
    assert not decodes_without_error(b"\xd5\xe2\xff\xff\xd2\xbb\xd6", "gbk")


def test_markup_declaration_survives_the_scan_limit_cut():
    # A complete, well-formed page with an honest declaration.  One byte of
    # padding shifts where every internal cut lands relative to the two-byte
    # characters -- which must not change the answer.
    body = (_ZH * 200).encode("gbk")
    for pad in (0, 1):
        data = b'<meta charset="gbk">' + b" " * pad + body
        assert decodes_without_error(data, "gbk")
        result = detect_markup_charset(
            data, frozenset(e.name for e in REGISTRY.values())
        )
        assert result is not None, f"pad={pad} rejected an honest declaration"
        assert result.mime_type == "text/html"


def test_detection_survives_the_max_bytes_cut():
    # Likewise for the orchestrator's own data[:max_bytes] slice, on a complete
    # file larger than that limit.
    body = _ZH.encode("gbk")
    data = body * (DEFAULT_MAX_BYTES // len(body) + 200)
    assert len(data) > DEFAULT_MAX_BYTES
    for pad in (0, 1):
        result = chardet.detect(b" " * pad + data, encoding_era=EncodingEra.ALL)
        assert (result["encoding"] or "").lower().startswith("gb"), (
            f"pad={pad} detected as {result['encoding']}"
        )


def test_decodes_completely_rejects_the_tail_tolerance_gap():
    """The strict sibling: same bytes, final=True flips the verdict."""
    dangling = "mamá".encode("iso-8859-1")  # ends in a lone 0xE1 lead
    assert decodes_without_error(dangling, "utf-8")
    assert not decodes_completely(dangling, "utf-8")
    assert decodes_completely(dangling, "windows-1250")


def test_dangling_tail_helper_classifies_evidence():
    r"""The flip's evidence test: non-empty ASCII prefix plus a deferred tail.

    ``b"mam\\xe1"`` decodes to ASCII ``mam`` with ``0xE1`` deferred: True.
    A clipped emoji is one dangling sequence with *nothing* decoded, which
    is zero evidence rather than ASCII evidence: False.  A mid-cut CJK
    fragment decodes real multi-byte characters first: False.
    """
    assert dangling_tail_with_ascii_prefix(b"mam\xe1", "utf-8")
    assert not dangling_tail_with_ascii_prefix(b"\xf0\x9f\x98", "utf-8")
    cjk = "こんにちは".encode("shift_jis")[:-1]
    assert not dangling_tail_with_ascii_prefix(cjk, "shift_jis")
    # Complete input has no deferred tail, so it is not a dangling shape.
    assert not dangling_tail_with_ascii_prefix(b"mama", "utf-8")


def test_flip_verifies_the_public_name_too():
    """A rival must decode under the name the caller will actually use.

    ``euc_jis_2004`` decodes JIS X 0213 pairs that plain ``EUC-JP`` (its
    ``compat_names=True`` display name) rejects, so promoting it would
    re-create the issue-380 symptom under the public name.
    """
    data = b"hello \xa2\xaf"
    assert decodes_completely(data, "euc_jis_2004")
    assert not decodes_completely(data, "EUC-JP")
    assert not decodes_under_public_name(data, "euc_jis_2004", complete=True)


def test_internal_slicing_keeps_the_prefix_tolerance():
    """When chardet itself sliced, a dangling tail is chardet's own cut.

    The same four bytes that flip to a decodable Latin answer as complete
    input stay utf-8 when they are a ``max_bytes`` slice of longer data ---
    the caller's input goes on past the cut, so the strict-decode question
    does not apply to the slice.

    The utf-8 assertion pins the current statistical coin flip over the
    Latin candidates deliberately: it is the only observable difference
    from the complete-input path (which flips to Windows-1250).  If a
    model retrain moves the coin flip, update the string here, not the
    invariant.
    """
    data = "mamá mamá mamá".encode("iso-8859-1")
    result = chardet.detect(data, max_bytes=4)
    assert result["encoding"] == "utf-8"


def test_internal_slicing_tolerance_is_structural_too():
    """Retrain-proof variant: sliced utf-8 keeps utf-8 via structure alone.

    Real multi-byte content before the cut resolves through the utf-8
    structural stage regardless of statistical models, so this pin cannot
    move under a retrain.
    """
    data = ("héllo wörld " * 50).encode()
    cut = 100
    while data[cut - 1] < 0x80:  # land the slice mid-character
        cut += 1
    result = chardet.detect(data, max_bytes=cut)
    assert result["encoding"] == "utf-8"


def test_streaming_buffer_cap_keeps_the_prefix_tolerance():
    """UniversalDetector's cap is chardet-made truncation (issue #380 review).

    ``feed()`` stops at exactly ``max_bytes``, so the pipeline cannot see
    the cut in ``len(data)``; the detector must say so itself.  Overflowed
    streams keep the tolerance and agree with ``detect()`` on the same
    call, and an exactly-filled buffer with nothing dropped counts as
    complete, also agreeing with ``detect()``.
    """
    data = "mamá mamá mamá".encode("iso-8859-1")

    overflowed = UniversalDetector(max_bytes=4)
    overflowed.feed(data)
    assert (
        overflowed.close()["encoding"] == chardet.detect(data, max_bytes=4)["encoding"]
    )

    exact = UniversalDetector(max_bytes=4)
    exact.feed(b"mam\xe1")
    assert (
        exact.close()["encoding"] == chardet.detect(b"mam\xe1", max_bytes=4)["encoding"]
    )


def test_streaming_truncation_flag_resets():
    """reset() must clear the truncation memory along with the buffer."""
    det = UniversalDetector(max_bytes=4)
    det.feed("mamá mamá".encode("iso-8859-1"))
    det.close()
    det.reset()
    det.feed(b"mam\xe1")
    result = det.close()
    assert result["encoding"] == chardet.detect(b"mam\xe1", max_bytes=4)["encoding"]


def test_truncated_cjk_chunk_keeps_its_answer():
    """A mid-character CJK cut must not flip to a low-confidence Latin codec.

    The guard is evidence, not confidence: a CJK winner has decoded real
    multi-byte characters before the cut, so its tolerant decode is not
    pure ASCII and the decode-safety flip never touches it.  A 5-40 byte
    sweep measured zero correct answers lost under this rule, against 34
    lost under a confidence-band rule.
    """
    chunk = (_JA * 8).encode("shift_jis")
    while decodes_completely(chunk, "shift_jis"):
        chunk = chunk[:-1]
    result = chardet.detect(chunk, encoding_era=EncodingEra.ALL)
    assert (result["encoding"] or "").lower().startswith(("shift_jis", "cp932"))
