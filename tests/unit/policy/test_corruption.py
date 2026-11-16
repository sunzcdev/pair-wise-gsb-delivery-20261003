from logging import getLogger
from pickle import PickleError

import pytest

from requests_cache.policy import (
    CorruptionPolicy,
    CorruptEntryError,
    get_exception_category,
    notify_corruption,
)

logger = getLogger(__name__)


@pytest.mark.parametrize(
    'value, expected',
    [
        (None, CorruptionPolicy.REPORT),
        ('report', CorruptionPolicy.REPORT),
        ('REPORT', CorruptionPolicy.REPORT),
        ('report-only', CorruptionPolicy.REPORT),
        ('delete', CorruptionPolicy.DELETE),
        ('delete_and_refetch', CorruptionPolicy.DELETE),
        ('refetch', CorruptionPolicy.DELETE),
        ('raise', CorruptionPolicy.RAISE),
        ('strict', CorruptionPolicy.RAISE),
        (CorruptionPolicy.DELETE, CorruptionPolicy.DELETE),
    ],
)
def test_corruption_policy_parse(value, expected):
    assert CorruptionPolicy.parse(value) == expected


def test_corruption_policy_parse__invalid():
    with pytest.raises(ValueError, match='Invalid corruption policy'):
        CorruptionPolicy.parse('explode')


def test_corruption_policy_str():
    assert str(CorruptionPolicy.REPORT) == 'report'
    assert CorruptionPolicy('delete') is CorruptionPolicy.DELETE


def test_corrupt_entry_error__fields():
    error = ValueError('garbage')
    event = CorruptEntryError('key_1', 'deserialize', error, policy=CorruptionPolicy.REPORT)

    assert event.key == 'key_1'
    assert event.stage == 'deserialize'
    assert event.error is error
    assert event.exception is error
    assert event.exception_type is ValueError
    assert event.exception_name == 'ValueError'
    assert event.exception_category == 'value'
    assert event.policy == CorruptionPolicy.REPORT

    summary = event.as_dict()
    assert summary == {
        'key': 'key_1',
        'stage': 'deserialize',
        'exception_type': 'ValueError',
        'exception_category': 'value',
        'message': 'garbage',
    }

    # The message identifies the key, stage, and error type
    assert 'key_1' in str(event)
    assert 'deserialize' in str(event)
    assert 'ValueError' in str(event)


@pytest.mark.parametrize(
    'error, expected',
    [
        (PickleError('x'), 'pickle'),
        (EOFError('x'), 'decode'),
        (UnicodeDecodeError('utf-8', b'\\xff', 0, 1, 'bad'), 'decode'),
        (ImportError('x'), 'import'),
        (AttributeError('x'), 'type'),
        (TypeError('x'), 'type'),
        (ValueError('x'), 'value'),
        (RuntimeError('x'), 'other'),
    ],
)
def test_get_exception_category(error, expected):
    assert get_exception_category(error) == expected


def test_notify_corruption__no_callback():
    # Should not raise when there's no callback
    notify_corruption(None, CorruptEntryError('key_1', 'deserialize', ValueError()), logger)


def test_notify_corruption__callback_error_is_swallowed(caplog):
    events = []

    def callback(event):
        events.append(event)
        raise RuntimeError('callback exploded')

    event = CorruptEntryError('key_1', 'deserialize', ValueError('garbage'))
    notify_corruption(callback, event, logger)

    # The callback error is logged, but does not propagate
    assert len(events) == 1
    assert 'stage=callback' in caplog.text
    assert 'RuntimeError' in caplog.text


def test_corrupt_entry_error__is_exception():
    """Sanity check so callers can catch CorruptEntryError like any other exception"""
    event = CorruptEntryError('key_1', 'deserialize', ValueError())
    assert isinstance(event, Exception)
    try:
        raise event from event.error
    except CorruptEntryError as e:
        assert isinstance(e.__cause__, ValueError)
