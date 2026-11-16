#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

"""Tests for the explicit template lexical mode.

ETL templates embed Jinja-style ``{{ ... }}`` expressions and
``{% ... %}`` statements inside SQL.  Each region is a single opaque
token whose text belongs to the template engine: it must not be
keyword-cased, split on inner semicolons or re-indented, and its
original whitespace has to survive formatting.
"""

import pytest

import sqlparse
from sqlparse import lexer, tokens as T


def ttypes(sql):
    return [ttype for ttype, _ in lexer.tokenize(sql)]


# ---------------------------------------------------------------------------
# Lexing: regions become single opaque tokens
# ---------------------------------------------------------------------------

@pytest.mark.parametrize('region, ttype', [
    ('{{ x }}', T.Template.Variable),
    ('{% if x %}', T.Template.Tag),
])
def test_template_region_is_single_opaque_token(region, ttype):
    assert list(lexer.tokenize(region)) == [(ttype, region)]


def test_variable_and_tag_regions_between_sql_tokens():
    sql = 'select {{ col }} from t where {% cond %}'
    tokens = list(lexer.tokenize(sql))
    assert (T.Template.Variable, '{{ col }}') in tokens
    assert (T.Template.Tag, '{% cond %}') in tokens
    # the expression text never appears as standalone SQL tokens
    flat = ''.join(value for _, value in tokens)
    assert flat == sql


def test_template_keywords_are_not_sql_keywords():
    # "select" inside a region must not classify as T.Keyword.DML
    tokens = list(lexer.tokenize('{{ select }} {% from %}'))
    assert tokens == [
        (T.Template.Variable, '{{ select }}'),
        (T.Whitespace, ' '),
        (T.Template.Tag, '{% from %}'),
    ]
    assert all(ttype not in T.Keyword for ttype, _ in tokens)


def test_region_without_inner_spaces():
    assert list(lexer.tokenize('{{x}}')) == [
        (T.Template.Variable, '{{x}}')]
    assert list(lexer.tokenize('{%x%}')) == [
        (T.Template.Tag, '{%x%}')]


def test_region_keeps_internal_whitespace_verbatim():
    region = '{{  user . id   }}\n'
    assert list(lexer.tokenize(region)) == [
        (T.Template.Variable, '{{  user . id   }}'),
        (T.Newline, '\n'),
    ]


def test_multiline_region_is_one_token_preserving_newlines():
    region = '{%\n  for row in rows\n%}'
    assert list(lexer.tokenize(region)) == [(T.Template.Tag, region)]


def test_adjacent_regions_are_separate_tokens():
    assert list(lexer.tokenize('{{ a }}{{ b }}')) == [
        (T.Template.Variable, '{{ a }}'),
        (T.Template.Variable, '{{ b }}'),
    ]


def test_empty_regions():
    assert list(lexer.tokenize('{{}} {% %}')) == [
        (T.Template.Variable, '{{}}'),
        (T.Whitespace, ' '),
        (T.Template.Tag, '{% %}'),
    ]


def test_crlf_inside_region_is_preserved():
    sql = 'select {{\r\n x\r\n}} from t'
    tokens = list(lexer.tokenize(sql))
    assert (T.Template.Variable, '{{\r\n x\r\n}}') in tokens


# ---------------------------------------------------------------------------
# Openers inside SQL strings or comments do not start a region
# ---------------------------------------------------------------------------

@pytest.mark.parametrize('sql', [
    "select '{{ x }}' from t",
    'select "{{ x }}" from t',
    "select '{% x %}' from t",
    'select `{{ x }}` from t',
    "select 'x''{{ y }}' from t",
    '-- {{ x }}\nselect 1',
    '# {{ x }}\nselect 1',
    '/* {{ x }} */ select 1',
    "/* {% x %} */ select 1",
])
def test_openers_in_strings_and_comments_do_not_open_regions(sql):
    assert not any(ttype in T.Template for ttype in ttypes(sql))


def test_opener_in_string_does_not_steal_later_real_closer():
    # The '{{' inside the literal is inert; the region after the literal
    # still has to pair with its own closer.
    sql = "select '{{' from t where x = {{ y }}"
    tokens = list(lexer.tokenize(sql))
    assert (T.Template.Variable, '{{ y }}') in tokens
    assert (T.String.Single, "'{{'") in tokens


