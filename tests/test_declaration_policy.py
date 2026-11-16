# tests/test_declaration_policy.py
from __future__ import annotations

import pytest

from chardet import DeclarationPolicy, UniversalDetector, detect, detect_all
from chardet.cli import main as cli_main
from chardet.pipeline import _NONE_RESULT, DetectionResult
from chardet.pipeline.orchestrator import _apply_declaration_policy

_LATIN1_PAGE = (
    b"<meta charset=\"iso-8859-1\">"
    + "Voilà, un résumé très détaillé. ".encode("latin-1") * 10
)
#: Declares EUC-JP but holds bytes no EUC-JP decoder accepts.
_LYING_PAGE = (
    b"<meta charset=\"euc-jp\">"
    + "これは文字コード判定のテストに用いる日本語の文章です。".encode()
)


def _names(results: list[dict]) -> list[str | None]:
    return [r["encoding"] for r in results]


# --- The option itself -------------------------------------------------------


def test_default_policy_is_strict():
    assert detect(_LATIN1_PAGE) == detect(
        _LATIN1_PAGE, declaration_policy=DeclarationPolicy.STRICT
    )


def test_strict_declaration_commits():
    # STRICT is the historical behavior: a compatible declaration is the
    # whole answer, no other candidate is listed.
    assert detect(_LATIN1_PAGE)["encoding"] == "ISO-8859-1"
    assert _names(detect_all(_LATIN1_PAGE, ignore_threshold=True)) == ["ISO-8859-1"]


@pytest.mark.parametrize("policy", list(DeclarationPolicy))
def test_policy_accepts_every_enum_member(policy: DeclarationPolicy):
    assert detect(_LATIN1_PAGE, declaration_policy=policy)["encoding"] == "ISO-8859-1"


@pytest.mark.parametrize("value", ["strict", "PREFER", "Hint"])
def test_policy_accepts_its_string_values(value: str):
    assert detect(_LATIN1_PAGE, declaration_policy=value)["encoding"] == "ISO-8859-1"


@pytest.mark.parametrize("value", ["bogus", "", 42, None])
def test_policy_validation_rejects_unknown_values(value: object):
    with pytest.raises(ValueError, match="declaration_policy"):
        detect(_LATIN1_PAGE, declaration_policy=value)  # type: ignore[arg-type]
    with pytest.raises(ValueError, match="declaration_policy"):
        UniversalDetector(declaration_policy=value)  # type: ignore[arg-type]


# --- PREFER: the declaration heads the ranking, others are still checked -----


def test_prefer_heads_the_ranking_and_keeps_the_other_candidates():
    results = detect_all(
        _LATIN1_PAGE, declaration_policy="prefer", ignore_threshold=True
    )
    assert results[0]["encoding"] == "ISO-8859-1"
    # The rest of the ranking is what the bytes alone would have said.
    assert len(results) > 1
    assert "Windows-1252" in _names(results)


def test_prefer_detect_and_detect_all_agree_on_the_top_answer():
    top = detect(_LATIN1_PAGE, declaration_policy="prefer")["encoding"]
    ranked = detect_all(_LATIN1_PAGE, declaration_policy="prefer")
    assert ranked[0]["encoding"] == top


def test_prefer_confidence_covers_the_best_alternative():
    # The preference must survive detect_all's confidence sort, so the
    # preferred answer adopts the best confidence the pipeline found.
    results = detect_all(
        _LATIN1_PAGE, declaration_policy="prefer", ignore_threshold=True
    )
    assert results[0]["confidence"] >= max(r["confidence"] for r in results[1:])


def test_prefer_lying_declaration_has_no_effect():
    assert detect(_LYING_PAGE, declaration_policy="prefer") == detect(_LYING_PAGE)


def test_prefer_genuine_utf8_still_beats_the_declaration():
    page = b'<meta charset="iso-8859-1">' + "café crème brûlée. ".encode() * 10
    assert detect(page, declaration_policy="prefer")["encoding"] == "utf-8"


def test_prefer_filtered_declaration_cannot_bypass_the_allowlist():
    for kwargs in (
        {"exclude_encodings": ["iso-8859-1"]},
        {"include_encodings": ["utf-8", "windows-1252"]},
    ):
        result = detect(_LATIN1_PAGE, declaration_policy="prefer", **kwargs)
        assert result["encoding"] != "ISO-8859-1"
        assert "ISO-8859-1" not in _names(
            detect_all(
                _LATIN1_PAGE,
                declaration_policy="prefer",
                ignore_threshold=True,
                **kwargs,
            )
        )


# --- HINT: the declaration only lifts its own candidate ----------------------


def test_hint_lifts_the_declared_encoding_inside_the_ranking():
    result = detect(_LATIN1_PAGE, declaration_policy="hint")
    assert result["encoding"] == "ISO-8859-1"
    # Lifted to the hint confidence, not the deterministic one a STRICT
    # declaration would get.
    assert result["confidence"] == 0.5
    results = detect_all(
        _LATIN1_PAGE, declaration_policy="hint", ignore_threshold=True
    )
    assert len(results) > 1


def test_hint_lying_declaration_has_no_effect():
    assert detect(_LYING_PAGE, declaration_policy="hint") == detect(_LYING_PAGE)


def test_hint_never_preempts_a_deterministic_stage():
    page = b'<meta charset="iso-8859-1">' + b"plain ascii text"
    assert detect(page, declaration_policy="hint")["encoding"] == "ascii"


