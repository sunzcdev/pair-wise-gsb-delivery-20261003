import pytest

from glom import (glom,
                  T,
                  DeepMerge,
                  deep_merge,
                  DeepMergeConflictError,
                  DeepMergeCycleError,
                  DeepMergeError,
                  DeepMergeKeyError,
                  DeepMergeTypeError)


def test_deep_merge_mappings_and_scalars():
    docs = [{'a': {'b': 1, 'keep': 'first'}},
            {'a': {'c': 2, 'keep': 'last'}}]

    assert deep_merge(docs) == {
        'a': {'b': 1, 'keep': 'last', 'c': 2}}
    assert deep_merge(docs, conflict='first') == {
        'a': {'b': 1, 'keep': 'first', 'c': 2}}
    assert glom(docs, DeepMerge(conflict='last')) == {
        'a': {'b': 1, 'keep': 'last', 'c': 2}}

    with pytest.raises(DeepMergeConflictError) as exc_info:
        deep_merge(docs, conflict='error')
    assert exc_info.value.path == '$.a.keep'
    assert '$.a.keep' in str(exc_info.value)


def test_deep_merge_maps_into_new_order():
    docs = [{'a': 1, 'b': {'x': 1}},
            {'a': 2, 'c': 3, 'b': {'y': 2}}]

    result = deep_merge(docs)

    assert list(result) == ['a', 'b', 'c']
    assert result == {'a': 2, 'b': {'x': 1, 'y': 2}, 'c': 3}


def test_deep_merge_list_strategies():
    docs = [{'items': [1, 2]}, {'items': [2, 3]}]

    assert deep_merge(docs, lists='replace') == {'items': [2, 3]}
    assert deep_merge(docs, lists='concat') == {'items': [1, 2, 2, 3]}
    with pytest.raises(DeepMergeError, match=r'\$\.items'):
        deep_merge(docs)


def test_deep_merge_keyed_lists():
    docs = [
        {'items': [
            {'id': 'b', 'value': 1, 'nested': {'first': True}},
            {'id': 'a', 'value': 2},
        ]},
        {'items': [
            {'id': 'a', 'value': 20, 'nested': {'second': True}},
            {'id': 'c', 'value': 3},
        ]},
    ]

    result = deep_merge(docs, lists='by_key', key='id')

    assert result == {'items': [
        {'id': 'b', 'value': 1, 'nested': {'first': True}},
        {'id': 'a', 'value': 20, 'nested': {'second': True}},
        {'id': 'c', 'value': 3},
    ]}
    assert [item['id'] for item in result['items']] == ['b', 'a', 'c']


def test_keyed_list_duplicate_policies():
    docs = [{'items': [
        {'id': 1, 'first': True},
        {'id': 1, 'second': True},
    ]}]

    assert deep_merge(docs, lists='by_key', key='id') == {
        'items': [{'id': 1, 'first': True, 'second': True}]}
    assert deep_merge(
        docs, lists='by_key', key='id', duplicate_key='first') == {
            'items': [{'id': 1, 'first': True}]}
    assert deep_merge(
        docs, lists='by_key', key='id', duplicate_key='last') == {
            'items': [{'id': 1, 'second': True}]}
    with pytest.raises(DeepMergeKeyError):
        deep_merge(
            docs, lists='by_key', key='id', duplicate_key='error')


def test_keyed_list_missing_key_policies():
    docs = [{'items': [
        {'id': 1},
        {'value': 'unkeyed'},
    ]}]

    assert deep_merge(
        docs, lists='by_key', key='id', missing_key='append') == {
            'items': [{'id': 1}, {'value': 'unkeyed'}]}
    assert deep_merge(
        docs, lists='by_key', key='id', missing_key='skip') == {
            'items': [{'id': 1}]}
    with pytest.raises(DeepMergeKeyError, match=r'\$\.items\[1\]'):
        deep_merge(docs, lists='by_key', key='id')


def test_type_changes_are_not_coerced():
    with pytest.raises(DeepMergeTypeError) as exc_info:
        deep_merge([{'value': [1, 2]}, {'value': {'a': 1}}],
                   lists='replace')
    assert exc_info.value.path == '$.value'
    assert '$.value' in str(exc_info.value)

    with pytest.raises(DeepMergeTypeError):
        deep_merge([{'value': [1]}, {'value': 'scalar'}], lists='replace')


def test_deep_merge_does_not_share_input_containers():
    first = {'a': {'items': [{'x': 1}]}}
    second = {'a': {'items': [{'y': 2}]}}

    result = deep_merge([first, second], lists='concat')
    result['a']['items'][0]['x'] = 100
    result['a']['items'].append('new')
    result['a']['new'] = True

    assert first == {'a': {'items': [{'x': 1}]}}
    assert second == {'a': {'items': [{'y': 2}]}}


def test_deep_merge_cycle_detection_has_path():
    cyclic = {}
    cyclic['nested'] = cyclic

    with pytest.raises(DeepMergeCycleError) as exc_info:
        deep_merge([cyclic], lists='replace')

    assert exc_info.value.path == '$.nested'
    assert '$.nested' in str(exc_info.value)
    assert 'already seen at $' in str(exc_info.value)


def test_deep_merge_cycle_in_keyed_list():
    item = {'id': 1}
    item['self'] = item
    document = {'items': [item]}

    with pytest.raises(DeepMergeCycleError) as exc_info:
        deep_merge([document], lists='by_key', key='id')

    assert exc_info.value.path == '$.items[0].self'


def test_deep_merge_subspec_and_positional_docs():
    one = {'a': {'x': 1}}
    two = {'a': {'y': 2}}

    assert deep_merge({'docs': [one, two]}, spec='docs') == {
        'a': {'x': 1, 'y': 2}}
    assert deep_merge(one, two) == {'a': {'x': 1, 'y': 2}}
    assert glom([one, two], DeepMerge(T)) == {
        'a': {'x': 1, 'y': 2}}


def test_deep_merge_validation():
    with pytest.raises(ValueError):
        DeepMerge(lists='by_key')
    with pytest.raises(ValueError):
        DeepMerge(key='id')
    with pytest.raises(ValueError):
        DeepMerge(conflict='unknown')
    with pytest.raises(ValueError):
        DeepMerge(lists='unknown')
    with pytest.raises(TypeError):
        DeepMerge(nonexistent=True)
    with pytest.raises(DeepMergeError):
        deep_merge([{'a': 1}, 2])
