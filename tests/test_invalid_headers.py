"""
Tests invalid header blocks, and validates that they fail appropriately.
"""
from __future__ import annotations

import itertools

import hyperframe.frame
import pytest
from hypothesis import given
from hypothesis.strategies import binary, lists, tuples

import h2.config
import h2.connection
import h2.errors
import h2.events
import h2.exceptions
import h2.settings
import h2.utilities

HEADERS_STRATEGY = lists(tuples(binary(min_size=1), binary()))


class TestInvalidFrameSequences:
    """
    Invalid header sequences cause ProtocolErrors to be thrown when received.
    """

    base_request_headers = [
        (":authority", "example.com"),
        (":path", "/"),
        (":scheme", "https"),
        (":method", "GET"),
        ("user-agent", "someua/0.0.1"),
    ]
    duplicate_host_header_blocks = [
        [*base_request_headers, ("host", "example.com"), ("host", "evil.example.com")],
        [*base_request_headers, ("host", "evil.example.com"), ("host", "example.com")],
        [*[header for header in base_request_headers if header[0] != ":authority"],
         ("host", "example.com"), ("host", "evil.example.com")],
    ]
    base_invalid_header_blocks = [
        [*base_request_headers, ("Uppercase", "name")],
        [*base_request_headers, (":late", "pseudo-header")],
        [(":path", "duplicate-pseudo-header"), *base_request_headers],
        [*base_request_headers, ("connection", "close")],
        [*base_request_headers, ("proxy-connection", "close")],
        [*base_request_headers, ("keep-alive", "close")],
        [*base_request_headers, ("transfer-encoding", "gzip")],
        [*base_request_headers, ("upgrade", "super-protocol/1.1")],
        [*base_request_headers, ("te", "chunked")],
        *duplicate_host_header_blocks,
        [*base_request_headers, ("host", "notexample.com")],
        [*base_request_headers, (" name", "name with leading space")],
        [*base_request_headers, ("name ", "name with trailing space")],
        [*base_request_headers, ("name", " value with leading space")],
        [*base_request_headers, ("name", "value with trailing space ")],
        [*base_request_headers, ("illegal:characters", "value")],
        [*base_request_headers, ("illegal-\r-characters", "value")],
        [*base_request_headers, ("illegal-\n-characters", "value")],
        [*base_request_headers, ("illegal-\x00-characters", "value")],
        [*base_request_headers, ("illegal-\x01-characters", "value")],
        [*base_request_headers, ("illegal-characters", "some \r value")],
        [*base_request_headers, ("illegal-characters", "some \n value")],
        [*base_request_headers, ("illegal-characters", "some \x00 value")],
        [header for header in base_request_headers
         if header[0] != ":authority"],
        [(":protocol", "websocket"), *base_request_headers],
    ]
    invalid_header_blocks = base_invalid_header_blocks + [
        h2.utilities.utf8_encode_headers(headers) for headers in base_invalid_header_blocks
    ]
    server_config = h2.config.H2Configuration(
        client_side=False, header_encoding="utf-8",
    )

    @pytest.mark.parametrize("headers", invalid_header_blocks)
    def test_headers_event(self, frame_factory, headers) -> None:
        """
        Test invalid headers are rejected with PROTOCOL_ERROR.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.receive_data(frame_factory.preamble())
        c.clear_outbound_data_buffer()

        f = frame_factory.build_headers_frame(headers)
        data = f.serialize()

        with pytest.raises(h2.exceptions.ProtocolError):
            c.receive_data(data)

        expected_frame = frame_factory.build_goaway_frame(
            last_stream_id=1, error_code=h2.errors.ErrorCodes.PROTOCOL_ERROR,
        )
        assert c.data_to_send() == expected_frame.serialize()

    @pytest.mark.parametrize("headers", duplicate_host_header_blocks + [
        h2.utilities.utf8_encode_headers(headers) for headers in duplicate_host_header_blocks
    ])
    def test_duplicate_host_headers_rejected(self, frame_factory, headers) -> None:
        """
        A request header block with more than one Host header is rejected
        by the duplicate Host check, not by any other header check.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.receive_data(frame_factory.preamble())
        c.clear_outbound_data_buffer()

        f = frame_factory.build_headers_frame(headers)
        data = f.serialize()

        with pytest.raises(h2.exceptions.ProtocolError, match="Request header block has multiple Host headers"):
            c.receive_data(data)

        expected_frame = frame_factory.build_goaway_frame(
            last_stream_id=1, error_code=h2.errors.ErrorCodes.PROTOCOL_ERROR,
        )
        assert c.data_to_send() == expected_frame.serialize()

    @pytest.mark.parametrize("headers", invalid_header_blocks)
    def test_push_promise_event(self, frame_factory, headers) -> None:
        """
        If a PUSH_PROMISE header frame is received with an invalid header block
        it is rejected with a PROTOCOL_ERROR.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(
            stream_id=1, headers=self.base_request_headers, end_stream=True,
        )
        c.clear_outbound_data_buffer()

        f = frame_factory.build_push_promise_frame(
            stream_id=1,
            promised_stream_id=2,
            headers=headers,
        )
        data = f.serialize()

        with pytest.raises(h2.exceptions.ProtocolError):
            c.receive_data(data)

        expected_frame = frame_factory.build_goaway_frame(
            last_stream_id=0, error_code=h2.errors.ErrorCodes.PROTOCOL_ERROR,
        )
        assert c.data_to_send() == expected_frame.serialize()

    @pytest.mark.parametrize("headers", invalid_header_blocks)
    def test_push_promise_skipping_validation(self, frame_factory, headers) -> None:
        """
        If we have ``validate_inbound_headers`` disabled, then invalid header
        blocks in push promise frames are allowed to pass.
        """
        config = h2.config.H2Configuration(
            client_side=True,
            validate_inbound_headers=False,
        )

        c = h2.connection.H2Connection(config=config)
        c.initiate_connection()
        c.send_headers(
            stream_id=1, headers=self.base_request_headers, end_stream=True,
        )
        c.clear_outbound_data_buffer()

        f = frame_factory.build_push_promise_frame(
            stream_id=1,
            promised_stream_id=2,
            headers=headers,
        )
        data = f.serialize()

        events = c.receive_data(data)
        assert len(events) == 1
        pp_event = events[0]
        assert pp_event.headers == h2.utilities.utf8_encode_headers(headers)

    @pytest.mark.parametrize("headers", invalid_header_blocks)
    def test_headers_event_skipping_validation(self, frame_factory, headers) -> None:
        """
        If we have ``validate_inbound_headers`` disabled, then all of these
        invalid header blocks are allowed to pass.
        """
        config = h2.config.H2Configuration(
            client_side=False,
            validate_inbound_headers=False,
        )

        c = h2.connection.H2Connection(config=config)
        c.receive_data(frame_factory.preamble())

        f = frame_factory.build_headers_frame(headers)
        data = f.serialize()

        events = c.receive_data(data)
        assert len(events) == 1
        request_event = events[0]
        assert request_event.headers == h2.utilities.utf8_encode_headers(headers)

    def test_te_trailers_is_valid(self, frame_factory) -> None:
        """
        `te: trailers` is allowed by the filter.
        """
        headers = (
            [*self.base_request_headers, ("te", "trailers")]
        )

        c = h2.connection.H2Connection(config=self.server_config)
        c.receive_data(frame_factory.preamble())

        f = frame_factory.build_headers_frame(headers)
        data = f.serialize()

        events = c.receive_data(data)
        assert len(events) == 1
        request_event = events[0]
        assert request_event.headers == headers

    def test_pseudo_headers_rejected_in_trailer(self, frame_factory) -> None:
        """
        Ensure we reject pseudo headers included in trailers
        """
        trailers = [(":path", "/"), ("extra", "value")]

        c = h2.connection.H2Connection(config=self.server_config)
        c.receive_data(frame_factory.preamble())
        c.clear_outbound_data_buffer()

        header_frame = frame_factory.build_headers_frame(
            self.base_request_headers,
        )
        trailer_frame = frame_factory.build_headers_frame(
            trailers, flags=["END_STREAM"],
        )
        head = header_frame.serialize()
        trailer = trailer_frame.serialize()

        c.receive_data(head)
        # Raise exception if pseudo header in trailer
        with pytest.raises(h2.exceptions.ProtocolError) as e:
            c.receive_data(trailer)
        assert "pseudo-header in trailer" in str(e.value)

        # Test appropriate response frame is generated
        expected_frame = frame_factory.build_goaway_frame(
            last_stream_id=1, error_code=h2.errors.ErrorCodes.PROTOCOL_ERROR,
        )
        assert c.data_to_send() == expected_frame.serialize()


class TestSendingInvalidFrameSequences:
    """
    Trying to send invalid header sequences cause ProtocolErrors to
    be thrown.
    """

    base_request_headers = [
        (":authority", "example.com"),
        (":path", "/"),
        (":scheme", "https"),
        (":method", "GET"),
        ("user-agent", "someua/0.0.1"),
    ]
    invalid_header_blocks = [
        [*base_request_headers, (":late", "pseudo-header")],
        [(":path", "duplicate-pseudo-header"), *base_request_headers],
        [*base_request_headers, ("te", "chunked")],
        [*base_request_headers, ("host", "notexample.com")],
        [header for header in base_request_headers
         if header[0] != ":authority"],
    ]
    strippable_header_blocks = [
        [*base_request_headers, ("connection", "close")],
        [*base_request_headers, ("proxy-connection", "close")],
        [*base_request_headers, ("keep-alive", "close")],
        [*base_request_headers, ("transfer-encoding", "gzip")],
        [*base_request_headers, ("upgrade", "super-protocol/1.1")],
    ]
    all_header_blocks = invalid_header_blocks + strippable_header_blocks

    server_config = h2.config.H2Configuration(client_side=False)

    @pytest.mark.parametrize("headers", invalid_header_blocks)
    def test_headers_event(self, frame_factory, headers) -> None:
        """
        Test sending invalid headers raise a ProtocolError.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()

        # Clear the data, then try to send headers.
        c.clear_outbound_data_buffer()
        with pytest.raises(h2.exceptions.ProtocolError):
            c.send_headers(1, headers)

    @pytest.mark.parametrize("headers", invalid_header_blocks)
    def test_send_push_promise(self, frame_factory, headers) -> None:
        """
        Sending invalid headers in a push promise raises a ProtocolError.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.initiate_connection()
        c.receive_data(frame_factory.preamble())

        header_frame = frame_factory.build_headers_frame(
            self.base_request_headers,
        )
        c.receive_data(header_frame.serialize())

        # Clear the data, then try to send a push promise.
        c.clear_outbound_data_buffer()
        with pytest.raises(h2.exceptions.ProtocolError):
            c.push_stream(
                stream_id=1, promised_stream_id=2, request_headers=headers,
            )

    @pytest.mark.parametrize("headers", all_header_blocks)
    def test_headers_event_skipping_validation(self, frame_factory, headers) -> None:
        """
        If we have ``validate_outbound_headers`` disabled, then all of these
        invalid header blocks are allowed to pass.
        """
        config = h2.config.H2Configuration(
            validate_outbound_headers=False,
        )

        c = h2.connection.H2Connection(config=config)
        c.initiate_connection()

        # Clear the data, then send headers.
        c.clear_outbound_data_buffer()
        c.send_headers(1, headers)

        # Ensure headers are still normalized.
        headers = h2.utilities.utf8_encode_headers(headers)
        norm_headers = h2.utilities.normalize_outbound_headers(
            headers, None, False,
        )
        f = frame_factory.build_headers_frame(norm_headers)
        assert c.data_to_send() == f.serialize()

    @pytest.mark.parametrize("headers", all_header_blocks)
    def test_push_promise_skipping_validation(self, frame_factory, headers) -> None:
        """
        If we have ``validate_outbound_headers`` disabled, then all of these
        invalid header blocks are allowed to pass.
        """
        config = h2.config.H2Configuration(
            client_side=False,
            validate_outbound_headers=False,
        )

        c = h2.connection.H2Connection(config=config)
        c.initiate_connection()
        c.receive_data(frame_factory.preamble())

        header_frame = frame_factory.build_headers_frame(
            self.base_request_headers,
        )
        c.receive_data(header_frame.serialize())

        frame_factory.refresh_encoder()

        # Create push promise frame with normalized headers.
        headers = h2.utilities.utf8_encode_headers(headers)
        norm_headers = h2.utilities.normalize_outbound_headers(
            headers, None, False,
        )
        pp_frame = frame_factory.build_push_promise_frame(
            stream_id=1, promised_stream_id=2, headers=norm_headers,
        )

        # Clear the data, then send a push promise.
        c.clear_outbound_data_buffer()
        c.push_stream(
            stream_id=1, promised_stream_id=2, request_headers=headers,
        )
        assert c.data_to_send() == pp_frame.serialize()

    @pytest.mark.parametrize("headers", all_header_blocks)
    def test_headers_event_skip_normalization(self, frame_factory, headers) -> None:
        """
        If we have ``normalize_outbound_headers`` disabled, then all of these
        invalid header blocks are sent through unmodified.
        """
        config = h2.config.H2Configuration(
            validate_outbound_headers=False,
            normalize_outbound_headers=False,
        )

        c = h2.connection.H2Connection(config=config)
        c.initiate_connection()

        f = frame_factory.build_headers_frame(
            headers,
            stream_id=1,
        )

        # Clear the data, then send headers.
        c.clear_outbound_data_buffer()
        c.send_headers(1, headers)
        assert c.data_to_send() == f.serialize()

    @pytest.mark.parametrize("headers", all_header_blocks)
    def test_push_promise_skip_normalization(self, frame_factory, headers) -> None:
        """
        If we have ``normalize_outbound_headers`` disabled, then all of these
        invalid header blocks are allowed to pass unmodified.
        """
        config = h2.config.H2Configuration(
            client_side=False,
            validate_outbound_headers=False,
            normalize_outbound_headers=False,
        )

        c = h2.connection.H2Connection(config=config)
        c.initiate_connection()
        c.receive_data(frame_factory.preamble())

        header_frame = frame_factory.build_headers_frame(
            self.base_request_headers,
        )
        c.receive_data(header_frame.serialize())

        frame_factory.refresh_encoder()
        pp_frame = frame_factory.build_push_promise_frame(
            stream_id=1, promised_stream_id=2, headers=headers,
        )

        # Clear the data, then send a push promise.
        c.clear_outbound_data_buffer()
        c.push_stream(
            stream_id=1, promised_stream_id=2, request_headers=headers,
        )
        assert c.data_to_send() == pp_frame.serialize()

    @pytest.mark.parametrize("headers", strippable_header_blocks)
    def test_strippable_headers(self, frame_factory, headers) -> None:
        """
        Test connection related headers are removed before sending.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()

        # Clear the data, then try to send headers.
        c.clear_outbound_data_buffer()
        c.send_headers(1, headers)

        f = frame_factory.build_headers_frame(self.base_request_headers)
        assert c.data_to_send() == f.serialize()


