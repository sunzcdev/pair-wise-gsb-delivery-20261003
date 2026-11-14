import pytest

from itertools import count, dropwhile, chain

from glom import Iter
from glom import glom, SKIP, STOP, T, Call, Spec, Glommer, Check, SKIP
from glom.core import bbrepr


RANGE_5 = list(range(5))


def test_iter():
    assert list(glom(['1', '2', '3'], Iter(int))) == [1, 2, 3]
    cnt = count()
    cnt_1 = glom(cnt, Iter(lambda t: t + 1))
    assert (next(cnt_1), next(cnt_1)) == (1, 2)
    assert next(cnt) == 2

    assert list(glom(['1', '2', '3'], (Iter(int), enumerate))) == [(0, 1), (1, 2), (2, 3)]

    assert list(glom([1, SKIP, 2], Iter())) == [1, 2]
    assert list(glom([1, STOP, 2], Iter())) == [1]

    with pytest.raises(TypeError):
        Iter(nonexistent_kwarg=True)


def test_filter():
    is_odd = lambda x: x % 2
    odd_spec = Iter().filter(is_odd)
    out = glom(RANGE_5, odd_spec)
    assert list(out) == [1, 3]

    # let's just make sure we're actually streaming just in case
    counter = count()
    out = glom(counter, odd_spec)
    assert next(out) == 1
    assert next(out) == 3
    assert next(counter) == 4
    assert next(counter) == 5
    assert next(out) == 7

    bools = [True, False, False, True, False]
    spec = Iter().filter().all()
    out = glom(bools, spec)
    assert out == [True, True]

    imags = [0j, 1j, 2, 2j, 3j]
    spec = Iter().filter(Check(T.imag.real, type=float, one_of=(0, 2), default=SKIP)).all()
    out = glom(imags, spec)
    assert out == [0j, 2j]

    assert repr(Iter().filter(T.a.b)) == 'Iter().filter(T.a.b)'
    assert repr(Iter(list).filter(sum)) == 'Iter(list).filter(sum)'


def test_map():
    spec = Iter().map(lambda x: x * 2)
    out = glom(RANGE_5, spec)
    assert list(out) == [0, 2, 4, 6, 8]
    assert repr(Iter().map(T.a.b)) == 'Iter().map(T.a.b)'


def test_split_flatten():
    falsey_stream = [1, None, None, 2, 3, None, 4]
    spec = Iter().split()
    out = glom(falsey_stream, spec)
    assert list(out) == [[1], [2, 3], [4]]

    spec = Iter().split().flatten()
    out = glom(falsey_stream, spec)
    assert list(out) == [1, 2, 3, 4]

    assert repr(Iter().split(sep=None, maxsplit=2)) == 'Iter().split(sep=None, maxsplit=2)'
    assert repr(Iter(T.a.b[1]).flatten()) == 'Iter(T.a.b[1]).flatten()'


def test_chunked():
    int_list = list(range(9))

    spec = Iter().chunked(3)
    out = glom(int_list, spec)
    assert list(out) == [[0, 1, 2], [3, 4, 5], [6, 7, 8]]

    spec = Iter().chunked(3).map(sum)
    out = glom(int_list, spec)
    assert list(out) == [3, 12, 21]


def test_weighted_chunked():
    # basic weight-budget chunking; items are never split
    out = list(glom([1, 2, 3, 4], Iter().weighted_chunked(lambda x: x, 5)))
    assert out == [[1, 2], [3], [4]]

    # zero-weight items pack together and never force a new chunk
    out = list(glom([0, 0, 1, 0], Iter().weighted_chunked(lambda x: x, 1)))
    assert out == [[0, 0, 1, 0]]

    # exact-fit boundary is allowed
    out = list(glom([2, 3], Iter().weighted_chunked(lambda x: x, 5)))
    assert out == [[2, 3]]

    # the final under-full chunk is still emitted
    out = list(glom([1, 1], Iter().weighted_chunked(lambda x: x, 5)))
    assert out == [[1, 1]]

    # empty input produces no chunks at all
    out = list(glom([], Iter().weighted_chunked(lambda x: x, 5)))
    assert out == []


def test_weighted_chunked_count():
    # count caps the number of items independently of weight
    out = list(glom([1, 2, 3, 4],
                    Iter().weighted_chunked(lambda x: x, 100, count=2)))
    assert out == [[1, 2], [3, 4]]

    # whichever limit is hit first closes the chunk
    out = list(glom([1, 2, 3, 4],
                    Iter().weighted_chunked(lambda x: x, 3, count=2)))
    assert out == [[1, 2], [3], [4]]

    # count=1 forces one item per chunk
    out = list(glom([0, 0], Iter().weighted_chunked(lambda x: x, 5, count=1)))
    assert out == [[0], [0]]


