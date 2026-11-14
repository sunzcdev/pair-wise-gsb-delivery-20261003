import json

import pytest

from glom import (glom, T, Path, Coalesce, Or, Match, Check,
                  GlomError, PathAccessError, PathAssignError, CoalesceError,
                  MatchError, TypeMatchError, CheckError, UnregisteredTarget)
from glom.mutation import assign, delete, PathDeleteError


def _to_jsonable(err, **kwargs):
    """serialize and prove JSON-compatibility by round-tripping"""
    return json.loads(json.dumps(err.to_dict(**kwargs)))


def test_access_error_to_dict():
    target = {'a': {'b': [10, 20]}}
    with pytest.raises(PathAccessError) as exc_info:
        glom(target, T['a']['b'][5])

    d = _to_jsonable(exc_info.value)
    assert d['type'] == 'PathAccessError'
    assert d['category'] == 'access'
    assert d['op'] == 'get'
    assert d['part_idx'] == 2
    assert 'could not access' in d['message']

    # integer indices and string keys stay distinct
    kinds = [seg['kind'] for seg in d['spec_path']]
    assert kinds == ['key', 'key', 'index']
    values = [seg['value'] for seg in d['spec_path']]
    assert values == ['a', 'b', 5]
    assert isinstance(d['spec_path'][2]['value'], int)

    # target path is the traversed prefix of the spec path
    assert [seg['value'] for seg in d['target_path']] == ['a', 'b']

    # the original exception is recorded as the cause
    assert d['cause']['type'] == 'IndexError'
    assert d['cause']['category'] == 'external'


def test_access_error_str_and_type_unchanged():
    target = {'a': {'b': None}}
    with pytest.raises(PathAccessError) as exc_info:
        glom(target, 'a.b.c')
    err = exc_info.value
    assert isinstance(err, (AttributeError, KeyError, IndexError))
    assert "could not access 'c', part 2 of Path('a', 'b', 'c')" in str(err)
    # structured output does not affect the classic string form
    err.to_dict()
    assert "could not access 'c', part 2 of Path('a', 'b', 'c')" in str(err)


def test_assign_error_to_dict():
    with pytest.raises(PathAssignError) as exc_info:
        assign(['short', 'list'], Path(5), 'too far')

    d = _to_jsonable(exc_info.value)
    assert d['type'] == 'PathAssignError'
    assert d['category'] == 'assign'
    assert d['op'] == 'assign'
    assert d['dest'] == {'kind': 'index', 'value': 5}
    assert d['cause']['type'] == 'IndexError'


def test_delete_error_to_dict():
    with pytest.raises(PathDeleteError) as exc_info:
        delete({'a': 1}, 'missing_key')

    d = _to_jsonable(exc_info.value)
    assert d['type'] == 'PathDeleteError'
    assert d['category'] == 'assign'
    assert d['op'] == 'delete'
    assert d['dest'] == {'kind': 'key', 'value': 'missing_key'}


def test_coalesce_branch_errors_ordered():
    with pytest.raises(CoalesceError) as exc_info:
        glom({}, Coalesce('a', 'b', 'c'))

    d = _to_jsonable(exc_info.value)
    assert d['type'] == 'CoalesceError'
    assert d['category'] == 'coalesce'
    assert d['op'] == 'coalesce'
    # branch errors keep the order of the Coalesce subspecs
    branch_keys = [child['spec_path'][0]['value'] for child in d['children']]
    assert branch_keys == ['a', 'b', 'c']


def test_or_branch_errors_ordered_and_cyclic_safe():
    with pytest.raises(PathAccessError) as exc_info:
        glom({}, Or('a', 'b'))

    # the last branch error still propagates, type unchanged
    err = exc_info.value
    assert err.path == Path('b')

    d = _to_jsonable(err)
    first, last = d['children']
    assert first['spec_path'][0]['value'] == 'a'
    # the final error is its own last branch error: cyclic, not recursive
    assert last == {'type': 'PathAccessError', 'cyclic_ref': True}


def test_explicit_cycle_does_not_recurse():
    err = CoalesceError(Coalesce('a'), [], Path('a'))
    err.skipped.append(err)  # pathological self-reference
    d = _to_jsonable(err)
    assert d['children'][0] == {'type': 'CoalesceError', 'cyclic_ref': True}


