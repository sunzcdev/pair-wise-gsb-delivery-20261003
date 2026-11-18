"""
Specific tests for any function that is logically self-contained as part of
events.py.
"""
from __future__ import annotations

import inspect
import sys

import hyperframe.frame
import pytest
from hypothesis import given
from hypothesis.strategies import integers, lists, tuples

import h2.config
import h2.connection
import h2.errors
import h2.events
import h2.settings

# We define a fairly complex Hypothesis strategy here. We want to build a list
# of two tuples of (Setting, value). For Setting we want to make sure we can
# handle settings that the rest of hyper knows nothing about, so we want to
# use integers from 0 to (2**16-1). For values, they're from 0 to (2**32-1).
# Define that strategy here for clarity.
SETTINGS_STRATEGY = lists(
    tuples(
        integers(min_value=0, max_value=2**16-1),
        integers(min_value=0, max_value=2**32-1),
    ),
)


class TestRemoteSettingsChanged:
    """
    Validate the function of the RemoteSettingsChanged event.
    """

    @given(SETTINGS_STRATEGY)
    def test_building_settings_from_scratch(self, settings_list) -> None:
        """
        Missing old settings are defaulted to None.
        """
        settings_dict = dict(settings_list)
        e = h2.events.RemoteSettingsChanged.from_settings(
            old_settings={},
            new_settings=settings_dict,
        )

        for setting, new_value in settings_dict.items():
            assert e.changed_settings[setting].setting == setting
            assert e.changed_settings[setting].original_value is None
            assert e.changed_settings[setting].new_value == new_value

    @given(SETTINGS_STRATEGY, SETTINGS_STRATEGY)
    def test_only_reports_changed_settings(self,
                                           old_settings_list,
                                           new_settings_list) -> None:
        """
        Settings that were not changed are not reported.
        """
        old_settings_dict = dict(old_settings_list)
        new_settings_dict = dict(new_settings_list)
        e = h2.events.RemoteSettingsChanged.from_settings(
            old_settings=old_settings_dict,
            new_settings=new_settings_dict,
        )

        assert len(e.changed_settings) == len(new_settings_dict)
        assert (
            sorted(e.changed_settings.keys()) ==
            sorted(new_settings_dict.keys())
        )

    @given(SETTINGS_STRATEGY, SETTINGS_STRATEGY)
    def test_correctly_reports_changed_settings(self,
                                                old_settings_list,
                                                new_settings_list) -> None:
        """
        Settings that are changed are correctly reported.
        """
        old_settings_dict = dict(old_settings_list)
        new_settings_dict = dict(new_settings_list)
        e = h2.events.RemoteSettingsChanged.from_settings(
            old_settings=old_settings_dict,
            new_settings=new_settings_dict,
        )

        for setting, new_value in new_settings_dict.items():
            original_value = old_settings_dict.get(setting)
            assert e.changed_settings[setting].setting == setting
            assert e.changed_settings[setting].original_value == original_value
            assert e.changed_settings[setting].new_value == new_value


