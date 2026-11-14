import pytest

from glom import (glom, Spec, T, SKIP, Auto, Dict, KeyConflict,
                  KeyConflictError, KeySpecError, ValueSpecError,
                  ConflictMergeError)


def _same_key_target():
    return {'key_a': 'same', 'key_b': 'same', 'a': 1, 'b': 2}


def test_default_overwrite_keeps_order_and_target():
    target = _same_key_target()
    original = dict(target)
    events = []

    def key_a(t):
        events.append('key_a')
        return 'same'

    def key_b(t):
        events.append('key_b')
        return 'same'

    def val_a(t):
        events.append('val_a')
        return t['a']

    def val_b(t):
        events.append('val_b')
        return t['b']

    spec = {Spec(key_a): val_a, Spec(key_b): val_b}
    assert glom(target, spec) == {'same': 2}

    # Historical ordering: value first, then the computed key.
    assert events == ['val_a', 'key_a', 'val_b', 'key_b']
    assert target == original


def test_strict_reports_both_paths_before_value_conversion():
    target = _same_key_target()
    original = dict(target)
    events = []

    def key_a(t):
        events.append('key_a')
        return 'same'

    def key_b(t):
        events.append('key_b')
        return 'same'

    def val_a(t):
        events.append('val_a')
        return t['a']

    def val_b(t):
        events.append('val_b')
        return t['b']

    spec = {Spec(key_a): val_a, Spec(key_b): val_b}
    with pytest.raises(KeyConflictError) as exc_info:
        glom(target, spec, key_conflict='strict')

    # The conflicting value conversion is never executed.
    assert events == ['key_a', 'val_a', 'key_b']
    assert target == original

    err = exc_info.value
    assert err.key == 'same'
    assert "Spec(" in repr(err.first_spec_path)
    assert "Spec(" in repr(err.second_spec_path)
    assert repr(err.first_spec_path) != repr(err.second_spec_path)
    assert 'KeyConflictError' in str(err)


def test_strict_is_available_on_dict_and_auto_wrappers():
    target = _same_key_target()
    spec = {Spec('key_a'): 'a', Spec('key_b'): 'b'}

    with pytest.raises(KeyConflictError):
        glom(target, Dict(spec, key_conflict='strict'))
    with pytest.raises(KeyConflictError):
        glom(target, Auto(spec, key_conflict=KeyConflict.STRICT))

    with pytest.raises(KeyConflictError):
        glom(target, spec, on_key_conflict='strict')


def test_conflict_merge_spec_and_aliases():
    target = _same_key_target()
    spec = {Spec('key_a'): 'a', Spec('key_b'): 'b'}

    assert glom(target, spec, key_conflict='strict',
                conflict_merge=lambda old, new: old + new) == {'same': 3}
    assert glom(target, spec, key_conflict=lambda old, new: old + new) == {'same': 3}
    assert glom(target, spec, conflict_merge=T[0] + T[1]) == {'same': 3}
    assert glom(target, spec, merge_spec=lambda pair: pair['old'] + pair['new']) == {'same': 3}
    assert glom(target, Dict(spec, key_conflict='strict',
                             conflict_merge=sum)) == {'same': 3}


def test_skipped_fields_do_not_participate_in_conflicts():
    target = _same_key_target()
    calls = []

    def skipped(t):
        calls.append('skipped')
        return SKIP

    def kept(t):
        calls.append('kept')
        return t['b']

    spec = {Spec('key_a'): skipped, Spec('key_b'): kept}
    assert glom(target, spec, key_conflict='strict') == {'same': 2}
    assert calls == ['skipped', 'kept']


def test_nested_mappings_count_conflicts_independently():
    target = {
        'parent_key': 'same',
        'key_a': 'same',
        'key_b': 'same',
        'a': 1,
        'b': 2,
        'c': 3,
    }
    spec = {
        'outer': {Spec('key_a'): 'a'},
        Spec('parent_key'): 'c',
    }

    assert glom(target, spec, key_conflict='strict') == {
        'outer': {'same': 1},
        'same': 3,
    }

    nested_only = {'outer': {Spec('key_a'): 'a', Spec('key_b'): 'b'}}
    with pytest.raises(KeyConflictError) as exc_info:
        glom(target, nested_only, key_conflict='strict')
    err = exc_info.value
    assert "'outer'" in repr(err.first_spec_path)
    assert "'outer'" in repr(err.second_spec_path)
    assert "Spec('key_a')" in repr(err.first_spec_path)
    assert "Spec('key_b')" in repr(err.second_spec_path)


def test_custom_mapping_type_and_glomit_protocol():
    target = _same_key_target()

    class MyDict(dict):
        pass

    spec = MyDict({Spec('key_a'): 'a', Spec('key_b'): 'b'})
    result = glom(target, spec, key_conflict='strict',
                  conflict_merge=lambda old, new: old + new)
    assert type(result) is MyDict
    assert result == {'same': 3}

    class CustomMapping(dict):
        def glomit(self, target, scope):
            return {'custom': target['a']}

    custom_spec = CustomMapping({Spec('key_a'): 'a', Spec('key_b'): 'b'})
    assert glom(target, custom_spec, key_conflict='strict') == {'custom': 1}


def test_key_and_value_failures_are_distinct():
    target = _same_key_target()
    calls = []

    def bad_key(t):
        calls.append('bad_key')
        raise ValueError('bad key')

    def would_be_value(t):
        calls.append('would_be_value')
        return 1

    with pytest.raises(KeySpecError) as exc_info:
        glom(target, {Spec(bad_key): would_be_value}, key_conflict='strict')
    assert not isinstance(exc_info.value, ValueSpecError)
    assert calls == ['bad_key']

    with pytest.raises(ValueSpecError) as exc_info:
        glom(target, {Spec('key_a'): bad_key}, key_conflict='strict')
    assert not isinstance(exc_info.value, KeySpecError)

    with pytest.raises(ConflictMergeError):
        glom(target, {Spec('key_a'): 'a', Spec('key_b'): 'b'},
             key_conflict='strict',
             conflict_merge=lambda old, new: 1 / 0)


def test_conflict_error_does_not_mutate_input():
    target = {'items': [{'key': 'same', 'value': 1},
                        {'key': 'same', 'value': 2}]}
    original = {'items': [dict(item) for item in target['items']]}
    spec = {'projected': ('items', [{'k': Spec('key'), 'v': 'value'}])}

    assert glom(target, spec) == {
        'projected': [{'k': 'same', 'v': 1}, {'k': 'same', 'v': 2}]}
    assert target == original

    conflict_target = {'left': 'same', 'right': 'same', 'a': 1, 'b': 2}
    conflict_original = dict(conflict_target)
    with pytest.raises(KeyConflictError):
        glom(conflict_target, {Spec('left'): 'a', Spec('right'): 'b'},
             key_conflict='strict')
    assert conflict_target == conflict_original
