# -*- coding: utf-8 -*-
from __future__ import absolute_import, print_function, division


import operator

import petl.config as config

from petl.compat import text_type
from petl.errors import MissingFieldError
from petl.util.base import Table, asindices, Record


# name used for structural problems found before any data row is read
HEADER_PROBLEM = '__header__'
# error label for a constraint that cannot be evaluated due to a missing field
MISSING_FIELD_ERROR = MissingFieldError.__name__


def validate(table, constraints=None, header=None, failonerror=None):
    """
    Validate a `table` against a set of `constraints` and/or an expected
    `header`, e.g.::

        >>> import petl as etl
        >>> # define some validation constraints
        ... header = ('foo', 'bar', 'baz')
        >>> constraints = [
        ...     dict(name='foo_int', field='foo', test=int),
        ...     dict(name='bar_date', field='bar', test=etl.dateparser('%Y-%m-%d')),
        ...     dict(name='baz_enum', field='baz', assertion=lambda v: v in ['Y', 'N']),
        ...     dict(name='not_none', assertion=lambda row: None not in row),
        ...     dict(name='qux_int', field='qux', test=int, optional=True),
        ... ]
        >>> # now validate a table
        ... table = (('foo', 'bar', 'bazzz'),
        ...          (1, '2000-01-01', 'Y'),
        ...          ('x', '2010-10-10', 'N'),
        ...          (2, '2000/01/01', 'Y'),
        ...          (3, '2015-12-12', 'x'),
        ...          (4, None, 'N'),
        ...          ('y', '1999-99-99', 'z'),
        ...          (6, '2000-01-01'),
        ...          (7, '2001-02-02', 'N', True))
        >>> problems = etl.validate(table, constraints=constraints, header=header)
        >>> problems.lookall()
        +--------------+-----+-------+--------------+------------------+
        | name         | row | field | value        | error            |
        +==============+=====+=======+==============+==================+
        | '__header__' |   0 | None  | None         | 'AssertionError' |
        +--------------+-----+-------+--------------+------------------+
        | 'foo_int'    |   2 | 'foo' | 'x'          | 'ValueError'     |
        +--------------+-----+-------+--------------+------------------+
        | 'bar_date'   |   3 | 'bar' | '2000/01/01' | 'ValueError'     |
        +--------------+-----+-------+--------------+------------------+
        | 'baz_enum'   |   4 | 'baz' | 'x'          | 'AssertionError' |
        +--------------+-----+-------+--------------+------------------+
        | 'bar_date'   |   5 | 'bar' | None         | 'AttributeError' |
        +--------------+-----+-------+--------------+------------------+
        | 'not_none'   |   5 | None  | None         | 'AssertionError' |
        +--------------+-----+-------+--------------+------------------+
        | 'foo_int'    |   6 | 'foo' | 'y'          | 'ValueError'     |
        +--------------+-----+-------+--------------+------------------+
        | 'bar_date'   |   6 | 'bar' | '1999-99-99' | 'ValueError'     |
        +--------------+-----+-------+--------------+------------------+
        | 'baz_enum'   |   6 | 'baz' | 'z'          | 'AssertionError' |
        +--------------+-----+-------+--------------+------------------+
        | '__len__'    |   7 | None  |            2 | 'AssertionError' |
        +--------------+-----+-------+--------------+------------------+
        | 'baz_enum'   |   7 | 'baz' | None         | 'AssertionError' |
        +--------------+-----+-------+--------------+------------------+
        | '__len__'    |   8 | None  |            4 | 'AssertionError' |
        +--------------+-----+-------+--------------+------------------+

    Returns a table of validation problems.

    Constraints are evaluated independently: an exception raised by one
    user supplied function is recorded (with the constraint name, row number
    and exception type) and the remaining constraints still run. Pass
    ``failonerror=True`` (or set :data:`petl.config.failonerror`) to re-raise
    the first exception raised by a user supplied function instead, aborting
    validation immediately.

    A constraint may declare the fields it depends on through ``depends_on``
    (``depends`` is accepted as an alias), e.g.::

        >>> constraints = [
        ...     dict(name='celsius_range',
        ...          depends_on=('unit', 'temperature'),
        ...          assertion=lambda row: row['unit'] != 'C'
        ...          or -273.15 <= row['temperature'] <= 1000),
        ... ]

    A dependency missing from the table header is reported once, before any
    data row is read, as a structural problem with error
    ``'MissingFieldError'`` and the constraint is then skipped for every
    row. A dependency missing from an individual (short) row is reported for
    that row with the same error label. Such an "undetermined" result is
    never silently treated as a pass: it appears explicitly in the problems
    table. Constraints without ``depends_on`` keep their historical behaviour.

    """  # noqa

    return ProblemsView(table, constraints=constraints, header=header,
                        failonerror=failonerror)


Table.validate = validate


class ProblemsView(Table):

    def __init__(self, table, constraints, header, failonerror=None):
        self.table = table
        self.constraints = constraints
        self._header = header
        self.failonerror = (config.failonerror if failonerror is None
                            else failonerror)

    def __iter__(self):
        return iterproblems(self.table, self.constraints, self._header,
                            self.failonerror)


