#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""
Tests for reproducible (deterministic) WARC writing:

* pluggable, per-writer record ID and date providers
* explicit rejection of duplicate IDs and invalid dates
* fixed gzip compression metadata
* preservation of original header order and duplicate headers
* request/response pairs and warcinfo going through the same providers
* unchanged default (random id / current time) behavior
* configuration isolation between writers
"""

import re

from io import BytesIO

import pytest

from warcio.archiveiterator import ArchiveIterator
from warcio.exceptions import (DuplicateRecordIDError, InvalidWARCDateError,
                               WARCWriterError)
from warcio.recordbuilder import (RecordBuilder, StaticRecordIDProvider,
                                  StaticDateProvider, UUID4RecordIDProvider,
                                  CurrentDateProvider)
from warcio.statusandheaders import StatusAndHeaders
from warcio.warcwriter import BufferWARCWriter, WARCWriter, GzippingWrapper


# ============================================================================
# helpers
# ============================================================================
FIXED_IDS = [
    '<urn:uuid:00000000-0000-0000-0000-000000000001>',
    '<urn:uuid:00000000-0000-0000-0000-000000000002>',
    '<urn:uuid:00000000-0000-0000-0000-000000000003>',
    '<urn:uuid:00000000-0000-0000-0000-000000000004>',
    '<urn:uuid:00000000-0000-0000-0000-000000000005>',
    '<urn:uuid:00000000-0000-0000-0000-000000000006>',
]
FIXED_DATE = '2020-01-02T03:04:05Z'


def make_writer(gzip=True, **kwargs):
    kwargs.setdefault('id_provider', StaticRecordIDProvider(list(FIXED_IDS)))
    kwargs.setdefault('date_provider', StaticDateProvider(FIXED_DATE))
    return BufferWARCWriter(gzip=gzip, **kwargs)


def make_response(writer, url='http://example.com/'):
    http_headers = StatusAndHeaders('200 OK',
                                    [('Content-Type', 'text/plain'),
                                     ('X-One', 'a')],
                                    protocol='HTTP/1.0')
    return writer.create_warc_record(url, 'response',
                                     payload=BytesIO(b'some\ntext'),
                                     length=9,
                                     http_headers=http_headers)


def make_request(writer, url='http://example.com/'):
    http_headers = StatusAndHeaders('GET / HTTP/1.0',
                                    [('Host', 'example.com')],
                                    is_http_request=True)
    return writer.create_warc_record(url, 'request',
                                     http_headers=http_headers)


def build_archive(gzip, **writer_kwargs):
    writer = make_writer(gzip=gzip, **writer_kwargs)
    warcinfo = writer.create_warcinfo_record(
        'test.warc.gz', {'software': 'test', 'format': 'WARC File Format 1.0'})
    writer.write_record(warcinfo)

    resp = make_response(writer)
    req = make_request(writer)
    writer.write_request_response_pair(req, resp)
    return writer.get_contents()


# ============================================================================
# determinism
# ============================================================================
class TestReproducibleOutput(object):
    @pytest.mark.parametrize('gzip', [True, False])
    def test_same_input_same_bytes(self, gzip):
        first = build_archive(gzip=gzip)
        second = build_archive(gzip=gzip)
        assert first == second
        assert len(first) > 0

    def test_gzip_and_plain_contain_same_records(self):
        plain = build_archive(gzip=False)
        gz = build_archive(gzip=True)

        plain_records = list(ArchiveIterator(BytesIO(plain)))
        gz_records = list(ArchiveIterator(BytesIO(gz)))

        assert len(plain_records) == len(gz_records) == 3
        for a, b in zip(plain_records, gz_records):
            assert a.rec_type == b.rec_type
            assert (a.rec_headers.get_header('WARC-Record-ID') ==
                    b.rec_headers.get_header('WARC-Record-ID'))
            assert (a.rec_headers.get_header('WARC-Date') ==
                    b.rec_headers.get_header('WARC-Date'))

    def test_providers_apply_to_warcinfo_and_req_resp(self):
        contents = build_archive(gzip=False)
        records = list(ArchiveIterator(BytesIO(contents)))

        assert [r.rec_type for r in records] == ['warcinfo', 'response', 'request']

        ids = [r.rec_headers.get_header('WARC-Record-ID') for r in records]
        assert ids == FIXED_IDS[:3]

        for record in records:
            assert record.rec_headers.get_header('WARC-Date') == FIXED_DATE

        # request/response pairing uses the response id
        resp, req = records[1], records[2]
        assert req.rec_headers.get_header('WARC-Concurrent-To') == \
            resp.rec_headers.get_header('WARC-Record-ID')

    def test_plain_callables_are_supported(self):
        ids = iter(['<urn:uuid:aaa>', '<urn:uuid:bbb>'])
        writer = BufferWARCWriter(
            gzip=False,
            id_provider=lambda: next(ids),
            date_provider=lambda: '2021-06-07T08:09:10Z')

        writer.write_record(make_response(writer))
        record = list(ArchiveIterator(BytesIO(writer.get_contents())))[0]
        assert record.rec_headers.get_header('WARC-Record-ID') == '<urn:uuid:aaa>'
        assert record.rec_headers.get_header('WARC-Date') == '2021-06-07T08:09:10Z'

    def test_static_provider_sequence(self):
        provider = StaticRecordIDProvider(['<urn:uuid:x>', '<urn:uuid:y>'])
        assert provider() == '<urn:uuid:x>'
        assert provider() == '<urn:uuid:y>'
        with pytest.raises(IndexError):
            provider()

        single = StaticRecordIDProvider('<urn:uuid:z>')
        assert single() == '<urn:uuid:z>'

        dates = StaticDateProvider(['2020-01-01T00:00:00Z',
                                    '2020-01-02T00:00:00Z'])
        assert dates() == '2020-01-01T00:00:00Z'
        assert dates() == '2020-01-02T00:00:00Z'
        # last value repeats once the sequence is exhausted
        assert dates() == '2020-01-02T00:00:00Z'

    def test_fractional_second_dates_accepted(self):
        writer = make_writer(gzip=False,
                             date_provider=StaticDateProvider(
                                 '2020-01-02T03:04:05.123456Z'))
        writer.write_record(make_response(writer))
        record = list(ArchiveIterator(BytesIO(writer.get_contents())))[0]
        assert record.rec_headers.get_header('WARC-Date') == \
            '2020-01-02T03:04:05.123456Z'


# ============================================================================
# gzip metadata
# ============================================================================
class TestGzipMetadata(object):
    def test_fixed_gzip_mtime_and_filename(self):
        import zlib

        contents = build_archive(gzip=True, gzip_mtime=1234567890,
                                gzip_filename='fixed.warc.gz', gzip_os=255)

        # walk the three independent gzip members and check each header
        offset = 0
        member_count = 0
        while offset < len(contents):
            member = contents[offset:]
            assert member[:2] == b'\x1f\x8b'
            flg = member[3]
            assert flg & 0x08  # FNAME present
            assert int.from_bytes(member[4:8], 'little') == 1234567890
            assert member[9] == 255  # OS
            fname_end = member.index(b'\x00', 10)
            assert member[10:fname_end] == b'fixed.warc.gz'

            # find exact member boundary from the decompressor's unused_data
            d = zlib.decompressobj(zlib.MAX_WBITS + 16)
            d.decompress(member)
            member_len = len(member) - len(d.unused_data)
            offset += member_len
            member_count += 1

        assert member_count == 3

        # reproducible with fixed metadata
        again = build_archive(gzip=True, gzip_mtime=1234567890,
                              gzip_filename='fixed.warc.gz', gzip_os=255)
        assert again == contents

    def test_default_gzip_header_has_zero_mtime_no_fname(self):
        contents = build_archive(gzip=True)
        assert contents[:2] == b'\x1f\x8b'
        assert contents[3] == 0          # no flags -> no FNAME
        assert contents[4:8] == b'\x00\x00\x00\x00'  # mtime 0

    def test_invalid_gzip_mtime_rejected(self):
        with pytest.raises(ValueError):
            GzippingWrapper(BytesIO(), mtime=-1)
        with pytest.raises(ValueError):
            GzippingWrapper(BytesIO(), mtime=2 ** 32)
        with pytest.raises(ValueError):
            GzippingWrapper(BytesIO(), filename=b'not-a-string')
        with pytest.raises(ValueError):
            GzippingWrapper(BytesIO(), filename='bad\x00name')

    def test_gzip_members_remain_independently_decompressible(self):
        contents = build_archive(gzip=True, gzip_mtime=42, gzip_os=255)
        records = list(ArchiveIterator(BytesIO(contents)))
        assert len(records) == 3


# ============================================================================
# duplicate IDs and invalid dates must be explicitly rejected
# ============================================================================
class TestProviderValidation(object):
    def test_duplicate_id_from_provider_rejected(self):
        writer = BufferWARCWriter(
            gzip=False,
            id_provider=StaticRecordIDProvider(
                ['<urn:uuid:same>', '<urn:uuid:same>']),
            date_provider=StaticDateProvider(FIXED_DATE))

        writer.write_record(make_response(writer))
        with pytest.raises(DuplicateRecordIDError) as exc:
            writer.write_record(make_request(writer))
        assert '<urn:uuid:same>' in str(exc.value)
        assert isinstance(exc.value, WARCWriterError)
        assert isinstance(exc.value, ValueError)

    def test_single_static_id_exhausted_raises_explicitly(self):
        # a one-shot provider can not silently reuse its only id: running out
        # of ids raises rather than producing a second (duplicate) record id
        writer = make_writer(gzip=False,
                             id_provider=StaticRecordIDProvider('<urn:uuid:only>'))
        writer.create_warc_record('http://example.com/a', 'resource',
                                  payload=BytesIO(b'a'), length=1)
        with pytest.raises(IndexError):
            writer.create_warc_record('http://example.com/b', 'resource',
                                      payload=BytesIO(b'b'), length=1)

    def test_explicit_duplicate_id_rejected(self):
        writer = make_writer(gzip=False)
        writer.create_warc_record(
            'http://example.com/a', 'resource',
            payload=BytesIO(b'a'), length=1,
            warc_headers_dict={'WARC-Record-ID': '<urn:uuid:dup>'})
        with pytest.raises(DuplicateRecordIDError):
            writer.create_warc_record(
                'http://example.com/b', 'resource',
                payload=BytesIO(b'b'), length=1,
                warc_headers_dict={'WARC-Record-ID': '<urn:uuid:dup>'})

    def test_empty_id_rejected(self):
        writer = BufferWARCWriter(gzip=False,
                                  id_provider=lambda: '',
                                  date_provider=StaticDateProvider(FIXED_DATE))
        with pytest.raises(DuplicateRecordIDError):
            writer.write_record(make_response(writer))

    @pytest.mark.parametrize('bad', [
        'not-a-date',
        '2020-13-01T00:00:00Z',       # impossible month
        '2020-02-30T00:00:00Z',       # impossible day
        '2020-01-01T25:00:00Z',       # impossible hour
        '2020/01/01 00:00:00',        # wrong shape
        None,
        12345,
        '2020-01-01T00:00:00',        # missing trailing Z
    ])
    def test_invalid_dates_rejected(self, bad):
        writer = BufferWARCWriter(gzip=False,
                                  date_provider=StaticDateProvider(bad))
        with pytest.raises(InvalidWARCDateError) as exc:
            writer.write_record(make_response(writer))
        assert isinstance(exc.value, WARCWriterError)
        assert isinstance(exc.value, ValueError)

    def test_builder_directly_validates(self):
        builder = RecordBuilder(
            id_provider=StaticRecordIDProvider(['<urn:uuid:a>', '<urn:uuid:a>']),
            date_provider=StaticDateProvider(FIXED_DATE))
        builder._make_warc_id()
        with pytest.raises(DuplicateRecordIDError):
            builder._make_warc_id()

        bad_builder = RecordBuilder(date_provider=lambda: 'nope')
        with pytest.raises(InvalidWARCDateError):
            bad_builder._make_warc_date()


# ============================================================================
# header order and duplicate headers are preserved
# ============================================================================
class TestHeaderOrderPreserved(object):
    def test_duplicate_and_ordered_headers_not_sorted_or_merged(self):
        writer = make_writer(gzip=False)
        http_headers = StatusAndHeaders('200 OK', [
            ('X-Zebra', '1'),
            ('Set-Cookie', 'a=1'),
            ('X-Apple', '2'),
            ('Set-Cookie', 'b=2'),       # duplicate header must be retained
            ('X-Mango', '3'),
        ], protocol='HTTP/1.0')

        record = writer.create_warc_record(
            'http://example.com/', 'response',
            payload=BytesIO(b'payload'), length=7,
            http_headers=http_headers)
        writer.write_record(record)

        raw = writer.get_contents().decode('utf-8')

        # both Set-Cookie headers survive, in original order
        assert raw.index('Set-Cookie: a=1') < raw.index('Set-Cookie: b=2')
        assert raw.count('Set-Cookie:') == 2

        # original relative order is not alphabetically sorted
        assert raw.index('X-Zebra') < raw.index('X-Apple') < raw.index('X-Mango')

        # round-trip through the parser keeps duplicates
        parsed = list(ArchiveIterator(BytesIO(writer.get_contents())))[0]
        cookies = [v for n, v in parsed.http_headers.headers
                   if n.lower() == 'set-cookie']
        assert cookies == ['a=1', 'b=2']
        names = [n for n, _ in parsed.http_headers.headers]
        assert names == ['X-Zebra', 'Set-Cookie', 'X-Apple',
                         'Set-Cookie', 'X-Mango']


# ============================================================================
# default behavior unchanged and writer isolation
# ============================================================================
class TestDefaultsAndIsolation(object):
    def test_default_ids_are_random_and_unique(self):
        writer = BufferWARCWriter(gzip=False)
        writer.write_record(make_response(writer))
        writer.write_record(make_request(writer))
        records = list(ArchiveIterator(BytesIO(writer.get_contents())))

        ids = [r.rec_headers.get_header('WARC-Record-ID') for r in records]
        assert ids[0] != ids[1]
        assert all(re.match(r'^<urn:uuid:[0-9a-f-]{36}>$', i) for i in ids)

    def test_default_dates_are_current_iso_utc(self):
        writer = BufferWARCWriter(gzip=False)
        writer.write_record(make_response(writer))
        record = list(ArchiveIterator(BytesIO(writer.get_contents())))[0]
        assert re.match(r'^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$',
                        record.rec_headers.get_header('WARC-Date'))

    def test_default_warc_1_1_keeps_microseconds(self):
        writer = BufferWARCWriter(gzip=False, warc_version='1.1')
        writer.write_record(make_response(writer))
        record = list(ArchiveIterator(BytesIO(writer.get_contents())))[0]
        date = record.rec_headers.get_header('WARC-Date')
        assert '.' in date and date.endswith('Z')

    def test_two_writers_do_not_share_state(self):
        first = make_writer(gzip=False)
        second = make_writer(gzip=False)

        # each writer allocates its own id sequence from the start
        first.write_record(make_response(first))
        second.write_record(make_response(second))
        first.write_record(make_request(first))
        second.write_record(make_request(second))

        first_records = list(ArchiveIterator(BytesIO(first.get_contents())))
        second_records = list(ArchiveIterator(BytesIO(second.get_contents())))

        first_ids = [r.rec_headers.get_header('WARC-Record-ID')
                     for r in first_records]
        second_ids = [r.rec_headers.get_header('WARC-Record-ID')
                      for r in second_records]

        # identical per-writer sequences, proving independent counters
        assert first_ids == second_ids == FIXED_IDS[:2]
        assert first.get_contents() == second.get_contents()

    def test_provider_instances_are_not_shared_globally(self):
        writer_a = BufferWARCWriter(gzip=False)
        writer_b = BufferWARCWriter(gzip=False)
        assert writer_a.id_provider is not writer_b.id_provider
        assert writer_a.date_provider is not writer_b.date_provider
        assert writer_a._used_record_ids is not writer_b._used_record_ids

    def test_default_provider_classes(self):
        assert isinstance(BufferWARCWriter(gzip=False).id_provider,
                          UUID4RecordIDProvider)
        assert isinstance(BufferWARCWriter(gzip=False).date_provider,
                          CurrentDateProvider)

    def test_recordbuilder_accepts_providers(self):
        builder = RecordBuilder(
            id_provider=StaticRecordIDProvider('<urn:uuid:b>'),
            date_provider=StaticDateProvider(FIXED_DATE))
        assert builder._make_warc_id() == '<urn:uuid:b>'
        assert builder._make_warc_date() == FIXED_DATE

    def test_warc_writer_to_real_file(self, tmp_path):
        path = tmp_path / 'out.warc'
        with open(str(path), 'wb') as fh:
            writer = WARCWriter(fh, gzip=False,
                                id_provider=StaticRecordIDProvider(list(FIXED_IDS)),
                                date_provider=StaticDateProvider(FIXED_DATE))
            writer.write_record(make_response(writer))

        records = list(ArchiveIterator(open(str(path), 'rb')))
        assert records[0].rec_headers.get_header('WARC-Date') == FIXED_DATE

    def test_request_response_pair_uses_same_provider(self):
        writer = make_writer(gzip=False)
        resp = make_response(writer)
        req = make_request(writer)
        writer.write_request_response_pair(req, resp)

        records = list(ArchiveIterator(BytesIO(writer.get_contents())))
        resp_rec, req_rec = records
        # both dates come from the single date provider value
        assert (resp_rec.rec_headers.get_header('WARC-Date') ==
                req_rec.rec_headers.get_header('WARC-Date') == FIXED_DATE)
        # distinct ids from the single id provider sequence
        assert (resp_rec.rec_headers.get_header('WARC-Record-ID') !=
                req_rec.rec_headers.get_header('WARC-Record-ID'))