def test_real_region_after_comment_with_opener():
    sql = '-- {{\nselect {{ x }} from t'
    tokens = list(lexer.tokenize(sql))
    assert (T.Template.Variable, '{{ x }}') in tokens


@pytest.mark.parametrize('sql', [
    'select $$ {{ x }} $$ from t',
    "select $body$ {{ x }} $body$ from t",
])
def test_openers_inside_dollar_quoted_strings_do_not_open_regions(sql):
    assert not any(ttype in T.Template for ttype in ttypes(sql))


def test_region_can_contain_dollar_quote_and_comment_text():
    sql = '{{ x /* not a comment */ y }}'
    assert list(lexer.tokenize(sql)) == [(T.Template.Variable, sql)]
    sql = '{% if $$x$$ %}'
    assert list(lexer.tokenize(sql)) == [(T.Template.Tag, sql)]


def test_multiline_comment_then_real_region():
    sql = '/* {{ fake */ {{ real }}'
    tokens = list(lexer.tokenize(sql))
    assert (T.Comment.Multiline, '/* {{ fake */') in tokens
    assert (T.Template.Variable, '{{ real }}') in tokens


def test_nested_opener_inside_region_is_unreachable():
    # Jinja nesting is not modeled: the inner opener is part of the outer
    # region, so its first closer ends the region and the dangling closer
    # is diagnosed as ordinary tokenization errors.
    sql = '{{ {{ x }} }}'
    tokens = list(lexer.tokenize(sql))
    assert tokens[0] == (T.Template.Variable, '{{ {{ x }}')
    assert all(ttype not in T.Template for ttype, _ in tokens[1:])


# ---------------------------------------------------------------------------
# Closing delimiters may appear inside quoted segments of a region
# ---------------------------------------------------------------------------

@pytest.mark.parametrize('region, ttype', [
    ("{{ x ~ '}}' }}", T.Template.Variable),
    ('{{ x ~ "}}" }}', T.Template.Variable),
    ("{% if x == '%}' %}", T.Template.Tag),
    ('{{ "it\'\'s }} ok" }}', T.Template.Variable),
    (r'{{ "x \" }} y" }}', T.Template.Variable),
    ("{{ 'a}}b' ~ 'c}}d' }}", T.Template.Variable),
])
def test_closer_inside_quoted_segment_does_not_terminate(region, ttype):
    assert list(lexer.tokenize(region)) == [(ttype, region)]


def test_unterminated_quote_inside_region_keeps_rest_opaque():
    sql = "{{ x ~ 'unclosed }}"
    assert list(lexer.tokenize(sql)) == [(T.Error.Template, sql)]


# ---------------------------------------------------------------------------
# Unclosed regions are diagnosed accurately
# ---------------------------------------------------------------------------

@pytest.mark.parametrize('sql, tail', [
    ('{{ x', '{{ x'),
    ('{% x', '{% x'),
    ('select {{ x', '{{ x'),
    ('select {% x %} {{ y', '{{ y'),
    ("{{ x ~ 'quote' ", "{{ x ~ 'quote' "),
])
def test_unclosed_region_emits_error_spanning_whole_region(sql, tail):
    tokens = list(lexer.tokenize(sql))
    ttype, value = tokens[-1]
    assert ttype is T.Error.Template
    assert ttype in T.Error
    # the error token covers the whole unclosed span, verbatim
    assert value == tail
    assert value.startswith(('{{', '{%'))


def test_unclosed_region_does_not_swallow_following_statement_text():
    # the region only runs to the end of the input, which for an unclosed
    # opener at the end is the whole remainder; verify the prefix survives
    sql = 'select 1; select {{ x'
    tokens = list(lexer.tokenize(sql))
    assert tokens[0] == (T.Keyword.DML, 'select')
    assert tokens[-1] == (T.Error.Template, '{{ x')


def test_bare_closers_outside_regions_are_not_template_tokens():
    tokens = list(lexer.tokenize('select }} from t'))
    assert all(ttype not in T.Template for ttype, _ in tokens)