def test_hint_filtered_declaration_has_no_effect():
    result = detect(
        _LATIN1_PAGE, declaration_policy="hint", exclude_encodings=["iso-8859-1"]
    )
    assert result["encoding"] != "ISO-8859-1"
    assert "ISO-8859-1" not in _names(
        detect_all(
            _LATIN1_PAGE,
            declaration_policy="hint",
            ignore_threshold=True,
            exclude_encodings=["iso-8859-1"],
        )
    )


# --- The merge helper --------------------------------------------------------


def test_hint_does_not_resurrect_an_eliminated_candidate():
    declared = DetectionResult("shift_jis_2004", 0.95, None, "text/html")
    results = [DetectionResult("cp1252", 0.4, None)]
    assert (
        _apply_declaration_policy(DeclarationPolicy.HINT, declared, results) is results
    )


def test_hint_keeps_a_higher_evidence_confidence():
    declared = DetectionResult("iso8859-1", 0.95, None, "text/html")
    results = [
        DetectionResult("iso8859-1", 0.8, None),
        DetectionResult("cp1252", 0.3, None),
    ]
    assert (
        _apply_declaration_policy(DeclarationPolicy.HINT, declared, results) is results
    )


def test_prefer_merge_lifts_confidence_and_dedupes():
    declared = DetectionResult("iso8859-1", 0.95, None, "text/html")
    results = [
        DetectionResult("iso8859-1", 0.99, None),
        DetectionResult("cp1252", 0.3, None),
    ]
    merged = _apply_declaration_policy(DeclarationPolicy.PREFER, declared, results)
    assert merged[0] == DetectionResult("iso8859-1", 0.99, None, "text/html")
    assert [r.encoding for r in merged] == ["iso8859-1", "cp1252"]


def test_prefer_merge_drops_the_nothing_fit_sentinel():
    declared = DetectionResult("iso8859-1", 0.95, None, "text/html")
    merged = _apply_declaration_policy(
        DeclarationPolicy.PREFER, declared, [_NONE_RESULT]
    )
    assert merged == [declared]


# --- BOM vs. declaration: the priority is fixed ------------------------------


@pytest.mark.parametrize("policy", list(DeclarationPolicy))
def test_bom_outranks_a_conflicting_declaration(policy: DeclarationPolicy):
    page = b"\xff\xfe" + '<meta charset="utf-8"><p>hello</p>'.encode("utf-16-le")
    assert detect(page, declaration_policy=policy)["encoding"] == "UTF-16"


@pytest.mark.parametrize("policy", list(DeclarationPolicy))
def test_utf8_bom_outranks_a_conflicting_declaration(policy: DeclarationPolicy):
    page = b"\xef\xbb\xbf" + b'<meta charset="iso-8859-1">' + "café".encode()
    assert detect(page, declaration_policy=policy)["encoding"] == "UTF-8-SIG"


def test_utf7_signature_yields_to_a_declaration_only_under_strict():
    # The UTF-7 "BOM" is five ASCII bytes, weak enough that STRICT lets a
    # declaration overrule it.  Under PREFER and HINT no declaration
    # pre-empts a deterministic stage, so the signature stands.
    body = b"+ADw-p+AD4-hello+ADw-/p+AD4-" * 5
    page = b"+/v8" + b'<meta charset="windows-1252">' + body
    assert detect(page)["encoding"] == "Windows-1252"
    for policy in (DeclarationPolicy.PREFER, DeclarationPolicy.HINT):
        assert detect(page, declaration_policy=policy)["encoding"] == "utf-7"


def test_escape_answer_stands_under_prefer_and_hint():
    # STRICT lets a declaration that decodes the window overrule the escape
    # stage's answer; PREFER and HINT keep the deterministic answer.
    jp = "こんにちは、世界。これはテストです。".encode("iso2022_jp")
    page = b'<meta charset="shift_jis">' + jp
    assert detect(page)["encoding"] == "SHIFT_JIS"
    for policy in (DeclarationPolicy.PREFER, DeclarationPolicy.HINT):
        assert detect(page, declaration_policy=policy)["encoding"] == "ISO-2022-JP"


# --- The verdict is about the window, not the whole file ---------------------


@pytest.mark.parametrize("policy", list(DeclarationPolicy))
def test_declaration_verdict_covers_the_window_not_the_file(policy: DeclarationPolicy):
    # The window ends before the bytes the declared encoding cannot read;
    # the declaration stands.  Shown the whole input, no policy honors it.
    eucjp = "日本語のテキストです。".encode("euc-jp")
    data = b'<meta charset="euc-jp">' + eucjp + b"\x80\x80\x80"
    assert detect(data, max_bytes=len(data) - 3, declaration_policy=policy)[
        "encoding"
    ] == "EUC-JP"
    assert detect(data, max_bytes=len(data), declaration_policy=policy)[
        "encoding"
    ] != "EUC-JP"


# --- The other adapters ------------------------------------------------------


def test_universal_detector_takes_the_policy():
    detector = UniversalDetector(declaration_policy="prefer")
    detector.feed(_LATIN1_PAGE)
    detector.close()
    assert detector.result["encoding"] == "ISO-8859-1"


def test_cli_takes_the_policy(tmp_path, capsys):
    page = tmp_path / "page.html"
    page.write_bytes(_LATIN1_PAGE)
    cli_main(["--declaration-policy", "hint", str(page)])
    assert "ISO-8859-1" in capsys.readouterr().out

