"""Test diskcache.persistent.Deque."""

import pickle
import shutil
import tempfile
from unittest import mock

import pytest

import diskcache as dc
from diskcache.core import ENOVAL


def rmdir(directory):
    try:
        shutil.rmtree(directory)
    except OSError:
        pass


@pytest.fixture
def deque():
    deque = dc.Deque()
    yield deque
    rmdir(deque.directory)


def test_init():
    directory = tempfile.mkdtemp()
    sequence = list('abcde')
    deque = dc.Deque(sequence, None)

    assert deque == sequence

    rmdir(deque.directory)
    del deque

    rmdir(directory)
    deque = dc.Deque(sequence, directory)

    assert deque.directory == directory
    assert deque == sequence

    other = dc.Deque(directory=directory)

    assert other == deque

    del deque
    del other
    rmdir(directory)


def test_getsetdel(deque):
    sequence = list('abcde')
    assert len(deque) == 0

    for key in sequence:
        deque.append(key)

    assert len(deque) == len(sequence)

    for index in range(len(sequence)):
        assert deque[index] == sequence[index]

    for index in range(len(sequence)):
        deque[index] = index

    for index in range(len(sequence)):
        assert deque[index] == index

    for index in range(len(sequence)):
        if index % 2:
            del deque[-1]
        else:
            del deque[0]

    assert len(deque) == 0


def test_append(deque):
    deque.maxlen = 3
    for item in 'abcde':
        deque.append(item)
    assert deque == 'cde'


def test_appendleft(deque):
    deque.maxlen = 3
    for item in 'abcde':
        deque.appendleft(item)
    assert deque == 'edc'


def test_index_positive(deque):
    cache = mock.MagicMock()
    cache.__len__.return_value = 3
    cache.iterkeys.return_value = ['a', 'b', 'c']
    cache.__getitem__.side_effect = [KeyError, 101, 102]
    with mock.patch.object(deque, '_cache', cache):
        assert deque[0] == 101


def test_index_negative(deque):
    cache = mock.MagicMock()
    cache.__len__.return_value = 3
    cache.iterkeys.return_value = ['c', 'b', 'a']
    cache.__getitem__.side_effect = [KeyError, 101, 100]
    with mock.patch.object(deque, '_cache', cache):
        assert deque[-1] == 101


def test_index_out_of_range(deque):
    cache = mock.MagicMock()
    cache.__len__.return_value = 3
    cache.iterkeys.return_value = ['a', 'b', 'c']
    cache.__getitem__.side_effect = [KeyError] * 3
    with mock.patch.object(deque, '_cache', cache):
        with pytest.raises(IndexError):
            deque[0]


def test_iter_keyerror(deque):
    cache = mock.MagicMock()
    cache.iterkeys.return_value = ['a', 'b', 'c']
    cache.__getitem__.side_effect = [KeyError, 101, 102]
    with mock.patch.object(deque, '_cache', cache):
        assert list(iter(deque)) == [101, 102]


def test_reversed(deque):
    sequence = list('abcde')
    deque += sequence
    assert list(reversed(deque)) == list(reversed(sequence))


def test_reversed_keyerror(deque):
    cache = mock.MagicMock()
    cache.iterkeys.return_value = ['c', 'b', 'a']
    cache.__getitem__.side_effect = [KeyError, 101, 100]
    with mock.patch.object(deque, '_cache', cache):
        assert list(reversed(deque)) == [101, 100]


def test_state(deque):
    sequence = list('abcde')
    deque.extend(sequence)
    assert deque == sequence
    deque.maxlen = 3
    assert list(deque) == sequence[-3:]
    state = pickle.dumps(deque)
    values = pickle.loads(state)
    assert values == sequence[-3:]
    assert values.maxlen == 3


def test_compare(deque):
    assert not (deque == {})
    assert not (deque == [0])
    assert deque != [1]
    deque.append(0)
    assert deque <= [0]
    assert deque <= [1]


def test_indexerror_negative(deque):
    with pytest.raises(IndexError):
        deque[-1]


def test_indexerror(deque):
    with pytest.raises(IndexError):
        deque[0]


def test_repr():
    directory = tempfile.mkdtemp()
    deque = dc.Deque(directory=directory)
    assert repr(deque) == 'Deque(directory=%r)' % directory


def test_copy(deque):
    sequence = list('abcde')
    deque.extend(sequence)
    temp = deque.copy()
    assert deque == sequence
    assert temp == sequence


def test_count(deque):
    deque += 'abbcccddddeeeee'

    for index, value in enumerate('abcde', 1):
        assert deque.count(value) == index


def test_extend(deque):
    sequence = list('abcde')
    deque.extend(sequence)
    assert deque == sequence


