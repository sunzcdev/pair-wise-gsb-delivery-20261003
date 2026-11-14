from __future__ import absolute_import, print_function, division


import operator

import pytest


from collections import OrderedDict
from petl.test.helpers import ieq
from petl.errors import ArgumentError, FieldSelectionError
from petl.util import strjoin
from petl.transform.reductions import rowreduce, aggregate, \
    mergeduplicates, merge, Conflict, fold, partial_aggregate, \
    merge_aggregate, StatefulAggregator, Count, Mean, Variance


def test_rowreduce():
    
    table1 = (('foo', 'bar'),
              ('a', 3),
              ('a', 7),
              ('b', 2),
              ('b', 1),
              ('b', 9),
              ('c', 4))
    
    def sumbar(key, rows):
        return [key, sum(row[1] for row in rows)]
        
    table2 = rowreduce(table1, key='foo', reducer=sumbar, 
                       header=['foo', 'barsum'])
    expect2 = (('foo', 'barsum'),
               ('a', 10),
               ('b', 12),
               ('c', 4))
    ieq(expect2, table2)
    

def test_rowreduce_fieldnameaccess():
    
    table1 = (('foo', 'bar'),
              ('a', 3),
              ('a', 7),
              ('b', 2),
              ('b', 1),
              ('b', 9),
              ('c', 4))
    
    def sumbar(key, records):
        return [key, sum([rec['bar'] for rec in records])]
        
    table2 = rowreduce(table1, key='foo', reducer=sumbar, 
                       header=['foo', 'barsum'])
    expect2 = (('foo', 'barsum'),
               ('a', 10),
               ('b', 12),
               ('c', 4))
    ieq(expect2, table2)
    

def test_rowreduce_more():
    
    table1 = (('foo', 'bar'),
              ('aa', 3),
              ('aa', 7),
              ('bb', 2),
              ('bb', 1),
              ('bb', 9),
              ('cc', 4))
    
    def sumbar(key, records):
        return [key, sum(rec['bar'] for rec in records)]
        
    table2 = rowreduce(table1, key='foo', reducer=sumbar, 
                       header=['foo', 'barsum'])
    expect2 = (('foo', 'barsum'),
               ('aa', 10),
               ('bb', 12),
               ('cc', 4))
    ieq(expect2, table2)
    

def test_rowreduce_empty():
    table = (('foo', 'bar'),)
    expect = (('foo', 'bar'),)
    reducer = lambda key, rows: (key, [r[0] for r in rows])
    actual = rowreduce(table, key='foo', reducer=reducer, 
                       header=('foo', 'bar'))
    ieq(expect, actual)


def test_aggregate_simple():
    
    table1 = (('foo', 'bar', 'baz'),
              ('a', 3, True),
              ('a', 7, False),
              ('b', 2, True),
              ('b', 2, False),
              ('b', 9, False),
              ('c', 4, True))

    # simplest signature - aggregate whole rows
    table2 = aggregate(table1, 'foo', len)
    expect2 = (('foo', 'value'),
               ('a', 2),
               ('b', 3),
               ('c', 1))
    ieq(expect2, table2)
    ieq(expect2, table2)

    # next simplest signature - aggregate single field
    table3 = aggregate(table1, 'foo', sum, 'bar')
    expect3 = (('foo', 'value'),
               ('a', 10),
               ('b', 13),
               ('c', 4))
    ieq(expect3, table3)
    ieq(expect3, table3)
    
    # alternative signature for simple aggregation
    table4 = aggregate(table1, key=('foo', 'bar'), aggregation=list, 
                       value=('bar', 'baz'))
    expect4 = (('foo', 'bar', 'value'),
               ('a', 3, [(3, True)]),
               ('a', 7, [(7, False)]),
               ('b', 2, [(2, True), (2, False)]),
               ('b', 9, [(9, False)]),
               ('c', 4, [(4, True)]))
    ieq(expect4, table4)
    ieq(expect4, table4)
    
    table5 = aggregate(table1, 'foo', len, field='nrows')
    expect5 = (('foo', 'nrows'),
               ('a', 2),
               ('b', 3),
               ('c', 1))
    ieq(expect5, table5)
    ieq(expect5, table5)


def test_aggregate_simple_key_is_None():

    table1 = (('foo', 'bar', 'baz'),
              ('a', 3, True),
              ('a', 7, False),
              ('b', 2, True),
              ('b', 2, False),
              ('b', 9, False),
              ('c', 4, True))

    # simplest signature - aggregate whole rows
    table2 = aggregate(table1, None, len)
    expect2 = (('value',), (6,))
    ieq(expect2, table2)
    ieq(expect2, table2)

    # next simplest signature - aggregate single field
    table3 = aggregate(table1, None, sum, 'bar')
    expect3 = (('value',), (27,))
    ieq(expect3, table3)
    ieq(expect3, table3)

    # alternative signature for simple aggregation
    table4 = aggregate(table1, key=None, aggregation=list,
                       value=('bar', 'baz'))
    expect4 = (('value',),
               ( [(3, True), (7, False),
                 (2, True), (2, False), (9, False),
                 (4, True)],),)

    ieq(expect4, table4)
    ieq(expect4, table4)

    table5 = aggregate(table1, None, len, field='nrows')
    expect5 = (('nrows',), (6,))
    ieq(expect5, table5)
    ieq(expect5, table5)

