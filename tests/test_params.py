#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

import pytest

from sqlparse import params
from sqlparse.exceptions import MixedParameterStylesError


def check_ranges(sql, found):
    # Every reported range must slice the exact placeholder text.
    for param in found:
        assert sql[param.start:param.end] == param.text
    assert [p.index for p in found] == list(range(len(found)))


def test_qmark():
    sql = 'SELECT * FROM t WHERE a = ? AND b = ?'
    found = params.find_params(sql)
    assert [p.text for p in found] == ['?', '?']
    assert all(p.style == params.QMARK and p.name is None for p in found)
    assert found[0].start == sql.index('?')
    check_ranges(sql, found)


def test_named():
    sql = 'SELECT * FROM t WHERE a = :foo AND b = :bar_2'
    found = params.find_params(sql)
    assert [p.name for p in found] == ['foo', 'bar_2']
    assert all(p.style == params.NAMED for p in found)
    check_ranges(sql, found)


def test_numeric():
    sql = 'SELECT * FROM t WHERE a = :1 AND b = :2'
    found = params.find_params(sql)
    assert [p.name for p in found] == ['1', '2']
    assert all(p.style == params.NUMERIC for p in found)
    check_ranges(sql, found)


def test_format():
    sql = 'SELECT * FROM t WHERE a = %s AND b = %s'
    found = params.find_params(sql)
    assert [p.text for p in found] == ['%s', '%s']
    assert all(p.style == params.FORMAT and p.name is None for p in found)
    check_ranges(sql, found)


def test_pyformat():
    sql = 'SELECT * FROM t WHERE a = %(foo)s AND b = %(bar)s'
    found = params.find_params(sql)
    assert [p.name for p in found] == ['foo', 'bar']
    assert all(p.style == params.PYFORMAT for p in found)
    check_ranges(sql, found)


def test_dollar():
    sql = 'SELECT * FROM t WHERE a = $1 AND b = $2'
    found = params.find_params(sql, dialect='postgres')
    assert [p.name for p in found] == ['1', '2']
    assert all(p.style == params.DOLLAR for p in found)
    check_ranges(sql, found)


def test_percent_escape_not_counted():
    sql = "SELECT * FROM t WHERE a = %s AND b LIKE 'x%%' AND c = d %% e"
    found = params.find_params(sql)
    assert [p.text for p in found] == ['%s']
    check_ranges(sql, found)


def test_percent_escaped_before_placeholder():
    # %%s is a literal percent followed by the letter s, not a placeholder.
    sql = 'SELECT * FROM t WHERE a = %%s AND b = %s'
    found = params.find_params(sql)
    assert [p.text for p in found] == ['%s']
    assert found[0].start == sql.rindex('%s')


def test_modulo_not_counted():
    sql = 'SELECT a % b FROM t WHERE c = %s'
    found = params.find_params(sql)
    assert [p.text for p in found] == ['%s']


@pytest.mark.parametrize('sql', [
    "SELECT '?'",
    "SELECT 'it''s ?'",
    'SELECT "col?" FROM t',
    'SELECT `col?` FROM t',
    'SELECT 1 -- where x = ?\n',
    'SELECT 1 # where x = ?\n',
    'SELECT 1 /* where x = ? */',
    'SELECT 1 /*+ hint with ? */',
])
def test_quoted_and_commented_text_ignored(sql):
    assert params.find_params(sql) == []


def test_unterminated_string_swallows_rest():
    assert params.find_params("SELECT 'oops ? AND x = ?") == []


def test_cast_not_counted():
    sql = 'SELECT col::text, n::int[]::text FROM t WHERE a = :x'
    found = params.find_params(sql)
    assert [p.text for p in found] == [':x']


def test_assignment_not_counted():
    sql = 'SELECT foo := :bar'
    found = params.find_params(sql)
    assert [p.text for p in found] == [':bar']


def test_postgres_json_operators_not_counted():
    sql = "SELECT * FROM t WHERE data ?| array['a'] AND data ?& array['b']"
    assert params.find_params(sql, dialect='postgres') == []


def test_postgres_json_operators_with_qmark_param():
    sql = "SELECT * FROM t WHERE data ?| array['a'] AND id = ?"
    found = params.find_params(sql, dialect='postgres')
    assert [p.text for p in found] == ['?']


def test_postgres_dollar_quoted_string_ignored():
    sql = 'SELECT $$body with ? and $1$$, $tag$x = ?$tag$ WHERE a = $1'
    found = params.find_params(sql, dialect='postgres')
    assert [p.text for p in found] == ['$1']
    assert found[0].start == sql.rindex('$1')


def test_postgres_unterminated_dollar_quote_is_not_a_quote():
    sql = 'SELECT $$ oops, $1'
    found = params.find_params(sql, dialect='postgres')
    assert [p.text for p in found] == ['$1']


def test_postgres_escape_string():
    sql = "SELECT E'it\\'s still string ?' WHERE a = $1"
    found = params.find_params(sql, dialect='postgres')
    assert [p.text for p in found] == ['$1']


def test_mysql_backslash_escape():
    sql = "SELECT 'a\\' , ?' WHERE x = ?"
    found = params.find_params(sql, dialect='mysql')
    assert [p.text for p in found] == ['?']
    assert found[0].start == sql.rindex('?')
    # Without backslash escapes the string ends at the escaped quote, so
    # the first ? counts and the trailing quote opens an unterminated
    # string that swallows the second one.
    found = params.find_params(sql)
    assert [p.start for p in found] == [sql.index('?')]


def test_mixed_styles_reported():
    sql = 'SELECT * FROM t WHERE a = ? AND b = :x'
    with pytest.raises(MixedParameterStylesError) as excinfo:
        params.find_params(sql)
    assert 'qmark' in str(excinfo.value)
    assert 'named' in str(excinfo.value)


def test_mixed_named_and_numeric_reported():
    with pytest.raises(MixedParameterStylesError):
        params.find_params('SELECT * FROM t WHERE a = :1 AND b = :x')


def test_mixed_format_and_pyformat_reported():
    with pytest.raises(MixedParameterStylesError):
        params.find_params('SELECT * FROM t WHERE a = %s AND b = %(x)s')


def test_explicit_style_filters_other_styles():
    sql = 'SELECT * FROM t WHERE a = ? AND b = :x'
    found = params.find_params(sql, style=params.QMARK)
    assert [p.text for p in found] == ['?']
    found = params.find_params(sql, style=params.NAMED)
    assert [p.text for p in found] == [':x']


def test_no_params():
    assert params.find_params('SELECT 1') == []


def test_unknown_style_rejected():
    with pytest.raises(ValueError):
        params.find_params('SELECT 1', style='brace')


def test_unknown_dialect_rejected():
    with pytest.raises(ValueError):
        params.find_params('SELECT 1', dialect='oracle')
