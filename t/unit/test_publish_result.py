"""Tests for explicit publish outcomes (kombu.publish).

Uses a confirm-channel stand-in that drops the connection at controlled
stages of the write path to observe how each failure is classified.
"""

from __future__ import annotations

from collections import deque
from unittest.mock import patch

import pytest
from amqp.exceptions import MessageNacked, RecoverableConnectionError

import kombu.messaging
from kombu import (Producer, PublishResult, PublishRetryPolicy,
                   PublishState)
from kombu.publish import PublishAttempt


class ConfirmChannelStandin:
    """Confirm-mode channel double.

    Disconnects at controlled stages of the write path, as directed by
    ``plan`` (one behavior per ``basic_publish`` call):

    * ``'ack'``: write the message, broker confirms it.
    * ``'nack'``: write the message, broker rejects it (basic.nack).
    * ``'drop_before_write'``: connection drops before any frame is
      written (reported via ``last_publish_write_completed = False``).
    * ``'drop_after_write'``: message is written, then the connection
      drops before the publisher confirm arrives.
    """

    confirm_publish = True

    def __init__(self, plan=()):
        self.plan = deque(plan)
        self.last_publish_write_completed = None
        self.written = []
        self.publish_calls = 0
        self.prepare_message_calls = 0
        self.events = {'basic_return': set()}

    def prepare_message(self, body, priority=0, content_type=None,
                        content_encoding=None, headers=None,
                        properties=None):
        self.prepare_message_calls += 1
        return {'body': body,
                'headers': headers,
                'properties': properties,
                'priority': priority,
                'content_type': content_type,
                'content_encoding': content_encoding}

    def basic_publish(self, message, exchange='', routing_key='',
                      **kwargs):
        self.publish_calls += 1
        behavior = self.plan.popleft() if self.plan else 'ack'
        if behavior == 'drop_before_write':
            self.last_publish_write_completed = False
            raise RecoverableConnectionError(
                'connection dropped before write')
        self.last_publish_write_completed = True
        self.written.append(message)
        if behavior == 'drop_after_write':
            raise RecoverableConnectionError(
                'connection dropped before confirm')
        if behavior == 'nack':
            raise MessageNacked('rejected by broker')
        return message


class NonConfirmChannelStandin(ConfirmChannelStandin):
    """Channel double without publisher confirms."""

    confirm_publish = False


def make_producer(channel):
    return Producer(channel, auto_declare=False)


class test_PublishRetryPolicy:

    def test_default_is_single_attempt(self):
        policy = PublishRetryPolicy()
        assert policy.max_attempts == 1
        result = PublishResult(message_id='m')
        result.attempts.append(
            PublishAttempt('a1', PublishState.NOT_SENT))
        result.state = PublishState.NOT_SENT
        assert not policy.should_retry(result)

    @pytest.mark.parametrize('max_attempts', [0, -1, True, 1.5, '3'])
    def test_max_attempts_must_be_positive_int(self, max_attempts):
        with pytest.raises(ValueError):
            PublishRetryPolicy(max_attempts=max_attempts)

    def test_retry_states_must_be_publish_states(self):
        with pytest.raises(ValueError):
            PublishRetryPolicy(retry_states={'not_sent'})

    def test_never_disallows_all_retries(self):
        policy = PublishRetryPolicy.never()
        for state in PublishState:
            result = PublishResult(message_id='m', state=state)
            assert not policy.should_retry(result)

    def test_no_duplicates_retries_not_sent_only(self):
        policy = PublishRetryPolicy.no_duplicates(max_attempts=3)
        result = PublishResult(message_id='m', state=PublishState.NOT_SENT)
        assert policy.should_retry(result)
        result.state = PublishState.UNKNOWN
        assert not policy.should_retry(result)
        result.state = PublishState.REJECTED
        assert not policy.should_retry(result)

    def test_at_least_once_opts_into_unknown(self):
        policy = PublishRetryPolicy.at_least_once(max_attempts=2)
        result = PublishResult(message_id='m', state=PublishState.UNKNOWN)
        assert policy.should_retry(result)
        result.state = PublishState.REJECTED
        assert not policy.should_retry(result)

    def test_should_retry_respects_max_attempts(self):
        policy = PublishRetryPolicy.no_duplicates(max_attempts=2)
        result = PublishResult(message_id='m', state=PublishState.NOT_SENT)
        result.attempts.append(
            PublishAttempt('a1', PublishState.NOT_SENT))
        assert policy.should_retry(result)
        result.attempts.append(
            PublishAttempt('a2', PublishState.NOT_SENT))
        assert not policy.should_retry(result)


