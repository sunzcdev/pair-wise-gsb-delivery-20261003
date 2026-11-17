# SPDX-License-Identifier: MIT OR Apache-2.0
# This file is dual licensed under the terms of the Apache License, Version
# 2.0, and the MIT License.  See the LICENSE file in the root of this
# repository for complete details.

from __future__ import annotations

import datetime as dt

import pytest

from structlog.syslog import RFC5424Renderer


@pytest.fixture(name="renderer")
def _renderer():
    return RFC5424Renderer(
        hostname="myhost", app_name="myapp", procid="1234"
    )


def _render(renderer, event_dict):
    return renderer(None, "", event_dict)


class TestHeader:
    def test_full_record(self, renderer):
        """
        All header fields, structured data, and a BOM-prefixed message are
        rendered in RFC 5424 order.
        """
        record = _render(
            renderer,
            {
                "event": "hello",
                "timestamp": "2026-11-18T10:00:00Z",
                "msgid": "ID42",
                "severity": 3,
                "facility": 4,
                "answer": 42,
            },
        )

        assert (
            b'<35>1 2026-11-18T10:00:00Z myhost myapp 1234 ID42 '
            b'[structlog answer="42"] \xef\xbb\xbfhello'
        ) == record

    def test_missing_fields_become_nilvalue(self, renderer):
        """
        Missing timestamp and msgid, and no structured data, are rendered
        as the NILVALUE "-".
        """
        record = _render(renderer, {"event": "hi"})

        assert record.startswith(b"<14>1 - myhost myapp 1234 - - ")

    def test_literal_dash_stays_nilvalue(self, renderer):
        """
        A literal "-" value is kept as the NILVALUE instead of being
        treated as an invalid field.
        """
        record = _render(
            renderer,
            {
                "event": "hi",
                "timestamp": "-",
                "hostname": "-",
                "app_name": "-",
                "procid": "-",
                "msgid": "-",
            },
        )

        assert record.startswith(b"<14>1 - - - - - - ")

    def test_no_message_no_msg_part(self, renderer):
        """
        Without an event key, the record ends after the structured data.
        """
        assert b"<14>1 - myhost myapp 1234 - -" == _render(renderer, {})

    def test_datetime_timestamp_is_utc_normalized(self, renderer):
        """
        Naive datetimes are assumed to be UTC; aware ones keep their
        offset.
        """
        record = _render(
            renderer,
            {"timestamp": dt.datetime(2026, 11, 18, 10, 0, 0, 3000)},
        )

        assert record.startswith(b"<14>1 2026-11-18T10:00:00.003000Z ")

        record = _render(
            renderer,
            {
                "timestamp": dt.datetime(
                    2026,
                    11,
                    18,
                    10,
                    0,
                    tzinfo=dt.timezone(dt.timedelta(hours=1)),
                )
            },
        )

        assert record.startswith(b"<14>1 2026-11-18T10:00:00+01:00 ")

    def test_posix_timestamp(self, renderer):
        """
        Numeric timestamps are rendered as UTC RFC 3339.
        """
        record = _render(renderer, {"timestamp": 0})

        assert record.startswith(b"<14>1 1970-01-01T00:00:00Z ")

    def test_severity_from_level(self, renderer):
        """
        Without an explicit severity, it is derived from the level key.
        """
        # warning -> 4, facility 1 -> 1 * 8 + 4 = 12
        assert _render(renderer, {"level": "warning"}).startswith(b"<12>1 ")

    def test_default_severity_is_informational(self, renderer):
        """
        Without severity or level, the severity defaults to 6
        (informational).
        """
        assert _render(renderer, {}).startswith(b"<14>1 ")

    def test_defaults_from_constructor(self):
        """
        Constructor defaults are used when the event provides nothing.
        """
        renderer = RFC5424Renderer(
            facility=2,
            severity=1,
            hostname="h",
            app_name="a",
            procid="p",
            msgid="m",
        )

        assert _render(renderer, {}).startswith(b"<17>1 - h a p m ")

    @pytest.mark.parametrize(
        "key",
        ["hostname", "app_name", "procid", "msgid"],
    )
    @pytest.mark.parametrize(
        "value", ["has space", "has\ttab", "nonascii-ä", "x" * 300]
    )
    def test_invalid_header_fields_rejected(self, renderer, key, value):
        """
        Header fields that violate the PRINTUSASCII grammar or their
        length limit are rejected before anything is rendered.
        """
        with pytest.raises(ValueError, match="Invalid RFC 5424"):
            _render(renderer, {key: value})

    def test_invalid_timestamp_rejected(self, renderer):
        """
        Timestamps containing non-printable characters are rejected.
        """
        with pytest.raises(ValueError, match="Invalid RFC 5424 TIMESTAMP"):
            _render(renderer, {"timestamp": "bad timestamp"})

    @pytest.mark.parametrize("facility", [-1, 24, "1"])
    def test_invalid_facility_rejected(self, facility):
        """
        Facilities outside 0-23 are rejected.
        """
        with pytest.raises(ValueError, match="Invalid RFC 5424 facility"):
            RFC5424Renderer(facility=facility)

    @pytest.mark.parametrize("severity", [-1, 8, "1"])
    def test_invalid_severity_rejected(self, severity):
        """
        Severities outside 0-7 are rejected.
        """
        with pytest.raises(ValueError, match="Invalid RFC 5424 severity"):
            RFC5424Renderer(severity=severity)

    def test_invalid_event_facility_rejected(self, renderer):
        """
        A facility from the event dict is validated too.
        """
        with pytest.raises(ValueError, match="Invalid RFC 5424 facility"):
            _render(renderer, {"facility": 42})


