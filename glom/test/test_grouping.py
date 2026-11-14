from pytest import raises
import json
import copy

from glom import glom, T, SKIP, STOP, Auto, BadSpec, Val
from glom.grouping import (Group, First, Avg, Max, Min, Sample, Limit,
                           GroupStateError, STATE_FORMAT_VERSION,
                           spec_fingerprint)

from glom.reduction import Merge, Flatten, Sum, Count


def test_bucketing():
    assert glom(range(4), Group({T % 2: [T]})) == {0: [0, 2], 1: [1, 3]}
    assert (glom(range(6), Group({T % 3: {T % 2: [T / 10.0]}})) ==
        {0: {0: [0.0], 1: [0.3]}, 1: {1: [0.1], 0: [0.4]}, 2: {0: [0.2], 1: [0.5]}})



def test_corner_cases():
    target = range(5)

    # immediate stop dict
    assert glom(target, Group({Val(STOP): [T]})) == {}

    # immediate stop list
    assert glom(target, Group([Val(STOP)])) == []

    # dict key SKIP
    assert glom(target, Group({(lambda t: SKIP if t < 3 else t): T})) == {3: 3, 4: 4}

    # dict val SKIP
    assert glom(target, Group({T: lambda t: t if t % 2 else SKIP})) == {3: 3, 1: 1}

    # list val SKIP
    assert glom(target, Group([lambda t: t if t % 2 else SKIP])) == [1, 3]

    # embedded auto spec (lol @ 0 being 0 bit length)
    assert glom(target, Group({Auto(('bit_length', T())): [T]})) == {0: [0], 1: [1], 2: [2, 3], 3: [4]}

    # no dicts inside lists in Group mode
    with raises(BadSpec):
        assert glom(target, Group([{T: T}]))

    # check only supported types
    with raises(BadSpec):
        assert glom(target, Group('no string support yet'))

    # bucket ints by their bit length and then odd/even, limited to 3 per bucket
    spec = Group({T.bit_length(): {lambda t: t % 2: Limit(3)}})
    res = glom(range(20), spec)
    assert res == {0: {0: [0]},
                   1: {1: [1]},
                   2: {0: [2], 1: [3]},
                   3: {0: [4, 6], 1: [5, 7]},
                   4: {0: [8, 10, 12], 1: [9, 11, 13]}}

    return


def test_agg():
    t = list(range(10))
    assert glom(t, Group(First())) == 0
    assert glom(t, Group(T)) == 9  # this is basically Last

    assert glom(t, Group(Avg())) == sum(t) / len(t)
    assert glom(t, Group(Sum())) == sum(t)

    assert glom([0, 1, 0], Group(Max())) == 1
    assert glom([1, 0, 1], Group(Min())) == 0

    assert repr(Group(First())) == 'Group(First())'
    assert repr(Avg()) == 'Avg()'
    assert repr(Max()) == 'Max()'
    assert repr(Min()) == 'Min()'
    assert repr(Sum()) == 'Sum()'
    assert repr(Count()) == 'Count()'

    assert glom(range(10), Group({lambda t: t % 2: Count()})) == {
		0: 5, 1: 5}


def test_limit():
    t = list(range(10))
    assert glom(t, Group(Limit(1, T))) == 0
    assert glom(t, Group(Limit(3, Max()))) == 2
    assert glom(t, Group(Limit(3, [T]))) == [0, 1, 2]

    assert repr(Group(Limit(3, Max()))) == 'Group(Limit(3, Max()))'

    with raises(BadSpec):
        assert glom(t, Limit(1))  # needs to be wrapped in Group for now
    return


def test_reduce():
    assert glom([[1], [2, 3]], Group(Flatten())) == [1, 2, 3]
    assert glom([{'a': 1}, {'b': 2}], Group(Merge())) == {'a': 1, 'b': 2}
    assert glom([[[1]], [[2, 3], [4]]], Group(Flatten(Flatten()))) == [1, 2, 3, 4]


def test_sample():
    spec = Group(Sample(5))
    assert glom([1, 2, 3], spec) == [1, 2, 3]

    assert repr(spec) == 'Group(Sample(5))'

    s = glom([1, 2, 3], Group(Sample(2)))
    assert s in [[1, 2], [1, 3], [2, 1], [2, 3], [3, 1], [3, 2]]