class test_publish_result_classification:

    def test_confirmed(self):
        channel = ConfirmChannelStandin(['ack'])
        result = make_producer(channel).publish_result({'hello': 'world'})
        assert result.state is PublishState.CONFIRMED
        assert result.confirmed
        assert not (result.rejected or result.unknown or result.not_sent)
        assert result.attempt_count == 1
        assert channel.written

    def test_rejected_is_terminal_by_default(self):
        channel = ConfirmChannelStandin(['nack', 'ack'])
        result = make_producer(channel).publish_result(
            {'hello': 'world'},
            retry_policy=PublishRetryPolicy.at_least_once(max_attempts=3))
        assert result.state is PublishState.REJECTED
        assert result.rejected
        assert not result.unknown
        assert result.attempt_count == 1
        assert isinstance(result.attempts[0].error, MessageNacked)

    def test_rejected_can_be_retried_only_by_declared_policy(self):
        channel = ConfirmChannelStandin(['nack', 'ack'])
        policy = PublishRetryPolicy(
            max_attempts=2, retry_states={PublishState.REJECTED})
        result = make_producer(channel).publish_result(
            {'hello': 'world'}, retry_policy=policy)
        assert result.state is PublishState.CONFIRMED
        assert [a.state for a in result.attempts] == [
            PublishState.REJECTED, PublishState.CONFIRMED]

    def test_drop_before_write_is_not_sent(self):
        channel = ConfirmChannelStandin(['drop_before_write'])
        result = make_producer(channel).publish_result({'hello': 'world'})
        assert result.state is PublishState.NOT_SENT
        assert result.not_sent
        assert not channel.written

    def test_drop_after_write_is_unknown_not_rejected(self):
        channel = ConfirmChannelStandin(['drop_after_write'])
        result = make_producer(channel).publish_result({'hello': 'world'})
        # unknown is neither a rejection nor a confirmation
        assert result.state is PublishState.UNKNOWN
        assert result.unknown
        assert not (result.rejected or result.confirmed)
        assert isinstance(
            result.attempts[0].error, RecoverableConnectionError)
        # the message was written before the connection dropped
        assert len(channel.written) == 1

    def test_no_duplicates_policy_retries_not_sent(self):
        channel = ConfirmChannelStandin(
            ['drop_before_write', 'drop_before_write', 'ack'])
        result = make_producer(channel).publish_result(
            {'hello': 'world'},
            retry_policy=PublishRetryPolicy.no_duplicates(max_attempts=3))
        assert [a.state for a in result.attempts] == [
            PublishState.NOT_SENT,
            PublishState.NOT_SENT,
            PublishState.CONFIRMED,
        ]
        assert result.confirmed

    def test_no_duplicates_policy_never_retries_unknown(self):
        channel = ConfirmChannelStandin(['drop_after_write', 'ack'])
        result = make_producer(channel).publish_result(
            {'hello': 'world'},
            retry_policy=PublishRetryPolicy.no_duplicates(max_attempts=3))
        assert result.state is PublishState.UNKNOWN
        assert result.attempt_count == 1
        assert channel.publish_calls == 1

    def test_at_least_once_retries_unknown_and_may_duplicate(self):
        channel = ConfirmChannelStandin(['drop_after_write', 'ack'])
        result = make_producer(channel).publish_result(
            {'hello': 'world'},
            retry_policy=PublishRetryPolicy.at_least_once(max_attempts=3))
        assert [a.state for a in result.attempts] == [
            PublishState.UNKNOWN, PublishState.CONFIRMED]
        # both attempts were written: the broker may see the message
        # twice, which is why UNKNOWN retries are opt-in only.
        assert len(channel.written) == 2

    def test_message_id_stable_across_attempts(self):
        channel = ConfirmChannelStandin(['drop_after_write', 'ack'])
        result = make_producer(channel).publish_result(
            {'hello': 'world'}, message_id='mid-1',
            retry_policy=PublishRetryPolicy.at_least_once(max_attempts=3))
        assert result.message_id == 'mid-1'
        assert [m['properties']['message_id']
                for m in channel.written] == ['mid-1', 'mid-1']

    def test_attempt_identity_unique_and_visible_to_broker(self):
        channel = ConfirmChannelStandin(['drop_after_write', 'ack'])
        result = make_producer(channel).publish_result(
            {'hello': 'world'},
            retry_policy=PublishRetryPolicy.at_least_once(max_attempts=3))
        attempt_ids = [a.attempt_id for a in result.attempts]
        assert len(set(attempt_ids)) == 2
        assert [m['headers']['x-publish-attempt-id']
                for m in channel.written] == attempt_ids

    def test_serialization_runs_exactly_once_per_call(self):
        channel = ConfirmChannelStandin(
            ['drop_before_write', 'drop_after_write', 'ack'])
        policy = PublishRetryPolicy.at_least_once(max_attempts=3)
        with patch.object(kombu.messaging, 'dumps',
                          wraps=kombu.messaging.dumps) as dumps:
            result = make_producer(channel).publish_result(
                {'hello': 'world'}, retry_policy=policy)
        assert result.confirmed
        assert result.attempt_count == 3
        # agreed serialization count: exactly once per publish call
        assert dumps.call_count == 1
        # transport message preparation still happens per attempt
        assert channel.prepare_message_calls == 3

    def test_non_confirm_channel_clean_write_is_unknown(self):
        channel = NonConfirmChannelStandin(['ack'])
        result = make_producer(channel).publish_result({'hello': 'world'})
        # without publisher confirms a clean return only proves the
        # transport accepted the write; not claimed as confirmed.
        assert result.state is PublishState.UNKNOWN
        assert result.attempts[0].error is None

    def test_default_policy_makes_exactly_one_attempt(self):
        channel = ConfirmChannelStandin(['drop_before_write', 'ack'])
        result = make_producer(channel).publish_result({'hello': 'world'})
        assert result.state is PublishState.NOT_SENT
        assert channel.publish_calls == 1

    def test_generated_message_id(self):
        channel = ConfirmChannelStandin(['ack'])
        result = make_producer(channel).publish_result({'hello': 'world'})
        assert result.message_id
        assert (channel.written[0]['properties']['message_id'] ==
                result.message_id)

    def test_rejects_use_inside_batch(self):
        channel = ConfirmChannelStandin(['ack'])
        producer = make_producer(channel)
        with producer.batch():
            with pytest.raises(RuntimeError):
                producer.publish_result({'hello': 'world'})

    def test_retry_revives_connection_between_attempts(self):
        from kombu import Connection
        from kombu.exceptions import OperationalError

        conn = Connection('memory://')
        conn.connect()
        producer = Producer(conn, auto_declare=False)
        first_channel = producer.channel
        dropped = {'done': False}
        calls = []
        real_publish = first_channel.basic_publish

        def drop_once(message, **kwargs):
            calls.append(message)
            if not dropped['done']:
                dropped['done'] = True
                # channel reports the write never happened
                first_channel.last_publish_write_completed = False
                raise OperationalError('connection dropped before write')
            return real_publish(message, **kwargs)

        first_channel.basic_publish = drop_once
        result = producer.publish_result(
            {'hello': 'world'},
            retry_policy=PublishRetryPolicy.no_duplicates(max_attempts=2))
        # first attempt failed before the write (the channel reported
        # that no frame was written) and was retried; memory transport
        # has no publisher confirms, so the final state is UNKNOWN,
        # not CONFIRMED.
        assert [a.state for a in result.attempts] == [
            PublishState.NOT_SENT, PublishState.UNKNOWN]
        assert len(calls) == 2

    def test_unreported_write_failure_is_conservatively_unknown(self):
        from kombu import Connection
        from kombu.exceptions import OperationalError

        conn = Connection('memory://')
        conn.connect()
        producer = Producer(conn, auto_declare=False)
        first_channel = producer.channel

        def drop(message, **kwargs):
            raise OperationalError('connection dropped mid-publish')

        first_channel.basic_publish = drop
        result = producer.publish_result(
            {'hello': 'world'},
            retry_policy=PublishRetryPolicy.no_duplicates(max_attempts=3))
        # the channel cannot say whether the frame was written, so the
        # outcome is UNKNOWN and the no-duplicates policy stops.
        assert [a.state for a in result.attempts] == [PublishState.UNKNOWN]


class test_publish_result_exports:

    def test_top_level_exports(self):
        import kombu
        assert kombu.PublishResult is PublishResult
        assert kombu.PublishRetryPolicy is PublishRetryPolicy
        assert kombu.PublishState is PublishState