def normalize_constraints(constraints, flds):
    """
    This method renders local constraints such that return value is:
      * a list, not None
      * a list of dicts
      * a list of non-optional constraints or optional with defined field

    .. note:: We use a new variable 'local_constraints' because the constraints
              parameter may be a mutable collection, and we do not wish to
              cause side-effects by modifying it locally
    """
    local_constraints = constraints or []
    local_constraints = [dict(**c) for c in local_constraints]
    local_constraints = [
        c for c in local_constraints
        if c.get('field') in flds or
        not c.get('optional')
    ]
    return local_constraints


def _dependent_fields(constraint):
    """Return the fields a constraint depends on, as a tuple."""
    depends = constraint.get('depends_on',
                             constraint.get('depends', None))
    if depends is None:
        return ()
    if isinstance(depends, (list, tuple)):
        return tuple(depends)
    return (depends,)


def _missing_header_fields(flds, fields):
    """Return the dependency fields absent from the header, in order."""
    flds = list(map(text_type, flds))
    missing = []
    for field in fields:
        if isinstance(field, int):
            if not 0 <= field < len(flds):
                if field not in missing:
                    missing.append(field)
        elif field not in flds:
            if field not in missing:
                missing.append(field)
    return missing


def iterproblems(table, constraints, expected_header, failonerror=None):

    if failonerror is None:
        failonerror = config.failonerror

    outhdr = ('name', 'row', 'field', 'value', 'error')
    yield outhdr

    it = iter(table)
    try:
        actual_header = next(it)
    except StopIteration:
        actual_header = []

    if expected_header is None:
        flds = list(map(text_type, actual_header))
    else:
        expected_flds = list(map(text_type, expected_header))
        actual_flds = list(map(text_type, actual_header))
        try:
            assert expected_flds == actual_flds
        except Exception as e:
            yield (HEADER_PROBLEM, 0, None, None, type(e).__name__)
        flds = expected_flds

    local_constraints = normalize_constraints(constraints, flds)

    # setup getters and resolve dependency fields
    for constraint in local_constraints:
        header_missing = _missing_header_fields(
            flds, _dependent_fields(constraint))
        constraint['_header_missing'] = header_missing
        if header_missing:
            # the constraint cannot run on any row; only build the
            # structural problem below and skip field/getter resolution
            constraint['_required_indices'] = []
            continue
        if 'getter' not in constraint and 'field' in constraint:
            # should ensure FieldSelectionError if bad field in constraint
            indices = asindices(flds, constraint['field'])
            getter = operator.itemgetter(*indices)
            constraint['getter'] = getter
        # only explicitly declared dependencies can leave a result
        # undetermined; a constraint's own field keeps its historical
        # behaviour (short rows are handled by the Record/getter)
        constraint['_required_indices'] = asindices(
            flds, _dependent_fields(constraint))

    # structural problems: dependencies missing from the table header are
    # reported once, before any data row is read
    for constraint in local_constraints:
        for field in constraint['_header_missing']:
            yield (constraint.get('name', None), 0, field, None,
                   MISSING_FIELD_ERROR)

    # generate problems
    expected_len = len(flds)
    for i, raw_row in enumerate(it):
        raw_row = tuple(raw_row)

        # row length constraint
        length = None
        try:
            length = len(raw_row)
            assert length == expected_len
        except Exception as e:
            yield ('__len__', i + 1, None, length, type(e).__name__)

        # user defined constraints, each evaluated independently
        row = Record(raw_row, flds)
        for constraint in local_constraints:
            name = constraint.get('name', None)
            field = constraint.get('field', None)
            assertion = constraint.get('assertion', None)
            test = constraint.get('test', None)
            getter = constraint.get('getter', lambda x: x)

            # a header-level dependency is missing: already reported once
            # above, so the constraint cannot run on any row
            if constraint['_header_missing']:
                continue

            # undetermined: a required field is absent from this (short) row
            row_missing = []
            for idx in constraint['_required_indices']:
                if idx >= len(raw_row) and idx not in row_missing:
                    row_missing.append(idx)
            if row_missing:
                for idx in row_missing:
                    yield (name, i + 1, flds[idx], None,
                           MISSING_FIELD_ERROR)
                continue

            try:
                target = getter(row)
            except Exception as e:
                if failonerror:
                    raise
                # getting target value failed, report problem
                yield (name, i + 1, field, None, type(e).__name__)
            else:
                value = target if field else None
                if test is not None:
                    try:
                        test(target)
                    except Exception as e:
                        if failonerror:
                            raise
                        # test raised exception, report problem
                        yield (name, i + 1, field, value,
                               type(e).__name__)
                if assertion is not None:
                    try:
                        assert assertion(target)
                    except Exception as e:
                        if failonerror:
                            raise
                        # assertion raised exception, report problem
                        yield (name, i + 1, field, value,
                               type(e).__name__)
