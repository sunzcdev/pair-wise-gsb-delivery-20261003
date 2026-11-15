#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

import pytest

import sqlparse
from sqlparse.exceptions import SQLParseError


class TestProtectedRegionsValidation:
    def test_boundary_inside_token(self):
        # "from" starts at offset 11; offset 12 is inside the token.
        with pytest.raises(SQLParseError):
            sqlparse.format('select foo from bar',
                            protected_regions=[(12, 15)])

    def test_boundary_inside_token_at_end(self):
        with pytest.raises(SQLParseError):
            sqlparse.format('select foo from bar',
                            protected_regions=[(11, 14)])

    def test_region_past_end_of_statement(self):
        with pytest.raises(SQLParseError):
            sqlparse.format('select 1', protected_regions=[(0, 100)])

    def test_overlapping_regions(self):
        with pytest.raises(SQLParseError):
            sqlparse.format('select foo from bar',
                            protected_regions=[(0, 6), (3, 9)])

    def test_overlapping_regions_unsorted(self):
        with pytest.raises(SQLParseError):
            sqlparse.format('select foo from bar',
                            protected_regions=[(3, 9), (0, 6)])

    def test_region_end_before_start(self):
        with pytest.raises(SQLParseError):
            sqlparse.format('select 1', protected_regions=[(4, 2)])

    def test_negative_region(self):
        with pytest.raises(SQLParseError):
            sqlparse.format('select 1', protected_regions=[(-1, 2)])

    def test_malformed_region(self):
        with pytest.raises(SQLParseError):
            sqlparse.format('select 1', protected_regions=[(1,)])
        with pytest.raises(SQLParseError):
            sqlparse.format('select 1', protected_regions=['x'])
        with pytest.raises(SQLParseError):
            sqlparse.format('select 1', protected_regions=[('0', 2)])

    def test_empty_region_is_ignored(self):
        res = sqlparse.format('select foo from bar', keyword_case='upper',
                              protected_regions=[(5, 5)])
        assert res == 'SELECT foo FROM bar'

    def test_adjacent_regions_do_not_overlap(self):
        # "a" at 7-8, "," at 8-9: adjacent regions are fine.
        res = sqlparse.format('select a,b from t', identifier_case='upper',
                              protected_regions=[(7, 8), (9, 10)])
        assert res == 'select a,b from T'

    def test_unsorted_regions_are_normalized(self):
        res = sqlparse.format('select foo from bar where baz = 1',
                              keyword_case='upper',
                              protected_regions=[(20, 25), (11, 15)])
        assert res == 'SELECT foo from bar where baz = 1'


class TestProtectedRegionsCase:
    def test_keyword_case(self):
        res = sqlparse.format('select foo from bar', keyword_case='upper',
                              protected_regions=[(11, 15)])
        assert res == 'SELECT foo from bar'

    def test_keyword_case_multiple_regions(self):
        res = sqlparse.format('select foo from bar where baz = 1',
                              keyword_case='upper',
                              protected_regions=[(11, 15), (20, 25)])
        assert res == 'SELECT foo from bar where baz = 1'

    def test_identifier_case(self):
        res = sqlparse.format('select foo from bar', identifier_case='upper',
                              protected_regions=[(7, 10)])
        assert res == 'select foo from BAR'

    def test_truncate_strings(self):
        sql = "select 'abcdefghij', 'klmnopqrst'"
        res = sqlparse.format(sql, truncate_strings=4,
                              protected_regions=[(7, 19)])
        assert res == "select 'abcdefghij', 'klmn[...]'"


