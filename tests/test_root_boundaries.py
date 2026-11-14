"""Tests for root value completion events."""

import asyncio
import io

import pytest

import ijson
from ijson import common


MULTI_VALUE_JSON = b' 1 []\n{} "x" {"a": [1, {}]} \r\n 2 '

MULTI_VALUE_EVENTS = [
    ('number', 1),
    ('root_value_complete', 0),
    ('start_array', None),
    ('end_array', None),
    ('root_value_complete', 1),
    ('start_map', None),
    ('end_map', None),
    ('root_value_complete', 2),
    ('string', 'x'),
    ('root_value_complete', 3),
    ('start_map', None),
    ('map_key', 'a'),
    ('start_array', None),
    ('number', 1),
    ('start_map', None),
    ('end_map', None),
    ('end_array', None),
    ('end_map', None),
    ('root_value_complete', 4),
    ('number', 2),
    ('root_value_complete', 5),
]


def test_root_boundaries_in_all_input_modes(adaptor):
    if not adaptor.backend.capabilities.multiple_values:
        pytest.skip("backend does not support multiple values")
    assert MULTI_VALUE_EVENTS == adaptor.basic_parse(
        MULTI_VALUE_JSON, multiple_values=True, root_boundaries=True
    )


def test_root_boundaries_are_absent_by_default(adaptor):
    events = adaptor.basic_parse(
        b'{"a": 1}', multiple_values=True
    )
    assert [event for event, _ in events if event == 'root_value_complete'] == []


def test_parse_root_boundaries_use_empty_root_prefix(adaptor):
    if not adaptor.backend.capabilities.multiple_values:
        pytest.skip("backend does not support multiple values")
    events = adaptor.parse(
        b'{"a": {}} [] 1', multiple_values=True, root_boundaries=True
    )
    assert [
        event for event in events
        if event[1] == common.ROOT_VALUE_COMPLETE_EVENT
    ] == [
        ('', common.ROOT_VALUE_COMPLETE_EVENT, 0),
        ('', common.ROOT_VALUE_COMPLETE_EVENT, 1),
        ('', common.ROOT_VALUE_COMPLETE_EVENT, 2),
    ]


def test_truncated_last_root_has_no_completion_event():
    events = []
    with pytest.raises(common.IncompleteJSONError):
        for event in ijson.basic_parse_with_root_boundaries(
            io.BytesIO(b'{"a": 1} {"b":')
        ):
            events.append(event)
    assert events[:5] == [
        ('start_map', None),
        ('map_key', 'a'),
        ('number', 1),
        ('end_map', None),
        ('root_value_complete', 0),
    ]
    assert all(
        event != ('root_value_complete', 1) for event in events
    )


def test_standalone_entry_points_enable_boundaries():
    expected = [('number', 1), ('root_value_complete', 0)]
    assert list(ijson.basic_parse_with_boundaries(io.BytesIO(b'1'))) == expected
    assert list(ijson.basic_parse_with_root_boundaries(io.BytesIO(b'1'))) == expected
    assert list(ijson.parse_with_root_boundaries(io.BytesIO(b'1'))) == [
        ('', 'number', 1),
        ('', 'root_value_complete', 0),
    ]


def test_add_root_boundaries_to_existing_events():
    events = ijson.basic_parse(io.BytesIO(b'1 {}'), multiple_values=True)
    assert list(ijson.add_root_boundaries(events)) == [
        ('number', 1),
        ('root_value_complete', 0),
        ('start_map', None),
        ('end_map', None),
        ('root_value_complete', 1),
    ]


def test_async_flushes_completed_roots_before_error():
    class Reader:
        def __init__(self, data):
            self.data = io.BytesIO(data)

        async def read(self, n=-1):
            return self.data.read(n)

    async def collect():
        events = []
        with pytest.raises(common.IncompleteJSONError):
            async for event in ijson.basic_parse_async(
                Reader(b'1 {"a":'), root_boundaries=True
            ):
                events.append(event)
        return events

    events = asyncio.run(collect())
    assert events[:3] == [
        ('number', 1),
        ('root_value_complete', 0),
        ('start_map', None),
    ]
    assert all(event != ('root_value_complete', 1) for event in events)