class TestFilter:
    """
    Test the filter function directly.

    These tests exists to confirm the behaviour of the filter function in a
    wide range of scenarios. Many of these scenarios may not be legal for
    HTTP/2 and so may never hit the function, but it's worth validating that it
    behaves as expected anyway.
    """

    validation_functions = [
        h2.utilities.validate_headers,
        h2.utilities.validate_outbound_headers,
    ]

    hdr_validation_combos = [
        h2.utilities.HeaderValidationFlags(
            is_client, is_trailer, is_response_header, is_push_promise,
        )
        for is_client, is_trailer, is_response_header, is_push_promise in (
            itertools.product([True, False], repeat=4)
        )
    ]

    hdr_validation_response_headers = [
        flags for flags in hdr_validation_combos
        if flags.is_response_header
    ]

    hdr_validation_request_headers_no_trailer = [
        flags for flags in hdr_validation_combos
        if not (flags.is_trailer or flags.is_response_header)
    ]

    invalid_request_header_blocks_bytes = (
        # First, missing :method
        (
            (b":authority", b"google.com"),
            (b":path", b"/"),
            (b":scheme", b"https"),
        ),
        # Next, missing :path
        (
            (b":authority", b"google.com"),
            (b":method", b"GET"),
            (b":scheme", b"https"),
        ),
        # Next, missing :scheme
        (
            (b":authority", b"google.com"),
            (b":method", b"GET"),
            (b":path", b"/"),
        ),
        # Finally, path present but empty.
        (
            (b":authority", b"google.com"),
            (b":method", b"GET"),
            (b":scheme", b"https"),
            (b":path", b""),
        ),
    )

    # All headers that are forbidden from either request or response blocks.
    forbidden_request_headers_bytes = (b":status",)
    forbidden_response_headers_bytes = (b":path", b":scheme", b":authority", b":method")

    @pytest.mark.parametrize("validation_function", validation_functions)
    @pytest.mark.parametrize("hdr_validation_flags", hdr_validation_combos)
    @given(headers=HEADERS_STRATEGY)
    def test_range_of_acceptable_outputs(self,
                                         headers,
                                         validation_function,
                                         hdr_validation_flags) -> None:
        """
        The header validation functions either return the data unchanged
        or throw a ProtocolError.
        """
        try:
            assert headers == list(validation_function(
                headers, hdr_validation_flags))
        except h2.exceptions.ProtocolError:
            assert True

    @pytest.mark.parametrize("hdr_validation_flags", hdr_validation_combos)
    def test_invalid_pseudo_headers(self, hdr_validation_flags) -> None:
        headers = [(b":custom", b"value")]
        with pytest.raises(h2.exceptions.ProtocolError):
            list(h2.utilities.validate_headers(headers, hdr_validation_flags))

    @pytest.mark.parametrize("validation_function", validation_functions)
    @pytest.mark.parametrize(
        "hdr_validation_flags", hdr_validation_request_headers_no_trailer,
    )
    def test_matching_authority_host_headers(self,
                                             validation_function,
                                             hdr_validation_flags) -> None:
        """
        If a header block has :authority and Host headers and they match,
        the headers should pass through unchanged.
        """
        headers = [
            (b":authority", b"example.com"),
            (b":path", b"/"),
            (b":scheme", b"https"),
            (b":method", b"GET"),
            (b"host", b"example.com"),
        ]
        assert headers == list(h2.utilities.validate_headers(
            headers, hdr_validation_flags,
        ))

    @pytest.mark.parametrize(
        "hdr_validation_flags", hdr_validation_response_headers,
    )
    def test_response_header_without_status(self, hdr_validation_flags) -> None:
        headers = [(b"content-length", b"42")]
        with pytest.raises(h2.exceptions.ProtocolError):
            list(h2.utilities.validate_headers(headers, hdr_validation_flags))

    @pytest.mark.parametrize(
        "hdr_validation_flags", hdr_validation_request_headers_no_trailer,
    )
    @pytest.mark.parametrize(
        "header_block",
        (invalid_request_header_blocks_bytes),
    )
    def test_outbound_req_header_missing_pseudo_headers(self,
                                                        hdr_validation_flags,
                                                        header_block) -> None:
        with pytest.raises(h2.exceptions.ProtocolError):
            list(
                h2.utilities.validate_outbound_headers(
                    header_block, hdr_validation_flags,
                ),
            )

    @pytest.mark.parametrize(
        "hdr_validation_flags", hdr_validation_request_headers_no_trailer,
    )
    @pytest.mark.parametrize(
        "header_block", invalid_request_header_blocks_bytes,
    )
    def test_inbound_req_header_missing_pseudo_headers(self,
                                                       hdr_validation_flags,
                                                       header_block) -> None:
        with pytest.raises(h2.exceptions.ProtocolError):
            list(
                h2.utilities.validate_headers(
                    header_block, hdr_validation_flags,
                ),
            )

    @pytest.mark.parametrize(
        "hdr_validation_flags", hdr_validation_request_headers_no_trailer,
    )
    @pytest.mark.parametrize(
        "invalid_header",
        forbidden_request_headers_bytes,
    )
    def test_outbound_req_header_extra_pseudo_headers(self,
                                                      hdr_validation_flags,
                                                      invalid_header) -> None:
        """
        Outbound request header blocks containing the forbidden request headers
        fail validation.
        """
        headers = [
            (b":path", b"/"),
            (b":scheme", b"https"),
            (b":authority", b"google.com"),
            (b":method", b"GET"),
        ]
        headers.append((invalid_header, b"some value"))
        with pytest.raises(h2.exceptions.ProtocolError):
            list(
                h2.utilities.validate_outbound_headers(
                    headers, hdr_validation_flags,
                ),
            )

    @pytest.mark.parametrize(
        "hdr_validation_flags", hdr_validation_request_headers_no_trailer,
    )
    @pytest.mark.parametrize(
        "invalid_header",
        forbidden_request_headers_bytes,
    )
    def test_inbound_req_header_extra_pseudo_headers(self,
                                                     hdr_validation_flags,
                                                     invalid_header) -> None:
        """
        Inbound request header blocks containing the forbidden request headers
        fail validation.
        """
        headers = [
            (b":path", b"/"),
            (b":scheme", b"https"),
            (b":authority", b"google.com"),
            (b":method", b"GET"),
        ]
        headers.append((invalid_header, b"some value"))
        with pytest.raises(h2.exceptions.ProtocolError):
            list(h2.utilities.validate_headers(headers, hdr_validation_flags))

    @pytest.mark.parametrize(
        "hdr_validation_flags", hdr_validation_response_headers,
    )
    @pytest.mark.parametrize(
        "invalid_header",
        forbidden_response_headers_bytes,
    )
    def test_outbound_resp_header_extra_pseudo_headers(self,
                                                       hdr_validation_flags,
                                                       invalid_header) -> None:
        """
        Outbound response header blocks containing the forbidden response
        headers fail validation.
        """
        headers = [(b":status", b"200")]
        headers.append((invalid_header, b"some value"))
        with pytest.raises(h2.exceptions.ProtocolError):
            list(
                h2.utilities.validate_outbound_headers(
                    headers, hdr_validation_flags,
                ),
            )

    @pytest.mark.parametrize(
        "hdr_validation_flags", hdr_validation_response_headers,
    )
    @pytest.mark.parametrize(
        "invalid_header",
        forbidden_response_headers_bytes,
    )
    def test_inbound_resp_header_extra_pseudo_headers(self,
                                                      hdr_validation_flags,
                                                      invalid_header) -> None:
        """
        Inbound response header blocks containing the forbidden response
        headers fail validation.
        """
        headers = [(b":status", b"200")]
        headers.append((invalid_header, b"some value"))
        with pytest.raises(h2.exceptions.ProtocolError):
            list(h2.utilities.validate_headers(headers, hdr_validation_flags))

    @pytest.mark.parametrize("hdr_validation_flags", hdr_validation_combos)
    def test_inbound_header_name_length(self, hdr_validation_flags) -> None:
        with pytest.raises(h2.exceptions.ProtocolError):
            list(h2.utilities.validate_headers([(b"", b"foobar")], hdr_validation_flags))

    def test_inbound_header_name_length_full_frame_decode(self, frame_factory) -> None:
        f = frame_factory.build_headers_frame([])
        f.data = b"\x00\x00\x01\x04"
        data = f.serialize()

        c = h2.connection.H2Connection(config=h2.config.H2Configuration(client_side=False))
        c.initiate_connection()
        c.receive_data(frame_factory.preamble())
        c.clear_outbound_data_buffer()

        with pytest.raises(h2.exceptions.ProtocolError, match="Received header name with zero length."):
            c.receive_data(data)


