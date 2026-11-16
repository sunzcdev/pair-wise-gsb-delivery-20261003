"""Tests for the corruption policy module (no backend required)"""

import logging

import pytest

from requests_cache.policy import CorruptionPolicy, CorruptEntryError, notify_corruption

logger = logging.getLogger(__name__)


@pytest.mark.parametrize(
    'value, expected',
    [
        (None, CorruptionPolicy.REPORT),
        ('report', CorruptionPolicy.REPORT),
        ('report_only', CorruptionPolicy.REPORT),
        ('REPORT', CorruptionPolicy.REPORT),
        ('delete', CorruptionPolicy.DELETE),
        ('delete-and-refetch', CorruptionPolicy.DELETE),
        ('Delete And Refetch', CorruptionPolicy.DELETE),
        ('raise', CorruptionPolicy.RAISE),
        ('strict', CorruptionPolicy.RAISE),
        ('error', CorruptionPolicy.RAISE),
        (CorruptionPolicy.DELETE, CorruptionPolicy.DELETE),
    ],
)
def test_parse_policy(value, expected):
    assert CorruptionPolicy.parse(value) == expected


def test_parse_policy__invalid():
    with pytest.raises(ValueError, match='Invalid corruption policy'):
        CorruptionPolicy.parse('nonsense')


def test_policy_is_str_enum():
    assert CorruptionPolicy.DELETE == 'delete'
    assert str(CorruptionPolicy.RAISE) == 'raise'


def test_corrupt_entry_error():
    cause = ValueError('something broke')
    error = CorruptEntryError('key_1', 'deserialize', cause, policy=CorruptionPolicy.RAISE)

    assert error.key == 'key_1'
    assert error.stage == 'deserialize'
    assert error.error is cause
    assert error.exception_type is ValueError
    assert error.exception_name == 'ValueError'
    assert error.exception_category == 'ValueError'
    assert error.policy == CorruptionPolicy.RAISE
    assert 'key_1' in str(error) and 'deserialize' in str(error)

    assert error.as_dict() == {
        'key': 'key_1',
        'stage': 'deserialize',
        'exception_type': 'ValueError',
        'exception_category': 'ValueError',
        'message': 'something broke',
    }
    assert error['key'] == 'key_1'
    assert error['exception_category'] == 'ValueError'


def test_notify_corruption__no_callback():
    # Should be a no-op, and must not raise
    notify_corruption(None, CorruptEntryError('k', 'deserialize', ValueError()), logger)


def test_notify_corruption__contains_callback_error(caplog):
    """A callback error is logged, not propagated, so it can't trigger recursive recovery"""
    calls = []

    def callback(event):
        calls.append(event)
        raise RuntimeError('callback failed')

    event = CorruptEntryError('key_1', 'deserialize', ValueError())
    notify_corruption(callback, event, logger)

    assert calls == [event]
    assert 'callback failed' in caplog.text
