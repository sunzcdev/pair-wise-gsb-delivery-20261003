from __future__ import absolute_import, print_function, division


from datetime import datetime
from collections import Counter


from petl.test.helpers import ieq
from petl.transform.setops import complement, intersection, diff, \
    recordcomplement, recordintersection, recorddiff, hashcomplement, \
    hashintersection


def _test_complement_1(complement_impl):

    table1 = (('foo', 'bar'),
              ('A', 1),
              ('B', 2),
              ('C', 7))

    table2 = (('foo', 'bar'),
              ('A', 9),
              ('B', 2),
              ('B', 3))

    expectation = (('foo', 'bar'),
                   ('A', 1),
                   ('C', 7))

    result = complement_impl(table1, table2)
    ieq(expectation, result)


def _test_complement_2(complement_impl):

    tablea = (('foo', 'bar', 'baz'),
              ('A', 1, True),
              ('C', 7, False),
              ('B', 2, False),
              ('C', 9, True))

    tableb = (('x', 'y', 'z'),
              ('B', 2, False),
              ('A', 9, False),
              ('B', 3, True),
              ('C', 9, True))

    aminusb = (('foo', 'bar', 'baz'),
               ('A', 1, True),
               ('C', 7, False))

    result = complement_impl(tablea, tableb)
    ieq(aminusb, result)

    bminusa = (('x', 'y', 'z'),
               ('A', 9, False),
               ('B', 3, True))

    result = complement_impl(tableb, tablea)
    ieq(bminusa, result)


def _test_complement_3(complement_impl):

    # make sure we deal with empty tables

    table1 = (('foo', 'bar'),
              ('A', 1),
              ('B', 2))

    table2 = (('foo', 'bar'),)

    expectation = (('foo', 'bar'),
                   ('A', 1),
                   ('B', 2))
    result = complement_impl(table1, table2)
    ieq(expectation, result)
    ieq(expectation, result)

    expectation = (('foo', 'bar'),)
    result = complement_impl(table2, table1)
    ieq(expectation, result)
    ieq(expectation, result)


def _test_complement_4(complement_impl):

    # test behaviour with duplicate rows

    table1 = (('foo', 'bar'),
              ('A', 1),
              ('B', 2),
              ('B', 2),
              ('C', 7))

    table2 = (('foo', 'bar'),
              ('B', 2))

    result = complement_impl(table1, table2)
    expectation = (('foo', 'bar'),
                   ('A', 1),
                   ('B', 2),
                   ('C', 7))
    ieq(expectation, result)
    ieq(expectation, result)

    # strict behaviour
    result = complement_impl(table1, table2, strict=True)
    expectation = (('foo', 'bar'),
                   ('A', 1),
                   ('C', 7))
    ieq(expectation, result)
    ieq(expectation, result)


def _test_complement_none(complement_impl):
    # test behaviour with unsortable types
    now = datetime.now()

    ta = [['a', 'b'], [None, None]]
    tb = [['a', 'b'], [None, now]]

    expectation = (('a', 'b'), (None, None))
    result = complement_impl(ta, tb)
    ieq(expectation, result)

    ta = [['a'], [now], [None]]
    tb = [['a'], [None], [None]]

    expectation = (('a',), (now,))
    result = complement_impl(ta, tb)
    ieq(expectation, result)


def _test_complement(f):
    _test_complement_1(f)
    _test_complement_2(f)
    _test_complement_3(f)
    _test_complement_4(f)
    _test_complement_none(f)


def test_complement():
    _test_complement(complement)


def test_complement_seqtypes():
    # test complement isn't confused by list vs tuple
    ta = [['a', 'b'], ['A', 1], ['B', 2]]
    tb = [('a', 'b'), ('A', 1), ('B', 2)]
    expectation = (('a', 'b'),)
    actual = complement(ta, tb, presorted=True)
    ieq(expectation, actual)