def test_error_tree_depth_limited():
    err = top = CoalesceError(Coalesce('a'), [], None)
    for _ in range(10):
        nxt = CoalesceError(Coalesce('a'), [], None)
        err.skipped.append(nxt)
        err = nxt

    d = _to_jsonable(top, max_depth=3)
    cur = d
    for _ in range(3):
        cur = cur['children'][0]
    assert cur == {'type': 'CoalesceError', 'truncated': 'max_depth_exceeded'}


def test_long_strings_truncated_and_marked():
    long_key = 'k' * 500
    with pytest.raises(PathAccessError) as exc_info:
        glom({}, long_key)

    d = _to_jsonable(exc_info.value, max_str_len=50)
    assert d['message_truncated'] is True
    assert '...[truncated]' in d['message']
    assert len(d['message']) < 100
    seg_value = d['spec_path'][0]['value']
    assert seg_value.startswith('k' * 50)
    assert '...[truncated]' in seg_value


def test_match_error_to_dict():
    with pytest.raises(TypeMatchError) as exc_info:
        glom({'id': 'a'}, Match({'id': int}))

    d = _to_jsonable(exc_info.value)
    assert d['type'] == 'TypeMatchError'
    assert d['category'] == 'match'
    assert d['op'] == 'match'
    assert d['expected_type'] == 'int'
    assert d['actual_type'] == 'str'
    # type names only, the target value itself is not structured in
    assert "'a'" not in json.dumps(d)


def test_check_error_to_dict():
    target = {'a': {'b': 'c'}}
    with pytest.raises(CheckError) as exc_info:
        glom(target, {'b': ('a.b', Check(type=int))})

    d = _to_jsonable(exc_info.value)
    assert d['type'] == 'CheckError'
    assert d['category'] == 'check'
    assert d['op'] == 'check'
    assert len(d['msgs']) == 1
    assert "expected type to be 'int'" in d['msgs'][0]
    assert [seg['value'] for seg in d['target_path']] == ['a.b']


def test_unregistered_target_to_dict():
    with pytest.raises(UnregisteredTarget) as exc_info:
        glom(object(), ['a', 'b'])

    d = _to_jsonable(exc_info.value)
    assert d['type'] == 'UnregisteredTarget'
    assert d['category'] == 'unregistered_target'
    assert d['op'] == 'iterate'
    assert d['target_type'] == 'object'
    assert 'dict' in d['registered_types']


def test_nested_failures_serialize_as_tree():
    with pytest.raises(CoalesceError) as exc_info:
        glom({}, Coalesce(Coalesce('a', 'b'), 'c'))

    d = _to_jsonable(exc_info.value)
    inner, outer = d['children']
    assert inner['type'] == 'CoalesceError'
    assert [c['spec_path'][0]['value'] for c in inner['children']] == ['a', 'b']
    assert outer['type'] == 'PathAccessError'
    assert outer['spec_path'][0]['value'] == 'c'


def test_target_values_not_included():
    target = {'password': 's3cret', 'nested': {'token': 'abc123'}}
    with pytest.raises(PathAccessError) as exc_info:
        glom(target, 'nested.missing')

    blob = json.dumps(exc_info.value.to_dict())
    assert 's3cret' not in blob
    assert 'abc123' not in blob


def test_coalesce_skipped_values_not_included():
    # a skipped *value* (not exception) may be sensitive; only its
    # type may appear in structured output
    with pytest.raises(CoalesceError) as exc_info:
        glom({'password': 's3cret'},
             Coalesce('missing', 'password', skip=lambda v: isinstance(v, str)))

    d = _to_jsonable(exc_info.value)
    assert d['children'][0]['type'] == 'PathAccessError'
    assert d['children'][1] == {'type': 'str', 'value_omitted': True}
    blob = json.dumps(d)
    assert 's3cret' not in blob
    # the callable skip is not repr'd either
    assert d['skip'] == {'kind': 'callable'}
    assert '<function' not in blob


def test_callable_reprs_not_included():
    err = PathAccessError(KeyError('x'), Path('a', len), 1)
    d = _to_jsonable(err)
    assert d['spec_path'][1] == {'op': 'P', 'kind': 'callable'}
    assert 'built-in' not in json.dumps(d)


def test_to_json_roundtrip():
    with pytest.raises(CoalesceError) as exc_info:
        glom({}, Coalesce('a', 'b'))
    err = exc_info.value
    assert json.loads(err.to_json()) == err.to_dict()
    assert json.loads(err.to_json(max_depth=2)) == err.to_dict(max_depth=2)