def test_weighted_chunked_oversize():
    # default 'separate': an over-budget item gets its own chunk
    out = list(glom([1, 9, 1], Iter().weighted_chunked(lambda x: x, 5)))
    assert out == [[1], [9], [1]]

    # an over-budget item at the head of the stream still stands alone
    out = list(glom([9, 1], Iter().weighted_chunked(lambda x: x, 5)))
    assert out == [[9], [1]]

    # 'reject' raises instead, carrying the source index
    with pytest.raises(ValueError, match='source index 1'):
        list(glom([1, 9], Iter().weighted_chunked(
            lambda x: x, 5, oversize='reject')))

    with pytest.raises(ValueError):
        list(glom([9], Iter().weighted_chunked(
            lambda x: x, 5, oversize='reject')))

    with pytest.raises(ValueError):
        list(glom([1], Iter().weighted_chunked(
            lambda x: x, 5, oversize='bogus')))


def test_weighted_chunked_invalid_weights():
    with pytest.raises(ValueError, match='source index 2'):
        list(glom([1, 2, -3], Iter().weighted_chunked(lambda x: x, 5)))

    with pytest.raises(ValueError, match='source index 0'):
        list(glom([1.5], Iter().weighted_chunked(lambda x: x, 5)))

    # booleans are ints in Python but are not valid weights
    with pytest.raises(ValueError, match='source index 1'):
        list(glom([1, True], Iter().weighted_chunked(lambda x: x, 5)))


def test_weighted_chunked_rfile_called_once():
    calls = []

    def rfile(item):
        calls.append(item)
        return item

    out = list(glom([1, 2, 3, 4], Iter().weighted_chunked(rfile, 5)))
    assert out == [[1, 2], [3], [4]]
    assert calls == [1, 2, 3, 4]


def test_weighted_chunked_lazy_lookahead():
    # only the first item of the next chunk may be read ahead; the
    # rest of the source stays unconsumed
    pulled = []

    def tracked(src):
        for item in src:
            pulled.append(item)
            yield item

    chunks = glom(tracked(iter([1, 2, 3, 4, 5])),
                  Iter().weighted_chunked(lambda x: x, 3))

    assert next(chunks) == [1, 2]
    # exactly one lookahead item beyond the chunk's own items
    assert pulled == [1, 2, 3]

    assert list(chunks) == [[3], [4], [5]]
    assert pulled == [1, 2, 3, 4, 5]

    # an infinite source must not be consumed up front
    out = glom(count(), Iter().weighted_chunked(lambda x: 1, 3).limit(4))
    assert list(out) == [[0, 1, 2], [3, 4, 5], [6, 7, 8], [9, 10, 11]]


def test_weighted_chunked_skip_filtered_before_budget():
    # SKIPped elements never reach the budget accounting
    out = list(glom([1, SKIP, 1, SKIP, 1],
                    Iter().weighted_chunked(lambda x: x, 2)))
    assert out == [[1, 1], [1]]

    # errors are indexed by position in the post-filter stream
    with pytest.raises(ValueError, match='source index 1'):
        list(glom([SKIP, 1, -2],
                  Iter().weighted_chunked(lambda x: x, 5)))


def test_weighted_chunked_spec_rfile_and_repr():
    # the size function can be any glom spec, not just a callable
    target = [{'n': 1}, {'n': 2}, {'n': 3}]
    out = list(glom(target, Iter().weighted_chunked(T['n'], 3)))
    assert out == [[{'n': 1}, {'n': 2}], [{'n': 3}]]

    rfile = lambda x: x
    assert repr(Iter().weighted_chunked(rfile, 5)) == \
        'Iter().weighted_chunked(rfile=%s, budget=5)' % bbrepr(rfile)
    assert repr(Iter().weighted_chunked(rfile, 5, count=2)) == \
        'Iter().weighted_chunked(%s, 5, count=2)' % bbrepr(rfile)
    assert repr(Iter().weighted_chunked(rfile, 5, oversize='reject')) == \
        "Iter().weighted_chunked(%s, 5, oversize='reject')" % bbrepr(rfile)

    # a module-level/builtin callable gets the short repr treatment
    assert repr(Iter().weighted_chunked(len, 5)) == \
        'Iter().weighted_chunked(rfile=len, budget=5)'

    # budget/count validation lives on the streaming generator itself
    from glom.streaming import weighted_chunked_iter
    with pytest.raises(ValueError):
        weighted_chunked_iter(iter([1]), rfile, 0)
    with pytest.raises(ValueError):
        weighted_chunked_iter(iter([1]), rfile, 5, count=0)
    with pytest.raises(ValueError):
        weighted_chunked_iter(iter([1]), rfile, True)
    with pytest.raises(ValueError):
        weighted_chunked_iter(iter([1]), rfile, 5, count=True)