class TestStructuredData:
    def test_escapes_param_values(self, renderer):
        r"""
        Backslashes, double quotes, and closing brackets in PARAM-VALUEs
        are escaped.
        """
        record = _render(
            renderer, {"tricky": 'a\\b"c]d', "event": "x"}
        )

        assert b'tricky="a\\\\b\\"c\\]d"' in record

    def test_reserved_keys_not_in_sd(self, renderer):
        """
        Keys consumed by the header don't show up as structured data.
        """
        record = _render(
            renderer,
            {
                "event": "x",
                "timestamp": "2026-11-18T10:00:00Z",
                "hostname": "h",
                "app_name": "a",
                "procid": "p",
                "msgid": "m",
                "severity": 1,
                "facility": 2,
                "level": "error",
            },
        )

        assert b"[structlog]" not in record
        assert record.startswith(b"<17>1 2026-11-18T10:00:00Z h a p m - ")

    @pytest.mark.parametrize("key", ["has space", 'quote"', "bracket]", "back\\slash"])
    def test_invalid_param_name_rejected(self, renderer, key):
        """
        Keys that are not valid PARAM-NAMEs are rejected.
        """
        with pytest.raises(ValueError, match="Invalid RFC 5424"):
            _render(renderer, {key: "v"})

    def test_invalid_sd_id_rejected(self):
        """
        An SD-ID containing forbidden characters is rejected.
        """
        with pytest.raises(ValueError, match="Invalid RFC 5424"):
            RFC5424Renderer(sd_id="bad id")


class TestMessage:
    def test_bom_prefixes_utf8_message(self, renderer):
        """
        A Unicode message is prefixed with the BOM and the whole record is
        UTF-8-encoded.
        """
        record = _render(renderer, {"event": "héllo"})

        assert record.endswith("\ufeffhéllo".encode())

    def test_bom_can_be_disabled(self, renderer):
        """
        include_bom=False omits the byte order mark.
        """
        renderer = RFC5424Renderer(
            hostname="h", app_name="a", procid="1", include_bom=False
        )

        record = _render(renderer, {"event": "hi"})

        assert record.endswith(b" hi")
        assert b"\xef\xbb\xbf" not in record

    def test_newlines_escaped_without_octet_counting(self, renderer):
        """
        Without octet counting, newlines in the message are escaped so one
        event always occupies exactly one line.
        """
        record = _render(renderer, {"event": "line1\nline2\r\nline3"})

        assert b"\n" not in record
        assert b"\r" not in record
        assert record.endswith(b"line1\\nline2\\r\\nline3")


class TestOctetCounting:
    @pytest.fixture(name="oc_renderer")
    def _oc_renderer(self, renderer):
        return RFC5424Renderer(
            hostname="myhost",
            app_name="myapp",
            procid="1234",
            octet_counting=True,
        )

    def test_length_prefix_matches_final_bytes(self, oc_renderer):
        """
        The length prefix is the exact octet count of the record that
        follows it, including multi-byte UTF-8 characters and the BOM.
        """
        record = _render(oc_renderer, {"event": "héllo"})

        length, _, message = record.partition(b" ")

        assert int(length) == len(message)

    def test_newlines_do_not_split_records(self, oc_renderer):
        """
        Two consecutive octet-counted records stay exactly two records,
        even if a message contains newlines.
        """
        stream = _render(oc_renderer, {"event": "one\ntwo"}) + _render(
            oc_renderer, {"event": "three"}
        )

        records = []
        while stream:
            length, _, stream = stream.partition(b" ")
            records.append(stream[: int(length)])
            stream = stream[int(length) :]

        assert 2 == len(records)
        assert records[0].endswith("\ufeffone\ntwo".encode())
        assert records[1].endswith("\ufeffthree".encode())


class TestEventDictImmutability:
    def test_event_dict_not_modified(self, renderer):
        """
        The passed event dict is left untouched.
        """
        event_dict = {
            "event": "hi",
            "timestamp": "2026-11-18T10:00:00Z",
            "hostname": "h",
            "extra": "value",
        }
        original = dict(event_dict)

        _render(renderer, event_dict)

        assert original == event_dict
