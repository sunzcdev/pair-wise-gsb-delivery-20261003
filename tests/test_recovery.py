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


def test_parse_recovery_returns_parse_results_in_order():
    results = parse_recovery("SELECT 1; SELECT 2; SELECT 3;")
    assert len(results) == 3
    assert all(isinstance(result, ParseResult) for result in results)
    assert [result.source for result in results] == [
        "SELECT 1; ", "SELECT 2; ", "SELECT 3;"]
    assert all(result.ok for result in results)
    assert all(result.error is None for result in results)
    assert all(not result.is_incomplete for result in results)
    assert [result.statement.get_type()
            for result in results] == ["SELECT", "SELECT", "SELECT"]


def test_recovery_is_explicit_and_off_by_default():
    stack = engine.FilterStack()
    stack.enable_grouping()
    with pytest.raises(SQLParseError, match="Maximum grouping depth"):
        list(stack.run(DEEP_PARENS))


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


def test_grouping_failure_keeps_earlier_statements(shallow_grouping_depth):
    results = parse_recovery("SELECT 1; " + DEEP_PARENS + " SELECT 3;")
    assert len(results) == 3
    assert [result.ok for result in results] == [True, False, True]
    assert [result.source for result in results] == [
        "SELECT 1; ", DEEP_PARENS + " ", "SELECT 3;"]


def test_failed_statement_is_returned_ungrouped(shallow_grouping_depth):
    failed = parse_recovery(DEEP_PARENS)[0]
    # Grouping aborted mid-way, so the statement keeps whatever partial
    # structure had been built but its text is returned verbatim.
    assert str(failed.statement) == DEEP_PARENS
    assert failed.statement.grouping_error is failed.error
    assert failed.source == DEEP_PARENS


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


def test_token_limit_failure_is_isolated():
    original = grouping.MAX_GROUPING_TOKENS
    grouping.MAX_GROUPING_TOKENS = 20
    try:
        results = parse_recovery(
            "SELECT a, b, c, d, e, f, g, h, i, j FROM t; SELECT 2;")
    finally:
        grouping.MAX_GROUPING_TOKENS = original

    assert len(results) == 2
    failed, healthy = results
    assert not failed.ok
    assert "Maximum number of tokens" in str(failed.error)
    assert failed.source.startswith("SELECT a, b")
    assert healthy.ok


def test_recursion_failure_is_isolated():
    # With the grouping limits disabled, a very deep tree makes grouping
    # hit the interpreter's recursion limit. Recovery still reports that
    # one statement and keeps parsing the rest.
    original_depth = grouping.MAX_GROUPING_DEPTH
    original_tokens = grouping.MAX_GROUPING_TOKENS
    grouping.MAX_GROUPING_DEPTH = None
    grouping.MAX_GROUPING_TOKENS = None
    deep = "SELECT " + "(" * 5000 + "1" + ")" * 5000 + ";"
    try:
        results = parse_recovery(deep + " SELECT 2;")
    finally:
        grouping.MAX_GROUPING_DEPTH = original_depth
        grouping.MAX_GROUPING_TOKENS = original_tokens

    assert len(results) == 2
    failed, healthy = results
    assert not failed.ok
    assert "recursion" in str(failed.error).lower()
    assert failed.source == deep + " "
    assert healthy.ok


def test_recovery_sources_match_legacy_split(load_file):
    sql = load_file("mysql_handler.sql")
    assert [result.source for result in parse_recovery(sql)] == [
        str(stmt) for stmt in sqlparse.parse(sql)]


def test_quote_in_line_comment_is_not_incomplete():
    # The quote never reaches the lexer's error branch, so it is plain
    # comment text, not an unclosed literal.
    results = parse_recovery("SELECT 1 -- don't\n; SELECT 2;")
    assert len(results) == 2
    assert all(result.ok for result in results)
    assert not any(result.is_incomplete for result in results)


def test_incomplete_statement_skips_statement_filters():
    stack = engine.FilterStack(recover=True)
    stack.enable_grouping()
    stack.stmtprocess.append(filters.ReindentFilter())

    statements = list(stack.run("SELECT a FROM t; SELECT b FROM 'oops"))
    healthy, incomplete = statements
    assert "\n" in str(healthy)
    assert incomplete.is_incomplete
    assert "\n" not in str(incomplete)


@pytest.mark.parametrize("sql, remainder", [
    ("SELECT 1; SELECT 'abc; SELECT 2", "'abc; SELECT 2"),
    ('SELECT 1; SELECT "abc; SELECT 2', '"abc; SELECT 2'),
    ("SELECT 1; /* unclosed comment; SELECT 2",
     "/* unclosed comment; SELECT 2"),
])
def test_unclosed_construct_is_single_incomplete_result(sql, remainder):
    results = parse_recovery(sql)
    assert len(results) == 2
    healthy, incomplete = results

    assert healthy.ok
    assert not healthy.is_incomplete

    assert not incomplete.ok
    assert incomplete.is_incomplete
    assert isinstance(incomplete.error, IncompleteInputError)
    assert incomplete.error.remainder == remainder
    # The semicolon inside the open construct must not start a statement.
    assert incomplete.source.endswith(remainder)


def test_unclosed_input_at_the_start():
    result = parse_recovery("SELECT 'never closed")[0]
    assert result.is_incomplete
    assert isinstance(result.error, IncompleteInputError)