def test_aggregate_simple_whole_records():
    table = (('foo', 'bar'), ('a', 3), ('b', 7))

    def aggregation(rows):
        return sum(row['bar'] for row in rows)

    actual = aggregate(table, None, aggregation)
    expect = (('value',), (10,))
    ieq(expect, actual)


def test_aggregate_simple_whole_records_single_pass():
    table = iter((('foo', 'bar'), ('a', 3), ('b', 7)))
    actual = aggregate(table, None, list)
    expect = (('value',), ([('a', 3), ('b', 7)],))
    ieq(expect, actual)


def test_aggregate_multifield():
    
    table1 = (('foo', 'bar'),
              ('a', 3),
              ('a', 7),
              ('b', 2),
              ('b', 1),
              ('b', 9),
              ('c', 4))
    
    # dict arg
    
    aggregators = OrderedDict()
    aggregators['count'] = len
    aggregators['minbar'] = 'bar', min
    aggregators['maxbar'] = 'bar', max
    aggregators['sumbar'] = 'bar', sum
    aggregators['listbar'] = 'bar', list
    aggregators['bars'] = 'bar', strjoin(', ')

    table2 = aggregate(table1, 'foo', aggregators)
    expect2 = (('foo', 'count', 'minbar', 'maxbar', 'sumbar', 'listbar', 
                'bars'),
               ('a', 2, 3, 7, 10, [3, 7], '3, 7'),
               ('b', 3, 1, 9, 12, [2, 1, 9], '2, 1, 9'),
               ('c', 1, 4, 4, 4, [4], '4'))
    ieq(expect2, table2)
    ieq(expect2, table2)  # check can iterate twice
    
    # use suffix notation
    
    table3 = aggregate(table1, 'foo')
    table3['count'] = len
    table3['minbar'] = 'bar', min
    table3['maxbar'] = 'bar', max
    table3['sumbar'] = 'bar', sum
    table3['listbar'] = 'bar'  # default aggregation is list
    table3['bars'] = 'bar', strjoin(', ')
    ieq(expect2, table3)
    
    # list arg

    aggregators = [('count', len),
                   ('minbar', 'bar', min),
                   ('maxbar', 'bar', max),
                   ('sumbar', 'bar', sum),
                   ('listbar', 'bar', list),
                   ('bars', 'bar', strjoin(', '))]

    table4 = aggregate(table1, 'foo', aggregators)
    ieq(expect2, table4)
    ieq(expect2, table4)  # check can iterate twice

def _aggregate_multifield(table, key, form, presorted):
    if form == 'dict':
        aggregation = OrderedDict([('total', ('bar', sum)), ('count', len)])
    elif form == 'list':
        aggregation = [('total', 'bar', sum), ('count', len)]
    else:
        aggregation = None
    result = aggregate(table, key, aggregation, presorted=presorted)
    if form == 'suffix':
        result['total'] = 'bar', sum
        result['count'] = len
    return result


@pytest.mark.parametrize('key_type', [list, tuple])
@pytest.mark.parametrize('key_values', [
    ('alpha', 'beta'),
    (1, 2),
    (None, 1),
    ('', 'a'),
    ((1, 2), (3, 4)),
    (False, True),
])
@pytest.mark.parametrize('form', ['dict', 'list', 'suffix'])
@pytest.mark.parametrize('presorted', [False, True])
def test_aggregate_multifield_singleton_key(key_type, key_values, form,
                                          presorted):
    first, second = key_values
    rows = [(first, 3), (first, 7), (second, 5)]
    if not presorted:
        rows.reverse()
    table = [('foo', 'bar')] + rows
    original = list(table)
    key = key_type(['foo'])
    result = _aggregate_multifield(table, key, form, presorted)
    expected = [('foo', 'total', 'count'), (first, 10, 2), (second, 5, 1)]

    assert list(result) == expected
    assert list(result) == expected
    assert key == key_type(['foo'])
    assert table == original


@pytest.mark.parametrize('key_type', [list, tuple])
@pytest.mark.parametrize('form', ['dict', 'list', 'suffix'])
@pytest.mark.parametrize('presorted', [False, True])
def test_aggregate_multifield_singleton_key_empty(key_type, form, presorted):
    result = _aggregate_multifield([('foo', 'bar')], key_type(['foo']),
                                  form, presorted)
    expected = [('foo', 'total', 'count')]
    assert list(result) == expected
    assert list(result) == expected


