#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

import pytest

import sqlparse
from sqlparse.engine import DirectiveEvent, StatementEvent
from sqlparse.exceptions import SQLParseError


def statements(events):
    return [event for event in events if event.kind == 'statement']


def directives(events):
    return [event for event in events if event.kind == 'directive']


def test_default_mode_matches_split():
    sql = 'SELECT 1;\nSELECT 2;\n\nSELECT 3'
    events = sqlparse.split_script(sql)
    assert all(isinstance(event, StatementEvent) for event in events)
    assert [event.statement for event in events] == [
        'SELECT 1;', 'SELECT 2;', 'SELECT 3']
    # Mode off keeps the original splitting of sqlparse.split().
    assert [event.statement for event in events] == sqlparse.split(sql)


def test_default_mode_ignores_delimiter_lines():
    sql = 'DELIMITER //\nSELECT 1;'
    events = sqlparse.split_script(sql, client_directives=False)
    assert not directives(events)
    assert [event.statement for event in events] == ['DELIMITER //\nSELECT 1;']


def test_delimiter_switch_and_procedure_body():
    sql = (
        'DELIMITER //\n'
        'CREATE PROCEDURE p()\n'
        'BEGIN\n'
        '  SELECT 1;\n'
        '  SELECT 2;\n'
        'END//\n'
        'DELIMITER ;\n'
        'SELECT 3;\n'
    )
    events = sqlparse.split_script(sql, client_directives=True)
    kinds = [event.kind for event in events]
    assert kinds == ['directive', 'statement', 'directive', 'statement']
    stmt = statements(events)[0]
    # Semicolons inside the procedure body must not split the statement.
    assert stmt.statement == (
        'CREATE PROCEDURE p()\nBEGIN\n  SELECT 1;\n  SELECT 2;\nEND//')
    assert stmt.delimiter == '//'
    assert statements(events)[1].statement == 'SELECT 3;'
    dirs = directives(events)
    assert [d.delimiter for d in dirs] == ['//', ';']
    assert [d.line_no for d in dirs] == [1, 7]


def test_directive_events_carry_original_line_numbers():
    sql = (
        '\n'
        '\n'
        'SELECT 1;\n'
        'DELIMITER $$\n'
        'SELECT 2$$\n'
    )
    events = sqlparse.split_script(sql, client_directives=True)
    (directive,) = directives(events)
    assert directive.line_no == 4
    assert directive.delimiter == '$$'
    stmt = statements(events)[1]
    assert stmt.statement == 'SELECT 2$$'
    assert stmt.delimiter == '$$'
    assert stmt.line_no == 5


def test_directive_inside_string_has_no_effect():
    sql = (
        "SELECT '\n"
        'DELIMITER //\n'
        "';\n"
        'SELECT 2;\n'
    )
    events = sqlparse.split_script(sql, client_directives=True)
    assert not directives(events)
    assert len(statements(events)) == 2
    assert statements(events)[0].statement == "SELECT '\nDELIMITER //\n';"


def test_directive_inside_comment_has_no_effect():
    sql = (
        '/*\n'
        'DELIMITER //\n'
        '*/\n'
        'SELECT 1;\n'
        '-- DELIMITER $$\n'
        'SELECT 2;\n'
    )
    events = sqlparse.split_script(sql, client_directives=True)
    assert not directives(events)
    assert len(statements(events)) == 2


def test_delimiter_inside_string_does_not_terminate():
    sql = "SELECT 'a;b';\nSELECT 2;\n"
    events = sqlparse.split_script(sql, client_directives=True)
    assert [event.statement for event in statements(events)] == [
        "SELECT 'a;b';", 'SELECT 2;']


def test_delimiter_inside_comment_does_not_terminate():
    sql = 'SELECT 1 /* ; */;\nSELECT 2;\n'
    events = sqlparse.split_script(sql, client_directives=True)
    assert [event.statement for event in statements(events)] == [
        'SELECT 1 /* ; */;', 'SELECT 2;']