def test_lexer_does_not_resume_at_semicolon_inside_unclosed_quote():
    # The trailing text after the opening quote stays in one result even
    # though it contains several semicolons.
    sql = "SELECT 'a; b; c; d"
    results = parse_recovery(sql)
    assert len(results) == 1
    assert results[0].is_incomplete
    assert results[0].source == sql


def test_incomplete_remainder_follows_preceding_statement():
    results = parse_recovery("SELECT 1;'abc")
    assert len(results) == 2
    assert results[0].source == "SELECT 1;"
    assert results[0].ok
    assert results[1].is_incomplete
    assert results[1].source == "'abc"


def test_recovery_accepts_file_like_input():
    results = parse_recovery(StringIO("SELECT 1; SELECT 'oops"))
    assert len(results) == 2
    assert results[0].ok
    assert results[1].is_incomplete


def test_recovery_round_trips_original_text():
    sql = "SELECT 1; " + DEEP_PARENS + " SELECT 2;"
    results = parse_recovery(sql)
    assert "".join(result.source for result in results) == sql


def test_recovery_without_grouping_does_not_report_grouping_errors():
    # The recovered API enables grouping; a bare recovery stack that does
    # not must not invent a diagnosis.
    stack = engine.FilterStack(recover=True)
    statements = list(stack.run("SELECT 1; SELECT 2;"))
    assert [stmt.grouping_error for stmt in statements] == [None, None]


def test_public_exports():
    assert sqlparse.parse_recovery is parse_recovery
    assert sqlparse.ParseResult is ParseResult
    assert sqlparse.IncompleteInputError is IncompleteInputError
    assert sqlparse.SQLParseError is SQLParseError


# ---------------------------------------------------------------------------
# Legacy behaviour must be unchanged when recovery is not requested.
# ---------------------------------------------------------------------------


def test_legacy_tokenize_keeps_error_tokens_for_unclosed_string():
    tokens = list(lexer.tokenize("SELECT 'abc"))
    # The historical, non-validating lexer emits an Error token instead of
    # raising.
    assert any(ttype is sqlparse.tokens.Error for ttype, _ in tokens)


def test_legacy_parse_does_not_raise_on_unclosed_string():
    # No exception: the old behaviour fragments the input on the semicolon.
    statements = sqlparse.parse("SELECT 1; SELECT 'abc; SELECT 2")
    assert len(statements) == 3


def test_legacy_tokenize_does_not_raise_on_unclosed_comment():
    tokens = list(lexer.tokenize("SELECT 1; /* unclosed"))
    # Without recovery an unclosed comment is not recognised as a comment
    # at all; the characters stay ordinary tokens and nothing raises.
    assert not any(ttype is sqlparse.tokens.Error for ttype, _ in tokens)


def test_legacy_split_does_not_raise_on_unclosed_construct():
    assert len(sqlparse.split("SELECT 1; SELECT 'abc; SELECT 2")) == 3


def test_recovery_lexer_flag_raises_on_unclosed_quote():
    with pytest.raises(IncompleteInputError) as excinfo:
        list(lexer.tokenize("SELECT 'abc", raise_on_incomplete=True))
    assert excinfo.value.remainder == "'abc"


def test_recovery_lexer_flag_raises_on_unclosed_comment():
    with pytest.raises(IncompleteInputError) as excinfo:
        list(lexer.tokenize("SELECT 1; /* unclosed",
                            raise_on_incomplete=True))
    assert excinfo.value.remainder == "/* unclosed"


def test_splitter_propagates_incomplete_input_without_recovery():
    tokens = lexer.tokenize("SELECT 1; 'unclosed", raise_on_incomplete=True)
    with pytest.raises(IncompleteInputError):
        list(engine.StatementSplitter().process(tokens))


def test_recovery_lexer_flag_keeps_error_for_unknown_character():
    # A genuinely unrecognised character is still an Error token, not an
    # incomplete input.
    tokens = list(lexer.tokenize("FOOBAR{", raise_on_incomplete=True))
    assert tokens[-1][0] is sqlparse.tokens.Error


def test_unmatched_dollar_tags_are_not_incomplete():
    # "$A$ x $B$" never forms a paired dollar literal; the tokens are
    # ordinary text and must keep their historical interpretation.
    results = parse_recovery("$A$ foo $B$; SELECT 1;")
    assert all(result.ok for result in results)
    assert not any(result.is_incomplete for result in results)
    assert results[-1].statement.get_type() == "SELECT"


def test_closed_dollar_literal_is_not_incomplete():
    results = parse_recovery("SELECT $$a; b$$; SELECT 2;")
    assert len(results) == 2
    assert all(result.ok for result in results)
    assert not any(result.is_incomplete for result in results)


def test_incomplete_input_error_is_sqlparse_error():
    assert issubclass(IncompleteInputError, SQLParseError)


def test_healthy_statement_still_grouped_in_recovery():
    result = parse_recovery("SELECT foo FROM bar;")[0]
    # Grouping turns the identifier list into structured groups.
    assert any(token.is_group for token in result.statement.tokens)


def test_statement_flags_default_to_false():
    statement = sqlparse.parse("SELECT 1;")[0]
    assert statement.grouping_error is None
    assert statement.is_incomplete is False