@pytest.mark.parametrize('presorted', [False, True])
@pytest.mark.parametrize('key, expected', [
    ('foo', [('foo', 'total', 'count'), ('alpha', 10, 2), ('beta', 5, 1)]),
    (('foo', 'baz'), [('foo', 'baz', 'total', 'count'),
                      ('alpha', 1, 10, 2), ('beta', 2, 5, 1)]),
    (operator.itemgetter(0, 2), [('key', 'total', 'count'),
                               (('alpha', 1), 10, 2), (('beta', 2), 5, 1)]),
    (None, [('total', 'count'), (15, 3)]),
])
def test_aggregate_multifield_key_shapes(key, expected, presorted):
    rows = [('alpha', 3, 1), ('alpha', 7, 1), ('beta', 5, 2)]
    if not presorted:
        rows.reverse()
    result = _aggregate_multifield([('foo', 'bar', 'baz')] + rows, key,
                                  'dict', presorted)
    if callable(key) and not presorted:
        # Sorting does not accept callable field selectors.
        with pytest.raises(FieldSelectionError):
            list(result)
        return
    assert list(result) == expected
    assert list(result) == expected


def test_aggregate_multifield_key_is_None():

    table1 = (('foo', 'bar'),
              ('a', 3),
              ('a', 7),
              ('b', 2),
              ('b', 1),
              ('b', 9),
              ('c', 4))

    # dict arg
    aggregators = OrderedDict()
    aggregators['count'] = len
    aggregators['minbar'] = 'bar', min
    aggregators['maxbar'] = 'bar', max
    aggregators['sumbar'] = 'bar', sum
    aggregators['listbar'] = 'bar', list
    aggregators['bars'] = 'bar', strjoin(', ')

    table2 = aggregate(table1, None, aggregators)
    expect2 = (('count', 'minbar', 'maxbar', 'sumbar', 'listbar',
                'bars'),
               (6, 1, 9, 26, [3, 7, 2, 1, 9, 4], '3, 7, 2, 1, 9, 4'))
    ieq(expect2, table2)
    ieq(expect2, table2)  # check can iterate twice

    # use suffix notation
    table3 = aggregate(table1, None)
    table3['count'] = len
    table3['minbar'] = 'bar', min
    table3['maxbar'] = 'bar', max
    table3['sumbar'] = 'bar', sum
    table3['listbar'] = 'bar'  # default aggregation is list
    table3['bars'] = 'bar', strjoin(', ')
    ieq(expect2, table3)

    # list arg
    aggregators = [('count', len),
                   ('minbar', 'bar', min),
                   ('maxbar', 'bar', max),
                   ('sumbar', 'bar', sum),
                   ('listbar', 'bar', list),
                   ('bars', 'bar', strjoin(', '))]

    table4 = aggregate(table1, None, aggregators)
    ieq(expect2, table4)
    ieq(expect2, table4)  # check can iterate twice

def test_aggregate_more():

    table1 = (('foo', 'bar'),
              ('aa', 3),
              ('aa', 7),
              ('bb', 2),
              ('bb', 1),
              ('bb', 9),
              ('cc', 4),
              ('dd', 3))

    aggregators = OrderedDict()
    aggregators['minbar'] = 'bar', min
    aggregators['maxbar'] = 'bar', max
    aggregators['sumbar'] = 'bar', sum
    aggregators['listbar'] = 'bar'  # default aggregation is list
    aggregators['bars'] = 'bar', strjoin(', ')

    table2 = aggregate(table1, 'foo', aggregators)
    expect2 = (('foo', 'minbar', 'maxbar', 'sumbar', 'listbar', 'bars'),
               ('aa', 3, 7, 10, [3, 7], '3, 7'),
               ('bb', 1, 9, 12, [2, 1, 9], '2, 1, 9'),
               ('cc', 4, 4, 4, [4], '4'),
               ('dd', 3, 3, 3, [3], '3'))
    ieq(expect2, table2)
    ieq(expect2, table2)  # check can iterate twice

    table3 = aggregate(table1, 'foo')
    table3['minbar'] = 'bar', min
    table3['maxbar'] = 'bar', max
    table3['sumbar'] = 'bar', sum
    table3['listbar'] = 'bar'  # default aggregation is list
    table3['bars'] = 'bar', strjoin(', ')
    ieq(expect2, table3)