class TestOversizedHeaders:
    """
    Tests that oversized header blocks are correctly rejected. This replicates
    the "HPACK Bomb" attack, and confirms that we're resistant against it.
    """

    request_header_block = [
        (b":method", b"GET"),
        (b":authority", b"example.com"),
        (b":scheme", b"https"),
        (b":path", b"/"),
    ]

    response_header_block = [
        (b":status", b"200"),
    ]

    # The first header block contains a single header that fills the header
    # table. To do that, we'll give it a single-character header name and a
    # 4063 byte header value. This will make it exactly the size of the header
    # table. It must come last, so that it evicts all other headers.
    # This block must be appended to either a request or response block.
    first_header_block = [
        (b"a", b"a" * 4063),
    ]

    # The second header "block" is actually a custom HEADERS frame body that
    # simply repeatedly refers to the first entry for 16kB. Each byte has the
    # high bit set (0x80), and then uses the remaining 7 bits to encode the
    # number 62 (0x3e), leading to a repeat of the byte 0xbe.
    second_header_block = b"\xbe" * 2**14

    server_config = h2.config.H2Configuration(client_side=False)

    def test_hpack_bomb_request(self, frame_factory) -> None:
        """
        A HPACK bomb request causes the connection to be torn down with the
        error code ENHANCE_YOUR_CALM.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.receive_data(frame_factory.preamble())
        c.clear_outbound_data_buffer()

        f = frame_factory.build_headers_frame(
            self.request_header_block + self.first_header_block,
        )
        data = f.serialize()
        c.receive_data(data)

        # Build the attack payload.
        attack_frame = hyperframe.frame.HeadersFrame(stream_id=3)
        attack_frame.data = self.second_header_block
        attack_frame.flags.add("END_HEADERS")
        data = attack_frame.serialize()

        with pytest.raises(h2.exceptions.DenialOfServiceError):
            c.receive_data(data)

        expected_frame = frame_factory.build_goaway_frame(
            last_stream_id=1, error_code=h2.errors.ErrorCodes.ENHANCE_YOUR_CALM,
        )
        assert c.data_to_send() == expected_frame.serialize()

    def test_hpack_bomb_response(self, frame_factory) -> None:
        """
        A HPACK bomb response causes the connection to be torn down with the
        error code ENHANCE_YOUR_CALM.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(
            stream_id=1, headers=self.request_header_block,
        )
        c.send_headers(
            stream_id=3, headers=self.request_header_block,
        )
        c.clear_outbound_data_buffer()

        f = frame_factory.build_headers_frame(
            self.response_header_block + self.first_header_block,
        )
        data = f.serialize()
        c.receive_data(data)

        # Build the attack payload.
        attack_frame = hyperframe.frame.HeadersFrame(stream_id=3)
        attack_frame.data = self.second_header_block
        attack_frame.flags.add("END_HEADERS")
        data = attack_frame.serialize()

        with pytest.raises(h2.exceptions.DenialOfServiceError):
            c.receive_data(data)

        expected_frame = frame_factory.build_goaway_frame(
            last_stream_id=0, error_code=h2.errors.ErrorCodes.ENHANCE_YOUR_CALM,
        )
        assert c.data_to_send() == expected_frame.serialize()

    def test_hpack_bomb_push(self, frame_factory) -> None:
        """
        A HPACK bomb push causes the connection to be torn down with the
        error code ENHANCE_YOUR_CALM.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(
            stream_id=1, headers=self.request_header_block,
        )
        c.clear_outbound_data_buffer()

        f = frame_factory.build_headers_frame(
            self.response_header_block + self.first_header_block,
        )
        data = f.serialize()
        c.receive_data(data)

        # Build the attack payload. We need to shrink it by four bytes because
        # the promised_stream_id consumes four bytes of body.
        attack_frame = hyperframe.frame.PushPromiseFrame(stream_id=3)
        attack_frame.promised_stream_id = 2
        attack_frame.data = self.second_header_block[:-4]
        attack_frame.flags.add("END_HEADERS")
        data = attack_frame.serialize()

        with pytest.raises(h2.exceptions.DenialOfServiceError):
            c.receive_data(data)

        expected_frame = frame_factory.build_goaway_frame(
            last_stream_id=0, error_code=h2.errors.ErrorCodes.ENHANCE_YOUR_CALM,
        )
        assert c.data_to_send() == expected_frame.serialize()

    def test_reject_headers_when_list_size_shrunk(self, frame_factory) -> None:
        """
        When we've shrunk the header list size, new header blocks that violate
        the new size are rejected at the stream level: the offending stream is
        reset with ENHANCE_YOUR_CALM, but the connection stays up and the
        HPACK dynamic table remains synchronised.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.receive_data(frame_factory.preamble())
        c.clear_outbound_data_buffer()

        # Receive the first request, which causes no problem.
        f = frame_factory.build_headers_frame(
            stream_id=1,
            headers=self.request_header_block,
        )
        data = f.serialize()
        c.receive_data(data)

        # Now, send a settings change. It's un-ACKed at this time. A new
        # request arrives, also without incident.
        c.update_settings({h2.settings.SettingCodes.MAX_HEADER_LIST_SIZE: 50})
        c.clear_outbound_data_buffer()
        f = frame_factory.build_headers_frame(
            stream_id=3,
            headers=self.request_header_block,
        )
        data = f.serialize()
        c.receive_data(data)

        # We get a SETTINGS ACK.
        f = frame_factory.build_settings_frame({}, ack=True)
        data = f.serialize()
        c.receive_data(data)

        # Now a third request comes in. It exceeds the shrunk limit, so the
        # stream is rejected: no exception, no GOAWAY, just a rejection event
        # and a RST_STREAM. The extra custom header is added to the HPACK
        # dynamic table by the peer's encoder, so its presence in the
        # rejected block must still update our decoder.
        oversized_headers = [
            *self.request_header_block,
            (b"x-custom-header", b"some-value"),
        ]
        f = frame_factory.build_headers_frame(
            stream_id=5,
            headers=oversized_headers,
        )
        data = f.serialize()

        events = c.receive_data(data)

        assert len(events) == 1
        event = events[0]
        assert isinstance(event, h2.events.StreamHeadersRejected)
        assert event.stream_id == 5
        assert event.error_code == h2.errors.ErrorCodes.ENHANCE_YOUR_CALM
        assert event.reason

        expected_frame = frame_factory.build_rst_stream_frame(
            5, h2.errors.ErrorCodes.ENHANCE_YOUR_CALM,
        )
        assert c.data_to_send() == expected_frame.serialize()

        # Restore a generous limit.
        c.update_settings(
            {h2.settings.SettingCodes.MAX_HEADER_LIST_SIZE: 2**16},
        )
        c.clear_outbound_data_buffer()
        f = frame_factory.build_settings_frame({}, ack=True)
        c.receive_data(f.serialize())

        # The connection is still usable: a further request that references
        # the dynamic table entry established by the rejected header block
        # decodes normally.
        f = frame_factory.build_headers_frame(
            stream_id=7,
            headers=oversized_headers,
        )
        data = f.serialize()

        events = c.receive_data(data)

        assert len(events) == 1
        event = events[0]
        assert isinstance(event, h2.events.RequestReceived)
        assert event.stream_id == 7
        assert event.headers == oversized_headers

    def test_reject_headers_when_table_size_shrunk(self, frame_factory) -> None:
        """
        When we've shrunk the header table size, we reject header blocks that
        do not respect the change.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.receive_data(frame_factory.preamble())
        c.clear_outbound_data_buffer()

        # Receive the first request, which causes no problem.
        f = frame_factory.build_headers_frame(
            stream_id=1,
            headers=self.request_header_block,
        )
        data = f.serialize()
        c.receive_data(data)

        # Now, send a settings change. It's un-ACKed at this time. A new
        # request arrives, also without incident.
        c.update_settings({h2.settings.SettingCodes.HEADER_TABLE_SIZE: 128})
        c.clear_outbound_data_buffer()
        f = frame_factory.build_headers_frame(
            stream_id=3,
            headers=self.request_header_block,
        )
        data = f.serialize()
        c.receive_data(data)

        # We get a SETTINGS ACK.
        f = frame_factory.build_settings_frame({}, ack=True)
        data = f.serialize()
        c.receive_data(data)

        # Now a third request comes in. This explodes, as it does not contain
        # a dynamic table size update: a compression error, which is a
        # connection error of type COMPRESSION_ERROR.
        f = frame_factory.build_headers_frame(
            stream_id=5,
            headers=self.request_header_block,
        )
        data = f.serialize()

        with pytest.raises(h2.exceptions.CompressionError):
            c.receive_data(data)

        expected_frame = frame_factory.build_goaway_frame(
            last_stream_id=3, error_code=h2.errors.ErrorCodes.COMPRESSION_ERROR,
        )
        assert c.data_to_send() == expected_frame.serialize()

    def test_reject_headers_exceeding_table_size(self, frame_factory) -> None:
        """
        When the remote peer sends a dynamic table size update that exceeds our
        setting, we reject it.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.receive_data(frame_factory.preamble())
        c.clear_outbound_data_buffer()

        # Receive the first request, which causes no problem.
        f = frame_factory.build_headers_frame(
            stream_id=1,
            headers=self.request_header_block,
        )
        data = f.serialize()
        c.receive_data(data)

        # Now a second request comes in that sets the table size too high.
        # This explodes: a compression error, which is a connection error of
        # type COMPRESSION_ERROR.
        frame_factory.change_table_size(c.local_settings.header_table_size + 1)
        f = frame_factory.build_headers_frame(
            stream_id=5,
            headers=self.request_header_block,
        )
        data = f.serialize()

        with pytest.raises(h2.exceptions.CompressionError):
            c.receive_data(data)

        expected_frame = frame_factory.build_goaway_frame(
            last_stream_id=1, error_code=h2.errors.ErrorCodes.COMPRESSION_ERROR,
        )
        assert c.data_to_send() == expected_frame.serialize()