def test_hashcomplement_seqtypes():
    # test complement isn't confused by list vs tuple
    ta = [['a', 'b'], ['A', 1], ['B', 2]]
    tb = [('a', 'b'), ('A', 1), ('B', 2)]
    expectation = (('a', 'b'),)
    actual = hashcomplement(ta, tb)
    ieq(expectation, actual)


def test_diff():

    tablea = (('foo', 'bar', 'baz'),
              ('A', 1, True),
              ('C', 7, False),
              ('B', 2, False),
              ('C', 9, True))

    tableb = (('x', 'y', 'z'),
              ('B', 2, False),
              ('A', 9, False),
              ('B', 3, True),
              ('C', 9, True))

    aminusb = (('foo', 'bar', 'baz'),
               ('A', 1, True),
               ('C', 7, False))

    bminusa = (('x', 'y', 'z'),
               ('A', 9, False),
               ('B', 3, True))

    added, subtracted = diff(tablea, tableb)
    ieq(bminusa, added)
    ieq(aminusb, subtracted)


def test_recordcomplement_1():

    table1 = (('foo', 'bar'),
              ('A', 1),
              ('B', 2),
              ('C', 7))

    table2 = (('bar', 'foo'),
              (9, 'A'),
              (2, 'B'),
              (3, 'B'))

    expectation = (('foo', 'bar'),
                   ('A', 1),
                   ('C', 7))

    result = recordcomplement(table1, table2)
    ieq(expectation, result)


def test_recordcomplement_2():

    tablea = (('foo', 'bar', 'baz'),
              ('A', 1, True),
              ('C', 7, False),
              ('B', 2, False),
              ('C', 9, True))

    tableb = (('bar', 'foo', 'baz'),
              (2, 'B', False),
              (9, 'A', False),
              (3, 'B', True),
              (9, 'C', True))

    aminusb = (('foo', 'bar', 'baz'),
               ('A', 1, True),
               ('C', 7, False))

    result = recordcomplement(tablea, tableb)
    ieq(aminusb, result)

    bminusa = (('bar', 'foo', 'baz'),
               (3, 'B', True),
               (9, 'A', False))

    result = recordcomplement(tableb, tablea)
    ieq(bminusa, result)


def test_recordcomplement_3():

    # make sure we deal with empty tables

    table1 = (('foo', 'bar'),
              ('A', 1),
              ('B', 2))

    table2 = (('bar', 'foo'),)

    expectation = (('foo', 'bar'),
                   ('A', 1),
                   ('B', 2))
    result = recordcomplement(table1, table2)
    ieq(expectation, result)
    ieq(expectation, result)

    expectation = (('bar', 'foo'),)
    result = recordcomplement(table2, table1)
    ieq(expectation, result)
    ieq(expectation, result)


def test_recordcomplement_4():

    # test behaviour with duplicate rows

    table1 = (('foo', 'bar'),
              ('A', 1),
              ('B', 2),
              ('B', 2),
              ('C', 7))

    table2 = (('bar', 'foo'),
              (2, 'B'))

    result = recordcomplement(table1, table2)
    expectation = (('foo', 'bar'),
                   ('A', 1),
                   ('B', 2),
                   ('C', 7))
    ieq(expectation, result)
    ieq(expectation, result)

    # strict behaviour
    result = recordcomplement(table1, table2, strict=True)
    expectation = (('foo', 'bar'),
                   ('A', 1),
                   ('C', 7))
    ieq(expectation, result)
    ieq(expectation, result)


def test_recorddiff():

    tablea = (('foo', 'bar', 'baz'),
              ('A', 1, True),
              ('C', 7, False),
              ('B', 2, False),
              ('C', 9, True))

    tableb = (('bar', 'foo', 'baz'),
              (2, 'B', False),
              (9, 'A', False),
              (3, 'B', True),
              (9, 'C', True))

    aminusb = (('foo', 'bar', 'baz'),
               ('A', 1, True),
               ('C', 7, False))

    bminusa = (('bar', 'foo', 'baz'),
               (3, 'B', True),
               (9, 'A', False))

    added, subtracted = recorddiff(tablea, tableb)
    ieq(aminusb, subtracted)
    ieq(bminusa, added)