def test_aggregate_more_key_is_None():

    table1 = (('foo', 'bar'),
              ('aa', 3),
              ('aa', 7),
              ('bb', 2),
              ('bb', 1),
              ('bb', 9),
              ('cc', 4),
              ('dd', 3))

    aggregators = OrderedDict()
    aggregators['minbar'] = 'bar', min
    aggregators['maxbar'] = 'bar', max
    aggregators['sumbar'] = 'bar', sum
    aggregators['listbar'] = 'bar'  # default aggregation is list
    aggregators['bars'] = 'bar', strjoin(', ')

    table2 = aggregate(table1, None, aggregators)
    expect2 = (('minbar', 'maxbar', 'sumbar', 'listbar', 'bars'),
               (1, 9, 29,
                   [3, 7, 2, 1, 9, 4, 3],
                   '3, 7, 2, 1, 9, 4, 3'))
    ieq(expect2, table2)
    ieq(expect2, table2)  # check can iterate twice

    table3 = aggregate(table1, None)
    table3['minbar'] = 'bar', min
    table3['maxbar'] = 'bar', max
    table3['sumbar'] = 'bar', sum
    table3['listbar'] = 'bar'  # default aggregation is list
    table3['bars'] = 'bar', strjoin(', ')
    ieq(expect2, table3)

def test_aggregate_multiple_source_fields():

    table = (('foo', 'bar', 'baz'),
             ('a', 3, True),
             ('a', 7, False),
             ('b', 2, True),
             ('b', 2, False),
             ('b', 9, False),
             ('c', 4, True))

    expect = (('foo', 'bar', 'value'),
              ('a', 3, [(3, True)]),
              ('a', 7, [(7, False)]),
              ('b', 2, [(2, True), (2, False)]),
              ('b', 9, [(9, False)]),
              ('c', 4, [(4, True)]))

    actual = aggregate(table, ('foo', 'bar'), list, ('bar', 'baz'))
    ieq(expect, actual)
    ieq(expect, actual)

    actual = aggregate(table, key=('foo', 'bar'), aggregation=list, 
                       value=('bar', 'baz'))
    ieq(expect, actual)
    ieq(expect, actual)
    
    actual = aggregate(table, key=('foo', 'bar'))
    actual['value'] = ('bar', 'baz'), list
    ieq(expect, actual)
    ieq(expect, actual)    

def test_aggregate_multiple_source_fields_key_is_None():

    table = (('foo', 'bar', 'baz'),
             ('a', 3, True),
             ('a', 7, False),
             ('b', 2, True),
             ('b', 2, False),
             ('b', 9, False),
             ('c', 4, True))

    expect  = (('value',),
               ( [(3, True), (7, False),
                 (2, True), (2, False), (9, False),
                 (4, True)],),)

    actual = aggregate(table, None, list, ('bar', 'baz'))
    ieq(expect, actual)
    ieq(expect, actual)

    actual = aggregate(table, key=None, aggregation=list,
                       value=('bar', 'baz'))
    ieq(expect, actual)
    ieq(expect, actual)

    actual = aggregate(table, key=None)
    actual['value'] = ('bar', 'baz'), list
    ieq(expect, actual)
    ieq(expect, actual)


def test_aggregate_empty():

    table = (('foo', 'bar'),)

    aggregators = OrderedDict()
    aggregators['minbar'] = 'bar', min
    aggregators['maxbar'] = 'bar', max
    aggregators['sumbar'] = 'bar', sum

    actual = aggregate(table, 'foo', aggregators)
    expect = (('foo', 'minbar', 'maxbar', 'sumbar'),)
    ieq(expect, actual)

def test_aggregate_empty_key_is_None():

    table = (('foo', 'bar'),)

    aggregators = OrderedDict()
    aggregators['minbar'] = 'bar', min
    aggregators['maxbar'] = 'bar', max
    aggregators['sumbar'] = 'bar', sum

    actual = aggregate(table, None, aggregators)
    expect = (('minbar', 'maxbar', 'sumbar'),)
    ieq(expect, actual)

def test_mergeduplicates():

    table = (('foo', 'bar', 'baz'),
             ('A', 1, 2),
             ('B', '2', None),
             ('D', 'xyz', 9.4),
             ('B', None, u'7.8', True),
             ('E', None, 42.),
             ('D', 'xyz', 12.3),
             ('A', 2, None))

    # value overrides missing
    result = mergeduplicates(table, 'foo', missing=None)
    expectation = (('foo', 'bar', 'baz'),
                   ('A', Conflict([1, 2]), 2),
                   ('B', '2', u'7.8'),
                   ('D', 'xyz', Conflict([9.4, 12.3])),
                   ('E', None, 42.))
    ieq(expectation, result)
    
    
def test_mergeduplicates_empty():
    table = (('foo', 'bar'),)
    expect = (('foo', 'bar'),)
    actual = mergeduplicates(table, key='foo')
    ieq(expect, actual)


def test_mergeduplicates_shortrows():
    table = [['foo', 'bar', 'baz'], 
             ['a', 1, True], 
             ['b', 2, True], 
             ['b', 3]]
    actual = mergeduplicates(table, 'foo')
    expect = [('foo', 'bar', 'baz'), 
              ('a', 1, True), 
              ('b', Conflict([2, 3]), True)]
    ieq(expect, actual)
        
    
