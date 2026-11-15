"""Tests for recovery-aware parsing and incomplete-input handling."""

from io import StringIO

import pytest

import sqlparse
import sqlparse.engine.grouping as grouping
from sqlparse import engine, filters, lexer
from sqlparse.exceptions import IncompleteInputError, SQLParseError
from sqlparse.recovery import ParseResult, parse_recovery

DEEP_PARENS = "SELECT " + "(" * 200 + "1" + ")" * 200 + ";"


@pytest.fixture
def shallow_grouping_depth():
    original = grouping.MAX_GROUPING_DEPTH
    grouping.MAX_GROUPING_DEPTH = 5
    yield
    grouping.MAX_GROUPING_DEPTH = original


def test_parse_recovery_returns_results_in_input_order():
    results = parse_recovery("SELECT 1; SELECT 2; SELECT 3;")
    assert len(results) == 3
    assert all(isinstance(result, ParseResult) for result in results)
    assert [result.source for result in results] == [
        "SELECT 1; ", "SELECT 2; ", "SELECT 3;"]
    assert all(result.ok for result in results)
    assert all(result.error is None for result in results)
    assert all(not result.is_incomplete for result in results)
    assert [result.statement.get_type() for result in results] == [
        "SELECT", "SELECT", "SELECT"]


def test_grouping_failure_does_not_drop_later_statements(
        shallow_grouping_depth):
    results = parse_recovery(DEEP_PARENS + " SELECT 2;")
    assert len(results) == 2

    failed, healthy = results
    assert not failed.ok
    assert isinstance(failed.error, SQLParseError)
    assert "grouping depth" in str(failed.error)
    assert failed.source == DEEP_PARENS + " "
    assert not failed.is_incomplete

    assert healthy.ok
    assert healthy.error is None
    assert healthy.source == "SELECT 2;"
    assert healthy.statement.get_type() == "SELECT"


def test_failed_statement_keeps_its_source_text(shallow_grouping_depth):
    failed, = parse_recovery(DEEP_PARENS)
    # Grouping aborted mid-way; the statement text is still reported
    # verbatim so the caller can review the original SQL.
    assert str(failed.statement) == DEEP_PARENS
    assert failed.source == DEEP_PARENS
    assert failed.statement.grouping_error is failed.error


def test_healthy_statement_after_failure_is_grouped(shallow_grouping_depth):
    _, healthy = parse_recovery(DEEP_PARENS + " SELECT foo FROM bar;")
    assert healthy.statement.grouping_error is None
    assert any(token.is_group for token in healthy.statement.tokens)


def test_failed_statement_skips_statement_filters(shallow_grouping_depth):
    stack = engine.FilterStack(recover=True)
    stack.enable_grouping()
    stack.stmtprocess.append(filters.ReindentFilter())

    statements = list(stack.run(DEEP_PARENS + " SELECT a FROM t;"))
    assert len(statements) == 2

    failed, healthy = statements
    assert failed.grouping_error is not None
    # Reindent would insert newlines; the failed statement is untouched.
    assert "\n" not in str(failed)
    assert str(failed) == DEEP_PARENS + " "
    assert healthy.grouping_error is None
    assert "\n" in str(healthy)


@pytest.mark.parametrize("sql, remainder", [
    ("SELECT 1; SELECT 'abc; SELECT 2", "'abc; SELECT 2"),
    ('SELECT 1; SELECT "abc; SELECT 2', '"abc; SELECT 2'),
    ("SELECT 1; /* unclosed comment; SELECT 2",
     "/* unclosed comment; SELECT 2"),
])
def test_unclosed_construct_is_a_single_incomplete_result(sql, remainder):
    results = parse_recovery(sql)
    assert len(results) == 2

    healthy, incomplete = results
    assert healthy.ok
    assert healthy.source == "SELECT 1; "
    assert not healthy.is_incomplete

    assert not incomplete.ok
    assert incomplete.is_incomplete
    assert isinstance(incomplete.error, IncompleteInputError)
    assert incomplete.error.remainder == remainder
    # The semicolon inside the open construct must not start a statement.
    assert incomplete.source.endswith(remainder)


def test_unclosed_input_at_the_start():
    result, = parse_recovery("SELECT 'never closed")
    assert result.is_incomplete
    assert isinstance(result.error, IncompleteInputError)
    assert result.source == "SELECT 'never closed"


def test_lexer_does_not_resume_at_semicolon_inside_unclosed_quote():
    sql = "SELECT 'a; b; c; d"
    results = parse_recovery(sql)
    assert len(results) == 1
    assert results[0].is_incomplete
    assert results[0].source == sql


def test_trailing_statement_after_incomplete_input_is_not_invented():
    # Nothing after the opening quote can be a statement, even though the
    # text contains a complete looking "SELECT 2;".
    sql = "SELECT 'oops; SELECT 2; SELECT 3;"
    results = parse_recovery(sql)
    assert len(results) == 1
    assert results[0].is_incomplete
    assert results[0].statement.get_type() == "SELECT"


def test_recovery_accepts_file_like_input():
    results = parse_recovery(StringIO("SELECT 1; SELECT 'oops"))
    assert len(results) == 2
    assert results[0].ok
    assert results[1].is_incomplete


def test_recovery_round_trips_original_text(shallow_grouping_depth):
    sql = "SELECT 1; " + DEEP_PARENS + " SELECT 2;"
    results = parse_recovery(sql)
    assert "".join(result.source for result in results) == sql