def test_extendleft(deque):
    sequence = list('abcde')
    deque.extendleft(sequence)
    assert deque == list(reversed(sequence))


def test_extend_batch_order(deque):
    assert deque.extend_batch(iter('abcde')) == (5, 0)
    assert deque == 'abcde'
    assert deque.extend_batch(iter('fg')) == (2, 0)
    assert deque == 'abcdefg'


def test_extendleft_batch_order(deque):
    # Unlike repeated appendleft, the batch must not be inverted.
    assert deque.extendleft_batch(iter('abcde')) == (5, 0)
    assert deque == 'abcde'
    assert deque.extendleft_batch(iter('fg')) == (2, 0)
    assert deque == 'fgabcde'


def test_extend_batch_maxlen(deque):
    deque.maxlen = 3
    assert deque.extend_batch([1, 2, 3, 4, 5]) == (3, 2)
    assert deque == [3, 4, 5]


def test_extendleft_batch_maxlen(deque):
    deque.maxlen = 3
    assert deque.extendleft_batch([1, 2, 3, 4, 5]) == (3, 2)
    assert deque == [1, 2, 3]


def test_extend_batch_maxlen_existing(deque):
    deque.maxlen = 3
    deque.extend_batch([1, 2, 3])
    assert deque.extend_batch([4, 5]) == (2, 0)
    assert deque == [3, 4, 5]
    assert deque.extendleft_batch([6, 7]) == (2, 0)
    assert deque == [6, 7, 3]


def test_extend_batch_larger_than_maxlen(deque):
    deque.maxlen = 2
    assert deque.extend_batch(range(6)) == (2, 4)
    assert deque == [4, 5]

    deque.clear()
    assert deque.extendleft_batch(range(6)) == (2, 4)
    assert deque == [0, 1]


def test_extend_batch_empty(deque):
    assert deque.extend_batch(iter(())) == (0, 0)
    assert deque.extendleft_batch([]) == (0, 0)
    assert len(deque) == 0


def test_extend_batch_reject(deque):
    deque.maxlen = 3
    deque.extend_batch([1, 2, 3])

    with pytest.raises(ValueError):
        deque.extend_batch([4], policy='reject')

    with pytest.raises(ValueError):
        deque.extendleft_batch([4], policy='reject')

    assert deque == [1, 2, 3]

    # A batch that fits exactly is accepted.
    deque.clear()
    assert deque.extend_batch([1, 2, 3], policy='reject') == (3, 0)
    assert deque == [1, 2, 3]


def test_extend_batch_bad_policy(deque):
    with pytest.raises(ValueError):
        deque.extend_batch([1], policy='unknown')
    assert len(deque) == 0


def test_extend_batch_iterator_error(deque):
    def iterable():
        yield 'a'
        yield 'b'
        raise RuntimeError('source failed')

    deque.extend_batch(['x'])

    with pytest.raises(RuntimeError):
        deque.extend_batch(iterable())

    assert deque == ['x']


def test_extendleft_batch_iterator_error(deque):
    def iterable():
        yield 'a'
        raise RuntimeError('source failed')

    with pytest.raises(RuntimeError):
        deque.extendleft_batch(iterable())

    assert len(deque) == 0


def test_extend_batch_serialization_error(deque):
    class BadValue:
        def __reduce__(self):
            raise TypeError('cannot serialize')

    deque.extend_batch(['x'])

    with pytest.raises(TypeError):
        deque.extend_batch(['a', BadValue(), 'c'])

    assert deque == ['x']


def test_extend_batch_rollback(deque):
    deque.extend_batch(['a', 'b'])

    with pytest.raises(RuntimeError):
        with deque.transact():
            assert deque.extend_batch(['c', 'd']) == (2, 0)
            assert deque == 'abcd'
            raise RuntimeError('abort')

    assert deque == 'ab'


def test_extendleft_batch_rollback(deque):
    deque.extendleft_batch(['a', 'b'])

    with pytest.raises(RuntimeError):
        with deque.transact():
            assert deque.extendleft_batch(['c', 'd']) == (2, 0)
            assert deque == 'cdab'
            raise RuntimeError('abort')

    assert deque == 'ab'


def test_extend_batch_evict_rollback():
    deque = dc.Deque(maxlen=2)
    try:
        deque.extend_batch([1, 2])

        with pytest.raises(RuntimeError):
            with deque.transact():
                assert deque.extend_batch([3, 4]) == (2, 0)
                assert deque == [3, 4]
                raise RuntimeError('abort')

        assert deque == [1, 2]
    finally:
        rmdir(deque.directory)