class TestEventReprs:
    """
    Events have useful representations.
    """

    example_request_headers = [
        (":authority", "example.com"),
        (":path", "/"),
        (":scheme", "https"),
        (":method", "GET"),
    ]
    example_informational_headers = [
        (":status", "100"),
        ("server", "fake-serv/0.1.0"),
    ]
    example_response_headers = [
        (":status", "200"),
        ("server", "fake-serv/0.1.0"),
    ]

    def test_requestreceived_repr(self) -> None:
        """
        RequestReceived has a useful debug representation.
        """
        e = h2.events.RequestReceived(
            stream_id=5,
            headers=self.example_request_headers
        )

        assert repr(e) == (
            "<RequestReceived stream_id:5, headers:["
            "(':authority', 'example.com'), "
            "(':path', '/'), "
            "(':scheme', 'https'), "
            "(':method', 'GET')]>"
        )

    def test_responsereceived_repr(self) -> None:
        """
        ResponseReceived has a useful debug representation.
        """
        e = h2.events.ResponseReceived(
            stream_id=500,
            headers=self.example_response_headers,
        )

        assert repr(e) == (
            "<ResponseReceived stream_id:500, headers:["
            "(':status', '200'), "
            "('server', 'fake-serv/0.1.0')]>"
        )

    def test_trailersreceived_repr(self) -> None:
        """
        TrailersReceived has a useful debug representation.
        """
        e = h2.events.TrailersReceived(stream_id=62, headers=self.example_response_headers)

        assert repr(e) == (
            "<TrailersReceived stream_id:62, headers:["
            "(':status', '200'), "
            "('server', 'fake-serv/0.1.0')]>"
        )

    def test_informationalresponsereceived_repr(self) -> None:
        """
        InformationalResponseReceived has a useful debug representation.
        """
        e = h2.events.InformationalResponseReceived(
            stream_id=62,
            headers=self.example_informational_headers,
        )

        assert repr(e) == (
            "<InformationalResponseReceived stream_id:62, headers:["
            "(':status', '100'), "
            "('server', 'fake-serv/0.1.0')]>"
        )

    def test_datareceived_repr(self) -> None:
        """
        DataReceived has a useful debug representation.
        """
        e = h2.events.DataReceived(
            stream_id=888,
            data=b"abcdefghijklmnopqrstuvwxyz",
            flow_controlled_length=88,
        )

        assert repr(e) == (
            "<DataReceived stream_id:888, flow_controlled_length:88, "
            "data:6162636465666768696a6b6c6d6e6f7071727374>"
        )

    def test_windowupdated_repr(self) -> None:
        """
        WindowUpdated has a useful debug representation.
        """
        e = h2.events.WindowUpdated(stream_id=0, delta=2**16)

        assert repr(e) == "<WindowUpdated stream_id:0, delta:65536>"

    def test_remotesettingschanged_repr(self) -> None:
        """
        RemoteSettingsChanged has a useful debug representation.
        """
        e = h2.events.RemoteSettingsChanged()
        e.changed_settings = {
            h2.settings.SettingCodes.INITIAL_WINDOW_SIZE:
                h2.settings.ChangedSetting(
                    h2.settings.SettingCodes.INITIAL_WINDOW_SIZE, 2**16, 2**15,
                ),
        }

        if sys.version_info >= (3, 11):
            assert repr(e) == (
                "<RemoteSettingsChanged changed_settings:{ChangedSetting("
                "setting=4, original_value=65536, "
                "new_value=32768)}>"
            )
        else:
            assert repr(e) == (
                "<RemoteSettingsChanged changed_settings:{ChangedSetting("
                "setting=SettingCodes.INITIAL_WINDOW_SIZE, original_value=65536, "
                "new_value=32768)}>"
            )

    def test_pingreceived_repr(self) -> None:
        """
        PingReceived has a useful debug representation.
        """
        e = h2.events.PingReceived(ping_data=b"abcdefgh")

        assert repr(e) == "<PingReceived ping_data:6162636465666768>"

    def test_pingackreceived_repr(self) -> None:
        """
        PingAckReceived has a useful debug representation.
        """
        e = h2.events.PingAckReceived(ping_data=b"abcdefgh")

        assert repr(e) == "<PingAckReceived ping_data:6162636465666768>"

    def test_streamended_repr(self) -> None:
        """
        StreamEnded has a useful debug representation.
        """
        e = h2.events.StreamEnded(stream_id=99)

        assert repr(e) == "<StreamEnded stream_id:99>"

    def test_streamreset_repr(self) -> None:
        """
        StreamEnded has a useful debug representation.
        """
        e = h2.events.StreamReset(
            stream_id=919,
            error_code=h2.errors.ErrorCodes.ENHANCE_YOUR_CALM,
            remote_reset=False,
        )

        if sys.version_info >= (3, 11):
            assert repr(e) == (
                "<StreamReset stream_id:919, "
                "error_code:11, remote_reset:False>"
            )
        else:
            assert repr(e) == (
                "<StreamReset stream_id:919, "
                "error_code:ErrorCodes.ENHANCE_YOUR_CALM, remote_reset:False>"
            )

    def test_pushedstreamreceived_repr(self) -> None:
        """
        PushedStreamReceived has a useful debug representation.
        """
        e = h2.events.PushedStreamReceived()
        e.pushed_stream_id = 50
        e.parent_stream_id = 11
        e.headers = self.example_request_headers

        assert repr(e) == (
            "<PushedStreamReceived pushed_stream_id:50, parent_stream_id:11, "
            "headers:["
            "(':authority', 'example.com'), "
            "(':path', '/'), "
            "(':scheme', 'https'), "
            "(':method', 'GET')]>"
        )

    def test_settingsacknowledged_repr(self) -> None:
        """
        SettingsAcknowledged has a useful debug representation.
        """
        e = h2.events.SettingsAcknowledged()
        e.changed_settings = {
            h2.settings.SettingCodes.INITIAL_WINDOW_SIZE:
                h2.settings.ChangedSetting(
                    h2.settings.SettingCodes.INITIAL_WINDOW_SIZE, 2**16, 2**15,
                ),
        }

        if sys.version_info >= (3, 11):
            assert repr(e) == (
                "<SettingsAcknowledged changed_settings:{ChangedSetting("
                "setting=4, original_value=65536, "
                "new_value=32768)}>"
            )
        else:
            assert repr(e) == (
                "<SettingsAcknowledged changed_settings:{ChangedSetting("
                "setting=SettingCodes.INITIAL_WINDOW_SIZE, original_value=65536, "
                "new_value=32768)}>"
            )

    def test_priorityupdated_repr(self) -> None:
        """
        PriorityUpdated has a useful debug representation.
        """
        e = h2.events.PriorityUpdated()
        e.stream_id = 87
        e.weight = 32
        e.depends_on = 8
        e.exclusive = True

        assert repr(e) == (
            "<PriorityUpdated stream_id:87, weight:32, depends_on:8, "
            "exclusive:True>"
        )

    @pytest.mark.parametrize(("additional_data", "data_repr"), [
        (None, "None"),
        (b"some data", "736f6d652064617461"),
    ])
    def test_connectionterminated_repr(self, additional_data, data_repr) -> None:
        """
        ConnectionTerminated has a useful debug representation.
        """
        e = h2.events.ConnectionTerminated()
        e.error_code = h2.errors.ErrorCodes.INADEQUATE_SECURITY
        e.last_stream_id = 33
        e.additional_data = additional_data

        if sys.version_info >= (3, 11):
            assert repr(e) == (
                "<ConnectionTerminated error_code:12, "
                f"last_stream_id:33, additional_data:{data_repr}>"
            )
        else:
            assert repr(e) == (
                "<ConnectionTerminated error_code:ErrorCodes.INADEQUATE_SECURITY, "
                f"last_stream_id:33, additional_data:{data_repr}>"
            )

    def test_alternativeserviceavailable_repr(self) -> None:
        """
        AlternativeServiceAvailable has a useful debug representation.
        """
        e = h2.events.AlternativeServiceAvailable()
        e.origin = b"example.com"
        e.field_value = b'h2=":8000"; ma=60'

        assert repr(e) == (
            '<AlternativeServiceAvailable origin:example.com, '
            'field_value:h2=":8000"; ma=60>'
        )

    def test_unknownframereceived_repr(self) -> None:
        """
        UnknownFrameReceived has a useful debug representation.
        """
        e = h2.events.UnknownFrameReceived(frame=hyperframe.frame.Frame(1))
        assert repr(e) == "<UnknownFrameReceived>"


