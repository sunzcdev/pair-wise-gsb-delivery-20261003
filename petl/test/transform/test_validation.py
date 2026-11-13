# -*- coding: utf-8 -*-
from __future__ import absolute_import, print_function, division


import logging

import pytest

import petl as etl
from petl.transform.validation import (
    validate,
    MISSING_FIELD_ERROR,
    HEADER_PROBLEM,
)
from petl.test.helpers import ieq
from petl.errors import FieldSelectionError


logger = logging.getLogger(__name__)
debug = logger.debug


def test_constraints():

    constraints = [
        dict(name='C1', field='foo', test=int),
        dict(name='C2', field='bar', test=etl.dateparser('%Y-%m-%d')),
        dict(name='C3', field='baz', assertion=lambda v: v in ['Y', 'N']),
        dict(name='C4', assertion=lambda row: None not in row)
    ]

    table = (('foo', 'bar', 'baz'),
             (1, '2000-01-01', 'Y'),
             ('x', '2010-10-10', 'N'),
             (2, '2000/01/01', 'Y'),
             (3, '2015-12-12', 'x'),
             (4, None, 'N'),
             ('y', '1999-99-99', 'z'))

    expect = (('name', 'row', 'field', 'value', 'error'),
              ('C1', 2, 'foo', 'x', 'ValueError'),
              ('C2', 3, 'bar', '2000/01/01', 'ValueError'),
              ('C3', 4, 'baz', 'x', 'AssertionError'),
              ('C2', 5, 'bar', None, 'AttributeError'),
              ('C4', 5, None, None, 'AssertionError'),
              ('C1', 6, 'foo', 'y', 'ValueError'),
              ('C2', 6, 'bar', '1999-99-99', 'ValueError'),
              ('C3', 6, 'baz', 'z', 'AssertionError'))

    actual = validate(table, constraints)
    debug(actual)

    ieq(expect, actual)
    ieq(expect, actual)


def test_non_optional_constraint_with_missing_field():
    constraints = [
        dict(name='C1', field='foo', test=int),
    ]

    table = (('bar', 'baz'),
             ('1999-99-99', 'z'))

    actual = validate(table, constraints)
    with pytest.raises(FieldSelectionError):
        debug(actual)


def test_optional_constraint_with_missing_field():
    constraints = [
        dict(name='C1', field='foo', test=int, optional=True),
    ]

    table = (('bar', 'baz'),
             ('1999-99-99', 'z'))

    expect = (('name', 'row', 'field', 'value', 'error'),)

    actual = validate(table, constraints)
    debug(actual)

    ieq(expect, actual)


def test_row_length():

    table = (('foo', 'bar', 'baz'),
             (1, '2000-01-01', 'Y'),
             ('x', '2010-10-10'),
             (2, '2000/01/01', 'Y', True))

    expect = (('name', 'row', 'field', 'value', 'error'),
              ('__len__', 2, None, 2, 'AssertionError'),
              ('__len__', 3, None, 4, 'AssertionError'))

    actual = validate(table)
    debug(actual)

    ieq(expect, actual)
    ieq(expect, actual)


def test_header():

    header = ('foo', 'bar', 'baz')

    table = (('foo', 'bar', 'bazzz'),
             (1, '2000-01-01', 'Y'),
             ('x', '2010-10-10', 'N'))

    expect = (('name', 'row', 'field', 'value', 'error'),
              ('__header__', 0, None, None, 'AssertionError'))

    actual = validate(table, header=header)
    debug(actual)

    ieq(expect, actual)
    ieq(expect, actual)

    header = ('foo', 'bar', 'baz', 'quux')

    table = (('foo', 'bar', 'baz'),
             (1, '2000-01-01', 'Y'),
             ('x', '2010-10-10', 'N'))

    expect = (('name', 'row', 'field', 'value', 'error'),
              ('__header__', 0, None, None, 'AssertionError'),
              ('__len__', 1, None, 3, 'AssertionError'),
              ('__len__', 2, None, 3, 'AssertionError'))

    actual = validate(table, header=header)
    debug(actual)

    ieq(expect, actual)
    ieq(expect, actual)


def test_validation_headerless():
    header = ('foo', 'bar', 'baz')
    table = []
    # Expect only a missing header - no exceptions please
    expect = (('name', 'row', 'field', 'value', 'error'),
              ('__header__', 0, None, None, 'AssertionError'))
    actual = validate(table, header=header)
    ieq(expect, actual)
    ieq(expect, actual)


def test_cross_field_constraint_pass_fail_and_undetermined():
    # a temperature range only applies when the unit is celsius
    constraints = [
        dict(name='celsius_range',
             depends_on=('unit', 'temperature'),
             assertion=lambda row: row['unit'] != 'C'
             or -273.15 <= row['temperature'] <= 1000),
    ]

    table = (('unit', 'temperature'),
             ('C', 20),          # pass
             ('F', 99999),       # pass: dependency present, rule not triggered
             ('C', 5000),        # fail
             ('K',))             # undetermined: temperature missing from row

    expect = (('name', 'row', 'field', 'value', 'error'),
              ('celsius_range', 3, None, None, 'AssertionError'),
              ('__len__', 4, None, 1, 'AssertionError'),
              ('celsius_range', 4, 'temperature', None,
               MISSING_FIELD_ERROR))

    actual = validate(table, constraints)
    ieq(expect, actual)
    ieq(expect, actual)


def test_depends_alias_accepts_single_field():
    constraints = [
        dict(name='unit_known', depends='unit',
             assertion=lambda row: row['unit'] in ('C', 'F', 'K')),
    ]
    table = (('unit', 'temperature'),
             ('C', 20),
             ('X', 5))
    expect = (('name', 'row', 'field', 'value', 'error'),
              ('unit_known', 2, None, None, 'AssertionError'))
    ieq(expect, validate(table, constraints))


