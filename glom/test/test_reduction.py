import operator

import pytest
from boltons.dictutils import OMD

from glom import (glom, T, Sum, Fold, Flatten, Coalesce, flatten, FoldError,
                  Glommer, Merge, merge, FoldStop, STOP, SKIP, Reduced,
                  FoldResult, GlomError)


def test_sum_integers():
    target = list(range(5))

    assert glom(target, Sum()) == 10

    assert glom(target, Sum(init=lambda: 2)) == 12

    target = []
    assert glom(target, Sum()) == 0


    target = [{"num": 3}, {"num": 2}, {"num": -1}]
    assert glom(target, Sum(['num'])) == 4

    target = target + [{}]  # add a non-compliant dict
    assert glom(target, Sum([Coalesce('num', default=0)])) ==4

    assert repr(Sum()) == 'Sum()'
    assert repr(Sum(len, init=float)) == 'Sum(len, init=float)'


def test_sum_seqs():
    target = [(x,) for x in range(4)]
    assert glom(target, Sum(init=tuple)) == (0, 1, 2, 3)

    # would not work with builtin sum(), gets:
    # "TypeError: sum() can't sum strings [use ''.join(seq) instead]"
    # Works here for now. If we're ok with that error, then we can
    # switch to sum().
    target = ['a', 'b', 'cd']
    assert glom(target, Sum(init=str)) == 'abcd'

    target = [['a'], ['b'], ['cde'], ['']]

    assert glom(target, Sum(Sum(init=list), init=str)) == 'abcde'


def test_fold():
    target = range(1, 5)
    assert glom(target, Fold(T, int)) == 10
    assert glom(target, Fold(T, init=lambda: 2)) == 12

    assert glom(target, Fold(T, lambda: 1, op=lambda l, r: l * r)) == 24

    assert repr(Fold(T, int)) == 'Fold(T, init=int)'
    assert repr(Fold(T, int, op=operator.imul)).startswith('Fold(T, init=int, op=<')

    # signature coverage
    with pytest.raises(TypeError):
        Fold(T, list, op=None)  # noncallable op

    with pytest.raises(TypeError):
        Fold(T, init=None)  # noncallable init


def test_fold_bad_iter():
    glommer = Glommer(register_default_types=False)

    def bad_iter(obj):
        raise RuntimeError('oops')

    glommer.register(list, iterate=bad_iter)

    with pytest.raises(TypeError):
        target = []
        glommer.glom(target, Flatten())


def test_fold_stop_early_termination():
    seen = []

    def op(acc, val):
        seen.append(val)
        if val == 3:
            return FoldStop(acc + val)
        return acc + val

    # stops as soon as FoldStop is returned, remaining values unread
    result = glom(range(1, 100), Fold(T, int, op=op))
    assert result == 6
    assert seen == [1, 2, 3]

    # the wrapped value, whatever it is, becomes the final accumulator
    sentinel = object()
    def op_sentinel(acc, val):
        return FoldStop(sentinel)
    assert glom([1, 2, 3], Fold(T, int, op=op_sentinel)) is sentinel

    assert FoldStop(42).value == 42
    assert FoldStop(42).unwrap() == 42
    assert repr(FoldStop(42)) == 'FoldStop(42)'

    # subclasses still trigger early termination
    class MyFoldStop(FoldStop):
        pass
    result = glom([1, 2, 3], Fold(T, int, op=lambda a, v: MyFoldStop(a + v)))
    assert result == 1


def test_fold_stop_lookalike_does_not_terminate():
    # termination is type-based, so a structurally similar plain
    # value must be treated as an ordinary accumulator
    class FoldStopLookalike:
        def __init__(self, value):
            self.value = value

        def unwrap(self):
            return self.value

        def __repr__(self):
            return 'FoldStop(%r)' % (self.value,)

    result = glom([1, 2, 3], Fold(T, init=list,
                                  op=lambda acc, v: acc + [FoldStopLookalike(v)]))
    assert len(result) == 3
    assert all(isinstance(item, FoldStopLookalike) for item in result)


def test_fold_stop_distinct_from_stop_and_skip():
    assert FoldStop(STOP) is not STOP
    assert FoldStop(SKIP) is not SKIP
    assert Reduced is FoldStop
    assert FoldResult is FoldStop

    # returning STOP/SKIP from op keeps its ordinary-value semantics
    # inside a Fold (it is not the early-termination signal)
    result = glom([1, 2], Fold(T, init=list, op=lambda acc, v: acc + [STOP]))
    assert result == [STOP, STOP]


def test_fold_stop_empty_input_and_init():
    # empty input never calls op and yields the init value
    assert glom([], Fold(T, int)) == 0
    assert glom([], Fold(T, init=lambda: 'start')) == 'start'


def test_fold_stop_nested_fold():
    def stop_op(acc, val):
        if val == 2:
            return FoldStop(acc + val)
        return acc + val

    inner = Fold(T, int, op=stop_op)
    # inner fold stops at 2, never reading 99; outer fold still runs
    result = glom([[1, 2, 99], [3]],
                  ([inner], Fold(T, init=str, op=lambda a, v: a + str(v))))
    assert result == '33'