@pytest.mark.parametrize('line', ['DELIMITER', 'DELIMITER   '])
def test_empty_delimiter_rejected(line):
    with pytest.raises(SQLParseError, match='[Ee]mpty delimiter'):
        sqlparse.split_script(line + '\nSELECT 1;\n', client_directives=True)


@pytest.mark.parametrize('line', ['DELIMITER / /', 'DELIMITER /\t/'])
def test_whitespace_in_delimiter_rejected(line):
    with pytest.raises(SQLParseError, match='[Ww]hitespace'):
        sqlparse.split_script(line + '\nSELECT 1;\n', client_directives=True)


def test_invalid_delimiter_in_string_is_not_rejected():
    sql = "SELECT '\nDELIMITER\n';\n"
    events = sqlparse.split_script(sql, client_directives=True)
    assert not directives(events)


def test_directive_must_occupy_own_line():
    sql = 'SELECT 1; DELIMITER //\nSELECT 2;\n'
    events = sqlparse.split_script(sql, client_directives=True)
    assert not directives(events)


def test_directive_keyword_is_case_insensitive():
    sql = 'delimiter //\nSELECT 1//\n'
    events = sqlparse.split_script(sql, client_directives=True)
    (directive,) = directives(events)
    assert directive.delimiter == '//'
    assert statements(events)[0].statement == 'SELECT 1//'


def test_unterminated_statement_before_directive():
    sql = 'SELECT 1\nDELIMITER //\nSELECT 2//\n'
    events = sqlparse.split_script(sql, client_directives=True)
    assert [event.kind for event in events] == [
        'statement', 'directive', 'statement']
    assert statements(events)[0].delimiter == ''
    assert statements(events)[0].statement == 'SELECT 1'


def test_trailing_unterminated_statement():
    sql = 'SELECT 1;\nSELECT 2\n'
    events = sqlparse.split_script(sql, client_directives=True)
    stmts = statements(events)
    assert stmts[-1].statement == 'SELECT 2'
    assert stmts[-1].delimiter == ''


@pytest.mark.parametrize('client_directives', [False, True])
def test_source_reconstructable_from_event_ranges(client_directives):
    sql = (
        '-- a dump\n'
        'DELIMITER //\n'
        'CREATE PROCEDURE p()\n'
        'BEGIN\n'
        '  SELECT 1;\n'
        'END//\n'
        '\n'
        'DELIMITER ;\n'
        "SELECT 'x;y';\n"
        '\n'
    )
    if not client_directives:
        sql = 'SELECT 1;\n\nSELECT 2;\n'
    events = sqlparse.split_script(sql, client_directives=client_directives)
    assert ''.join(sql[event.start:event.end] for event in events) == sql


def test_event_ranges_are_contiguous():
    sql = 'SELECT 1;\n\n\nDELIMITER //\n\nSELECT 2//\n\n'
    events = sqlparse.split_script(sql, client_directives=True)
    assert events[0].start == 0
    for prev, cur in zip(events, events[1:]):
        assert prev.end == cur.start
    assert events[-1].end == len(sql)


def test_multichar_delimiter_with_semicolon_content():
    sql = (
        'DELIMITER $$\n'
        'CREATE TRIGGER t BEFORE INSERT ON a FOR EACH ROW\n'
        'BEGIN\n'
        '  SET NEW.x = 1;\n'
        'END$$\n'
        'DELIMITER ;\n'
    )
    events = sqlparse.split_script(sql, client_directives=True)
    (stmt,) = statements(events)
    assert stmt.statement.count(';') == 1  # only the body semicolon
    assert stmt.delimiter == '$$'


def test_empty_script():
    assert sqlparse.split_script('', client_directives=True) == []
    assert sqlparse.split_script('  \n ', client_directives=True) == []
    assert sqlparse.split_script('') == []


def test_split_script_exposed_in_engine():
    from sqlparse import engine
    assert engine.split_script is not None
    assert issubclass(DirectiveEvent, object)