# ---------------------------------------------------------------------------
# Statement splitting understands the token boundary
# ---------------------------------------------------------------------------

def test_semicolon_inside_region_does_not_split():
    sql = "select {{ ';' }} from t; select 2"
    stmts = sqlparse.split(sql)
    assert len(stmts) == 2
    assert stmts[0].startswith("select {{ ';' }} from t")
    assert stmts[1] == 'select 2'


def test_semicolon_in_tag_region_does_not_split():
    sql = 'select {% if x %} a {% endif %} from t; select 3'
    stmts = sqlparse.split(sql)
    assert len(stmts) == 2
    assert '{% if x %}' in stmts[0]


def test_parentheses_inside_region_do_not_change_split_level():
    # the '(' / ')' inside the region are opaque, so they cannot unbalance
    # the splitter's parenthesis accounting
    sql = 'select {{ f(a, b) }} from t; select 4'
    stmts = sqlparse.split(sql)
    assert len(stmts) == 2


# ---------------------------------------------------------------------------
# Formatting: case filters, reindent and whitespace
# ---------------------------------------------------------------------------

def test_keyword_case_leaves_region_content_untouched():
    sql = 'select {{ user_id }} from t where x = {% if true %}a{% endif %}'
    out = sqlparse.format(sql, keyword_case='upper')
    assert out.startswith('SELECT')
    assert '{{ user_id }}' in out
    assert '{% if true %}' in out


def test_identifier_case_leaves_region_content_untouched():
    sql = 'select {{ Foo }} from bar'
    out = sqlparse.format(sql, identifier_case='lower')
    assert '{{ Foo }}' in out
    assert 'bar' in out


def test_reindent_preserves_region_whitespace_and_value():
    sql = 'select {{  a.b   }},x from t where id={{id}};'
    out = sqlparse.format(sql, reindent=True)
    assert '{{  a.b   }}' in out
    assert '{{id}}' in out


def test_reindent_aligned_keeps_region_intact():
    out = sqlparse.format('select {{a}}, b from t', reindent_aligned=True)
    assert '{{a}}' in out


def test_format_roundtrip_without_options_is_stable():
    sql = 'select {{ x }}\nfrom {% tablename %}'
    assert sqlparse.format(sql) == sql


def test_strip_comments_does_not_remove_regions():
    out = sqlparse.format('select /* c */ {{ x }} from t',
                          strip_comments=True)
    assert '{{ x }}' in out
    assert '/* c */' not in out


def test_region_inside_values_list_roundtrips_through_reindent():
    out = sqlparse.format(
        'insert into t (a, b) values ({{ x }}, {% y %})', reindent=True)
    assert '{{ x }}' in out
    assert '{% y %}' in out


def test_multiline_region_survives_serialization():
    sql = 'select {{\n  x\n}} from t'
    out = sqlparse.format(sql, reindent=True)
    assert '{{\n  x\n}}' in out


def test_strip_whitespace_keeps_region_internals():
    out = sqlparse.format('select {{  x  }} from  t',
                          strip_whitespace=True)
    assert '{{  x  }}' in out
    # whitespace outside regions is still normalized
    assert 'from  t' not in out


def test_region_with_newline_roundtrips_through_serializer():
    sql = 'select {{\n x }} from t'
    assert sqlparse.format(sql) == sql


# ---------------------------------------------------------------------------
# find_template_spans unit behavior
# ---------------------------------------------------------------------------

def test_find_template_spans_reports_all_openers():
    from sqlparse.keywords import find_template_spans
    spans = find_template_spans('{{a}} x {%b%}')
    assert sorted(spans.openers) == [0, 8]
    end, ttype = spans.resolve(0)
    assert (end, ttype) == (5, T.Template.Variable)
    end, ttype = spans.resolve(8)
    assert ttype is T.Template.Tag
    assert end == len('{{a}} x {%b%}')


def test_find_template_spans_skips_when_no_openers():
    from sqlparse.keywords import find_template_spans, _NO_TEMPLATE_SPANS
    assert find_template_spans('select 1') is _NO_TEMPLATE_SPANS
