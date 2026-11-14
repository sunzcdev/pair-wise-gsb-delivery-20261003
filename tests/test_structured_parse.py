"""Tests for the structured-path event interface:
ijson.structured_parse and tuple/list paths in items/kvitems"""

from decimal import Decimal

import pytest

import ijson


DOC = b'{"array": [1, 2], "map": {"key": "value"}, "": {"0": null}, "0.1": [[1.5]]}'
DOC_EVENTS = [
    ((), 'start_map', None),
    ((), 'map_key', 'array'),
    (('array',), 'start_array', None),
    (('array', 0), 'number', 1),
    (('array', 1), 'number', 2),
    (('array',), 'end_array', None),
    ((), 'map_key', 'map'),
    (('map',), 'start_map', None),
    (('map',), 'map_key', 'key'),
    (('map', 'key'), 'string', 'value'),
    (('map',), 'end_map', None),
    ((), 'map_key', ''),
    (('',), 'start_map', None),
    (('',), 'map_key', '0'),
    (('', '0'), 'null', None),
    (('',), 'end_map', None),
    ((), 'map_key', '0.1'),
    (('0.1',), 'start_array', None),
    (('0.1', 0), 'start_array', None),
    (('0.1', 0, 0), 'number', Decimal('1.5')),
    (('0.1', 0), 'end_array', None),
    (('0.1',), 'end_array', None),
    ((), 'end_map', None),
]


def test_structured_parse(adaptor):
    assert DOC_EVENTS == adaptor.structured_parse(DOC)


def test_structured_parse_root_scalar(adaptor):
    assert [((), 'number', 0)] == adaptor.structured_parse(b'0')


def test_structured_parse_nested_arrays(adaptor):
    expected = [
        ((), 'start_array', None),
        ((0,), 'start_array', None),
        ((0, 0), 'number', 1),
        ((0, 1), 'start_array', None),
        ((0, 1, 0), 'number', 2),
        ((0, 1), 'end_array', None),
        ((0,), 'end_array', None),
        ((1,), 'number', 3),
        ((), 'end_array', None),
    ]
    assert expected == adaptor.structured_parse(b'[[1, [2]], 3]')


def test_structured_parse_segments_are_typed(adaptor):
    """Array indices are ints, object keys are always strings"""
    events = adaptor.structured_parse(DOC)
    for path, event, value in events:
        assert isinstance(path, tuple)
        for segment in path:
            assert type(segment) in (str, int)
    # numeric-looking keys stay strings, distinct from array indices
    assert (('', '0'), 'null', None) in events
    assert (('', 0), 'null', None) not in events
    # keys containing dots are a single segment
    assert (('0.1',), 'start_array', None) in events
    assert (('0', '1'), 'start_array', None) not in events


def test_structured_parse_empty_key_paths(adaptor):
    events = adaptor.structured_parse(b'{"": 1}')
    assert [
        ((), 'start_map', None),
        ((), 'map_key', ''),
        (('',), 'number', 1),
        ((), 'end_map', None),
    ] == events


def test_structured_parse_container_event_positions(adaptor):
    """start/end and map_key events of a container all carry its own path"""
    events = adaptor.structured_parse(b'{"a": {"b": 1}}')
    assert (('a',), 'start_map', None) in events
    assert (('a',), 'map_key', 'b') in events
    assert (('a',), 'end_map', None) in events


def test_structured_parse_from_basic_parse_events(backend):
    """An iterable of basic_parse events works as source"""
    basic_events = list(backend.basic_parse(b'[1, 2]'))
    expected = [
        ((), 'start_array', None),
        ((0,), 'number', 1),
        ((1,), 'number', 2),
        ((), 'end_array', None),
    ]
    assert expected == list(backend.structured_parse(basic_events))


def test_structured_parse_multiple_values(adaptor):
    if not adaptor.backend.capabilities.multiple_values:
        pytest.skip("backend doesn't support multiple values")
    expected = [
        ((), 'number', 1),
        ((), 'number', 2),
    ]
    assert expected == adaptor.structured_parse(b'1 2', multiple_values=True)


def test_items_with_structured_path(adaptor):
    json = b'{"a": [{"b": 1}, {"b": 2}]}'
    assert [1] == adaptor.items(json, ('a', 0, 'b'))
    assert [2] == adaptor.items(json, ('a', 1, 'b'))
    assert [{'b': 1}] == adaptor.items(json, ('a', 0))
    assert [{'b': 2}] == adaptor.items(json, ('a', 1))


def test_items_with_structured_path_exact_matching(adaptor):
    """Structured paths match exactly, unlike dotted string prefixes"""
    json = b'{"0.1": 0, "0": {"1": 1}}'
    assert [0] == adaptor.items(json, ('0.1',))
    assert [1] == adaptor.items(json, ('0', '1'))
    json = b'{"abc.def": 0}'
    assert [0] == adaptor.items(json, ('abc.def',))
    assert [] == adaptor.items(json, ('abc', 'def'))


def test_items_with_structured_path_real_item_key(adaptor):
    """A real 'item' key is told apart from array positions"""
    json = b'{"item": [1, 2]}'
    assert [1] == adaptor.items(json, ('item', 0))
    assert [2] == adaptor.items(json, ('item', 1))
    assert [] == adaptor.items(json, ('item', 'item'))
    # the classic string prefix cannot make this distinction
    assert [1, 2] == adaptor.items(json, 'item.item')


def test_items_with_structured_path_empty_key(adaptor):
    json = b'{"a": {"": {"b": 1, "c": 2}}}'
    assert [{'b': 1, 'c': 2}] == adaptor.items(json, ('a', ''))


def test_items_with_structured_path_root(adaptor):
    assert [[1, 2]] == adaptor.items(b'[1, 2]', ())
    assert [0] == adaptor.items(b'0', ())


def test_items_with_list_path(adaptor):
    """Lists are accepted as structured paths too"""
    json = b'{"a": [{"b": 1}, {"b": 2}]}'
    assert [2] == adaptor.items(json, ['a', 1, 'b'])


def test_items_with_structured_path_map_type(adaptor):
    import collections
    obj = adaptor.items(b'{"a": {"b": 1}}', ('a',), map_type=collections.OrderedDict)[0]
    assert isinstance(obj, collections.OrderedDict)


def test_kvitems_with_structured_path(adaptor):
    json = b'{"a": {"b": 1, "c": 2}}'
    assert [('b', 1), ('c', 2)] == adaptor.kvitems(json, ('a',))


def test_kvitems_with_structured_path_array_index(adaptor):
    json = b'[{"x": 1}, {"y": 2}]'
    assert [('x', 1)] == adaptor.kvitems(json, (0,))
    assert [('y', 2)] == adaptor.kvitems(json, (1,))


def test_kvitems_with_structured_path_exact_matching(adaptor):
    json = b'{"a.b": {"c": 1}, "a": {"b": {"c": 2}}}'
    assert [('c', 1)] == adaptor.kvitems(json, ('a.b',))
    assert [('c', 2)] == adaptor.kvitems(json, ('a', 'b'))


def test_kvitems_with_structured_path_root(adaptor):
    assert [('a', 1)] == adaptor.kvitems(b'{"a": 1}', ())


def test_top_level_api():
    for name in ('structured_parse', 'structured_parse_coro', 'structured_parse_async'):
        assert hasattr(ijson, name)


def test_backend_api(backend):
    for suffix in ('', '_basecoro', '_coro', '_gen', '_async'):
        assert hasattr(backend, 'structured_parse' + suffix)
