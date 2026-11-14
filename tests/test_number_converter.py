"""Tests for the python backend's number conversion callback."""

import asyncio
import io
from decimal import Decimal

import pytest

from ijson import common, utils
from ijson.backends import python


NUMBERS_JSON = b'[1, 1.00, 1e0, -0.5E+2]'
NUMBER_TOKENS = ['1', '1.00', '1e0', '-0.5E+2']


def _numbers(events):
    return [value for event, value in events if event == 'number']


def test_default_conversion_unchanged():
    numbers = _numbers(python.basic_parse(NUMBERS_JSON))
    assert numbers == [1, Decimal('1.00'), Decimal('1e0'), Decimal('-0.5E+2')]


def test_basic_parse_uses_converter():
    seen = []

    def converter(token):
        seen.append(token)
        return token

    numbers = _numbers(python.basic_parse(NUMBERS_JSON, number_converter=converter))
    assert numbers == NUMBER_TOKENS
    assert seen == NUMBER_TOKENS


def test_parse_and_items_use_converter():
    events = list(python.parse(b'{"a": 1.00, "b": [1e0]}', number_converter=str))
    assert events == [
        ('', 'start_map', None),
        ('', 'map_key', 'a'),
        ('a', 'number', '1.00'),
        ('', 'map_key', 'b'),
        ('b', 'start_array', None),
        ('b.item', 'number', '1e0'),
        ('b', 'end_array', None),
        ('', 'end_map', None),
    ]
    assert list(python.items(
        b'{"a": 1.00, "b": [1e0]}', '', number_converter=str
    )) == [{'a': '1.00', 'b': ['1e0']}]


def test_async_uses_converter():
    class AsyncReader:
        def __init__(self, data):
            self.reader = io.BytesIO(data)

        async def read(self, size=-1):
            return self.reader.read(size)

    async def collect():
        numbers = [
            value
            async for event, value in python.basic_parse_async(
                AsyncReader(NUMBERS_JSON), number_converter=str
            )
            if event == 'number'
        ]
        items = [
            value
            async for value in python.items_async(
                AsyncReader(b'{"a": 1.00, "b": [1e0]}'), '', number_converter=str
            )
        ]
        return numbers, items

    numbers, items = asyncio.run(collect())
    assert numbers == NUMBER_TOKENS
    assert items == [{'a': '1.00', 'b': ['1e0']}]


@pytest.mark.parametrize('argument', ('number_converter', 'number_callback'))
def test_cross_chunk_token_calls_converter_once(argument):
    seen = []

    def converter(token):
        seen.append(token)
        return token

    events = utils.sendable_list()
    coro = python.basic_parse_coro(events, **{argument: converter})
    try:
        for byte in b'[123.456e-7]':
            coro.send(bytes([byte]))
    finally:
        coro.close()

    assert _numbers(events) == ['123.456e-7']
    assert seen == ['123.456e-7']


def test_push_object_paths_use_converter():
    for routine, expected in (
        (python.items_coro, [{'a': '1.00', 'b': ['1e0']}]),
        (python.kvitems_coro, [('a', '1.00'), ('b', ['1e0'])]),
    ):
        events = utils.sendable_list()
        coro = routine(events, '', number_converter=str)
        try:
            for byte in b'{"a": 1.00, "b": [1e0]}':
                coro.send(bytes([byte]))
        finally:
            coro.close()
        assert events == expected


@pytest.mark.parametrize(
    'json', (b'[01]', b'[1e]', b'[1e+]', b'[1.]', b'[.1]', b'[+1]', b'[1.2.3]')
)
def test_invalid_number_does_not_reach_converter(json):
    seen = []
    with pytest.raises(common.JSONError):
        list(python.basic_parse(json, number_converter=seen.append))
    assert seen == []


def test_converter_exception_chain_and_position():
    class ConversionError(ValueError):
        pass

    def converter(token):
        if token == '12':
            raise ConversionError('cannot convert %s' % token)
        return int(token)

    with pytest.raises(common.JSONError) as excinfo:
        list(python.basic_parse(b'[1, 12]', number_converter=converter))

    error = excinfo.value
    assert isinstance(error.__cause__, ConversionError)
    assert error.position == 4
    assert error.token == '12'
    assert 'position 4' in str(error)


def test_use_float_conflict_is_eager():
    with pytest.raises(ValueError, match='use_float'):
        python.basic_parse(b'[1]', use_float=True, number_converter=str)


def test_multiple_converter_options_are_rejected():
    with pytest.raises(ValueError, match='Only one'):
        python.basic_parse(
            b'[1]', number_converter=str, number_callback=str
        )