# ---------------------------------------------------------------------------
# aggregation state export / restore
# ---------------------------------------------------------------------------

def _roundtrip(state):
    # the state document must survive a plain JSON encode/decode
    return json.loads(json.dumps(state))


def test_group_state_basic_count_sum_avg():
    spec = {T % 2: Count()}
    full = Group(spec)
    expected = glom(range(10), full)

    first = Group({T % 2: Count()})
    glom(range(4), first)
    state = _roundtrip(first.export_state())

    assert state['format_version'] == STATE_FORMAT_VERSION
    assert isinstance(state['spec_fingerprint'], str) and len(state['spec_fingerprint']) == 64

    second = Group({T % 2: Count()})
    second.restore_state(state)
    glom(range(4, 10), second)
    assert glom([], second) == expected


def test_state_split_equivalence_all_aggs():
    data = list(range(25))
    for leaf in (Count(), Sum(), Sum(init=float), Avg()):
        spec = {T % 3: leaf}
        one_shot = glom(data, Group(spec))

        left = Group({T % 3: _fresh(leaf)})
        glom(data[:10], left)
        state = _roundtrip(left.export_state())

        right = Group({T % 3: _fresh(leaf)})
        right.restore_state(state)
        glom(data[10:], right)
        assert glom([], right) == one_shot


def _fresh(agg):
    if isinstance(agg, Sum):
        return Sum(init=agg.init)
    return type(agg)()


def test_state_nested_groups():
    spec = {T % 2: {T % 3: Count()}}
    data = list(range(30))
    one_shot = glom(data, Group(spec))

    left = Group({T % 2: {T % 3: Count()}})
    glom(data[:11], left)
    state = _roundtrip(left.export_state())

    right = Group({T % 2: {T % 3: Count()}})
    right.restore_state(state)
    glom(data[11:], right)
    assert glom([], right) == one_shot


def test_state_multiple_keyspec_branches():
    # two keyspecs in one dict, each with its own aggregation, using
    # tuple-tagged keys so the buckets never collide
    spec = {(lambda t: ('par', t % 2)): Count(),
            (lambda t: ('mod', t % 3)): Sum()}
    data = list(range(20))
    one_shot = glom(data, Group(spec))

    left = Group(copy.deepcopy(spec))
    glom(data[:8], left)
    state = _roundtrip(left.export_state())
    # each group remembers which branch produced it
    assert {g['valspec'] for g in state['state']['groups']} == {0, 1}

    right = Group(copy.deepcopy(spec))
    right.restore_state(state)
    glom(data[8:], right)
    assert glom([], right) == one_shot


def test_state_preserves_key_and_value_types():
    def key(t):
        return t
    # bool/int, int/str, None, bytes, float and tuple keys must not collide
    data = [0, 1, True, False, None, '0', '1', b'x', 1.5, (1, 'a'), (1, 'b')]
    spec = {key: Count()}
    one_shot = glom(data, Group(spec))

    left = Group({key: Count()})
    glom(data[:5], left)
    state = _roundtrip(left.export_state())

    right = Group({key: Count()})
    right.restore_state(state)
    glom(data[5:], right)
    result = glom([], right)
    assert result == one_shot
    # tuple keys come back as real tuples, not stringified
    assert all(isinstance(k, tuple) for k in result if isinstance(k, tuple))
    assert b'x' in result and None in result

    # large ints keep exact precision and stay ints across a restore
    int_group = Group(Sum())
    glom([10 ** 50, 10 ** 50], int_group)
    int_restored = Group(Sum())
    int_restored.restore_state(_roundtrip(int_group.export_state()))
    int_value = int_restored._acc_tree[int_restored.spec]
    assert isinstance(int_value, int) and int_value == 2 * 10 ** 50

    # a float-initialized Sum stays a float
    float_group = Group(Sum(init=float))
    glom([1.25, 2.5], float_group)
    float_restored = Group(Sum(init=float))
    float_restored.restore_state(_roundtrip(float_group.export_state()))
    float_value = float_restored._acc_tree[float_restored.spec]
    assert isinstance(float_value, float) and float_value == 3.75