def test_fold_stop_merge():
    def op(acc, val):
        acc.update(val)
        if len(acc) == 2:
            return FoldStop(acc)

    result = glom([{'a': 1}, {'b': 2}, {'c': 3}], Merge(op=op))
    assert result == {'a': 1, 'b': 2}


class _CloseTrackingIterator:
    def __init__(self, values):
        self._values = iter(values)
        self.closed = False

    def __iter__(self):
        return self

    def __next__(self):
        return next(self._values)

    def close(self):
        self.closed = True


def test_fold_borrowed_iterator_retains_position():
    # an iterator passed in directly is borrowed: it is not closed and
    # remains positioned right after the last consumed record
    iterator = _CloseTrackingIterator([1, 2, 3, 4, 5])

    def op(acc, val):
        if val == 3:
            return FoldStop(acc + val)
        return acc + val

    result = glom(iterator, Fold(T, int, op=op))
    assert result == 6
    assert iterator.closed is False
    assert next(iterator) == 4

    # same for a borrowed list iterator
    list_iter = iter([1, 2, 3, 4, 5])
    assert glom(list_iter, Fold(T, int, op=op)) == 6
    assert next(list_iter) == 4


def test_fold_owned_iterator_is_closed():
    created = []

    class IterableFactory:
        def __iter__(self):
            gen = (v for v in range(1, 6))
            created.append(gen)
            return gen

    def op(acc, val):
        if val == 3:
            return FoldStop(acc + val)
        return acc + val

    result = glom(IterableFactory(), Fold(T, int, op=op))
    assert result == 6
    assert created[0].gi_frame is None  # generator was closed


def test_fold_stop_does_not_read_past_termination():
    # even if later source records would raise, early stop means they
    # are never pulled
    def raising_source():
        yield 1
        yield 2
        yield 3
        raise RuntimeError('should not be reached')

    class RaisingIterable:
        def __iter__(self):
            return raising_source()

    seen = []

    def op(acc, val):
        seen.append(val)
        if val == 2:
            return FoldStop(acc + val)
        return acc + val

    result = glom(RaisingIterable(), Fold(T, int, op=op))
    assert result == 3
    assert seen == [1, 2]


def test_fold_close_failure_does_not_mask_main_exception():
    class BadCloseIterator:
        def __iter__(self):
            return self

        def __next__(self):
            raise ValueError('main error')

        def close(self):
            raise RuntimeError('close error')

    class OwnedIterable:
        def __iter__(self):
            return BadCloseIterator()

    with pytest.raises(GlomError) as exc_info:
        glom(OwnedIterable(), Fold(T, int))

    messages = []
    current = exc_info.value
    seen_ids = set()
    while current is not None and id(current) not in seen_ids:
        seen_ids.add(id(current))
        messages.append(str(current))
        current = current.__cause__ or current.__context__
    assert any('main error' in msg for msg in messages)
    assert not any('close error' in msg for msg in messages)


def test_flatten():
    target = [[1], [2], [3, 4]]
    assert glom(target, Flatten()) == [1, 2, 3, 4]

    target = [(1, 2), [3]]
    assert glom(target, Flatten()) == [1, 2, 3]

    gen = glom(target, Flatten(init='lazy'))
    assert next(gen) == 1
    assert list(gen) == [2, 3]

    assert repr(Flatten()) == 'Flatten()'
    assert repr(Flatten(init='lazy')) == "Flatten(init='lazy')"
    assert repr(Flatten(init=tuple)) == "Flatten(init=tuple)"


def test_flatten_func():
    target = [[1], [2], [3, 4]]
    assert flatten(target) == [1, 2, 3, 4]

    two_level_target = [[x] for x in target]
    assert flatten(two_level_target, levels=2) == [1, 2, 3, 4]
    assert flatten(two_level_target, levels=0) == two_level_target

    unflattenable = 2

    with pytest.raises(FoldError):
        assert flatten(unflattenable)

    # kind of an odd use, but it works for now
    assert flatten(['a', 'b', 'cd'], init=str) == 'abcd'

    # another odd case
    subspec_target = {'items': {'numbers': [1, 2, 3]}}
    assert (flatten(subspec_target, spec='items.numbers', init=int) == 6)

    # basic signature tests
    with pytest.raises(ValueError):
        flatten([], levels=-1)

    with pytest.raises(TypeError):
        flatten([], nonexistentkwarg=False)

    return


def test_merge():

    target = [{'a': 'A'}, {'b': 'B'}]

    assert glom(target, Merge()) == {'a': 'A', 'b': 'B'}

    assert glom(target, Merge(op=dict.update)) == {'a': 'A', 'b': 'B'}

    with pytest.raises(ValueError):
        Merge(init=list)  # has no .update()

    with pytest.raises(ValueError):
        Merge(op='update_extend')  # doesn't work on base dict, the default init


def test_merge_omd():
    target = [{'a': 'A'}, {'a': 'aleph'}]

    assert glom(target, Merge(init=OMD)) == OMD({'a': 'aleph'})
    assert glom(target, Merge(init=OMD, op='update_extend')) == OMD([('a', 'A'), ('a', 'aleph')])


def test_merge_func():

    target = [{'a': 'A'}, {'b': 'B'}]
    assert merge(target) == {'a': 'A', 'b': 'B'}
    assert merge([]) == {}

    # basic signature test
    with pytest.raises(TypeError):
        merge([], nonexistentkwarg=False)