def test_missing_dependency_in_header_reported_once_before_data():
    constraints = [
        dict(name='celsius_range', depends_on='unit',
             assertion=lambda row: row['unit'] == 'C'),
    ]
    # three data rows, but the missing header field is reported only once
    table = (('temperature',), (1,), (2,), (3,))

    expect = (('name', 'row', 'field', 'value', 'error'),
              ('celsius_range', 0, 'unit', None, MISSING_FIELD_ERROR))

    actual = validate(table, constraints)
    problems = list(actual)
    assert problems == list(expect)
    # the constraint never ran against a data row
    assert all(row[1] == 0 for row in problems[1:])


def test_multiple_missing_dependencies_each_reported_once():
    constraints = [
        dict(name='celsius_range', depends_on=('unit', 'scale')),
    ]
    table = (('temperature',), (1,), (2,))
    expect = (('name', 'row', 'field', 'value', 'error'),
              ('celsius_range', 0, 'unit', None, MISSING_FIELD_ERROR),
              ('celsius_range', 0, 'scale', None, MISSING_FIELD_ERROR))
    ieq(expect, validate(table, constraints))


def test_undetermined_is_not_silently_passed():
    # a ragged row missing the dependency field must show up explicitly
    constraints = [
        dict(name='needs_b', depends_on='b',
             assertion=lambda row: row['b'] > 0),
    ]
    table = (('a', 'b'),
             (1, 2),
             (3,))
    problems = list(validate(table, constraints))
    assert ('needs_b', 2, 'b', None, MISSING_FIELD_ERROR) in problems


def test_constraint_exception_is_isolated_and_recorded():

    def boom(value):
        raise RuntimeError('boom')

    constraints = [
        dict(name='boom', field='unit', test=boom),
        dict(name='still_runs', field='temperature',
             assertion=lambda v: v < 100),
    ]
    table = (('unit', 'temperature'),
             ('C', 5000),
             ('F', 10))

    expect = (('name', 'row', 'field', 'value', 'error'),
              # first constraint blows up, but the second still evaluates
              ('boom', 1, 'unit', 'C', 'RuntimeError'),
              ('still_runs', 1, 'temperature', 5000, 'AssertionError'),
              ('boom', 2, 'unit', 'F', 'RuntimeError'))

    ieq(expect, validate(table, constraints))


def test_failonerror_aborts_immediately():

    class BoomError(Exception):
        pass

    def boom(value):
        raise BoomError()

    constraints = [
        dict(name='boom', field='unit', test=boom),
        dict(name='never_reached', field='temperature',
             assertion=lambda v: True),
    ]
    table = (('unit', 'temperature'),
             ('C', 1),
             ('F', 2))

    with pytest.raises(BoomError):
        list(validate(table, constraints, failonerror=True))


def test_failonerror_false_keeps_collecting_problems():

    def boom(value):
        raise ValueError()

    constraints = [
        dict(name='boom', field='unit', test=boom),
    ]
    table = (('unit', 'temperature'), ('C', 1), ('F', 2))
    actual = validate(table, constraints, failonerror=False)
    assert len(list(actual)) == 3  # header plus one problem per data row


class OneShotTable(object):
    """A table whose rows can only ever be produced once.

    Backed by a single iterator: once exhausted it yields nothing on any
    further iteration, so any extra scan during validation would silently
    drop rows or headers rather than be hidden by a re-readable container.
    """

    def __init__(self, rows):
        self._it = iter(rows)
        self.iterations = 0

    def __iter__(self):
        self.iterations += 1
        return self._it


def _consume_once(view):
    # one explicit pass; avoid list()/len() which re-iterates the view
    return [row for row in iter(view)]


def test_validate_does_not_rescan_the_source_table():
    constraints = [
        dict(name='foo_int', field='foo', test=int),
        dict(name='celsius', depends_on='bar',
             assertion=lambda row: row['bar'] in ('Y', 'N')),
    ]
    source = OneShotTable(iter([('foo', 'bar'),
                                (1, 'Y'),
                                ('x', 'N')]))

    problems = _consume_once(validate(source, constraints))

    # a single pass: header read once, no extra scans of the same rows
    iterations = source.iterations
    assert iterations == 1
    assert problems[0] == ('name', 'row', 'field', 'value', 'error')
    assert ('foo_int', 2, 'foo', 'x', 'ValueError') in problems


def test_structural_missing_dependency_does_not_rescan_rows():
    constraints = [
        dict(name='needs_unit', depends_on='unit'),
    ]
    source = OneShotTable(iter([('temperature',), (1,), (2,), (3,)]))
    problems = _consume_once(validate(source, constraints))
    iterations = source.iterations
    assert iterations == 1
    assert problems == [
        ('name', 'row', 'field', 'value', 'error'),
        ('needs_unit', 0, 'unit', None, MISSING_FIELD_ERROR),
    ]


def test_legacy_constraints_ignore_undetermined_on_short_rows():
    # old format: a short row keeps the historical Record/getter behaviour
    # rather than being reported as a missing-field (undetermined) result
    constraints = [
        dict(name='baz_enum', field='baz',
             assertion=lambda v: v in ['Y', 'N']),
    ]
    table = (('foo', 'bar', 'baz'),
             (1, 'a', 'Y'),
             (2, 'b'))
    expect = (('name', 'row', 'field', 'value', 'error'),
              ('__len__', 2, None, 2, 'AssertionError'),
              ('baz_enum', 2, 'baz', None, 'AssertionError'))
    ieq(expect, validate(table, constraints))


def test_header_problem_constant():
    assert HEADER_PROBLEM == '__header__'
