from __future__ import absolute_import, print_function, division

import pytest

from petl.errors import ArgumentError
from petl.test.helpers import ieq
from petl.transform.unpacks import unpack, unpackdict, unpackzip


def test_unpack():

    table1 = (('foo', 'bar'),
              (1, ['a', 'b']),
              (2, ['c', 'd']),
              (3, ['e', 'f']))
    table2 = unpack(table1, 'bar', ['baz', 'quux'])
    expect2 = (('foo', 'baz', 'quux'),
               (1, 'a', 'b'),
               (2, 'c', 'd'),
               (3, 'e', 'f'))
    ieq(expect2, table2)
    ieq(expect2, table2)  # check twice

    # check no new fields
    table3 = unpack(table1, 'bar')
    expect3 = (('foo',),
               (1,),
               (2,),
               (3,))
    ieq(expect3, table3)

    # check more values than new fields
    table4 = unpack(table1, 'bar', ['baz'])
    expect4 = (('foo', 'baz'),
               (1, 'a'),
               (2, 'c'),
               (3, 'e'))
    ieq(expect4, table4)

    # check include original
    table5 = unpack(table1, 'bar', ['baz'], include_original=True)
    expect5 = (('foo', 'bar', 'baz'),
               (1, ['a', 'b'], 'a'),
               (2, ['c', 'd'], 'c'),
               (3, ['e', 'f'], 'e'))
    ieq(expect5, table5)

    # check specify number to unpack
    table6 = unpack(table1, 'bar', 3)
    expect6 = (('foo', 'bar1', 'bar2', 'bar3'),
               (1, 'a', 'b', None),
               (2, 'c', 'd', None),
               (3, 'e', 'f', None))
    ieq(expect6, table6)

    # check specify number to unpack, non-default missing value
    table7 = unpack(table1, 'bar', 3, missing='NA')
    expect7 = (('foo', 'bar1', 'bar2', 'bar3'),
               (1, 'a', 'b', 'NA'),
               (2, 'c', 'd', 'NA'),
               (3, 'e', 'f', 'NA'))
    ieq(expect7, table7)

    # check can use field index
    table8 = unpack(table1, 1, 3)
    expect8 = (('foo', 'bar1', 'bar2', 'bar3'),
               (1, 'a', 'b', None),
               (2, 'c', 'd', None),
               (3, 'e', 'f', None))
    ieq(expect8, table8)


@pytest.mark.parametrize('table, field, expected', [
    ([['id', 'parts'], [1, ['a', 'b']]], -1,
     [('id', 'a', 'b'), (1, 'a', 'b')]),
    ([['parts', 'id'], [['a', 'b'], 1]], -2,
     [('id', 'a', 'b'), (1, 'a', 'b')]),
])
def test_unpack_negative_index(table, field, expected):
    ieq(expected, unpack(table, field, ['a', 'b']))


@pytest.mark.parametrize('field', [2, -1])
def test_unpack_duplicate_field_names(field):
    table = [['item', 'keep', 'item'], ['left', 'middle', ['a', 'b']]]
    expected = [('item', 'keep', 'x', 'y'), ('left', 'middle', 'a', 'b')]
    ieq(expected, unpack(table, field, ['x', 'y']))


@pytest.mark.parametrize('field', [-3, 2])
def test_unpack_invalid_index(field):
    with pytest.raises(ArgumentError):
        list(unpack([['id', 'parts'], [1, ['a']]], field, ['a']))


def test_unpack_empty():

    table1 = (('foo', 'bar'),)
    table2 = unpack(table1, 'bar', ['baz', 'quux'])
    expect2 = (('foo', 'baz', 'quux'),)
    ieq(expect2, table2)


def test_unpack_headerless():
    table = []
    with pytest.raises(ArgumentError):
        for i in unpack(table, 'bar', ['baz', 'quux']):
            pass