def test_mergeduplicates_compoundkey():
    table = [['foo', 'bar', 'baz'], 
             ['a', 1, True], 
             ['a', 1, True], 
             ['a', 2, False],
             ['a', 2, None],
             ['c', 3, True],
             ['c', 3, False],
             ]
    actual = mergeduplicates(table, key=('foo', 'bar'))
    expect = [('foo', 'bar', 'baz'), 
              ('a', 1, True), 
              ('a', 2, False), 
              ('c', 3, Conflict([True, False]))]
    ieq(expect, actual)
        

def test_mergeduplicates_field_index():
    table = [['foo', 'bar', 'baz'],
             ['aa', 1, True],
             ['aa', None, False],
             ['bb', 2, True]]
    expect = [('foo', 'bar', 'baz'),
              ('aa', 1, Conflict([True, False])),
              ('bb', 2, True)]
    for key in 0, -3, [0], ['foo'], (-3,):
        ieq(expect, mergeduplicates(table, key))
        ieq(expect, mergeduplicates(table, key, presorted=True))
        ieq(expect, merge(table[:2], table[:1] + table[2:], key=key))

    # key fields come first in the output, as with field names
    expect = [('baz', 'foo', 'bar'),
              (False, 'aa', None),
              (True, Conflict(['aa', 'bb']), Conflict([1, 2]))]
    ieq(expect, mergeduplicates(table, -1))
    ieq(expect, mergeduplicates(table, 'baz'))


def test_mergeduplicates_compoundkey_field_index():
    table = [['foo', 'bar', 'baz'],
             ['a', 1, True],
             ['a', 1, None],
             ['a', 2, False],
             ['c', 3, True],
             ['c', 3, False]]
    expect = [('foo', 'bar', 'baz'),
              ('a', 1, True),
              ('a', 2, False),
              ('c', 3, Conflict([True, False]))]
    for key in (0, 1), [-3, -2], ('foo', 1):
        ieq(expect, mergeduplicates(table, key))
        ieq(expect, mergeduplicates(table, key, presorted=True))


def test_mergeduplicates_field_index_out_of_range():
    table = [['foo', 'bar'],
             ['a', 1]]
    for key in 2, -3, [0, 2]:
        with pytest.raises(FieldSelectionError):
            mergeduplicates(table, key).nrows()
        with pytest.raises(FieldSelectionError):
            mergeduplicates(table, key, presorted=True).nrows()


def test_mergeduplicates_field_index_ragged_rows():
    # a negative index counts from the end of the header, not of the row
    table = [['foo', 'bar', 'baz'],
             ['s1', 'a'],
             ['f', 0, 'x'],
             ['s2', 'z']]
    expect = [('baz', 'foo', 'bar'),
              (None, Conflict(['s1', 's2']), Conflict(['a', 'z'])),
              ('x', 'f', 0)]
    for key in 'baz', 2, -1:
        ieq(expect, mergeduplicates(table, key))

    table = [['foo', 'bar'],
             ['a', 1, 'zz'],
             ['b', 2, 'mm'],
             ['c', 1, 'aa']]
    expect = [('bar', 'foo'),
              (1, Conflict(['a', 'c'])),
              (2, 'b')]
    for key in 'bar', 1, -1:
        ieq(expect, mergeduplicates(table, key))


def test_mergeduplicates_header_change_between_iterations():
    # the key is resolved against the header of each iteration, so a reused
    # view still groups equal keys after its source changes the header
    table = [['id', 'value'],
             ['a', 1],
             ['b', 2],
             ['a', 3]]
    views = [mergeduplicates(table, key, cache=False) for key in ('id', -2)]
    for view in views:
        ieq([('id', 'value'), ('a', Conflict([1, 3])), ('b', 2)], view)

    table[:] = [['x', 'id', 'value'],
                [1, 'a', 1],
                [2, 'b', 2],
                [3, 'a', 3]]
    expect = [('id', 'x', 'value'),
              ('a', Conflict([1, 3]), Conflict([1, 3])),
              ('b', 2, 2)]
    for view in views:
        ieq(expect, view)


def test_merge_field_index_reordered_fields():
    # an index refers to the combined header, whatever the order of the
    # fields in each table
    table1 = [['id', 'value'],
              [1, 'z'],
              [3, 'c']]
    table2 = [['value', 'id'],
              ['a', 2],
              ['b', 1]]
    expect = [('id', 'value'),
              (1, Conflict(['z', 'b'])),
              (2, 'a'),
              (3, 'c')]
    for key in 'id', 0, -2, [0]:
        ieq(expect, merge(table1, table2, key=key))
        ieq(expect[:1] + expect[:0:-1],
            merge(table1, table2, key=key, reverse=True))
    for key in 2, -3:
        with pytest.raises(FieldSelectionError):
            merge(table1, table2, key=key).nrows()