class TestProtectedRegionsWhitespace:
    def test_strip_whitespace(self):
        res = sqlparse.format('select  a   from t', strip_whitespace=True,
                              protected_regions=[(6, 8)])
        assert res == 'select  a from t'

    def test_reindent_keeps_protected_keyword_verbatim(self):
        res = sqlparse.format('select a from t where x = 1', reindent=True,
                              keyword_case='upper', protected_regions=[(9, 13)])
        assert res == 'SELECT a\nfrom t\nWHERE x = 1'

    def test_reindent_keeps_protected_newlines(self):
        sql = 'select a,\n\n\n   b from t'
        res = sqlparse.format(sql, reindent=True, protected_regions=[(9, 15)])
        assert res == 'select a,\n\n\n   \n       b\nfrom t'

    def test_reindent_aligned(self):
        sql = 'select a, b from t where x = 1'
        res = sqlparse.format(sql, reindent_aligned=True, keyword_case='upper',
                              protected_regions=[(19, 24)])
        assert res == 'SELECT a,\n       b\n  FROM t\n where x = 1'

    def test_columns_after_protected_newline(self):
        # The protected comment contains a newline; indentation of the
        # following identifiers must be computed from the column after
        # the last protected line break ("line */ a" -> column 8).
        sql = 'select /* multi\nline */ a, b, c from t'
        res = sqlparse.format(sql, reindent=True, protected_regions=[(7, 23)])
        assert res == 'select /* multi\nline */ a,\n        b,\n        c\nfrom t'

    def test_use_space_around_operators(self):
        res = sqlparse.format('select a+b from t',
                              use_space_around_operators=True,
                              protected_regions=[(7, 10)])
        assert res == 'select a+b from t'

    def test_entire_statement_protected(self):
        sql = 'select  a   from t'
        res = sqlparse.format(sql, reindent=True, keyword_case='upper',
                              protected_regions=[(0, 18)])
        assert res == sql


class TestProtectedRegionsStripComments:
    def test_strip_comments(self):
        # The comment token is "-- keep\n" (offsets 9-17).
        sql = 'select a -- keep\nfrom t -- drop'
        res = sqlparse.format(sql, strip_comments=True,
                              protected_regions=[(9, 17)])
        assert res == 'select a -- keep\nfrom t'

    def test_strip_comments_unprotected(self):
        sql = 'select a -- keep\nfrom t -- drop'
        res = sqlparse.format(sql, strip_comments=True)
        assert res == 'select a\nfrom t'

    def test_strip_comments_multiline(self):
        sql = 'select /* keep\nthis */ a from t'
        res = sqlparse.format(sql, strip_comments=True,
                              protected_regions=[(7, 22)])
        assert res == sql


class TestProtectedRegionsOutput:
    def test_output_python_no_escaping(self):
        sql = "select 'back\\slash'"
        res = sqlparse.format(sql, output_format='python',
                              protected_regions=[(7, 19)])
        assert res == "sql = 'select 'back\\slash''"

    def test_output_python_escapes_unprotected(self):
        sql = "select 'back\\slash'"
        res = sqlparse.format(sql, output_format='python')
        assert res == "sql = 'select \\'back\\\\slash\\''"

    def test_output_php_no_escaping(self):
        sql = "select 'back\\slash'"
        res = sqlparse.format(sql, output_format='php',
                              protected_regions=[(7, 19)])
        assert res == '$sql = "select \'back\\slash\'";'


class TestProtectedRegionsSerializer:
    def test_protected_trailing_whitespace(self):
        # Trailing whitespace inside the protected region survives the
        # unicode serializer, unprotected trailing whitespace is stripped.
        sql = 'select a,   \n  b from t'
        res = sqlparse.format(sql, protected_regions=[(9, 14)])
        assert res == 'select a,   \n  b from t'

    def test_unprotected_trailing_whitespace_is_stripped(self):
        sql = 'select a,   \n  b from t'
        res = sqlparse.format(sql)
        assert res == 'select a,\n  b from t'

    def test_protected_crlf(self):
        sql = 'select a,\r\nb from t'
        res = sqlparse.format(sql, protected_regions=[(9, 11)])
        assert res == 'select a,\r\nb from t'

    def test_unprotected_crlf_is_normalized(self):
        sql = 'select a,\r\nb from t'
        res = sqlparse.format(sql)
        assert res == 'select a,\nb from t'

    def test_no_options_returns_input(self):
        sql = 'select  a,\n  b from t -- tail'
        assert sqlparse.format(sql, protected_regions=[(0, 20)]) == sql


class TestProtectedRegionsMisc:
    def test_format_without_regions_unchanged(self):
        # The regular entry point does not require protected regions.
        res = sqlparse.format('select foo from bar', keyword_case='upper')
        assert res == 'SELECT foo FROM bar'

    def test_parse_tokens_are_not_protected(self):
        stmt = sqlparse.parse('select 1')[0]
        assert not any(t.is_protected for t in stmt.flatten())

    def test_region_covering_several_tokens(self):
        # Region spans "foo  from" including the whitespace in between.
        res = sqlparse.format('select foo  from bar', keyword_case='upper',
                              protected_regions=[(7, 16)])
        assert res == 'SELECT foo  from bar'
