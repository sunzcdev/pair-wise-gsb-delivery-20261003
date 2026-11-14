from __future__ import absolute_import, print_function, division

import pytest

import petl as etl
from petl import FieldOccurrence
from petl.errors import ArgumentError, FieldSelectionError
from petl.test.helpers import ieq
from petl.util.base import asindices


# A table with a duplicated 'foo' header, a non-string header (4) and a
# short final row so that padding can also be exercised.
DUPLICATE_TABLE = [
    ['foo', 'bar', 'foo', 4],
    [1, 2, 3, 40],
    [10, 20],
]


def test_field_occurrence_is_tuple_like():
    ref = FieldOccurrence('foo', 2)
    assert tuple(ref) == ('foo', 2)
    assert ref.field == 'foo'
    assert ref.occurrence == 2
    assert ref == ('foo', 2)
    # usable as a dictionary key, e.g. for rename/convert specs
    assert {ref: 'bar'}[('foo', 2)] == 'bar'


def test_field_occurrence_validates_occurrence():
    with pytest.raises(ArgumentError):
        FieldOccurrence('foo', 0)
    with pytest.raises(ArgumentError):
        FieldOccurrence('foo', -1)
    with pytest.raises(ArgumentError):
        FieldOccurrence('foo', 1.5)
    with pytest.raises(ArgumentError):
        FieldOccurrence('foo', '1')


def test_asindices_occurrence():
    hdr = ('foo', 'bar', 'foo', 4)
    assert asindices(hdr, FieldOccurrence('foo', 1)) == [0]
    assert asindices(hdr, FieldOccurrence('foo', 2)) == [2]
    # non-string header values are matched on their text representation
    assert asindices(hdr, FieldOccurrence(4, 1)) == [3]
    # combine occurrences with names, integer and negative positions
    assert asindices(
        hdr, (FieldOccurrence('foo', 2), 'bar', -1, 0)
    ) == [2, 1, -1, 0]
    # the same occurrence may be selected more than once
    assert asindices(hdr, (FieldOccurrence('foo', 2),
                           FieldOccurrence('foo', 2))) == [2, 2]


def test_asindices_missing_occurrence_raises_field_selection_error():
    hdr = ('foo', 'bar', 'foo')
    # only two 'foo' columns exist
    with pytest.raises(FieldSelectionError):
        asindices(hdr, FieldOccurrence('foo', 3))
    # the field name does not exist at all
    with pytest.raises(FieldSelectionError):
        asindices(hdr, FieldOccurrence('nope', 1))
    # a bare occurrence is a single selector, not a (name, number) sequence
    with pytest.raises(FieldSelectionError):
        asindices(hdr, FieldOccurrence('foo', 3))


def test_cut_occurrence():
    # select the second duplicated column and keep tracing its original values
    expectation = (('foo',), (3,), (None,))
    ieq(expectation, etl.cut(DUPLICATE_TABLE, FieldOccurrence('foo', 2)))

    # both occurrences, a negative position and an integer position together
    expectation = (
        ('foo', 'foo', 4, 'bar'),
        (1, 3, 40, 2),
        (10, None, 20, 20),
    )
    ieq(expectation,
        etl.cut(DUPLICATE_TABLE,
                FieldOccurrence('foo', 1), FieldOccurrence('foo', 2),
                -1, 1))

    # passing the selectors as a single sequence also works
    ieq(expectation,
        etl.cut(DUPLICATE_TABLE,
                [FieldOccurrence('foo', 1), FieldOccurrence('foo', 2),
                 -1, 1]))


def test_cut_missing_occurrence_raises_field_selection_error():
    with pytest.raises(FieldSelectionError):
        list(etl.cut(DUPLICATE_TABLE, FieldOccurrence('foo', 3)))
    with pytest.raises(FieldSelectionError):
        list(etl.cut(DUPLICATE_TABLE, FieldOccurrence('nope', 1)))