def test_incomplete_statement_keeps_original_text():
    sql = "SELECT 'oops; SELECT 2; -- not a boundary\n"
    result, = parse_recovery(sql)
    assert result.is_incomplete
    assert str(result.statement) == sql


def test_quote_inside_line_comment_is_not_incomplete():
    results = parse_recovery("SELECT 1; -- don't split here\nSELECT 2;")
    assert len(results) == 2
    assert all(result.ok for result in results)


def test_unmatched_dollar_tags_are_not_incomplete():
    # "$A$ ... $B$" never forms a paired dollar-quoted literal, so the
    # tokens keep their historical interpretation.
    results = parse_recovery("$A$ foo $B$; SELECT 1;")
    assert all(result.ok for result in results)
    assert not any(result.is_incomplete for result in results)
    assert results[-1].statement.get_type() == "SELECT"


def test_incomplete_statement_skips_statement_filters():
    stack = engine.FilterStack(recover=True)
    stack.enable_grouping()
    stack.stmtprocess.append(filters.ReindentFilter())

    statements = list(stack.run("SELECT 1; SELECT a FROM t; /* oops"))
    assert len(statements) == 3
    # Completed statements are still formatted normally.
    assert "\n" in str(statements[1])
    incomplete = statements[-1]
    assert incomplete.is_incomplete
    assert "\n" not in str(incomplete)
    assert str(incomplete) == "/* oops"


def test_recovery_stack_skips_postprocessors_for_failed_statements(
        shallow_grouping_depth):
    stack = engine.FilterStack(recover=True)
    stack.enable_grouping()
    stack.postprocess.append(filters.SerializerUnicode())

    statements = list(stack.run(DEEP_PARENS + " SELECT 1;"))
    failed, healthy = statements
    # The serializer only rstrips lines; the failed statement keeps
    # everything, but is still never reformatted as a query.
    assert str(failed) == DEEP_PARENS + " "
    assert str(healthy) == "SELECT 1;"


def test_recursion_failure_is_isolated_per_statement():
    import sys

    limit = sys.getrecursionlimit()
    sys.setrecursionlimit(100)
    try:
        results = parse_recovery(
            "SELECT 1; " + "[" * 1000 + "]" * 1000 + " SELECT 2;")
    finally:
        sys.setrecursionlimit(limit)

    assert results[0].ok
    assert results[0].statement.get_type() == "SELECT"
    failed = results[1]
    assert isinstance(failed.error, SQLParseError)
    assert "recursion" in str(failed.error)
    assert failed.source.startswith("[[[")


def test_incomplete_input_error_is_a_sqlparse_error():
    assert issubclass(IncompleteInputError, SQLParseError)


# ---------------------------------------------------------------------------
# Lexer level: the strict mode is opt-in.
# ---------------------------------------------------------------------------


def test_recovery_lexer_flag_raises_on_unclosed_quote():
    for sql, remainder in [("SELECT 'abc", "'abc"),
                           ('SELECT "abc', '"abc')]:
        with pytest.raises(IncompleteInputError) as excinfo:
            list(lexer.tokenize(sql, raise_on_incomplete=True))
        assert excinfo.value.remainder == remainder


def test_recovery_lexer_flag_raises_on_unclosed_comment():
    with pytest.raises(IncompleteInputError) as excinfo:
        list(lexer.tokenize("SELECT 1; /* unclosed", raise_on_incomplete=True))
    assert excinfo.value.remainder == "/* unclosed"


def test_recovery_lexer_flag_keeps_error_token_for_unknown_character():
    # A genuinely unrecognised character is still an Error token, not an
    # incomplete input.
    tokens = list(lexer.tokenize("FOOBAR{", raise_on_incomplete=True))
    assert tokens[-1][0] is sqlparse.tokens.Error


def test_recovery_lexer_flag_accepts_closed_constructs():
    tokens = list(lexer.tokenize(
        "SELECT 'a; b', $$x; y$$, /* c; d */ 1", raise_on_incomplete=True))
    assert tokens


# ---------------------------------------------------------------------------
# Legacy behaviour must be unchanged when recovery is not requested.
# ---------------------------------------------------------------------------


def test_legacy_tokenize_keeps_error_token_for_unclosed_string():
    tokens = list(lexer.tokenize("SELECT 'abc"))
    # The historical, non-validating lexer emits an Error token instead
    # of raising.
    assert (sqlparse.tokens.Error, "'") in tokens


def test_legacy_parse_does_not_raise_on_unclosed_string():
    statements = sqlparse.parse("SELECT 1; SELECT 'abc; SELECT 2")
    assert len(statements) == 3
    assert str(statements[-1]) == "SELECT 2"


def test_legacy_split_and_format_do_not_raise_on_unclosed_input():
    sql = "SELECT 1; SELECT 'abc; SELECT 2"
    assert len(sqlparse.split(sql)) == 3
    assert isinstance(sqlparse.format(sql, reindent=True), str)


def test_legacy_format_does_not_raise_on_unclosed_comment():
    assert isinstance(sqlparse.format("SELECT 1; /* unclosed"), str)


def test_legacy_statements_have_no_recovery_state():
    stmt = sqlparse.parse("SELECT 1;")[0]
    assert stmt.grouping_error is None
    assert stmt.is_incomplete is False


def test_grouping_failure_still_raises_without_recovery(
        shallow_grouping_depth):
    with pytest.raises(SQLParseError, match="grouping depth"):
        sqlparse.parse(DEEP_PARENS)
