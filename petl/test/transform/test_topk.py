from __future__ import absolute_import, print_function, division


import gc
import weakref

import pytest

from petl.test.helpers import eq_, ieq
from petl.transform.sorts import topk


def test_topk_normal_mode_sort_and_rank_order():
    table = (
        ('group', 'score', 'id'),
        ('b', 2, 'b2'),
        ('a', 10, 'a10'),
        ('b', 1, 'b1'),
        ('a', 2, 'a2'),
        ('a', None, 'an'),
        ('b', 3, 'b3'),
    )

    expect = (
        ('group', 'score', 'id'),
        ('a', 2, 'a2'),
        ('a', 10, 'a10'),
        ('b', 1, 'b1'),
        ('b', 2, 'b2'),
    )
    ieq(expect, topk(table, 'group', 2, rank='score'))


def test_topk_presorted_group_order_is_preserved():
    table = (
        ('group', 'score', 'id'),
        ('b', 2, 'b2'),
        ('b', 1, 'b1'),
        ('a', 9, 'a9'),
        ('a', 3, 'a3'),
    )

    expect = (
        ('group', 'score', 'id'),
        ('b', 1, 'b1'),
        ('b', 2, 'b2'),
        ('a', 3, 'a3'),
        ('a', 9, 'a9'),
    )
    ieq(expect, topk(table, 'group', 2, rank='score', presorted=True))


def test_topk_multiple_rank_fields_with_mixed_directions():
    table = (
        ('group', 'score', 'time', 'id'),
        ('a', 5, 2, 'a52'),
        ('a', 5, 1, 'a51'),
        ('a', 7, 3, 'a73'),
        ('b', 3, 9, 'b39'),
        ('b', 3, 1, 'b31'),
        ('b', 1, 0, 'b10'),
    )

    expect = (
        ('group', 'score', 'time', 'id'),
        ('a', 7, 3, 'a73'),
        ('a', 5, 1, 'a51'),
        ('b', 3, 1, 'b31'),
        ('b', 3, 9, 'b39'),
    )
    result = topk(table, 'group', 2, rank=['score', 'time'],
                  reverse=[True, False])
    ieq(expect, result)

    # A field/direction pair is also accepted, together with the order alias.
    result = topk(table, 'group', 2,
                  order=[('score', 'desc'), ('time', 'asc')])
    ieq(expect, result)


def test_topk_ties_use_input_order():
    table = (
        ('group', 'score', 'id'),
        ('g', 5, 'first'),
        ('g', 5, 'second'),
        ('g', 5, 'third'),
        ('g', 5, 'fourth'),
    )

    expect = (
        ('group', 'score', 'id'),
        ('g', 5, 'first'),
        ('g', 5, 'second'),
    )
    ieq(expect, topk(table, 'group', 2, rank='score', presorted=True))


def test_topk_none_position_is_explicit():
    table = (
        ('group', 'score', 'id'),
        ('g', 2, 'two'),
        ('g', None, 'none'),
        ('g', 10, 'ten'),
    )

    expect = (
        ('group', 'score', 'id'),
        ('g', 2, 'two'),
        ('g', 10, 'ten'),
        ('g', None, 'none'),
    )
    ieq(expect, topk(table, 'group', 3, rank='score',
                     none_position='last'))

    expect = (
        ('group', 'score', 'id'),
        ('g', None, 'none'),
        ('g', 2, 'two'),
        ('g', 10, 'ten'),
    )
    ieq(expect, topk(table, 'group', 3, rank='score',
                     none_position='first'))

    expect = (
        ('group', 'score', 'id'),
        ('g', 10, 'ten'),
        ('g', 2, 'two'),
        ('g', None, 'none'),
    )
    ieq(expect, topk(table, 'group', 3, rank='score', reverse=True,
                     none_position='last'))


def test_topk_zero_only_reads_header():
    consumed = []

    def source():
        yield ('group', 'score', 'id')
        consumed.append(True)
        yield ('g', 1, 'one')

    expect = (('group', 'score', 'id'),)
    ieq(expect, topk(source(), 'group', 0, rank='score'))
    eq_([], consumed)


def test_topk_negative_rejected_before_reading_source():

    def source():
        yield ('group', 'score', 'id')
        raise AssertionError('source should not be consumed')

    with pytest.raises(ValueError):
        topk(source(), 'group', -1, rank='score')


class _Row(list):
    pass


def test_topk_generator_does_not_materialise_group():
    nrows = 5000
    k = 3
    refs = []

    def source():
        yield ('group', 'score', 'id')
        for index in range(nrows):
            row = _Row(['g', 1, index])
            refs.append(weakref.ref(row))
            yield row

    result = topk(source(), 'group', k, rank='score', presorted=True)
    output = []
    max_live = 0
    for row in result:
        output.append(row)
        gc.collect()
        live = sum(ref() is not None for ref in refs)
        max_live = max(max_live, live)

    expect = (
        ('group', 'score', 'id'),
        ('g', 1, 0),
        ('g', 1, 1),
        ('g', 1, 2),
    )
    ieq(expect, output)

    # Sampling while the result generator was suspended between rows ensures
    # a list(group) implementation would have kept all 5000 rows alive.
    assert max_live <= k + 2