def test_merge_field_index_header_change_between_iterations():
    # an index key is looked up by name in each table, so a table that
    # changes the order of its fields is still sorted by the same field
    table1 = [['id', 'value'],
              [1, 'z'],
              [3, 'c']]
    table2 = [['id', 'value'],
              [2, 'a'],
              [1, 'b']]
    expect = [('id', 'value'),
              (1, Conflict(['z', 'b'])),
              (2, 'a'),
              (3, 'c')]
    view = merge(table1, table2, key=0, cache=False)
    ieq(expect, view)
    table2[:] = [['value', 'id'],
                 ['a', 2],
                 ['b', 1]]
    ieq(expect, view)


def test_fold():

    t1 = (('id', 'count'), (1, 3), (1, 5), (2, 4), (2, 8))
    t2 = fold(t1, 'id', operator.add, 'count', presorted=True)
    expect = (('key', 'value'), (1, 8), (2, 12))
    ieq(expect, t2)
    ieq(expect, t2)


def _assert_close(e, a, tol=1e-9):
    # floating point results of merged partial states may differ from a
    # one-shot computation by a small rounding error
    if e is None or a is None:
        assert e is a
    elif isinstance(e, tuple) and isinstance(a, tuple):
        assert len(e) == len(a)
        for x, y in zip(e, a):
            _assert_close(x, y, tol)
    elif isinstance(e, float) or isinstance(a, float):
        assert abs(e - a) <= tol * max(1.0, abs(e)), (e, a)
    else:
        assert e == a


def _ieq_close(expect, actual, tol=1e-9):
    expect = [tuple(row) for row in expect]
    actual = [tuple(row) for row in actual]
    assert expect[0] == actual[0]
    assert len(expect) == len(actual)
    for erow, arow in zip(expect[1:], actual[1:]):
        assert len(erow) == len(arow)
        for e, a in zip(erow, arow):
            _assert_close(e, a, tol)


def test_aggregate_stateful_simple():

    table1 = (('foo', 'bar'),
              ('a', 3),
              ('a', 7),
              ('b', 2),
              ('b', 1),
              ('b', 9),
              ('c', 4))

    # count
    table2 = aggregate(table1, 'foo', Count(), 'bar')
    expect2 = (('foo', 'value'),
               ('a', 2),
               ('b', 3),
               ('c', 1))
    ieq(expect2, table2)
    ieq(expect2, table2)

    # mean
    table3 = aggregate(table1, 'foo', Mean(), 'bar')
    expect3 = (('foo', 'value'),
               ('a', 5.0),
               ('b', 4.0),
               ('c', 4.0))
    ieq(expect3, table3)

    # variance, reported together with the sample count
    table4 = aggregate(table1, 'foo', Variance(), 'bar')
    expect4 = (('foo', 'value'),
               ('a', (8.0, 2)),
               ('b', (19.0, 3)),
               ('c', (None, 1)))
    ieq(expect4, table4)

    # population variance
    table5 = aggregate(table1, 'foo', Variance(sample=False), 'bar')
    expect5 = (('foo', 'value'),
               ('a', (4.0, 2)),
               ('b', (38.0 / 3, 3)),
               ('c', (0.0, 1)))
    _ieq_close(expect5, table5)

    # whole rows, no value selector
    table6 = aggregate(table1, 'foo', Count())
    ieq(expect2, table6)

    # no key
    ieq((('value',), (6,)), aggregate(table1, None, Count(), 'bar'))
    ieq((('value',), (26 / 6.0,)), aggregate(table1, None, Mean(), 'bar'))

    # singleton list key
    ieq(expect3, aggregate(table1, ['foo'], Mean(), 'bar'))


def test_aggregate_stateful_multi():

    table1 = (('foo', 'bar', 'baz'),
              ('a', 3, True),
              ('a', 7, False),
              ('b', 2, True),
              ('b', 2, False),
              ('b', 9, False),
              ('c', 4, True))

    # stateful aggregators mix freely with plain callables
    aggregation = OrderedDict()
    aggregation['count'] = 'bar', Count()
    aggregation['meanbar'] = 'bar', Mean()
    aggregation['varbar'] = 'bar', Variance()
    aggregation['sumbar'] = 'bar', sum
    aggregation['nrows'] = Count()
    table2 = aggregate(table1, 'foo', aggregation)
    expect2 = (('foo', 'count', 'meanbar', 'varbar', 'sumbar', 'nrows'),
               ('a', 2, 5.0, (8.0, 2), 10, 2),
               ('b', 3, 13 / 3.0, (16.333333333333336, 3), 13, 3),
               ('c', 1, 4.0, (None, 1), 4, 1))
    _ieq_close(expect2, table2)
    _ieq_close(expect2, table2)


