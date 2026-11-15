from io import StringIO

import pytest

import sqlparse
from sqlparse import keywords, tokens as T
from sqlparse.engine import FilterStack
from sqlparse.exceptions import SQLParseError
from sqlparse.lexer import Lexer, LexerConfig, REMOVE_KEYWORD


def _types(stream):
    return [
        ttype
        for ttype, value in stream
        if not (ttype in T.Whitespace)
    ]


def test_default_singleton_behavior_is_unchanged():
    tokens_ = _types(sqlparse.lexer.tokenize("select bacon"))
    assert tokens_ == [T.Keyword.DML, T.Name]

    lex = Lexer.get_default_instance()
    assert lex.is_keyword("select")[0] is T.Keyword.DML
    assert FilterStack()._lexer is lex


def test_config_builds_independent_lexer():
    config = LexerConfig(keywords={"BACON": T.Keyword})
    lex = Lexer.from_config(config)
    assert lex is not Lexer.get_default_instance()
    assert lex.is_keyword("bacon")[0] is T.Keyword
    # The default singleton must not pick up the custom keyword.
    assert Lexer.get_default_instance().is_keyword("bacon")[0] is T.Name


def test_keyword_overrides_take_precedence_over_base_vocabulary():
    config = LexerConfig(
        keywords={"SELECT": T.Keyword.CTE, "BACON": T.Name.Builtin})
    assert _types(
        sqlparse.lexer.tokenize("select bacon", lexer_config=config)
    ) == [T.Keyword.CTE, T.Name.Builtin]


def test_remove_keyword_drops_inherited_entry():
    config = LexerConfig(keywords={"SELECT": REMOVE_KEYWORD})
    assert _types(
        sqlparse.lexer.tokenize("select bacon", lexer_config=config)
    ) == [T.Name, T.Name]
    # The shared module-level dictionary and singleton are untouched.
    assert "SELECT" in keywords.KEYWORDS_COMMON
    assert (
        Lexer.get_default_instance().is_keyword("select")[0]
        is T.Keyword.DML
    )


def test_two_configs_interleave_with_stable_classification():
    config_a = LexerConfig(
        keywords={"SELECT": T.Keyword.CTE, "BACON": T.Name.Builtin})
    config_b = LexerConfig(
        keywords={"SELECT": T.Name, "BACON": T.Keyword})

    for _ in range(5):
        assert _types(
            sqlparse.lexer.tokenize("select bacon", lexer_config=config_a)
        ) == [T.Keyword.CTE, T.Name.Builtin]
        assert _types(
            sqlparse.lexer.tokenize("select bacon", lexer_config=config_b)
        ) == [T.Name, T.Keyword]

    # And the default singleton still reports the built-in classification.
    assert _types(sqlparse.lexer.tokenize("select bacon")) == [
        T.Keyword.DML, T.Name]


def test_config_does_not_mutate_caller_dictionaries():
    source = {"BACON": T.Keyword}
    config = LexerConfig(keywords=source)
    source["BACON"] = T.Name
    source["EGGS"] = T.Keyword
    assert config.keyword_overrides == {"BACON": T.Keyword}

    # Exposed copies are defensive too.
    overrides = config.keyword_overrides
    overrides["BACON"] = T.Error
    assert config.keyword_overrides["BACON"] is T.Keyword


def test_keyword_names_are_normalized_to_upper_case():
    config = LexerConfig(keywords={"bacon": T.Keyword})
    assert config.keyword_overrides == {"BACON": T.Keyword}
    assert _types(
        sqlparse.lexer.tokenize("BACON", lexer_config=config)
    ) == [T.Keyword]


def test_custom_sql_regex_is_used():
    my_regex = (r"ZORDER\s+BY\b", T.Keyword)
    regex = (
        keywords.SQL_REGEX[:38] + [my_regex] + keywords.SQL_REGEX[38:]
    )
    config = LexerConfig(sql_regex=regex)
    tokens_ = list(
        sqlparse.parse(
            "select * from foo zorder by bar;", lexer_config=config
        )[0].flatten()
    )
    matched = [
        token for token in tokens_
        if token.ttype is T.Keyword and token.value == "zorder by"
    ]
    assert len(matched) == 1

    # The regex list is copied, so later caller mutation is isolated.
    regex.clear()
    assert len(config.sql_regex) > 0


def test_config_threads_through_all_entry_points():
    config = LexerConfig(keywords={"BACON": T.Keyword})

    parsed = sqlparse.parse("bacon", lexer_config=config)[0]
    assert parsed.tokens[0].ttype is T.Keyword

    statements = sqlparse.split(
        "bacon 1; bacon 2", lexer_config=config)
    assert len(statements) == 2

    formatted = sqlparse.format(
        "bacon", keyword_case="upper", lexer_config=config)
    assert "BACON" in formatted

    streamed = list(
        sqlparse.parsestream(StringIO("bacon"),
                             lexer_config=config))
    assert streamed[0].tokens[0].ttype is T.Keyword


def test_filter_stack_never_silently_falls_back_to_default():
    config = LexerConfig(keywords={"BACON": T.Keyword})
    stack = FilterStack(lexer_config=config)
    assert stack._lexer is not Lexer.get_default_instance()

    statement = list(stack.run("bacon"))[0]
    assert statement.tokens[0].ttype is T.Keyword


@pytest.mark.parametrize("factory", [
    lambda: LexerConfig(keywords={"": T.Keyword}),
    lambda: LexerConfig(keywords={"X": "not-a-token-type"}),
    lambda: LexerConfig(keywords=["not", "a", "mapping"]),
    lambda: LexerConfig(sql_regex="not-a-list"),
    lambda: LexerConfig(sql_regex=[("only-one-element",)]),
    lambda: LexerConfig(sql_regex=[("(unclosed", T.Name)]),
    lambda: LexerConfig(sql_regex=[(123, T.Name)]),
    lambda: LexerConfig(sql_regex=[("x", "not-a-token-type")]),
])
def test_invalid_config_is_rejected_before_reading_sql(factory):
    with pytest.raises(SQLParseError):
        factory()


@pytest.mark.parametrize("entry_point", [
    lambda: sqlparse.parse("select 1", lexer_config=object()),
    lambda: sqlparse.split("select 1", lexer_config=object()),
    lambda: sqlparse.format("select 1", lexer_config=object()),
    lambda: list(
        sqlparse.lexer.tokenize("select 1", lexer_config=object())),
])
def test_wrong_lexer_config_type_is_rejected(entry_point):
    with pytest.raises(SQLParseError):
        entry_point()


def test_filter_stack_rejects_invalid_config_at_construction():
    with pytest.raises(SQLParseError):
        FilterStack(lexer_config=object())