def test_windowed():
    int_list = list(range(5))

    spec = Iter().windowed(3)
    out = glom(int_list, spec)
    assert list(out) == [(0, 1, 2), (1, 2, 3), (2, 3, 4)]
    assert repr(spec) == 'Iter().windowed(3)'

    spec = spec.filter(lambda x: bool(x[0] % 2)).map(sum)
    out = glom(int_list, spec)
    assert next(out) == 6

    out = glom(range(10), spec)
    assert list(out) == [6, 12, 18, 24]


def test_unique():
    int_list = list(range(10))

    spec = Iter().unique()
    out = glom(int_list, spec)
    assert list(out) == int_list

    spec = Iter(lambda x: x % 4).unique()
    out = glom(int_list, spec)
    assert list(out) == int_list[:4]
    assert repr(Iter().unique(T.a)) == 'Iter().unique(T.a)'


def test_slice():
    cnt = count()

    spec = Iter().slice(3)
    out = glom(cnt, spec)

    assert list(out) == [0, 1, 2]
    assert next(cnt) == 3

    out = glom(range(10), Iter().slice(1, 5))
    assert list(out) == [1, 2, 3, 4]

    out = glom(range(10), Iter().slice(1, 6, 2))
    assert list(out) == [1, 3, 5]
    assert repr(Iter().slice(1, 6, 2)) == 'Iter().slice(1, 6, 2)'

    out = glom(range(10), Iter().limit(3))
    assert list(out) == [0, 1, 2]
    assert repr(Iter().limit(3)) == 'Iter().limit(3)'

    out = glom(range(5), Iter().limit(10))
    assert list(out) == [0, 1, 2, 3, 4]

    # test broken args
    with pytest.raises(TypeError):
        Iter().slice(1, 2, 3, 4)


def test_while():
    cnt = count()
    out = glom(cnt, Iter().takewhile(lambda x: x < 3))
    assert list(out) == [0, 1, 2]
    assert next(cnt) == 4
    assert repr(Iter().takewhile(T.a) == 'Iter().takewhile(T.a)')

    range_iter = iter(range(7))
    out = glom(range_iter, Iter().dropwhile(lambda x: x < 3 or x > 5))
    assert list(out) == [3, 4, 5, 6]  # 6 still here despite the x>5 above

    out = glom(range(10), Iter().dropwhile(lambda x: x >= 0).limit(10))
    assert list(out) == []

    out = glom(range(8), Iter().dropwhile((T.bit_length(), lambda x: x < 3)))
    assert list(out) == [4, 5, 6, 7]
    assert repr(Iter().dropwhile(T.a) == 'Iter().dropwhile(T.a)')


def test_iter_composition():
    int_list = list(range(10))
    out = glom(int_list, (Iter(), Iter(), list))
    assert out == int_list

    out = glom([int_list] * 3, Iter(Iter(lambda x: x % 4)).flatten().unique())
    assert list(out) == [0, 1, 2, 3]


def test_faulty_iterate():
    glommer = Glommer()

    def bad_iter(obj):
        raise RuntimeError('oops')

    glommer.register(str, iterate=bad_iter)

    with pytest.raises(TypeError):
        glommer.glom('abc', (Iter(), list))


def test_first():
    spec = Iter().first(T.imag)

    target = iter([1, 2, 3j, 4])
    out = glom(target, spec)
    assert out == 3j
    assert next(target) == 4
    assert repr(spec) == '(Iter(), First(T.imag))'

    spec = Iter().first(T.imag, default=0)
    target = iter([1, 2, 4])
    out = glom(target, spec)
    assert out == 0
    assert repr(spec) == '(Iter(), First(T.imag, default=0))'


def test_all():
    int_iter = iter(range(10))

    out = glom(int_iter, Iter().all())
    assert out == list(range(10))
    assert next(int_iter, None) is None
    assert repr(Iter().all()) == 'Pipe(Iter(), list)'
