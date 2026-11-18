"""
Tests for the inspectable GOAWAY drain view exposed on H2Connection.

The view lets the application determine which client-side requests may not
have been processed by the remote peer after a GOAWAY frame is received.
h2 itself never retries requests: these tests only validate the
classification and its live updates.
"""
from __future__ import annotations

import pytest

import h2.config
import h2.connection
import h2.events
import h2.exceptions

example_request_headers = [
    (":authority", "example.com"),
    (":path", "/"),
    (":scheme", "https"),
    (":method", "GET"),
]
example_response_headers = [
    (":status", "200"),
    ("server", "fake-serv/0.1.0"),
]
example_informational_headers = [
    (":status", "100"),
]
server_config = h2.config.H2Configuration(client_side=False)


class TestGoAwayDrainViewBasics:
    def test_view_is_empty_before_goaway(self, frame_factory) -> None:
        """
        Before any GOAWAY frame has been received, the drain view reports no
        GOAWAY and contains no entries, even though streams exist.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, example_request_headers, end_stream=True)

        view = c.goaway_drain_view
        assert not view.received
        assert view.last_stream_id is None
        assert view.entries() == ()
        assert view.resubmittable_streams == ()
        assert view.processed_streams == ()
        assert view.unknown_streams == ()
        assert view.get(1) is None
        assert len(view) == 0
        assert list(view) == []

    def test_goaway_populates_view_for_outbound_streams(
        self, frame_factory,
    ) -> None:
        """
        Outbound streams are split into resubmittable / processed / unknown
        based on last_stream_id and whether a final response was seen.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, example_request_headers, end_stream=True)
        c.send_headers(3, example_request_headers, end_stream=True)
        c.send_headers(5, example_request_headers, end_stream=True)

        # Stream 1 gets a final response; streams 3 and 5 do not.
        f = frame_factory.build_headers_frame(
            example_response_headers,
            flags=["END_STREAM"],
            stream_id=1,
        )
        c.receive_data(f.serialize())

        c.receive_data(
            frame_factory.build_goaway_frame(last_stream_id=3).serialize(),
        )

        view = c.goaway_drain_view
        assert view.received
        assert view.last_stream_id == 3

        entries = {entry.stream_id: entry for entry in view.entries()}
        assert set(entries) == {1, 3, 5}

        assert entries[1].direction is h2.events.StreamDirection.OUTBOUND
        assert entries[1].state is h2.events.GoAwayStreamState.PROCESSED
        assert entries[1].response_received is True
        assert entries[1].closed is True

        assert entries[3].state is h2.events.GoAwayStreamState.UNKNOWN
        assert entries[3].response_received is False

        assert entries[5].state is h2.events.GoAwayStreamState.RESUBMITTABLE
        assert entries[5].response_received is False

        assert view.resubmittable_streams == (5,)
        assert view.processed_streams == (1,)
        assert view.unknown_streams == (3,)
        assert view.get(3).state is h2.events.GoAwayStreamState.UNKNOWN

    def test_view_is_live_and_not_a_snapshot(
        self, frame_factory,
    ) -> None:
        """
        The same view object reflects frames received after it was obtained.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, example_request_headers, end_stream=True)
        view = c.goaway_drain_view

        c.receive_data(
            frame_factory.build_goaway_frame(last_stream_id=1).serialize(),
        )
        assert view.received
        assert view.unknown_streams == (1,)

        # A final response arriving afterwards promotes the stream to
        # processed without any new GOAWAY frame.
        f = frame_factory.build_headers_frame(
            example_response_headers,
            flags=["END_STREAM"],
            stream_id=1,
        )
        c.receive_data(f.serialize())
        assert view.processed_streams == (1,)
        assert view.unknown_streams == ()


class TestGoAwayStreamClassification:
    def test_informational_response_does_not_count_as_processed(
        self, frame_factory,
    ) -> None:
        """
        A 1xx response alone leaves an outbound stream unknown: the remote
        peer may never have produced a final response.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, example_request_headers)

        f = frame_factory.build_headers_frame(
            example_informational_headers, stream_id=1,
        )
        c.receive_data(f.serialize())
        c.receive_data(
            frame_factory.build_goaway_frame(last_stream_id=1).serialize(),
        )

        entry = c.goaway_drain_view.get(1)
        assert entry.state is h2.events.GoAwayStreamState.UNKNOWN
        assert entry.response_received is False
        assert entry.closed is False

    def test_closed_stream_without_response_can_be_resubmittable(
        self, frame_factory,
    ) -> None:
        """
        Classification must not be based on the stream being closed. A
        closed outbound stream above last_stream_id is resubmittable.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, example_request_headers, end_stream=True)
        c.send_headers(3, example_request_headers, end_stream=True)

        # Reset stream 3 locally: it is closed, but no response was seen.
        c.reset_stream(3)
        # Force the closed stream out of the live streams dict.
        _ = c.open_outbound_streams

        c.receive_data(
            frame_factory.build_goaway_frame(last_stream_id=1).serialize(),
        )

        entries = {entry.stream_id: entry for entry in c.goaway_drain_view}
        assert entries[3].closed is True
        assert entries[3].response_received is False
        assert entries[3].state is h2.events.GoAwayStreamState.RESUBMITTABLE

    def test_remote_reset_marks_stream_processed(
        self, frame_factory,
    ) -> None:
        """
        A RST_STREAM from the remote peer is evidence it handled the stream,
        so an outbound stream at or below last_stream_id is processed.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, example_request_headers, end_stream=True)

        c.receive_data(
            frame_factory.build_rst_stream_frame(stream_id=1).serialize(),
        )
        c.receive_data(
            frame_factory.build_goaway_frame(last_stream_id=1).serialize(),
        )

        entry = c.goaway_drain_view.get(1)
        assert entry.state is h2.events.GoAwayStreamState.PROCESSED
        assert entry.closed is True

    def test_local_reset_above_last_stream_id_is_resubmittable(
        self, frame_factory,
    ) -> None:
        """
        A RST_STREAM we sent ourselves proves nothing about remote
        processing, so a locally reset stream above last_stream_id remains
        resubmittable rather than being marked processed.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, example_request_headers, end_stream=True)
        c.reset_stream(1)

        c.receive_data(
            frame_factory.build_goaway_frame(last_stream_id=0).serialize(),
        )

        entry = c.goaway_drain_view.get(1)
        assert entry.closed is True
        assert entry.response_received is False
        assert entry.state is h2.events.GoAwayStreamState.RESUBMITTABLE

    def test_local_reset_below_last_stream_id_stays_unknown(
        self, frame_factory,
    ) -> None:
        """
        A locally reset stream at or below last_stream_id with no response
        cannot be declared processed, nor proven unprocessed: it stays
        unknown.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, example_request_headers, end_stream=True)
        c.reset_stream(1)

        c.receive_data(
            frame_factory.build_goaway_frame(last_stream_id=1).serialize(),
        )

        entry = c.goaway_drain_view.get(1)
        assert entry.closed is True
        assert entry.state is h2.events.GoAwayStreamState.UNKNOWN

    def test_inbound_streams_are_never_resubmittable(
        self, frame_factory,
    ) -> None:
        """
        Streams initiated by the remote peer cannot be re-submitted by this
        endpoint, so they are always classified as processed.
        """
        c = h2.connection.H2Connection(config=server_config)
        c.initiate_connection()
        c.receive_data(frame_factory.preamble())

        f = frame_factory.build_headers_frame(
            example_request_headers,
            flags=["END_STREAM"],
            stream_id=1,
        )
        c.receive_data(f.serialize())
        c.receive_data(
            frame_factory.build_goaway_frame(last_stream_id=0).serialize(),
        )

        entry = c.goaway_drain_view.get(1)
        assert entry.direction is h2.events.StreamDirection.INBOUND
        assert entry.state is h2.events.GoAwayStreamState.PROCESSED
        assert entry.response_received is False
        assert c.goaway_drain_view.resubmittable_streams == ()
        assert c.goaway_drain_view.unknown_streams == ()

    def test_completed_streams_remain_classified_after_cleanup(
        self, frame_factory,
    ) -> None:
        """
        Streams removed from the live streams dictionary keep being
        classified correctly thanks to the retained drain records.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, example_request_headers, end_stream=True)
        c.send_headers(3, example_request_headers, end_stream=True)

        # Both streams complete with a final response ending the stream.
        for stream_id in (1, 3):
            f = frame_factory.build_headers_frame(
                example_response_headers,
                flags=["END_STREAM"],
                stream_id=stream_id,
            )
            c.receive_data(f.serialize())

        # Trigger removal of every closed stream from c.streams.
        _ = c.open_outbound_streams
        assert not c.streams

        c.receive_data(
            frame_factory.build_goaway_frame(last_stream_id=3).serialize(),
        )

        entries = {entry.stream_id: entry for entry in c.goaway_drain_view}
        assert entries[1].state is h2.events.GoAwayStreamState.PROCESSED
        assert entries[3].state is h2.events.GoAwayStreamState.PROCESSED
        assert entries[3].response_received is True


class TestMultipleGoAwayFrames:
    def test_smaller_last_stream_id_moves_unknown_to_resubmittable(
        self, frame_factory,
    ) -> None:
        """
        A second GOAWAY with a smaller last_stream_id re-classifies the
        affected streams. Unknown streams become resubmittable; they are
        never silently marked processed.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, example_request_headers, end_stream=True)
        c.send_headers(3, example_request_headers, end_stream=True)
        c.send_headers(5, example_request_headers, end_stream=True)

        c.receive_data(
            frame_factory.build_goaway_frame(last_stream_id=5).serialize(),
        )
        view = c.goaway_drain_view
        assert view.last_stream_id == 5
        assert view.unknown_streams == (1, 3, 5)

        c.receive_data(
            frame_factory.build_goaway_frame(last_stream_id=3).serialize(),
        )
        assert view.last_stream_id == 3
        assert view.unknown_streams == (1, 3)
        assert view.resubmittable_streams == (5,)
        assert view.processed_streams == ()

        c.receive_data(
            frame_factory.build_goaway_frame(last_stream_id=1).serialize(),
        )
        assert view.last_stream_id == 1
        assert view.unknown_streams == (1,)
        assert view.resubmittable_streams == (3, 5)
        assert view.processed_streams == ()

    def test_larger_last_stream_id_does_not_widen_boundary(
        self, frame_factory,
    ) -> None:
        """
        A later GOAWAY with a larger last_stream_id must not relax the
        boundary: the smallest value seen stays authoritative.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, example_request_headers, end_stream=True)
        c.send_headers(3, example_request_headers, end_stream=True)

        c.receive_data(
            frame_factory.build_goaway_frame(last_stream_id=1).serialize(),
        )
        c.receive_data(
            frame_factory.build_goaway_frame(last_stream_id=3).serialize(),
        )

        view = c.goaway_drain_view
        assert view.last_stream_id == 1
        assert view.resubmittable_streams == (3,)
        assert view.unknown_streams == (1,)

    def test_processed_stream_never_downgraded_to_unknown(
        self, frame_factory,
    ) -> None:
        """
        Evidence of processing (a final response) is not forgotten when
        further GOAWAY frames arrive.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, example_request_headers, end_stream=True)
        c.send_headers(3, example_request_headers, end_stream=True)

        f = frame_factory.build_headers_frame(
            example_response_headers,
            flags=["END_STREAM"],
            stream_id=1,
        )
        c.receive_data(f.serialize())

        c.receive_data(
            frame_factory.build_goaway_frame(last_stream_id=3).serialize(),
        )
        c.receive_data(
            frame_factory.build_goaway_frame(last_stream_id=0).serialize(),
        )

        view = c.goaway_drain_view
        assert view.get(1).state is h2.events.GoAwayStreamState.PROCESSED
        assert view.get(3).state is h2.events.GoAwayStreamState.RESUBMITTABLE