class TestBoundedHeaderListRejection:
    """
    Tests for the bounded acceptance path for oversized header lists.

    A single stream whose decompressed header list exceeds the locally
    advertised SETTINGS_MAX_HEADER_LIST_SIZE is rejected at the stream level:
    the header block is still fully decoded (keeping the HPACK dynamic table
    synchronised with the peer), the stream is reset, and the connection
    remains usable. This is distinct from a HPACK compression error, which
    remains a connection error.
    """

    request_header_block = [
        (b":method", b"GET"),
        (b":authority", b"example.com"),
        (b":scheme", b"https"),
        (b":path", b"/"),
    ]

    response_header_block = [
        (b":status", b"200"),
    ]

    server_config = h2.config.H2Configuration(client_side=False)

    # A header block that inflates past the default limit of 2**16, but stays
    # well below the hard bound applied to the rejection path.
    oversized_header_block = request_header_block + [
        (b"x-padding", b"x" * 2**16),
    ]

    @staticmethod
    def _serialize_split(first_frame, encoded, frame_factory, chunk_size=2**13):
        """
        Serialize an encoded header block as a leading HEADERS or
        PUSH_PROMISE frame followed by as many CONTINUATION frames as needed
        to respect the maximum frame size.
        """
        first_frame.data = encoded[:chunk_size]
        rest = encoded[chunk_size:]
        frames = [first_frame]
        while len(rest) > chunk_size:
            frames.append(
                frame_factory.build_continuation_frame(
                    rest[:chunk_size], stream_id=first_frame.stream_id,
                ),
            )
            rest = rest[chunk_size:]
        frames.append(
            frame_factory.build_continuation_frame(
                rest, flags=["END_HEADERS"], stream_id=first_frame.stream_id,
            ),
        )
        return b"".join(f.serialize() for f in frames)

    def build_split_headers(self, frame_factory, headers, stream_id):
        """
        Build a HEADERS + CONTINUATION chain for a header block whose encoded
        form exceeds the maximum frame size.
        """
        encoded = frame_factory.encoder.encode(headers)
        return self._serialize_split(
            hyperframe.frame.HeadersFrame(stream_id), encoded, frame_factory,
        )

    def test_oversized_request_rejected_at_stream_level(self, frame_factory) -> None:
        """
        An oversized request header block rejects only its stream: a
        StreamHeadersRejected event is emitted, the stream is reset with
        ENHANCE_YOUR_CALM, and no exception or GOAWAY occurs.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.receive_data(frame_factory.preamble())
        c.clear_outbound_data_buffer()

        data = self.build_split_headers(
            frame_factory, self.oversized_header_block, stream_id=1,
        )
        events = c.receive_data(data)

        assert len(events) == 1
        event = events[0]
        assert isinstance(event, h2.events.StreamHeadersRejected)
        assert event.stream_id == 1
        assert event.error_code == h2.errors.ErrorCodes.ENHANCE_YOUR_CALM
        assert event.reason

        expected_frame = frame_factory.build_rst_stream_frame(
            1, h2.errors.ErrorCodes.ENHANCE_YOUR_CALM,
        )
        assert c.data_to_send() == expected_frame.serialize()

        # The connection remains usable for subsequent streams.
        f = frame_factory.build_headers_frame(
            stream_id=3,
            headers=self.request_header_block,
        )
        events = c.receive_data(f.serialize())

        assert len(events) == 1
        event = events[0]
        assert isinstance(event, h2.events.RequestReceived)
        assert event.stream_id == 3
        assert event.headers == self.request_header_block

    def test_oversized_headers_split_across_continuations(self, frame_factory) -> None:
        """
        An oversized header block split across a HEADERS frame and a chain of
        CONTINUATION frames is processed to its protocol boundary before
        being rejected: the whole chain is decoded, keeping the dynamic table
        synchronised, and only the stream is reset.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.receive_data(frame_factory.preamble())
        c.clear_outbound_data_buffer()

        # The custom header is added to the dynamic table by the encoder, so
        # the rejected block establishes state that a later block references.
        headers = [
            *self.request_header_block,
            (b"x-custom-header", b"some-value"),
            (b"x-padding", b"x" * 2**16),
        ]
        data = self.build_split_headers(frame_factory, headers, stream_id=1)

        events = c.receive_data(data)

        assert len(events) == 1
        event = events[0]
        assert isinstance(event, h2.events.StreamHeadersRejected)
        assert event.stream_id == 1
        assert event.error_code == h2.errors.ErrorCodes.ENHANCE_YOUR_CALM

        expected_frame = frame_factory.build_rst_stream_frame(
            1, h2.errors.ErrorCodes.ENHANCE_YOUR_CALM,
        )
        assert c.data_to_send() == expected_frame.serialize()

        # A subsequent header block that references the dynamic table entry
        # established by the rejected block still decodes.
        f = frame_factory.build_headers_frame(
            stream_id=3,
            headers=[*self.request_header_block, (b"x-custom-header", b"some-value")],
        )
        events = c.receive_data(f.serialize())

        assert len(events) == 1
        event = events[0]
        assert isinstance(event, h2.events.RequestReceived)
        assert event.stream_id == 3
        assert event.headers == [
            *self.request_header_block,
            (b"x-custom-header", b"some-value"),
        ]

    def test_oversized_response_rejected_at_stream_level(self, frame_factory) -> None:
        """
        An oversized response header block rejects only its stream on the
        client side.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(stream_id=1, headers=self.request_header_block)
        c.send_headers(stream_id=3, headers=self.request_header_block)
        c.clear_outbound_data_buffer()

        oversized_response = [
            (b":status", b"200"),
            (b"x-padding", b"x" * 2**16),
        ]
        data = self.build_split_headers(
            frame_factory, oversized_response, stream_id=1,
        )
        events = c.receive_data(data)

        assert len(events) == 1
        event = events[0]
        assert isinstance(event, h2.events.StreamHeadersRejected)
        assert event.stream_id == 1
        assert event.error_code == h2.errors.ErrorCodes.ENHANCE_YOUR_CALM
        assert event.reason

        expected_frame = frame_factory.build_rst_stream_frame(
            1, h2.errors.ErrorCodes.ENHANCE_YOUR_CALM,
        )
        assert c.data_to_send() == expected_frame.serialize()

        # The connection remains usable: a response on another stream, which
        # the encoder emits using the dynamic table, decodes normally.
        f = frame_factory.build_headers_frame(
            stream_id=3,
            headers=self.response_header_block,
        )
        events = c.receive_data(f.serialize())

        assert len(events) == 1
        event = events[0]
        assert isinstance(event, h2.events.ResponseReceived)
        assert event.stream_id == 3
        assert event.headers == self.response_header_block

    def test_oversized_push_promise_rejected_at_stream_level(self, frame_factory) -> None:
        """
        An oversized PUSH_PROMISE header block rejects only the promised
        stream: the promised stream is reset, and subsequent header blocks
        still decode.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(stream_id=1, headers=self.request_header_block)
        c.clear_outbound_data_buffer()

        oversized_promise = [
            *self.request_header_block,
            (b"x-custom-header", b"some-value"),
            (b"x-padding", b"x" * 2**16),
        ]
        encoded = frame_factory.encoder.encode(oversized_promise)
        promise_frame = hyperframe.frame.PushPromiseFrame(stream_id=1)
        promise_frame.promised_stream_id = 2
        data = self._serialize_split(promise_frame, encoded, frame_factory)
        events = c.receive_data(data)

        assert len(events) == 1
        event = events[0]
        assert isinstance(event, h2.events.StreamHeadersRejected)
        assert event.stream_id == 2
        assert event.error_code == h2.errors.ErrorCodes.ENHANCE_YOUR_CALM
        assert event.reason

        expected_frame = frame_factory.build_rst_stream_frame(
            2, h2.errors.ErrorCodes.ENHANCE_YOUR_CALM,
        )
        assert c.data_to_send() == expected_frame.serialize()

        # A subsequent push promise that references dynamic table entries
        # established by the rejected block still decodes.
        pushed_headers = [
            *self.request_header_block,
            (b"x-custom-header", b"some-value"),
        ]
        f = frame_factory.build_push_promise_frame(
            stream_id=1,
            promised_stream_id=4,
            headers=pushed_headers,
        )
        events = c.receive_data(f.serialize())

        assert len(events) == 1
        event = events[0]
        assert isinstance(event, h2.events.PushedStreamReceived)
        assert event.pushed_stream_id == 4
        assert event.headers == pushed_headers

    def test_compression_error_remains_connection_error(self, frame_factory) -> None:
        """
        A header block that cannot be decoded at all is a compression error:
        a connection error of type COMPRESSION_ERROR, distinct from the
        stream-level rejection of oversized header lists.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.receive_data(frame_factory.preamble())
        c.clear_outbound_data_buffer()

        # An indexed header field referring to a dynamic table entry that
        # does not exist cannot be decoded.
        f = hyperframe.frame.HeadersFrame(stream_id=1)
        f.data = b"\xbe"
        f.flags.add("END_HEADERS")

        with pytest.raises(h2.exceptions.CompressionError) as exc_info:
            c.receive_data(f.serialize())

        assert isinstance(exc_info.value, h2.exceptions.ProtocolError)
        assert exc_info.value.error_code == h2.errors.ErrorCodes.COMPRESSION_ERROR

        expected_frame = frame_factory.build_goaway_frame(
            last_stream_id=0, error_code=h2.errors.ErrorCodes.COMPRESSION_ERROR,
        )
        assert c.data_to_send() == expected_frame.serialize()