def test_cutout_occurrence():
    # remove only the first 'foo'; the second 'foo' values survive
    expectation = (
        ('bar', 'foo', 4),
        (2, 3, 40),
        (20, None, None),
    )
    ieq(expectation, etl.cutout(DUPLICATE_TABLE, FieldOccurrence('foo', 1)))

    expectation = (
        ('foo', 'bar', 4),
        (1, 2, 40),
        (10, 20, None),
    )
    ieq(expectation, etl.cutout(DUPLICATE_TABLE, FieldOccurrence('foo', 2)))

    # combine with a negative position
    ieq((('bar',), (2,), (20,)),
        etl.cutout(DUPLICATE_TABLE, FieldOccurrence('foo', 2), -1, 0))


def test_cutout_missing_occurrence_raises_field_selection_error():
    with pytest.raises(FieldSelectionError):
        list(etl.cutout(DUPLICATE_TABLE, FieldOccurrence('foo', 3)))


def test_rename_occurrence():
    # rename only the second duplicated column; the first keeps its name
    expectation = (
        ('foo', 'bar', 'two', '4'),
        (1, 2, 3, 40),
        (10, 20),
    )
    ieq(expectation,
        etl.rename(DUPLICATE_TABLE, {FieldOccurrence('foo', 2): 'two'}))
    # kwargs-style call with a single pair
    ieq(expectation,
        etl.rename(DUPLICATE_TABLE, FieldOccurrence('foo', 2), 'two'))


def test_rename_occurrence_creating_new_duplicate_is_kept():
    # renaming the second 'foo' to 'bar' creates another duplicate header,
    # which must not be removed automatically
    result = list(etl.rename(
        DUPLICATE_TABLE, {FieldOccurrence('foo', 2): 'bar'}))
    assert result[0] == ('foo', 'bar', 'bar', '4')
    assert len(result[0]) == 4
    # values remain traceable to their original columns
    assert result[1] == (1, 2, 3, 40)


def test_rename_string_still_renames_every_matching_column():
    # plain string selectors retain their historical behaviour
    result = list(etl.rename(DUPLICATE_TABLE, {'foo': 'qux'}))
    assert result[0] == ('qux', 'bar', 'qux', '4')


def test_rename_missing_occurrence_raises_field_selection_error():
    with pytest.raises(FieldSelectionError):
        list(etl.rename(DUPLICATE_TABLE,
                        {FieldOccurrence('foo', 3): 'x'}))
    # strict=False ignores unknown occurrences instead of raising
    result = etl.rename(
        DUPLICATE_TABLE, {FieldOccurrence('nope', 1): 'x'}, strict=False)
    assert next(iter(result)) == ('foo', 'bar', 'foo', '4')


def test_convert_occurrence():
    # only the second 'foo' column is converted; original values stay traceable
    expectation = (
        ('foo', 'bar', 'foo', 4),
        (1, 2, 300, 40),
        (10, 20),
    )
    ieq(expectation,
        etl.convert(DUPLICATE_TABLE, FieldOccurrence('foo', 2),
                    lambda v: v * 100))


def test_convert_occurrence_with_non_string_header():
    table = [['a', 1], [2, 3]]
    expectation = (('a', 1), (2, 30))
    ieq(expectation,
        etl.convert(table, FieldOccurrence(1, 1), lambda v: v * 10))


def test_convert_missing_occurrence_raises_field_selection_error():
    with pytest.raises(FieldSelectionError):
        list(etl.convert(DUPLICATE_TABLE, FieldOccurrence('foo', 3), str))
    with pytest.raises(FieldSelectionError):
        list(etl.convert(DUPLICATE_TABLE, FieldOccurrence('nope', 1), str))


def test_convert_negative_position_still_supported():
    table = [['foo', 'bar'], [1, 2], [3, 4]]
    ieq((('foo', 'bar'), (1, 20), (3, 40)),
        etl.convert(table, -1, lambda v: v * 10))