class TestGoAwayProtocolBehaviour:
    def test_new_local_streams_are_rejected_after_goaway(
        self, frame_factory,
    ) -> None:
        """
        Once a GOAWAY has been received, the protocol state machine prevents
        the creation of further local streams.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, example_request_headers, end_stream=True)

        c.receive_data(
            frame_factory.build_goaway_frame(last_stream_id=1).serialize(),
        )

        with pytest.raises(h2.exceptions.ProtocolError):
            c.send_headers(3, example_request_headers)

    def test_local_data_still_rejected_but_drain_frames_allowed(
        self, frame_factory,
    ) -> None:
        """
        After a GOAWAY, sending request DATA is still forbidden, but the local
        endpoint can still finish housekeeping on existing streams by sending
        RST_STREAM and WINDOW_UPDATE frames.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, example_request_headers)

        c.receive_data(
            frame_factory.build_goaway_frame(last_stream_id=1).serialize(),
        )
        c.clear_outbound_data_buffer()

        with pytest.raises(h2.exceptions.ProtocolError):
            c.send_data(1, b"some data")

        c.reset_stream(1)
        assert c.data_to_send()

        c2 = h2.connection.H2Connection()
        c2.initiate_connection()
        c2.send_headers(1, example_request_headers)
        c2.receive_data(
            frame_factory.build_goaway_frame(last_stream_id=1).serialize(),
        )
        c2.clear_outbound_data_buffer()
        c2.increment_flow_control_window(10, stream_id=1)
        assert c2.data_to_send()

    def test_existing_streams_keep_receiving_frames_after_goaway(
        self, frame_factory,
    ) -> None:
        """
        Legitimate in-flight streams continue to receive their outstanding
        frames after a GOAWAY, and the view updates accordingly.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, example_request_headers, end_stream=True)

        c.receive_data(
            frame_factory.build_goaway_frame(last_stream_id=1).serialize(),
        )
        assert c.goaway_drain_view.unknown_streams == (1,)

        f = frame_factory.build_headers_frame(
            example_response_headers,
            flags=["END_STREAM"],
            stream_id=1,
        )
        events = c.receive_data(f.serialize())
        assert any(
            isinstance(event, h2.events.ResponseReceived) for event in events
        )
        assert any(
            isinstance(event, h2.events.StreamEnded) for event in events
        )
        assert c.goaway_drain_view.processed_streams == (1,)

    def test_new_inbound_stream_rejected_after_goaway(
        self, frame_factory,
    ) -> None:
        """
        After a GOAWAY has been received, HEADERS on an unknown inbound
        stream must not create a new stream: only already existing streams
        keep draining.
        """
        c = h2.connection.H2Connection(config=server_config)
        c.initiate_connection()
        c.receive_data(frame_factory.preamble())
        c.receive_data(
            frame_factory.build_goaway_frame(last_stream_id=0).serialize(),
        )

        f = frame_factory.build_headers_frame(
            example_request_headers, stream_id=1,
        )
        with pytest.raises(h2.exceptions.ProtocolError):
            c.receive_data(f.serialize())
        assert 1 not in c.streams

    def test_data_frames_drain_existing_stream_after_goaway(
        self, frame_factory,
    ) -> None:
        """
        DATA frames for an already accepted inbound stream keep being
        processed after a GOAWAY, up to the stream's normal end.
        """
        c = h2.connection.H2Connection(config=server_config)
        c.initiate_connection()
        c.receive_data(frame_factory.preamble())

        f = frame_factory.build_headers_frame(
            example_request_headers, stream_id=1,
        )
        c.receive_data(f.serialize())
        c.clear_outbound_data_buffer()

        c.receive_data(
            frame_factory.build_goaway_frame(last_stream_id=1).serialize(),
        )

        data_frame = frame_factory.build_data_frame(
            b"hello", flags=["END_STREAM"], stream_id=1,
        )
        events = c.receive_data(data_frame.serialize())
        assert any(
            isinstance(event, h2.events.DataReceived) for event in events
        )
        assert any(
            isinstance(event, h2.events.StreamEnded) for event in events
        )
        # The stream remains classified as a processed inbound stream.
        assert c.goaway_drain_view.get(1).state is (
            h2.events.GoAwayStreamState.PROCESSED
        )

    def test_no_automatic_retry_or_frames_emitted_on_goaway(
        self, frame_factory,
    ) -> None:
        """
        Receiving GOAWAY never causes h2 to re-submit anything: nothing is
        written to the outbound buffer beyond what frame handling requires.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.clear_outbound_data_buffer()
        c.send_headers(1, example_request_headers, end_stream=True)
        c.clear_outbound_data_buffer()

        c.receive_data(
            frame_factory.build_goaway_frame(last_stream_id=0).serialize(),
        )
        assert not c.data_to_send()
        assert c.goaway_drain_view.resubmittable_streams == (1,)

    def test_goaway_with_zero_last_stream_id(
        self, frame_factory,
    ) -> None:
        """
        A last_stream_id of zero means every outbound stream is
        resubmittable when no response has been seen.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, example_request_headers, end_stream=True)
        c.send_headers(3, example_request_headers, end_stream=True)

        c.receive_data(
            frame_factory.build_goaway_frame(last_stream_id=0).serialize(),
        )
        assert c.goaway_drain_view.resubmittable_streams == (1, 3)
        assert c.goaway_drain_view.unknown_streams == ()


class TestGoAwayDrainBookkeeping:
    def test_evicted_records_are_tolerated_by_frames(
        self, frame_factory, monkeypatch,
    ) -> None:
        """
        Once a drain record is evicted by the size limit, further frames for
        that stream must not raise while the bookkeeping helpers run.
        """
        monkeypatch.setattr(
            h2.connection.H2Connection, "MAX_DRAIN_STREAMS", 2,
        )
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, example_request_headers, end_stream=True)
        c.send_headers(3, example_request_headers, end_stream=True)
        # Creating a third stream evicts the record for stream 1.
        c.send_headers(5, example_request_headers, end_stream=True)
        assert 1 not in c._drain_stream_records

        # A final response on the evicted stream updates nothing but must
        # still be processed normally.
        f = frame_factory.build_headers_frame(
            example_response_headers,
            flags=["END_STREAM"],
            stream_id=1,
        )
        events = c.receive_data(f.serialize())
        assert any(
            isinstance(event, h2.events.ResponseReceived) for event in events
        )

        # A remote RST_STREAM on the same evicted stream is likewise
        # tolerated (the stream object is still present, only its record is
        # gone).
        c.receive_data(
            frame_factory.build_rst_stream_frame(stream_id=1).serialize(),
        )

        # Inspecting the view after a GOAWAY walks the live streams, some of
        # which no longer have drain records: those are simply skipped.
        c.receive_data(
            frame_factory.build_goaway_frame(last_stream_id=5).serialize(),
        )
        view_ids = {entry.stream_id for entry in c.goaway_drain_view.entries()}
        assert view_ids == {3, 5}

    def test_cleanup_without_a_record_is_tolerated(
        self, frame_factory, monkeypatch,
    ) -> None:
        """
        Removing a closed stream whose drain record was already evicted does
        not raise, and the view only contains streams that still have
        records.
        """
        monkeypatch.setattr(
            h2.connection.H2Connection, "MAX_DRAIN_STREAMS", 1,
        )
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, example_request_headers, end_stream=True)
        c.send_headers(3, example_request_headers, end_stream=True)

        # Stream 1's record is gone; closing it out and counting open streams
        # triggers the cleanup helper with no record present.
        f = frame_factory.build_headers_frame(
            example_response_headers,
            flags=["END_STREAM"],
            stream_id=1,
        )
        c.receive_data(f.serialize())
        _ = c.open_outbound_streams

        c.receive_data(
            frame_factory.build_goaway_frame(last_stream_id=3).serialize(),
        )
        assert 1 not in {
            entry.stream_id for entry in c.goaway_drain_view.entries()
        }


class TestGoAwayDrainRepr:
    def test_entry_and_view_repr(self, frame_factory) -> None:
        """
        The public drain objects have useful debug representations.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, example_request_headers, end_stream=True)
        c.receive_data(
            frame_factory.build_goaway_frame(last_stream_id=0).serialize(),
        )

        view = c.goaway_drain_view
        entry = view.get(1)
        assert repr(entry) == (
            "<GoAwayStreamEntry stream_id:1, direction:outbound, "
            "state:resubmittable, response_received:False, closed:False>"
        )
        view_repr = repr(view)
        assert "GoAwayDrainView" in view_repr
        assert "last_stream_id:0" in view_repr
        assert "resubmittable:[1]" in view_repr
