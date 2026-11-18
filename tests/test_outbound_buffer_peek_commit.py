"""
Tests for the non-destructive peek/commit interface to the outbound data
buffer (``peek_data_to_send`` / ``commit_sent_data``).
"""
from __future__ import annotations

import pytest

import h2.config
import h2.connection
import h2.exceptions


@pytest.fixture
def client():
    c = h2.connection.H2Connection(
        config=h2.config.H2Configuration(client_side=True),
    )
    c.initiate_connection()
    return c


class TestPeekDataToSend:
    def test_peek_does_not_consume(self, client):
        view = client.peek_data_to_send()
        assert view
        # The buffer is untouched: a destructive read returns the same data.
        assert client.data_to_send() == view
        assert client.data_to_send() == b""

    def test_peek_with_amount_returns_prefix(self, client):
        view = client.peek_data_to_send(5)
        assert len(view) == 5
        assert client.data_to_send().startswith(view)

    def test_repeated_peeks_are_identical(self, client):
        assert client.peek_data_to_send() == client.peek_data_to_send()

    def test_peek_negative_amount_rejected(self, client):
        with pytest.raises(ValueError, match="negative"):
            client.peek_data_to_send(-1)

    def test_peek_does_not_change_stream_state(self, client):
        client.send_headers(1, [(":method", "GET"), (":scheme", "https"),
                                (":authority", "example.com"), (":path", "/")])
        stream_state_before = client.streams[1].state_machine.state
        window_before = client.outbound_flow_control_window
        client.peek_data_to_send()
        assert client.streams[1].state_machine.state is stream_state_before
        assert client.outbound_flow_control_window == window_before


class TestCommitSentData:
    def test_commit_partial_prefix(self, client):
        view = client.peek_data_to_send()
        client.commit_sent_data(10)
        assert client.data_to_send() == view[10:]

    def test_commit_everything(self, client):
        view = client.peek_data_to_send()
        client.commit_sent_data(len(view))
        assert client.data_to_send() == b""

    def test_commit_zero_bytes(self, client):
        view = client.peek_data_to_send()
        client.commit_sent_data(0)
        assert client.data_to_send() == view

    def test_commit_without_peek_rejected(self, client):
        with pytest.raises(h2.exceptions.StaleOutboundViewError):
            client.commit_sent_data(1)

    def test_commit_more_than_peeked_rejected(self, client):
        client.peek_data_to_send(5)
        with pytest.raises(h2.exceptions.InvalidCommitLengthError):
            client.commit_sent_data(6)

    def test_commit_negative_rejected(self, client):
        client.peek_data_to_send()
        with pytest.raises(h2.exceptions.InvalidCommitLengthError):
            client.commit_sent_data(-1)

    def test_commit_length_error_is_value_error(self, client):
        client.peek_data_to_send()
        with pytest.raises(ValueError):
            client.commit_sent_data(-1)

    def test_double_commit_requires_new_peek(self, client):
        view = client.peek_data_to_send()
        client.commit_sent_data(5)
        with pytest.raises(h2.exceptions.StaleOutboundViewError):
            client.commit_sent_data(5)
        # A fresh peek allows committing the remainder.
        assert client.peek_data_to_send() == view[5:]
        client.commit_sent_data(len(view) - 5)
        assert client.data_to_send() == b""

    def test_partial_peek_then_commit(self, client):
        full = client.peek_data_to_send()
        prefix = client.peek_data_to_send(10)
        client.commit_sent_data(len(prefix))
        assert client.data_to_send() == full[10:]


class TestStaleViews:
    def test_new_frames_invalidate_view_and_are_not_lost(self, client):
        view = client.peek_data_to_send()
        # A control frame generated between peek and commit.
        client.ping(b"12345678")
        with pytest.raises(h2.exceptions.StaleOutboundViewError):
            client.commit_sent_data(len(view))
        # The new frame was appended, not lost: a fresh peek sees both.
        new_view = client.peek_data_to_send()
        assert new_view.startswith(view)
        assert len(new_view) == len(view) + 17  # 9-byte header + 8-byte ping
        client.commit_sent_data(len(new_view))
        assert client.data_to_send() == b""

    def test_data_to_send_invalidates_view(self, client):
        client.peek_data_to_send()
        client.data_to_send(5)
        with pytest.raises(h2.exceptions.StaleOutboundViewError):
            client.commit_sent_data(1)

    def test_clear_buffer_invalidates_view(self, client):
        client.peek_data_to_send()
        client.clear_outbound_data_buffer()
        with pytest.raises(h2.exceptions.StaleOutboundViewError):
            client.commit_sent_data(0)

    def test_fresh_peek_after_mutation_is_committable(self, client):
        client.peek_data_to_send()
        client.ping(b"12345678")
        view = client.peek_data_to_send()
        client.commit_sent_data(len(view))
        assert client.data_to_send() == b""


class TestMixedConsumption:
    def test_data_to_send_still_works(self, client):
        data = client.data_to_send()
        assert data
        assert client.data_to_send() == b""

    def test_commit_then_data_to_send(self, client):
        view = client.peek_data_to_send()
        client.commit_sent_data(10)
        assert client.data_to_send() == view[10:]

    def test_data_to_send_then_peek_commit(self, client):
        full = client.data_to_send(5)
        view = client.peek_data_to_send()
        assert not view.startswith(full) or len(full) == 0
        client.commit_sent_data(len(view))
        assert client.data_to_send() == b""