def test_extend_batch_file_values(deque):
    value = b'x' * 100_000

    assert deque.extend_batch([value, value], policy='reject') == (2, 0)
    assert len(deque) == 2
    assert list(deque) == [value, value]

    with pytest.raises(RuntimeError):
        with deque.transact():
            deque.extend_batch([value])
            raise RuntimeError('abort')

    assert list(deque) == [value, value]
    assert len(deque.cache.check()) == 0


def test_extend_batch_database_error(deque):
    deque.extend_batch(['a'])

    cache = deque.cache
    transact = mock.Mock()
    transact.side_effect = dc.Timeout

    with mock.patch.object(cache, '_transact', transact):
        with pytest.raises(dc.Timeout):
            deque.extend_batch(['b', 'c'])

    assert deque == ['a']


def test_extend_batch_nested_transaction(deque):
    # Nested transaction blocks share the outer transaction. A batch that
    # fails inside the nested block must still leave no partial rows behind
    # while the outer transaction stays usable.
    with deque.transact():
        with deque.transact():
            with pytest.raises(dc.Timeout):
                with mock.patch.object(
                    deque.cache, '_transact', mock.Mock(side_effect=dc.Timeout)
                ):
                    deque.extend_batch(['a', 'b'])

        assert len(deque) == 0
        assert deque.extend_batch(['c']) == (1, 0)

    assert deque == ['c']


def test_extend_batch_key_overflow():
    deque = dc.Deque()
    try:
        deque.extend_batch([1, 2, 3])

        # Move the last two keys next to the upper bound to force renumbering
        # when the next batch is appended.
        cache = deque.cache
        update = 'UPDATE Cache SET key = ? WHERE key = ?'
        cache._sql(update, (999999999999996, 500000000000001))
        cache._sql(update, (999999999999997, 500000000000002))

        assert deque == [1, 2, 3]
        assert deque.extend_batch([4, 5]) == (2, 0)
        assert deque == [1, 2, 3, 4, 5]
        assert len(cache.check()) == 0
    finally:
        rmdir(deque.directory)


def test_extend_batch_reject_rollback(deque):
    deque.maxlen = 2
    deque.extend_batch([1, 2])

    with pytest.raises(ValueError):
        with deque.transact():
            deque.extend_batch([3, 4], policy='reject')

    assert deque == [1, 2]


def test_pop(deque):
    sequence = list('abcde')
    deque.extend(sequence)

    while sequence:
        assert deque.pop() == sequence.pop()


def test_pop_indexerror(deque):
    with pytest.raises(IndexError):
        deque.pop()


def test_popleft(deque):
    sequence = list('abcde')
    deque.extend(sequence)

    while sequence:
        value = sequence[0]
        assert deque.popleft() == value
        del sequence[0]


def test_popleft_indexerror(deque):
    with pytest.raises(IndexError):
        deque.popleft()


def test_remove(deque):
    deque.extend('abaca')
    deque.remove('a')
    assert deque == 'baca'
    deque.remove('a')
    assert deque == 'bca'
    deque.remove('a')
    assert deque == 'bc'


def test_remove_valueerror(deque):
    with pytest.raises(ValueError):
        deque.remove(0)


def test_remove_keyerror(deque):
    cache = mock.MagicMock()
    cache.iterkeys.return_value = ['a', 'b', 'c']
    cache.__getitem__.side_effect = [KeyError, 100, 100]
    cache.__delitem__.side_effect = [KeyError, None]
    with mock.patch.object(deque, '_cache', cache):
        deque.remove(100)


def test_reverse(deque):
    deque += 'abcde'
    deque.reverse()
    assert deque == 'edcba'


def test_rotate_typeerror(deque):
    with pytest.raises(TypeError):
        deque.rotate(0.5)


def test_rotate(deque):
    deque.rotate(1)
    deque.rotate(-1)
    deque += 'abcde'
    deque.rotate(3)
    assert deque == 'cdeab'


def test_rotate_negative(deque):
    deque += 'abcde'
    deque.rotate(-2)
    assert deque == 'cdeab'


def test_rotate_indexerror(deque):
    deque += 'abc'

    cache = mock.MagicMock()
    cache.__len__.return_value = 3
    cache.pull.side_effect = [(None, ENOVAL)]

    with mock.patch.object(deque, '_cache', cache):
        deque.rotate(1)


def test_rotate_indexerror_negative(deque):
    deque += 'abc'

    cache = mock.MagicMock()
    cache.__len__.return_value = 3
    cache.pull.side_effect = [(None, ENOVAL)]

    with mock.patch.object(deque, '_cache', cache):
        deque.rotate(-1)


def test_peek(deque):
    value = b'x' * 100_000
    deque.append(value)
    assert len(deque) == 1
    assert deque.peek() == value
    assert len(deque) == 1
    assert deque.peek() == value
    assert len(deque) == 1