def test_state_empty_group_and_leaf():
    # no data at all: empty group round-trips and keeps accepting data
    g = Group({T: Count()})
    state = _roundtrip(g.export_state())
    assert state['state'] == {'kind': 'dict', 'groups': []}
    g.restore_state(state)
    glom([1, 2, 3], g)
    assert glom([], g) == {1: 1, 2: 1, 3: 1}

    # empty Avg must not blow up on restore or a subsequent empty batch
    ag = Group({T: Avg()})
    ag.restore_state(_roundtrip(ag.export_state()))
    assert glom([], ag) == {}
    glom([2, 4], ag)
    assert glom([], ag) == {2: 2.0, 4: 4.0}


def test_exported_state_is_an_immutable_snapshot():
    g = Group({T % 2: Count()})
    glom(range(4), g)
    snapshot = g.export_state()
    snapshot_copy = json.loads(json.dumps(snapshot))

    # keep running the original aggregator
    glom(range(4, 10), g)
    # the previously exported document is unchanged
    assert json.loads(json.dumps(snapshot)) == snapshot_copy


def test_state_rejects_different_grouping_definition():
    g = Group({T % 2: Count()})
    glom(range(4), g)
    state = g.export_state()

    other = Group({T % 5: Count()})
    with raises(GroupStateError):
        other.restore_state(state)

    # same keys, different aggregation is also a different definition
    other_agg = Group({T % 2: Avg()})
    with raises(GroupStateError):
        other_agg.restore_state(state)


def test_state_rejects_bad_document():
    g = Group({T % 2: Count()})
    with raises(GroupStateError):
        g.restore_state({'format_version': 999,
                         'spec_fingerprint': spec_fingerprint(g.spec),
                         'state': {}})
    with raises(GroupStateError):
        g.restore_state('not a mapping')


def test_state_unsupported_aggregations_rejected_upfront():
    for bad_spec, feed in ((First(), [1, 2]), (Max(), [1, 2]), (Min(), [1, 2]),
                           (Sample(2), [1, 2]), ([T], [1, 2]),
                           (Merge(), [{'a': 1}, {'b': 2}]),
                           (Flatten(), [[[1]], [[2]]])):
        g = Group(bad_spec)
        glom(feed, g)  # normal processing still works
        with raises(GroupStateError):
            g.export_state()

    # custom callable cannot be restored
    g = Group({T: lambda t: t})
    glom([1], g)
    with raises(GroupStateError):
        g.export_state()

    # Sum with custom init callable is not portable
    custom_sum = Sum(init=lambda: 5)
    g = Group({T % 2: custom_sum})
    glom(range(3), g)
    with raises(GroupStateError):
        g.export_state()


def test_state_unsupported_key_rejected():
    class Marker:
        pass
    g = Group({lambda t: Marker(): Count()})
    glom([1], g)
    with raises(GroupStateError):
        g.export_state()


def test_persistent_group_across_batches_without_export():
    # the same Group instance accumulates across glom calls even without
    # an explicit export/restore round-trip
    g = Group({T % 2: Count()})
    glom(range(3), g)
    glom(range(3, 10), g)
    assert glom([], g) == glom(range(10), Group({T % 2: Count()}))


def test_persistent_group_empty_and_stopped_batches():
    # an empty batch on a fresh group yields the same base cases as a
    # single-shot Group
    assert glom([], Group({T: T})) == {}
    assert glom([], Group([T])) == []
    assert glom([], Group(First())) is None

    # once First() has stopped, later batches must not leak the internal
    # STOP sentinel; a no-new-value batch resolves to None (matching a
    # fresh Group fed an empty target)
    first = Group(First())
    assert glom([5, 6], first) == 5
    assert glom([7, 8], first) is None

    # Limit carries its capped list and never emits STOP
    limited = Group(Limit(2))
    assert glom([1, 2], limited) == [1, 2]
    assert glom([3], limited)[0] == 3


def test_state_fingerprint_stable_for_equivalent_specs():
    def mod2(t):
        return t % 2
    assert spec_fingerprint({mod2: Count()}) == spec_fingerprint({mod2: Count()})
    assert spec_fingerprint({T % 2: Count()}) != spec_fingerprint({T % 3: Count()})
    assert spec_fingerprint({T % 2: Count()}) != spec_fingerprint({T % 2: Sum()})