def _test_intersection_1(intersection_impl):

    table1 = (('foo', 'bar'),
              ('A', 1),
              ('B', 2),
              ('C', 7))

    table2 = (('foo', 'bar'),
              ('A', 9),
              ('B', 2),
              ('B', 3))

    expectation = (('foo', 'bar'),
                   ('B', 2))

    result = intersection_impl(table1, table2)
    ieq(expectation, result)


def _test_intersection_2(intersection_impl):

    table1 = (('foo', 'bar', 'baz'),
              ('A', 1, True),
              ('C', 7, False),
              ('B', 2, False),
              ('C', 9, True))

    table2 = (('x', 'y', 'z'),
              ('B', 2, False),
              ('A', 9, False),
              ('B', 3, True),
              ('C', 9, True))

    expect = (('foo', 'bar', 'baz'),
              ('B', 2, False),
              ('C', 9, True))

    table3 = intersection_impl(table1, table2)
    ieq(expect, table3)


def _test_intersection_3(intersection_impl):

    # empty table
    table1 = (('foo', 'bar'),
              ('A', 1),
              ('B', 2),
              ('C', 7))

    table2 = (('foo', 'bar'),)

    expectation = (('foo', 'bar'),)
    result = intersection_impl(table1, table2)
    ieq(expectation, result)
    ieq(expectation, result)


def _test_intersection_4(intersection_impl):

    # duplicate rows

    table1 = (('foo', 'bar'),
              ('A', 1),
              ('B', 2),
              ('B', 2),
              ('B', 2),
              ('C', 7))

    table2 = (('foo', 'bar'),
              ('A', 9),
              ('B', 2),
              ('B', 2),
              ('B', 3))

    expectation = (('foo', 'bar'),
                   ('B', 2),
                   ('B', 2))

    result = intersection_impl(table1, table2)
    ieq(expectation, result)
    ieq(expectation, result)


def _test_intersection_empty(intersection_impl):

    table1 = (('foo', 'bar'),
              ('A', 1),
              ('B', 2),
              ('C', 7))
    table2 = (('foo', 'bar'),)
    expectation = (('foo', 'bar'),)
    result = intersection_impl(table1, table2)
    ieq(expectation, result)


def _test_intersection(intersection_impl):
    _test_intersection_1(intersection_impl)
    _test_intersection_2(intersection_impl)
    _test_intersection_3(intersection_impl)
    _test_intersection_4(intersection_impl)
    _test_intersection_empty(intersection_impl)


def test_intersection():
    _test_intersection(intersection)


def test_hashcomplement():
    _test_complement(hashcomplement)


def test_hashintersection():
    _test_intersection(hashintersection)


def _multiset_tables():
    # row x occurs three times on the left and once on the right
    a = (('foo', 'bar'),
         ('A', 1),
         ('B', 2), ('B', 2), ('B', 2),
         ('C', 7))
    b = (('foo', 'bar'),
         ('B', 2))
    return a, b


def _test_multiset_complement(impl):
    a, b = _multiset_tables()
    # multiset: the single right-hand B cancels one of the three left rows
    expectation = (('foo', 'bar'),
                   ('A', 1),
                   ('B', 2), ('B', 2),
                   ('C', 7))
    ieq(expectation, impl(a, b))
    ieq(expectation, impl(a, b, multiset=True))
    # strict set mode: B is found on the right, so every B is removed
    strict_expectation = (('foo', 'bar'),
                          ('A', 1),
                          ('C', 7))
    ieq(strict_expectation, impl(a, b, strict=True))


def test_multiset_complement():
    _test_multiset_complement(complement)


def test_hash_multiset_complement():
    _test_multiset_complement(hashcomplement)