def test_aggregate_stateful_empty():

    table = (('foo', 'bar'),)

    # keyed aggregation of an empty table yields only the header
    ieq((('foo', 'value'),), aggregate(table, 'foo', Count(), 'bar'))
    ieq((('foo', 'state'),), partial_aggregate(table, 'foo', Mean(), 'bar'))

    # without a key a single row is emitted from the empty state
    ieq((('value',), (0,)), aggregate(table, None, Count(), 'bar'))
    ieq((('value',), (None,)), aggregate(table, None, Mean(), 'bar'))
    ieq((('value',), ((None, 0),)),
        aggregate(table, None, Variance(), 'bar'))

    # merging empty partials
    p = partial_aggregate(table, 'foo', Mean(), 'bar')
    ieq((('foo', 'value'),), merge_aggregate([p], 'foo', Mean()))
    ieq((('foo', 'value'),), merge_aggregate([], 'foo', Mean()))
    ieq((('value',), (0,)), merge_aggregate([], None, Count()))
    ieq((('value',), (None,)), merge_aggregate([], None, Mean()))
    ieq((('value',), ((None, 0),)), merge_aggregate([], None, Variance()))


def test_aggregate_stateful_missing():

    # group 'a' has only missing values
    table = (('foo', 'bar'),
             ('a', None),
             ('a', None),
             ('b', 1))

    ieq((('foo', 'value'), ('a', None), ('b', 1.0)),
        aggregate(table, 'foo', Mean(), 'bar'))
    ieq((('foo', 'value'), ('a', (None, 0)), ('b', (None, 1))),
        aggregate(table, 'foo', Variance(), 'bar'))
    # Count counts all values by default, optionally skipping missing
    ieq((('foo', 'value'), ('a', 2), ('b', 1)),
        aggregate(table, 'foo', Count(), 'bar'))
    ieq((('foo', 'value'), ('a', 0), ('b', 1)),
        aggregate(table, 'foo', Count(skip_missing=True), 'bar'))

    # same results when computed via partial states
    p = partial_aggregate(table, 'foo', Mean(), 'bar')
    ieq((('foo', 'value'), ('a', None), ('b', 1.0)),
        merge_aggregate([p], 'foo', Mean()))


def test_aggregate_stateful_decimal():

    from decimal import Decimal

    table = (('foo', 'bar'),
             ('a', Decimal('1.5')),
             ('a', Decimal('2.5')),
             ('b', Decimal('4')))

    actual = list(aggregate(table, 'foo', Mean(), 'bar'))
    assert actual[0] == ('foo', 'value')
    assert actual[1] == ('a', Decimal('2.0'))
    assert actual[2] == ('b', Decimal('4'))
    assert isinstance(actual[1][1], Decimal)

    # chunked computation merges to the same exact Decimal result
    oneshot = list(aggregate(table, 'foo', Variance(), 'bar'))
    assert oneshot[1] == ('a', (Decimal('0.5'), 2))
    assert oneshot[2] == ('b', (None, 1))
    for cut in range(len(table) + 1):
        p1, p2 = [partial_aggregate(chunk, 'foo', Variance(), 'bar')
                  for chunk in _chunked(table, (cut,))]
        assert oneshot == list(merge_aggregate([p1, p2], 'foo', Variance()))


def _floattable(n=60):
    table = [('foo', 'bar')]
    for i in range(n):
        table.append(('abc'[i % 3], (i * 7 % 13) + 0.1 * (i % 7)))
    return tuple(map(tuple, table))


def _chunked(table, cuts):
    # split the data rows of a table at the given positions
    rows = table[1:]
    chunks = []
    prev = 0
    for cut in list(cuts) + [len(rows)]:
        chunks.append(table[:1] + tuple(rows[prev:cut]))
        prev = cut
    return chunks


def test_partial_merge_equivalence():

    table = _floattable()
    aggregators = [Count(), Mean(), Variance(), Variance(sample=False)]

    for agg in aggregators:
        oneshot = list(aggregate(table, 'foo', agg, 'bar'))
        # various split positions
        for cuts in [(0,), (1,), (17,), (59,), (60,), (10, 40), (1, 2, 59)]:
            partials = [partial_aggregate(chunk, 'foo', agg, 'bar')
                        for chunk in _chunked(table, cuts)]
            _ieq_close(oneshot, merge_aggregate(partials, 'foo', agg))
            # merge order does not matter for commutative aggregators
            _ieq_close(oneshot,
                       merge_aggregate(partials[::-1], 'foo', agg))
        # a single partial table may be passed directly
        p = partial_aggregate(table, 'foo', agg, 'bar')
        _ieq_close(oneshot, merge_aggregate(p, 'foo', agg))


def test_partial_merge_equivalence_nokey():

    table = _floattable()
    for agg in [Count(), Mean(), Variance()]:
        oneshot = list(aggregate(table, None, agg, 'bar'))
        for cuts in [(0,), (23,), (60,), (10, 40)]:
            partials = [partial_aggregate(chunk, None, agg, 'bar')
                        for chunk in _chunked(table, cuts)]
            _ieq_close(oneshot, merge_aggregate(partials, None, agg))
            _ieq_close(oneshot, merge_aggregate(partials[::-1], None, agg))


