# Tests for PostgreSQL dump script mode (COPY ... FROM stdin blocks).

from io import StringIO

import pytest

import sqlparse
from sqlparse.engine import pgcopy


def test_split_dump_segments():
    sql = ('SELECT 1;\n'
           'COPY public.foo (id, name) FROM stdin;\n'
           '1\talice\n'
           '2\tbob\n'
           '\\.\n'
           'SELECT 2;\n')
    segments = list(pgcopy.split_dump(sql))
    assert len(segments) == 3
    assert segments[0] == 'SELECT 1;\n'
    assert isinstance(segments[1], pgcopy.CopyBlock)
    block = segments[1]
    assert block.header == 'COPY public.foo (id, name) FROM stdin;\n'
    assert block.data == '1\talice\n2\tbob\n'
    assert block.terminator == '\\.\n'
    assert block.complete
    assert segments[2] == 'SELECT 2;\n'
    # segments reproduce the input
    assert ''.join(str(s) for s in segments) == sql


def test_split_basic():
    sql = ('SELECT 1;\n'
           'COPY public.foo (id, name) FROM stdin;\n'
           '1\talice\n'
           '2\tbob\n'
           '\\.\n'
           'SELECT 2;')
    stmts = sqlparse.split(sql, pg_dump=True)
    assert stmts == [
        'SELECT 1;',
        'COPY public.foo (id, name) FROM stdin;\n1\talice\n2\tbob\n\\.',
        'SELECT 2;',
    ]


def test_data_with_quotes_and_semicolons():
    # quotes and semicolons in the data must not reach the SQL lexer
    sql = ("COPY t (a, b) FROM stdin;\n"
           "1\tit's a ; test \"quoted\"\n"
           "2\t'unclosed\n"
           "3\tselect 1; select 2;\n"
           "\\.\n"
           "SELECT 1;")
    stmts = sqlparse.split(sql, pg_dump=True)
    assert len(stmts) == 2
    assert stmts[1] == 'SELECT 1;'


def test_terminator_only_on_full_line():
    # a terminator is only recognized as a complete physical line
    sql = ('COPY t (a) FROM stdin;\n'
           '1\tdata\\.\n'
           '2\t\\.x\n'
           '3\t\\. trailing\n'
           '\\.\n'
           'SELECT 1;')
    stmts = sqlparse.split(sql, pg_dump=True)
    assert len(stmts) == 2
    assert stmts[1] == 'SELECT 1;'


def test_crlf_line_endings():
    sql = ('COPY t (a) FROM stdin;\r\n'
           '1\tx\r\n'
           '\\.\r\n'
           'SELECT 1;')
    stmts = sqlparse.split(sql, pg_dump=True)
    assert stmts == ['COPY t (a) FROM stdin;\r\n1\tx\r\n\\.', 'SELECT 1;']


def test_terminator_at_eof_without_newline():
    sql = 'COPY t (a) FROM stdin;\n1\tx\n\\.'
    stmts = sqlparse.parse(sql, pg_dump=True)
    assert len(stmts) == 1
    assert isinstance(stmts[0], pgcopy.CopyStatement)
    assert stmts[0].copy_data_complete is True
    assert str(stmts[0]) == sql


def test_missing_terminator_is_incomplete():
    sql = 'COPY t (a) FROM stdin;\n1\tx\n2\ty\n'
    stmts = sqlparse.parse(sql, pg_dump=True)
    assert len(stmts) == 1
    assert isinstance(stmts[0], pgcopy.CopyStatement)
    assert stmts[0].copy_data_complete is False
    # data is still preserved verbatim
    assert str(stmts[0]) == sql


def test_missing_terminator_at_eof_no_trailing_newline():
    sql = 'COPY t (a) FROM stdin;\n1\tx'
    stmts = sqlparse.parse(sql, pg_dump=True)
    assert len(stmts) == 1
    assert stmts[0].copy_data_complete is False
    assert str(stmts[0]) == sql