def _test_strict_intersection(impl):
    a = (('foo', 'bar'),
         ('B', 2), ('B', 2), ('B', 2),
         ('C', 7))
    b = (('foo', 'bar'),
         ('B', 2), ('B', 2))
    # default and explicit multiset keep the minimum multiplicity
    multiset_expectation = (('foo', 'bar'),
                            ('B', 2), ('B', 2))
    ieq(multiset_expectation, impl(a, b))
    ieq(multiset_expectation, impl(a, b, multiset=True))
    # strict set mode keeps each matching row only once
    strict_expectation = (('foo', 'bar'),
                          ('B', 2))
    ieq(strict_expectation, impl(a, b, strict=True))


def test_strict_intersection():
    _test_strict_intersection(intersection)


def test_hash_strict_intersection():
    _test_strict_intersection(hashintersection)


def _test_exclusive_flags(impl):
    a, b = _multiset_tables()
    import pytest
    with pytest.raises(ValueError):
        impl(a, b, strict=True, multiset=True)


def test_exclusive_flags():
    for impl in (complement, intersection, hashcomplement, hashintersection,
                 recordcomplement, recordintersection, diff, recorddiff):
        _test_exclusive_flags(impl)


def test_recordintersection_field_alignment():
    # columns are in a different order; record versions align by field name
    a = (('foo', 'bar'),
         ('A', 1),
         ('B', 2), ('B', 2), ('B', 2),
         ('C', 7))
    b = (('bar', 'foo'),
         (2, 'B'), (2, 'B'),
         (1, 'A'))
    multiset_expectation = (('foo', 'bar'),
                            ('A', 1),
                            ('B', 2), ('B', 2))
    ieq(multiset_expectation, recordintersection(a, b))
    ieq(multiset_expectation, recordintersection(a, b, multiset=True))
    strict_expectation = (('foo', 'bar'),
                          ('A', 1),
                          ('B', 2))
    ieq(strict_expectation, recordintersection(a, b, strict=True))


def test_positional_intersection_ignores_field_names():
    # the ordinary version compares by position, so reordered columns do not
    # match even when the records are equal by field name
    a = (('foo', 'bar'),
         ('B', 2), ('B', 2))
    b = (('bar', 'foo'),
         (2, 'B'), (2, 'B'))
    expectation = (('foo', 'bar'),)
    ieq(expectation, intersection(a, b))
    ieq(expectation, intersection(a, b, strict=True))


def test_multiset_reconstruction_identity():
    # multiplicity must be conserved across both directions:
    # A == (A - B) + intersection(A, B) and likewise for B
    a = (('k',),
         ('p',), ('p',),
         ('q',), ('q',), ('q',),
         ('r',))
    b = (('k',),
         ('q',), ('q',),
         ('r',), ('r',),
         ('s',))

    def counts(table):
        return Counter(row for row in table)

    for cmpl, inter in ((complement, intersection),
                        (hashcomplement, hashintersection)):
        a_only = counts(list(cmpl(a, b))[1:])
        b_only = counts(list(cmpl(b, a))[1:])
        common_ab = counts(list(inter(a, b))[1:])
        common_ba = counts(list(inter(b, a))[1:])
        assert a_only + common_ab == counts(list(a)[1:])
        assert b_only + common_ba == counts(list(b)[1:])
        # multiset intersection is symmetric
        assert common_ab == common_ba
        # strict set mode reconstructs the de-duplicated inputs
        assert (set(list(cmpl(a, b, strict=True))[1:])
                | set(list(inter(a, b, strict=True))[1:])) \
            == set(list(a)[1:])


def test_large_duplicate_run_presorted():
    # a single merge must handle a very large repeated group without keeping
    # the whole group in memory
    n = 10000
    a = (('k',),) + (('x',),) * n + (('z',),)
    b = (('k',),) + (('x',),) * (n - 3)
    ieq((('k',),) + (('x',),) * 3 + (('z',),),
        complement(a, b, presorted=True))
    ieq((('k',),) + (('x',),) * (n - 3),
        intersection(a, b, presorted=True))
    ieq((('k',), ('x',)),
        intersection(a, b, presorted=True, strict=True))