def test_unpackdict():

    table1 = (('foo', 'bar'),
              (1, {'baz': 'a', 'quux': 'b'}),
              (2, {'baz': 'c', 'quux': 'd'}),
              (3, {'baz': 'e', 'quux': 'f'}))
    table2 = unpackdict(table1, 'bar')
    expect2 = (('foo', 'baz', 'quux'),
               (1, 'a', 'b'),
               (2, 'c', 'd'),
               (3, 'e', 'f'))
    ieq(expect2, table2)
    ieq(expect2, table2)  # check twice

    # test include original
    table1 = (('foo', 'bar'),
              (1, {'baz': 'a', 'quux': 'b'}),
              (2, {'baz': 'c', 'quux': 'd'}),
              (3, {'baz': 'e', 'quux': 'f'}))
    table2 = unpackdict(table1, 'bar', includeoriginal=True)
    expect2 = (('foo', 'bar', 'baz', 'quux'),
               (1, {'baz': 'a', 'quux': 'b'}, 'a', 'b'),
               (2, {'baz': 'c', 'quux': 'd'}, 'c', 'd'),
               (3, {'baz': 'e', 'quux': 'f'}, 'e', 'f'))
    ieq(expect2, table2)
    ieq(expect2, table2)  # check twice

    # test specify keys
    table1 = (('foo', 'bar'),
              (1, {'baz': 'a', 'quux': 'b'}),
              (2, {'baz': 'c', 'quux': 'd'}),
              (3, {'baz': 'e', 'quux': 'f'}))
    table2 = unpackdict(table1, 'bar', keys=['quux'])
    expect2 = (('foo', 'quux'),
               (1, 'b'),
               (2, 'd'),
               (3, 'f'))
    ieq(expect2, table2)
    ieq(expect2, table2)  # check twice

    # test dodgy data
    table1 = (('foo', 'bar'),
              (1, {'baz': 'a', 'quux': 'b'}),
              (2, 'foobar'),
              (3, {'baz': 'e', 'quux': 'f'}))
    table2 = unpackdict(table1, 'bar')
    expect2 = (('foo', 'baz', 'quux'),
               (1, 'a', 'b'),
               (2, None, None),
               (3, 'e', 'f'))
    ieq(expect2, table2)
    ieq(expect2, table2)  # check twice


def test_unpackzip_basic():
    table1 = (('id', 'times', 'values'),
              (1, ['t1', 't2'], [10, 20]),
              (2, ['t3', 't4'], [30, 40]))
    table2 = unpackzip(table1, ['times', 'values'])
    expect2 = (('id', 'times', 'values'),
               (1, 't1', 10),
               (1, 't2', 20),
               (2, 't3', 30),
               (2, 't4', 40))
    ieq(expect2, table2)
    ieq(expect2, table2)  # check twice, generators must not be consumed


def test_unpackzip_single_field():
    table1 = (('id', 'vals'),
              (1, ['a', 'b']),
              (2, ['c']))
    table2 = unpackzip(table1, ['vals'])
    expect2 = (('id', 'vals'),
               (1, 'a'),
               (1, 'b'),
               (2, 'c'))
    ieq(expect2, table2)


def test_unpackzip_newfields():
    table1 = (('id', 'times', 'values'),
              (1, ['t1', 't2'], [10, 20]))
    table2 = unpackzip(table1, ['times', 'values'],
                       newfields=['when', 'reading'])
    expect2 = (('id', 'when', 'reading'),
               (1, 't1', 10),
               (1, 't2', 20))
    ieq(expect2, table2)


def test_unpackzip_include_original():
    table1 = (('id', 'times', 'values'),
              (1, ['t1', 't2'], [10, 20]))
    table2 = unpackzip(table1, ['times', 'values'], include_original=True)
    expect2 = (('id', 'times', 'values', 'times', 'values'),
               (1, ['t1', 't2'], [10, 20], 't1', 10),
               (1, ['t1', 't2'], [10, 20], 't2', 20))
    ieq(expect2, table2)


def test_unpackzip_with_index():
    table1 = (('id', 'times', 'values'),
              (1, ['t1', 't2'], [10, 20]))
    table2 = unpackzip(table1, ['times', 'values'], index='n')
    expect2 = (('id', 'times', 'values', 'n'),
               (1, 't1', 10, 0),
               (1, 't2', 20, 1))
    ieq(expect2, table2)


def test_unpackzip_strict_drops_unequal_rows():
    table1 = (('id', 'times', 'values'),
              (1, ['t1', 't2'], [10, 20]),
              (2, ['t3'], [30, 40]),
              (3, ['t5', 't6'], [50, 60]))
    table2 = unpackzip(table1, ['times', 'values'])
    expect2 = (('id', 'times', 'values'),
               (1, 't1', 10),
               (1, 't2', 20),
               (3, 't5', 50),
               (3, 't6', 60))
    ieq(expect2, table2)


def test_unpackzip_non_strict_pads_to_longest():
    table1 = (('id', 'times', 'values'),
              (1, ['t1', 't2', 't3'], [10]),
              (2, ['t4'], [40, 50]))
    table2 = unpackzip(table1, ['times', 'values'], strict=False)
    expect2 = (('id', 'times', 'values'),
               (1, 't1', 10),
               (1, 't2', None),
               (1, 't3', None),
               (2, 't4', 40),
               (2, None, 50))
    ieq(expect2, table2)


def test_unpackzip_non_strict_custom_missing():
    table1 = (('id', 'times', 'values'),
              (1, ['t1'], [10, 20]))
    table2 = unpackzip(table1, ['times', 'values'], strict=False,
                       missing='NA')
    expect2 = (('id', 'times', 'values'),
               (1, 't1', 10),
               (1, 'NA', 20))
    ieq(expect2, table2)


