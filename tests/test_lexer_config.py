"""Tests for independent, per-call lexical configurations (LexerConfig)."""
from io import StringIO

import pytest

import sqlparse
from sqlparse import tokens as T
from sqlparse.engine import FilterStack
from sqlparse.exceptions import SQLParseError
from sqlparse.lexer import Lexer, LexerConfig


def _first_ttype(sql_text, lexer=None):
    stmt = sqlparse.parse(sql_text, lexer=lexer)[0]
    for tok in stmt.flatten():
        if not tok.is_whitespace:
            return tok.ttype
    return None


def test_config_adds_keyword_without_touching_default():
    cfg = LexerConfig(keywords={'FOOBAR': T.Keyword})
    assert _first_ttype('foobar 1', lexer=cfg) is T.Keyword
    # The shared default lexer is not affected.
    assert _first_ttype('foobar 1') is T.Name
    assert Lexer.get_default_instance().is_keyword('FOOBAR')[0] is T.Name


def test_config_remove_keyword():
    cfg = LexerConfig(remove_keywords=['SELECT'])
    assert _first_ttype('select a', lexer=cfg) is T.Name
    # Default behavior unchanged.
    assert _first_ttype('select a') is T.Keyword.DML


def test_config_overrides_inherited_keyword():
    cfg = LexerConfig(keywords={'SELECT': T.Keyword})
    assert _first_ttype('select a', lexer=cfg) is T.Keyword
    assert _first_ttype('select a') is T.Keyword.DML


def test_config_without_inheritance():
    cfg = LexerConfig(keywords={'ONLY': T.Keyword}, inherit=False)
    assert _first_ttype('only 1', lexer=cfg) is T.Keyword
    assert _first_ttype('select a', lexer=cfg) is T.Name


def test_config_isolated_from_source_dict():
    words = {'FOOBAR': T.Keyword}
    cfg = LexerConfig(keywords=words)
    words['BAZ'] = T.Keyword  # must not leak into the config
    words['FOOBAR'] = T.Name  # must not leak either
    assert _first_ttype('baz 1', lexer=cfg) is T.Name
    assert _first_ttype('foobar 1', lexer=cfg) is T.Keyword


def test_config_isolated_from_source_removals():
    removals = ['SELECT']
    cfg = LexerConfig(remove_keywords=removals)
    removals.clear()
    assert _first_ttype('select a', lexer=cfg) is T.Name


def test_config_reusable_across_calls():
    cfg = LexerConfig(keywords={'FOOBAR': T.Keyword})
    for _ in range(3):
        assert _first_ttype('foobar 1', lexer=cfg) is T.Keyword
        assert _first_ttype('foobar 1') is T.Name


def test_interleaved_configs_classify_same_word_differently():
    cfg_kw = LexerConfig(keywords={'WIDGET': T.Keyword})
    cfg_name = LexerConfig()  # plain inheritance: WIDGET stays a Name
    for _ in range(3):
        assert _first_ttype('widget x', lexer=cfg_kw) is T.Keyword
        assert _first_ttype('widget x', lexer=cfg_name) is T.Name
        assert _first_ttype('widget x') is T.Name


def test_parsestream_with_config():
    cfg = LexerConfig(keywords={'FOOBAR': T.Keyword})
    stmts = list(sqlparse.parsestream(StringIO('foobar 1;'), lexer=cfg))
    assert next(stmts[0].flatten()).ttype is T.Keyword


def test_filterstack_accepts_lexer_instance():
    lex = LexerConfig(keywords={'FOOBAR': T.Keyword}).build_lexer()
    stack = FilterStack(lexer=lex)
    assert stack._lexer is lex
    stmt = next(stack.run('foobar 1'))
    assert next(stmt.flatten()).ttype is T.Keyword


def test_split_with_config():
    cfg = LexerConfig(keywords={'FOOBAR': T.Keyword})
    assert sqlparse.split('foobar 1; foobar 2', lexer=cfg) == [
        'foobar 1;', 'foobar 2']
    with pytest.raises(SQLParseError):
        sqlparse.split('select 1', lexer=object())


def test_format_with_config():
    cfg = LexerConfig(keywords={'FOOBAR': T.Keyword})
    # keyword_case only applies to tokens classified as keywords.
    assert sqlparse.format('foobar a', keyword_case='upper',
                           lexer=cfg) == 'FOOBAR a'
    assert sqlparse.format('foobar a', keyword_case='upper') == 'foobar a'


def test_filterstack_does_not_fall_back_to_default_lexer():
    for bogus in ('lexer', 42, object(), {'FOOBAR': T.Keyword}):
        with pytest.raises(SQLParseError):
            FilterStack(lexer=bogus)
    with pytest.raises(SQLParseError):
        sqlparse.parse('select 1', lexer='bogus')


def test_invalid_lexer_rejected_before_stream_is_read():
    class ExplodingStream:
        def read(self, *args, **kwargs):  # pragma: no cover
            raise AssertionError('stream must not be read')

    with pytest.raises(SQLParseError):
        sqlparse.parsestream(ExplodingStream(), lexer='bogus')
    with pytest.raises(SQLParseError):
        sqlparse.split(ExplodingStream(), lexer='bogus')


def test_invalid_keyword_mapping_rejected():
    with pytest.raises(SQLParseError):
        LexerConfig(keywords={'X': 'not-a-token-type'})
    with pytest.raises(SQLParseError):
        LexerConfig(keywords={'X': T.Keyword, 1: T.Name})
    with pytest.raises(SQLParseError):
        LexerConfig(keywords=['not', 'a', 'mapping'])
    with pytest.raises(SQLParseError):
        LexerConfig(keywords={'': T.Keyword})


def test_invalid_removals_rejected():
    with pytest.raises(SQLParseError):
        LexerConfig(remove_keywords='SELECT')  # a string, not an iterable
    with pytest.raises(SQLParseError):
        LexerConfig(remove_keywords=['SELECT', 42])
    with pytest.raises(SQLParseError):
        LexerConfig(remove_keywords=42)


def test_invalid_sql_regex_rejected():
    with pytest.raises(SQLParseError):
        LexerConfig(sql_regex='not-a-list')
    with pytest.raises(SQLParseError):
        LexerConfig(sql_regex=[('(', T.Keyword)])  # does not compile
    with pytest.raises(SQLParseError):
        LexerConfig(sql_regex=[(r'\w+', 'not-a-token-type')])
    with pytest.raises(SQLParseError):
        LexerConfig(sql_regex=[r'\w+'])  # not a pair


def test_invalid_inherit_rejected():
    with pytest.raises(SQLParseError):
        LexerConfig(inherit='yes')


def test_custom_sql_regex():
    cfg = LexerConfig(
        sql_regex=[(r'\s+', T.Whitespace),
                   (r'\w+', sqlparse.keywords.PROCESS_AS_KEYWORD)],
        keywords={'FOO': T.Keyword},
        inherit=False)
    assert _first_ttype('foo bar', lexer=cfg) is T.Keyword
    assert _first_ttype('bar foo', lexer=cfg) is T.Name