def all_events():
    """
    Generates all the classes (i.e., events) defined in h2.events.
    """
    for _, obj in inspect.getmembers(sys.modules["h2.events"]):

        # We are only interested in objects that are defined in h2.events;
        # objects that are imported from other modules are not of interest.
        if hasattr(obj, "__module__") and (obj.__module__ != "h2.events"):
            continue

        # Not every class in h2.events is an event (the lifecycle observer
        # and its supporting types live here too), so only yield event classes.
        if inspect.isclass(obj) and issubclass(obj, h2.events.Event):
            yield obj


@pytest.mark.parametrize("event", all_events())
def test_all_events_subclass_from_event(event) -> None:
    """
    Every event defined in h2.events subclasses from h2.events.Event.
    """
    assert (event is h2.events.Event) or issubclass(event, h2.events.Event)


class FakeClock:
    """
    A deterministic clock for lifecycle-observer tests.
    """

    def __init__(self) -> None:
        self.now = 100.0

    def __call__(self) -> float:
        return self.now

    def advance(self, amount: float) -> None:
        self.now += amount


class TestStreamLifecycleObserver:
    """
    Tests for the optional per-stream lifecycle observer.
    """

    request_headers = [
        (b":method", b"GET"),
        (b":path", b"/"),
        (b":authority", b"example.com"),
        (b":scheme", b"https"),
    ]

    response_headers = [
        (b":status", b"200"),
        (b"content-length", b"11"),
    ]

    informational_headers = [
        (b":status", b"103"),
        (b"link", b"</style.css>; rel=preload"),
    ]

    def _client(self, clock=None, **kwargs):
        observer = h2.events.StreamLifecycleObserver(clock=clock, **kwargs)
        config = h2.config.H2Configuration(
            client_side=True,
            stream_lifecycle_observer=observer,
        )
        return observer, h2.connection.H2Connection(config=config)

    def _server(self, clock=None, **kwargs):
        observer = h2.events.StreamLifecycleObserver(clock=clock, **kwargs)
        config = h2.config.H2Configuration(
            client_side=False,
            stream_lifecycle_observer=observer,
        )
        return observer, h2.connection.H2Connection(config=config)

    def test_no_observer_by_default(self) -> None:
        """
        When no observer is configured, nothing is recorded and the connection
        works normally.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, self.request_headers)
        assert c.config.stream_lifecycle_observer is None

    def test_local_request_lifecycle_and_completion(self, frame_factory) -> None:
        """
        A simple request/response records initiation, response headers, first
        body, and clean completion.
        """
        clock = FakeClock()
        observer, c = self._client(clock=clock)
        c.initiate_connection()
        c.send_headers(1, self.request_headers, end_stream=True)

        record = observer.get(1)
        assert record is not None
        assert record.origin is h2.events.StreamOrigin.LOCAL
        assert record.is_push is False
        assert record.initiated_at == 100.0
        assert record.outcome is h2.events.StreamOutcome.HALF_CLOSED_LOCAL
        assert record.local_ended
        assert not record.remote_ended

        # Deliver a response with headers, body, and END_STREAM.
        clock.advance(5)
        _, server = self._server()
        server.initiate_connection()
        server.receive_data(c.data_to_send())
        c.receive_data(server.data_to_send())

        clock.advance(2)
        server.send_headers(1, self.response_headers, end_stream=False)
        c.receive_data(server.data_to_send())

        clock.advance(1)
        server.send_data(1, b"hello world")
        c.receive_data(server.data_to_send())

        clock.advance(3)
        server.end_stream(1)
        events = c.receive_data(server.data_to_send())
        assert any(isinstance(e, h2.events.StreamEnded) for e in events)

        record = observer.get(1)
        assert record is not None
        assert record.response_headers_at == 107.0
        assert record.first_body_at == 108.0
        assert record.body_bytes == len(b"hello world")
        assert record.body_chunks == 1
        assert record.flow_controlled_bytes == len(b"hello world")
        assert record.settled_at == 111.0
        assert record.outcome is h2.events.StreamOutcome.COMPLETED
        assert record.timings_complete
        assert record.time_to_response_headers == 7.0
        assert record.time_to_first_body == 8.0
        assert record.total_duration == 11.0

    def test_informational_response_is_not_final(self, frame_factory) -> None:
        """
        An informational response is counted, but does not replace the final
        response headers timestamp.
        """
        clock = FakeClock()
        observer, c = self._client(clock=clock)
        c.initiate_connection()
        c.send_headers(1, self.request_headers)

        _, server = self._server()
        server.initiate_connection()
        server.receive_data(c.data_to_send())
        c.receive_data(server.data_to_send())

        clock.advance(1)
        server.send_headers(1, self.informational_headers)
        c.receive_data(server.data_to_send())

        record = observer.get(1)
        assert record is not None
        assert record.informational_responses == 1
        assert record.response_headers_count == 0
        assert record.response_headers_at is None

        clock.advance(1)
        server.send_headers(1, self.response_headers)
        events = c.receive_data(server.data_to_send())
        assert any(isinstance(e, h2.events.ResponseReceived) for e in events)

        record = observer.get(1)
        assert record is not None
        assert record.informational_responses == 1
        assert record.response_headers_count == 1
        assert record.response_headers_at == 102.0

    def test_half_close_only_settles_one_direction(self, frame_factory) -> None:
        """
        Ending one direction half-closes the stream but does not settle it.
        """
        clock = FakeClock()
        observer, c = self._client(clock=clock)
        c.initiate_connection()
        c.send_headers(1, self.request_headers, end_stream=True)

        _, server = self._server()
        server.initiate_connection()
        server.receive_data(c.data_to_send())
        c.receive_data(server.data_to_send())

        server.send_headers(1, self.response_headers)  # no end_stream
        c.receive_data(server.data_to_send())

        record = observer.get(1)
        assert record is not None
        assert record.outcome is h2.events.StreamOutcome.HALF_CLOSED_LOCAL
        assert record.local_ended
        assert not record.remote_ended
        assert not record.settled

    def test_reset_settles_once(self) -> None:
        """
        A reset settles the stream immediately and only once.
        """
        clock = FakeClock()
        observer, c = self._client(clock=clock)
        c.initiate_connection()
        c.send_headers(1, self.request_headers)

        clock.advance(4)
        c.reset_stream(1, h2.errors.ErrorCodes.CANCEL)

        record = observer.get(1)
        assert record is not None
        assert record.outcome is h2.events.StreamOutcome.RESET
        assert record.reset_by_remote is False
        assert record.reset_error_code == h2.errors.ErrorCodes.CANCEL
        assert record.settled_at == 104.0

        # A second reset attempt for a now-closed stream must not change the
        # recorded settlement.
        assert record.settled

    def test_remote_reset_via_event(self, frame_factory) -> None:
        """
        A peer RST_STREAM settles the stream as a remote reset.
        """
        clock = FakeClock()
        observer, c = self._client(clock=clock)
        c.initiate_connection()
        c.send_headers(1, self.request_headers)

        _, server = self._server()
        server.initiate_connection()
        server.receive_data(c.data_to_send())
        c.receive_data(server.data_to_send())

        clock.advance(2)
        server.reset_stream(1, h2.errors.ErrorCodes.REFUSED_STREAM)
        events = c.receive_data(server.data_to_send())
        assert any(isinstance(e, h2.events.StreamReset) for e in events)

        record = observer.get(1)
        assert record is not None
        assert record.outcome is h2.events.StreamOutcome.RESET
        assert record.reset_by_remote is True
        assert record.reset_error_code == h2.errors.ErrorCodes.REFUSED_STREAM
        assert record.settled

    def test_push_stream_is_distinct_from_local(self, frame_factory) -> None:
        """
        A locally pushed stream is recorded as a push, distinct from ordinary
        locally-initiated streams.
        """
        clock = FakeClock()
        client_observer, client = self._client(clock=clock)
        server_observer, server = self._server(clock=clock)
        client.initiate_connection()
        client.send_headers(1, self.request_headers, end_stream=True)

        server.initiate_connection()
        server.receive_data(client.data_to_send())
        client.receive_data(server.data_to_send())
        server.receive_data(client.data_to_send())

        clock.advance(1)
        server.push_stream(1, 2, self.request_headers)

        record = server_observer.get(2)
        assert record is not None
        assert record.is_push
        assert record.origin is h2.events.StreamOrigin.LOCAL
        # The peer never sends on a pushed stream, so the remote side is done.
        assert record.remote_ended

        request_record = server_observer.get(1)
        assert request_record is not None
        assert request_record.is_push is False
        assert request_record.origin is h2.events.StreamOrigin.REMOTE

    def test_remotely_pushed_stream_is_recorded(self, frame_factory) -> None:
        """
        A stream pushed by the peer is recorded as a push with the peer as
        origin.
        """
        clock = FakeClock()
        client_observer, client = self._client(clock=clock)
        _, server = self._server(clock=clock)
        client.initiate_connection()
        client.send_headers(1, self.request_headers, end_stream=True)

        server.initiate_connection()
        server.receive_data(client.data_to_send())
        client.receive_data(server.data_to_send())
        server.receive_data(client.data_to_send())

        server.push_stream(1, 2, self.request_headers)
        events = client.receive_data(server.data_to_send())
        assert any(isinstance(e, h2.events.PushedStreamReceived) for e in events)

        record = client_observer.get(2)
        assert record is not None
        assert record.is_push
        assert record.origin is h2.events.StreamOrigin.REMOTE
        assert record.local_ended

    def test_late_attach_marks_timings_incomplete(self) -> None:
        """
        An observer attached part-way through a stream cannot claim complete
        timings.
        """
        observer = h2.events.StreamLifecycleObserver(clock=FakeClock())
        observer.stream_closed_remotely(7)

        record = observer.get(7)
        assert record is not None
        assert record.initiated_at is None
        assert not record.timings_complete
        assert record.total_duration is None

    def test_clock_failure_does_not_change_protocol_state(self, frame_factory) -> None:
        """
        A clock that raises is swallowed: the connection still processes data
        and the record simply marks its timings as incomplete.
        """
        observer = h2.events.StreamLifecycleObserver(clock=lambda: (_ for _ in ()).throw(RuntimeError("boom")))
        config = h2.config.H2Configuration(
            client_side=True,
            stream_lifecycle_observer=observer,
        )
        c = h2.connection.H2Connection(config=config)
        c.initiate_connection()
        c.send_headers(1, self.request_headers, end_stream=True)

        record = observer.get(1)
        assert record is not None
        assert record.clock_failed
        assert record.initiated_at is None
        assert not record.timings_complete
        assert record.outcome is h2.events.StreamOutcome.HALF_CLOSED_LOCAL

    def test_on_settled_exception_is_swallowed(self) -> None:
        """
        A failing settlement listener never disturbs the protocol.
        """
        def explode(record) -> None:
            raise RuntimeError("listener boom")

        observer = h2.events.StreamLifecycleObserver(
            clock=FakeClock(),
            on_settled=explode,
        )
        observer.stream_opened(1, origin=h2.events.StreamOrigin.LOCAL)
        observer.stream_closed_locally(1)
        observer.stream_closed_remotely(1)

        record = observer.get(1)
        assert record is not None
        assert record.settled
        assert record.outcome is h2.events.StreamOutcome.COMPLETED

    def test_on_settled_called_exactly_once(self) -> None:
        """
        The settlement callback fires exactly once per stream.
        """
        settled = []
        observer = h2.events.StreamLifecycleObserver(
            clock=FakeClock(),
            on_settled=settled.append,
        )
        observer.stream_opened(1, origin=h2.events.StreamOrigin.LOCAL)
        observer.stream_closed_locally(1)
        observer.stream_closed_remotely(1)
        observer.stream_closed_remotely(1)
        observer.stream_reset(1)

        assert len(settled) == 1
        assert settled[0].stream_id == 1

    def test_server_records_inbound_request_lifecycle(self) -> None:
        """
        From the server's point of view a request is peer-initiated; a clean
        request/response completes once the server ends its response.
        """
        clock = FakeClock()
        observer, server = self._server(clock=clock)
        server.initiate_connection()

        _, client = self._client()
        client.initiate_connection()
        client.send_headers(1, self.request_headers, end_stream=True)
        server.receive_data(client.data_to_send())
        client.receive_data(server.data_to_send())

        record = observer.get(1)
        assert record is not None
        assert record.origin is h2.events.StreamOrigin.REMOTE
        assert record.remote_ended
        assert not record.local_ended
        assert record.outcome is h2.events.StreamOutcome.HALF_CLOSED_REMOTE
        assert not record.settled

        clock.advance(4)
        server.send_headers(1, [(b":status", b"200")], end_stream=True)

        record = observer.get(1)
        assert record is not None
        assert record.settled
        assert record.outcome is h2.events.StreamOutcome.COMPLETED
        # The server never "received" a response, so that timing stays unset.
        assert record.response_headers_at is None
        assert record.total_duration == 4.0

    def test_upgrade_stream_marks_request_direction(self) -> None:
        """
        The h2c upgrade stream is half-closed as soon as the upgrade happens.
        """
        observer = h2.events.StreamLifecycleObserver(clock=FakeClock())
        config = h2.config.H2Configuration(
            client_side=True,
            stream_lifecycle_observer=observer,
        )
        c = h2.connection.H2Connection(config=config)
        c.initiate_upgrade_connection()

        record = observer.get(1)
        assert record is not None
        assert record.local_ended
        assert record.outcome is h2.events.StreamOutcome.HALF_CLOSED_LOCAL

    def test_reset_before_completion_wins(self) -> None:
        """
        A reset that happens before the clean completion settles the stream as
        a reset, and a later clean completion cannot overwrite that.
        """
        observer = h2.events.StreamLifecycleObserver(clock=FakeClock())
        observer.stream_opened(1, origin=h2.events.StreamOrigin.LOCAL)
        observer.stream_reset(1, error_code=0, remote_reset=False)
        observer.stream_closed_locally(1)
        observer.stream_closed_remotely(1)

        record = observer.get(1)
        assert record is not None
        assert record.outcome is h2.events.StreamOutcome.RESET

    def test_completion_before_reset_wins(self) -> None:
        """
        A stream that already completed is not flipped to reset by a late
        reset.
        """
        observer = h2.events.StreamLifecycleObserver(clock=FakeClock())
        observer.stream_opened(1, origin=h2.events.StreamOrigin.LOCAL)
        observer.stream_closed_locally(1)
        observer.stream_closed_remotely(1)
        observer.stream_reset(1, error_code=0, remote_reset=True)

        record = observer.get(1)
        assert record is not None
        assert record.outcome is h2.events.StreamOutcome.COMPLETED

    def test_open_outcome_before_any_end(self) -> None:
        """
        A freshly opened stream that has not ended either direction is OPEN.
        """
        clock = FakeClock()
        observer, c = self._client(clock=clock)
        c.initiate_connection()
        c.send_headers(1, self.request_headers)  # no end_stream

        record = observer.get(1)
        assert record is not None
        assert record.outcome is h2.events.StreamOutcome.OPEN
        assert not record.local_ended
        assert not record.remote_ended
        assert not record.settled

    def test_multiple_body_chunks_keep_first_body_timestamp(self) -> None:
        """
        Only the first non-empty body chunk sets first_body_at, while bytes
        and chunk counts keep accumulating.
        """
        clock = FakeClock()
        observer, c = self._client(clock=clock)
        c.initiate_connection()
        c.send_headers(1, self.request_headers, end_stream=True)

        _, server = self._server()
        server.initiate_connection()
        server.receive_data(c.data_to_send())
        c.receive_data(server.data_to_send())

        clock.advance(1)
        server.send_headers(1, [(b":status", b"200")], end_stream=False)
        c.receive_data(server.data_to_send())

        clock.advance(1)
        server.send_data(1, b"abc")
        c.receive_data(server.data_to_send())
        first = observer.get(1)
        assert first is not None
        assert first.first_body_at == 102.0

        clock.advance(1)
        server.send_data(1, b"de")
        c.receive_data(server.data_to_send())

        record = observer.get(1)
        assert record is not None
        assert record.first_body_at == 102.0
        assert record.body_bytes == 5
        assert record.body_chunks == 2
        assert record.flow_controlled_bytes == 5

    def test_headers_only_response_completes(self) -> None:
        """
        A response that ends on the HEADERS frame (e.g. 204) still completes
        the stream without a first-body timestamp.
        """
        clock = FakeClock()
        observer, c = self._client(clock=clock)
        c.initiate_connection()
        c.send_headers(1, self.request_headers, end_stream=True)

        _, server = self._server()
        server.initiate_connection()
        server.receive_data(c.data_to_send())
        c.receive_data(server.data_to_send())

        clock.advance(3)
        server.send_headers(1, [(b":status", b"204")], end_stream=True)
        c.receive_data(server.data_to_send())

        record = observer.get(1)
        assert record is not None
        assert record.outcome is h2.events.StreamOutcome.COMPLETED
        assert record.first_body_at is None
        assert record.body_chunks == 0
        assert record.settled_at == 103.0
        assert record.total_duration == 3.0

    def test_trailers_do_not_replace_response_headers(self) -> None:
        """
        Trailers end the stream but are not mistaken for the response headers.
        """
        clock = FakeClock()
        observer, c = self._client(clock=clock)
        c.initiate_connection()
        c.send_headers(1, self.request_headers, end_stream=True)

        _, server = self._server()
        server.initiate_connection()
        server.receive_data(c.data_to_send())
        c.receive_data(server.data_to_send())

        clock.advance(1)
        server.send_headers(1, [(b":status", b"200")], end_stream=False)
        c.receive_data(server.data_to_send())

        clock.advance(1)
        server.send_data(1, b"body")
        c.receive_data(server.data_to_send())

        clock.advance(1)
        server.send_headers(1, [(b"trailer", b"value")], end_stream=True)
        events = c.receive_data(server.data_to_send())
        assert any(isinstance(e, h2.events.TrailersReceived) for e in events)

        record = observer.get(1)
        assert record is not None
        assert record.response_headers_count == 1
        assert record.response_headers_at == 101.0
        assert record.outcome is h2.events.StreamOutcome.COMPLETED

    def test_stream_opened_twice_keeps_first_initiation(self) -> None:
        """
        Opening an already-known stream (as happens for push streams, which
        are refined right after creation) does not overwrite the first
        initiation timestamp or origin, but can still add push/half-close
        information.
        """
        clock = FakeClock()
        observer = h2.events.StreamLifecycleObserver(clock=clock)
        observer.stream_opened(2, origin=h2.events.StreamOrigin.LOCAL)
        clock.advance(5)
        record = observer.stream_opened(
            2,
            origin=h2.events.StreamOrigin.REMOTE,
            is_push=True,
            remote_closed=True,
        )

        assert record.origin is h2.events.StreamOrigin.LOCAL
        assert record.is_push
        assert record.initiated_at == 100.0
        assert record.remote_ended

    def test_repeated_direction_close_is_idempotent(self) -> None:
        """
        Closing the same direction twice keeps the first timestamp and does
        not settle the stream prematurely.
        """
        clock = FakeClock()
        observer = h2.events.StreamLifecycleObserver(clock=clock)
        observer.stream_opened(1, origin=h2.events.StreamOrigin.LOCAL)

        clock.advance(1)
        observer.stream_closed_locally(1)
        first_local = observer.get(1).local_closed_at  # type: ignore[union-attr]

        clock.advance(1)
        observer.stream_closed_locally(1)

        record = observer.get(1)
        assert record is not None
        assert record.local_closed_at == first_local
        assert not record.settled

    def test_close_ignored_after_settlement(self) -> None:
        """
        Once a stream is settled, further direction-close calls are no-ops.
        """
        observer = h2.events.StreamLifecycleObserver(clock=FakeClock())
        observer.stream_opened(1, origin=h2.events.StreamOrigin.LOCAL)
        observer.stream_closed_locally(1)
        observer.stream_closed_remotely(1)
        settled_at = observer.get(1).settled_at  # type: ignore[union-attr]

        observer.stream_closed_locally(1)
        observer.stream_closed_remotely(1)

        record = observer.get(1)
        assert record is not None
        assert record.settled_at == settled_at

    def test_observer_does_not_retain_body(self, frame_factory) -> None:
        """
        The observer records body counts but never keeps body content.
        """
        clock = FakeClock()
        observer, c = self._client(clock=clock)
        c.initiate_connection()
        c.send_headers(1, self.request_headers, end_stream=True)

        _, server = self._server()
        server.initiate_connection()
        server.receive_data(c.data_to_send())
        c.receive_data(server.data_to_send())

        secret = b"super-secret-body"
        server.send_headers(1, [(b":status", b"200")], end_stream=False)
        c.receive_data(server.data_to_send())
        server.send_data(1, secret)
        c.receive_data(server.data_to_send())

        record = observer.get(1)
        assert record is not None
        assert record.body_bytes == len(secret)
        assert secret not in repr(record).encode("ascii", "ignore")
        for value in vars(record).values():
            if isinstance(value, (bytes, bytearray, memoryview)):
                msg = "lifecycle records must not retain body content"
                raise AssertionError(msg)
            assert secret.decode() not in repr(value)

    def test_max_records_evicts_settled_first(self) -> None:
        """
        Bounded retention evicts settled records first and never drops an
        unsettled one.
        """
        observer = h2.events.StreamLifecycleObserver(clock=FakeClock(), max_records=2)
        observer.stream_opened(1, origin=h2.events.StreamOrigin.LOCAL)
        observer.stream_closed_locally(1)
        observer.stream_closed_remotely(1)
        observer.stream_opened(2, origin=h2.events.StreamOrigin.LOCAL)
        observer.stream_closed_locally(2)
        observer.stream_closed_remotely(2)
        observer.stream_opened(3, origin=h2.events.StreamOrigin.LOCAL)

        assert observer.get(1) is None
        assert observer.get(2) is not None
        assert observer.get(3) is not None

    def test_max_records_keeps_unsettled(self) -> None:
        """
        When every retained record is unsettled, none are evicted.
        """
        observer = h2.events.StreamLifecycleObserver(clock=FakeClock(), max_records=1)
        observer.stream_opened(1, origin=h2.events.StreamOrigin.LOCAL)
        observer.stream_opened(2, origin=h2.events.StreamOrigin.LOCAL)

        assert observer.get(1) is not None
        assert observer.get(2) is not None

    def test_invalid_max_records(self) -> None:
        """
        A non-positive max_records is rejected.
        """
        with pytest.raises(ValueError):
            h2.events.StreamLifecycleObserver(max_records=0)

    def test_clear_discards_records(self) -> None:
        observer = h2.events.StreamLifecycleObserver(clock=FakeClock())
        observer.stream_opened(1, origin=h2.events.StreamOrigin.LOCAL)
        observer.clear()
        assert observer.get(1) is None
        assert observer.records == {}

    def test_empty_data_does_not_count_as_first_body(self, frame_factory) -> None:
        """
        A zero-length DATA frame is not the first body chunk.
        """
        clock = FakeClock()
        observer, c = self._client(clock=clock)
        c.initiate_connection()
        c.send_headers(1, self.request_headers, end_stream=True)

        _, server = self._server()
        server.initiate_connection()
        server.receive_data(c.data_to_send())
        c.receive_data(server.data_to_send())

        server.send_headers(1, [(b":status", b"200")], end_stream=False)
        c.receive_data(server.data_to_send())
        server.send_data(1, b"")
        c.receive_data(server.data_to_send())

        record = observer.get(1)
        assert record is not None
        assert record.first_body_at is None
        assert record.body_chunks == 0
        assert record.body_bytes == 0