def test_unpackzip_empty_and_none():
    table1 = (('id', 'times', 'values'),
              (1, [], []),
              (2, None, None),
              (3, ['t3'], [30]))

    # strict mode: empty and None rows produce no output rows
    table2 = unpackzip(table1, ['times', 'values'])
    expect2 = (('id', 'times', 'values'),
               (3, 't3', 30))
    ieq(expect2, table2)

    # non-strict mode: None behaves as an empty sequence and is padded
    table3 = unpackzip(table1, ['times', 'values'], strict=False)
    expect3 = (('id', 'times', 'values'),
               (3, 't3', 30))
    ieq(expect3, table3)


def test_unpackzip_non_strict_none_padded_against_longer():
    table1 = (('id', 'times', 'values'),
              (1, None, [10, 20]),
              (2, ['t2'], []))
    table2 = unpackzip(table1, ['times', 'values'], strict=False,
                       missing='NA')
    expect2 = (('id', 'times', 'values'),
               (1, 'NA', 10),
               (1, 'NA', 20),
               (2, 't2', 'NA'))
    ieq(expect2, table2)


@pytest.mark.parametrize('bad', ['abc', b'abc', {'a': 1}, 42, 3.14])
def test_unpackzip_rejects_non_sequences(bad):
    table1 = (('id', 'times', 'values'),
              (1, bad, [10, 20]))
    with pytest.raises(ArgumentError):
        list(unpackzip(table1, ['times', 'values']))


def test_unpackzip_accepts_tuples_and_generators():
    def gen():
        for item in (10, 20):
            yield item

    table1 = (('id', 'times', 'values'),
              (1, ('t1', 't2'), gen()))
    table2 = unpackzip(table1, ['times', 'values'])
    expect2 = (('id', 'times', 'values'),
               (1, 't1', 10),
               (1, 't2', 20))
    ieq(expect2, table2)


def test_unpackzip_single_use_generator_consumed_once():
    class OneShotSequence(object):
        def __init__(self, values):
            self.values = values
            self.iterations = 0

        def __iter__(self):
            self.iterations += 1
            return iter(self.values)

    expected = (('id', 'times', 'values'),
                (1, 't1', 10),
                (1, 't2', 20))

    seq = OneShotSequence([10, 20])
    table1 = (('id', 'times', 'values'),
              (1, ['t1', 't2'], seq))
    table2 = unpackzip(table1, ['times', 'values'])

    # within a single pass the generator is materialised exactly once
    ieq(expected, table2)
    assert seq.iterations == 1

    # a second pass rebuilds the generator instead of re-consuming the old one
    ieq(expected, table2)
    assert seq.iterations == 2


def test_unpackzip_preserves_other_fields_and_order():
    table1 = (('a', 'times', 'b', 'values', 'c'),
              (1, ['t1', 't2'], 2, [10, 20], 3),
              (4, ['t3'], 5, [30], 6))
    table2 = unpackzip(table1, ['times', 'values'])
    expect2 = (('a', 'b', 'c', 'times', 'values'),
               (1, 2, 3, 't1', 10),
               (1, 2, 3, 't2', 20),
               (4, 5, 6, 't3', 30))
    ieq(expect2, table2)


def test_unpackzip_negative_index_and_duplicate_names():
    table1 = [['item', 'keep', 'item'],
              [['a', 'b'], 'middle', [1, 2]]]
    expected = [('keep', 'item', 'item'),
                ('middle', 'a', 1),
                ('middle', 'b', 2)]
    ieq(expected, unpackzip(table1, [-3, -1]))


def test_unpackzip_lazy_does_not_materialise_whole_table():
    class ExpandingSource(object):
        def __init__(self):
            self.rows_read = 0

        def __iter__(self):
            yield ('id', 'vals')
            for number in range(1000000):
                self.rows_read += 1
                yield (number, [number, number + 1])

    source = ExpandingSource()
    table2 = unpackzip(source, ['vals'])
    it = iter(table2)
    assert next(it) == ('id', 'vals')
    assert next(it) == (0, 0)
    assert next(it) == (0, 1)
    # only the first source row needs to have been pulled
    assert source.rows_read == 1


def test_unpackzip_missing_field():
    table1 = (('id', 'vals'),
              (1, ['a']))
    with pytest.raises(ArgumentError):
        list(unpackzip(table1, ['nope']))


@pytest.mark.parametrize('fields', [[], 'vals', None])
def test_unpackzip_invalid_fields(fields):
    table1 = (('id', 'vals'),
              (1, ['a']))
    with pytest.raises(ArgumentError):
        list(unpackzip(table1, fields))


def test_unpackzip_newfields_wrong_length():
    table1 = (('id', 'times', 'values'),
              (1, ['t1'], [10]))
    with pytest.raises(ArgumentError):
        list(unpackzip(table1, ['times', 'values'], newfields=['only']))


def test_unpackzip_header_only():
    table1 = (('id', 'times', 'values'),)
    table2 = unpackzip(table1, ['times', 'values'])
    expect2 = (('id', 'times', 'values'),)
    ieq(expect2, table2)


def test_unpackzip_empty_table():
    table2 = unpackzip([], ['vals'])
    assert list(table2) == []