def test_partial_merge_multi():

    table = _floattable()
    aggregation = OrderedDict()
    aggregation['count'] = 'bar', Count()
    aggregation['meanbar'] = 'bar', Mean()
    aggregation['varbar'] = 'bar', Variance()

    oneshot = list(aggregate(table, 'foo', aggregation))
    for cuts in [(0,), (1,), (33,), (60,), (10, 40)]:
        partials = [partial_aggregate(chunk, 'foo', aggregation)
                    for chunk in _chunked(table, cuts)]
        _ieq_close(oneshot, merge_aggregate(partials, 'foo', aggregation))
        _ieq_close(oneshot,
                   merge_aggregate(partials[::-1], 'foo', aggregation))

    # without a key
    oneshot = list(aggregate(table, None, aggregation))
    partials = [partial_aggregate(chunk, None, aggregation)
                for chunk in _chunked(table, (20, 40))]
    _ieq_close(oneshot, merge_aggregate(partials, None, aggregation))


def test_partial_aggregate_requires_stateful():

    table = (('foo', 'bar'), ('a', 1))
    with pytest.raises(ArgumentError):
        partial_aggregate(table, 'foo', sum, 'bar')
    with pytest.raises(ArgumentError):
        partial_aggregate(table, 'foo', {'s': ('bar', sum)})
    with pytest.raises(ArgumentError):
        merge_aggregate([], 'foo', sum)
    with pytest.raises(ArgumentError):
        merge_aggregate([], 'foo', {'s': ('bar', len)})


def test_merge_aggregate_mismatched_headers():

    table = (('foo', 'bar'), ('a', 1), ('b', 2))
    p1 = partial_aggregate(table, 'foo', Mean(), 'bar')
    p2 = partial_aggregate(table, 'foo', Mean(), 'bar', field='other')
    with pytest.raises(ArgumentError):
        list(merge_aggregate([p1, p2], 'foo', Mean()))


class _MutListAgg(StatefulAggregator):
    # deliberately mutates the framework-owned state in place

    def initial(self):
        return []

    def accumulate(self, state, value):
        state.append(value)
        return state

    def merge(self, state, other):
        state.extend(other)
        return state

    def finalize(self, state):
        return len(state)


def test_stateful_state_isolation():

    table = (('foo', 'bar'),
             ('a', 1),
             ('a', 2),
             ('b', 3))
    expect = (('foo', 'value'),
              ('a', 2),
              ('b', 1))

    # each group and each iteration accumulates from a fresh state
    t = aggregate(table, 'foo', _MutListAgg(), 'bar')
    ieq(expect, t)
    ieq(expect, t)

    # user-supplied state objects are not mutated by merging
    p = [list(row) for row in partial_aggregate(table, 'foo',
                                                _MutListAgg(), 'bar')]
    snapshot = [list(row[1]) for row in p[1:]]
    m = merge_aggregate([p], 'foo', _MutListAgg())
    ieq(expect, m)
    ieq(expect, m)
    assert [list(row[1]) for row in p[1:]] == snapshot


class _Concat(StatefulAggregator):
    # string concatenation is not commutative

    commutative = False

    def initial(self):
        return ''

    def accumulate(self, state, value):
        return state + str(value)

    def merge(self, state, other):
        return state + other

    def finalize(self, state):
        return state


def test_merge_aggregate_noncommutative():

    table = (('foo', 'bar'),
             ('a', 'x'),
             ('a', 'y'),
             ('b', 'z'))

    # one-shot use is fine, values arrive in input order
    ieq((('foo', 'value'), ('a', 'xy'), ('b', 'z')),
        aggregate(table, 'foo', _Concat(), 'bar', presorted=True))

    p1 = partial_aggregate(table[:2], 'foo', _Concat(), 'bar',
                           presorted=True)
    p2 = partial_aggregate(table[:1] + table[2:], 'foo', _Concat(), 'bar',
                           presorted=True)

    # unordered merging of a non-commutative aggregator is rejected
    with pytest.raises(ArgumentError):
        merge_aggregate([p1, p2], 'foo', _Concat())

    # ordered merging preserves the chunk order
    m = merge_aggregate([p1, p2], 'foo', _Concat(), ordered=True)
    ieq((('foo', 'value'), ('a', 'xy'), ('b', 'z')), m)

    # ... also without a key
    c1 = partial_aggregate(table[:3], None, _Concat(), 'bar')
    c2 = partial_aggregate(table[:1] + table[3:], None, _Concat(), 'bar')
    with pytest.raises(ArgumentError):
        merge_aggregate([c1, c2], None, _Concat())
    ieq((('value',), ('xyz',)),
        merge_aggregate([c1, c2], None, _Concat(), ordered=True))