def test_empty_data_block():
    sql = 'COPY t FROM stdin;\n\\.\nSELECT 1;'
    stmts = sqlparse.split(sql, pg_dump=True)
    assert stmts == ['COPY t FROM stdin;\n\\.', 'SELECT 1;']


def test_normal_splitting_resumes_after_block():
    sql = ('COPY t (a) FROM stdin;\n'
           '1\tx\n'
           '\\.\n'
           "SELECT 'a;b';\n"
           'SELECT 2;')
    stmts = sqlparse.split(sql, pg_dump=True)
    assert len(stmts) == 3
    assert stmts[1] == "SELECT 'a;b';"
    assert stmts[2] == 'SELECT 2;'


def test_copy_to_stdout_is_not_a_block():
    sql = 'COPY t TO stdout;\nSELECT 1;'
    assert sqlparse.split(sql, pg_dump=True) == ['COPY t TO stdout;',
                                                 'SELECT 1;']


def test_copy_from_program_is_not_a_block():
    sql = "COPY t FROM PROGRAM 'cat file';\nSELECT 1;"
    assert sqlparse.split(sql, pg_dump=True) == [
        "COPY t FROM PROGRAM 'cat file';", 'SELECT 1;']


def test_copy_inside_string_is_not_a_block():
    sql = ("SELECT '\nCOPY t FROM stdin;\n';\n"
           'SELECT 1;')
    stmts = sqlparse.split(sql, pg_dump=True)
    assert len(stmts) == 2


def test_copy_inside_function_body_is_not_a_block():
    sql = ('CREATE FUNCTION f() RETURNS void AS $$\n'
           'COPY t FROM stdin;\n'
           '$$ LANGUAGE sql;\n'
           'SELECT 1;')
    stmts = sqlparse.split(sql, pg_dump=True)
    assert len(stmts) == 2


def test_copy_inside_comment_is_not_a_block():
    sql = ('/*\nCOPY t FROM stdin;\n*/\n'
           '-- COPY t FROM stdin;\n'
           'SELECT 1;')
    stmts = sqlparse.split(sql, pg_dump=True)
    assert len(stmts) == 1


def test_quoted_table_name_with_semicolon():
    sql = 'COPY "weird;name" (a) FROM stdin;\n1\tx\n\\.\nSELECT 1;'
    stmts = sqlparse.split(sql, pg_dump=True)
    assert len(stmts) == 2
    assert stmts[0].startswith('COPY "weird;name"')


def test_parse_roundtrip():
    sql = ('-- a dump\n'
           'SELECT 1;\n'
           '\n'
           'COPY public.foo (id, name) FROM stdin;\n'
           '1\talice\n'
           '\\.\n'
           'COPY bar FROM stdin;\n'
           '\\.\n'
           'SELECT 2;')
    stmts = sqlparse.parse(sql, pg_dump=True)
    assert ''.join(str(s) for s in stmts) == sql
    assert isinstance(stmts[1], pgcopy.CopyStatement)
    assert isinstance(stmts[2], pgcopy.CopyStatement)


def test_parsestream_pg_dump():
    sql = 'COPY t FROM stdin;\n1\tx\n\\.\nSELECT 1;'
    stmts = list(sqlparse.parsestream(StringIO(sql), pg_dump=True))
    assert len(stmts) == 2
    assert isinstance(stmts[0], pgcopy.CopyStatement)


def test_pg_dump_keyword_only_mode():
    # without pg_dump=True the data is lexed as SQL (old behavior)
    sql = 'COPY t FROM stdin;\n1\tx\n\\.\nSELECT 1;'
    with_data = sqlparse.split(sql, pg_dump=True)
    without = sqlparse.split(sql)
    assert len(with_data) == 2
    assert with_data != without


def test_split_dump_no_copy_fastpath():
    assert list(pgcopy.split_dump('SELECT 1;')) == ['SELECT 1;']


def test_split_dump_type_error():
    with pytest.raises(TypeError):
        list(pgcopy.split_dump(42))
