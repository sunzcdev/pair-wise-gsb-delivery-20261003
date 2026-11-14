from warcio.archiveiterator import ArchiveIterator
from warcio.exceptions import ArchiveLoadFailed, StrictLengthError
from warcio.recordloader import ArcWarcRecordLoader
from warcio.warcwriter import BufferWARCWriter
from warcio.statusandheaders import StatusAndHeaders

from io import BytesIO
import gzip
import pytest


# ==========================================================================
# helpers
# ==========================================================================
RECORD_ID = '<urn:uuid:aa000000-0000-0000-0000-000000000001>'
RECORD_ID_2 = '<urn:uuid:bb000000-0000-0000-0000-000000000002>'


def warc_record(headers, body=b'', record_id=RECORD_ID,
                rec_type='resource', uri='http://example.com/'):
    """ Build raw WARC/1.0 record bytes with explicit header lines so that
    malformed length fields can be injected freely. """
    lines = ['WARC/1.0']
    lines.append('WARC-Type: ' + rec_type)
    lines.append('WARC-Record-ID: ' + record_id)
    if uri is not None:
        lines.append('WARC-Target-URI: ' + uri)
    lines.append('WARC-Date: 2020-01-01T00:00:00Z')
    for name, value in headers:
        lines.append('{0}: {1}'.format(name, value))
    out = ('\r\n'.join(lines) + '\r\n\r\n').encode('utf-8')
    out += body
    out += b'\r\n\r\n'
    return out


def read_all_records(data, **kwargs):
    return list(ArchiveIterator(BytesIO(data), **kwargs))


def first_record(data, **kwargs):
    it = ArchiveIterator(BytesIO(data), **kwargs)
    return next(iter(it))


def valid_warc_bytes(body=b'hello world', http_headers=None):
    writer = BufferWARCWriter(gzip=False)
    record = writer.create_warc_record(
        'http://example.com/', 'response' if http_headers else 'resource',
        payload=BytesIO(body),
        length=len(body),
        http_headers=http_headers)
    writer.write_record(record)
    return writer.get_contents()


# ==========================================================================
class TestStrictLengthValid(object):
    def test_valid_archive_strict(self):
        data = valid_warc_bytes(b'hello world')
        record = first_record(data, strict_length=True)
        assert record.rec_type == 'resource'
        assert record.length == len(b'hello world')
        assert record.raw_stream.read() == b'hello world'

    def test_valid_zero_length_body_accepted(self):
        # a legal Content-Length: 0 must remain distinguishable from a
        # missing/invalid field and be accepted in strict mode
        data = warc_record([('Content-Length', '0')], body=b'')
        record = first_record(data, strict_length=True)
        assert record.length == 0
        assert record.raw_stream.read() == b''

    def test_zero_length_direct_loader(self):
        data = warc_record([('Content-Length', '0')], body=b'')
        loader = ArcWarcRecordLoader(strict_length=True)
        record = loader.parse_record_stream(BytesIO(data))
        assert record.length == 0

    def test_body_containing_warc_marker_is_not_a_separator(self):
        # WARC/1.1 text inside a correctly bounded body must not be treated
        # as the start of another record in strict mode
        body = b'some data\r\nWARC/1.1\r\nmore data'
        data = valid_warc_bytes(body)
        record = first_record(data, strict_length=True)
        assert record.raw_stream.read() == body
        # and there is no second record guessed from the body text
        assert len(read_all_records(data, strict_length=True)) == 1

    def test_http_content_length_differs_from_warc_length(self):
        # the HTTP Content-Length describes the HTTP payload and is not
        # required to equal the enclosing WARC Content-Length (which also
        # covers the HTTP header block)
        http_body = b'abc'
        http_headers = StatusAndHeaders(
            '200 OK',
            [('Content-Type', 'text/plain'),
             ('Content-Length', str(len(http_body)))],
            protocol='HTTP/1.1')
        data = valid_warc_bytes(http_body, http_headers=http_headers)
        record = first_record(data, strict_length=True)
        warc_len = int(record.rec_headers.get_header('Content-Length'))
        http_len = int(record.http_headers.get_header('Content-Length'))
        assert warc_len != http_len
        assert record.content_stream().read() == http_body

    def test_two_records_strict(self):
        first = valid_warc_bytes(b'first')
        second = valid_warc_bytes(b'second body')
        it = ArchiveIterator(BytesIO(first + second), strict_length=True)
        records = []
        for record in it:
            records.append(record.rec_type)
            # read the body while the record is still the current one, as
            # advancing the iterator drains the previous record
            if len(records) == 1:
                assert record.raw_stream.read() == b'first'
            else:
                assert record.raw_stream.read() == b'second body'
        assert records == ['resource', 'resource']

    def test_valid_gzip_archive_strict(self):
        writer = BufferWARCWriter(gzip=True)
        record = writer.create_warc_record(
            'http://example.com/', 'resource',
            payload=BytesIO(b'gzip body'), length=len(b'gzip body'))
        writer.write_record(record)
        gz = writer.get_contents()

        record = first_record(gz, strict_length=True)
        assert record.rec_type == 'resource'
        assert record.raw_stream.read() == b'gzip body'


# ==========================================================================
class TestStrictLengthHeaderErrors(object):
    def _check_error(self, data, expected_reason):
        with pytest.raises(StrictLengthError) as exc_info:
            read_all_records(data, strict_length=True)

        err = exc_info.value
        assert isinstance(err, ArchiveLoadFailed)
        assert err.record_id == RECORD_ID
        assert err.offset == 0
        assert expected_reason in str(err)
        assert RECORD_ID in str(err)
        assert 'archive offset: 0' in str(err)

    def test_missing_content_length(self):
        data = warc_record([], body=b'abc')
        self._check_error(data, 'missing Content-Length')

    def test_non_integer_content_length(self):
        data = warc_record([('Content-Length', 'abc')], body=b'abc')
        self._check_error(data, 'invalid Content-Length')

    def test_negative_content_length(self):
        data = warc_record([('Content-Length', '-5')], body=b'abc')
        self._check_error(data, 'negative record length')

    def test_conflicting_duplicate_content_length(self):
        data = warc_record([('Content-Length', '3'),
                            ('Content-Length', '4')], body=b'abc')
        self._check_error(data, 'conflicting Content-Length')

    def test_identical_duplicate_content_length_accepted(self):
        # redundant but unambiguous, the boundary is still well defined
        data = warc_record([('Content-Length', '3'),
                            ('Content-Length', '3')], body=b'abc')
        record = first_record(data, strict_length=True)
        assert record.length == 3

    def test_error_offset_of_second_record(self):
        first = valid_warc_bytes(b'abcdefgh')  # well formed first record
        bad = warc_record([('Content-Length', 'nope')], body=b'xyz',
                          record_id=RECORD_ID_2)
        combined = first + bad
        with pytest.raises(StrictLengthError) as exc_info:
            read_all_records(combined, strict_length=True)

        err = exc_info.value
        assert err.record_id == RECORD_ID_2
        # offset points at the start of the second record; the first
        # record's digest headers are generated, so locate its end instead
        # of hard-coding a byte position
        assert err.offset == combined.index(b'WARC/1.0', 1)

    def test_direct_loader_raises_strict_error(self):
        data = warc_record([('Content-Length', 'xyz')], body=b'abc')
        loader = ArcWarcRecordLoader(strict_length=True)
        with pytest.raises(StrictLengthError) as exc_info:
            loader.parse_record_stream(BytesIO(data))
        assert exc_info.value.record_id == RECORD_ID


# ==========================================================================
class TestStrictLengthBoundaryErrors(object):
    def test_short_read_uncompressed(self):
        # declare more bytes than the (final) record actually contains
        data = warc_record([('Content-Length', '12')], body=b'short')
        with pytest.raises(StrictLengthError) as exc_info:
            read_all_records(data, strict_length=True)

        err = exc_info.value
        assert 'short read' in str(err)
        assert err.record_id == RECORD_ID
        assert err.offset == 0

    def test_short_read_gzip(self):
        data = warc_record([('Content-Length', '12')], body=b'short')
        gz = gzip.compress(data)
        with pytest.raises(StrictLengthError) as exc_info:
            read_all_records(gz, strict_length=True)
        assert 'short read' in str(exc_info.value)
        assert exc_info.value.record_id == RECORD_ID

    def test_gzip_error_offset_is_member_boundary(self):
        # in a gzipped archive the tracked offset is the position in the
        # compressed stream, i.e. the start of the offending gzip member
        writer = BufferWARCWriter(gzip=True)
        good = writer.create_warc_record(
            'http://example.com/', 'resource',
            payload=BytesIO(b'abcd'), length=4)
        writer.write_record(good)
        good_gz = writer.get_contents()

        bad = gzip.compress(warc_record([('Content-Length', 'nope')],
                                        body=b'xyz', record_id=RECORD_ID_2))
        with pytest.raises(StrictLengthError) as exc_info:
            read_all_records(good_gz + bad, strict_length=True)

        err = exc_info.value
        assert err.record_id == RECORD_ID_2
        assert err.offset == len(good_gz)

    def test_overlong_declared_length_boundary_mismatch(self):
        # declared length spills into the blank separator and the following
        # record, so the expected blank boundary is never found where the
        # length says the record ends
        body = b'five5'
        following = warc_record([('Content-Length', '2')], body=b'zz',
                                record_id=RECORD_ID_2)
        bad = warc_record([('Content-Length', str(len(body) + 6))],
                          body=body)
        with pytest.raises(StrictLengthError) as exc_info:
            read_all_records(bad + following, strict_length=True)

        err = exc_info.value
        assert 'boundary mismatch' in str(err)
        assert err.record_id == RECORD_ID

    def test_overlong_final_record_is_short_read(self):
        # on the final record an over-long declaration simply runs out of
        # data, which is reported as a short read
        body = b'five5'
        data = warc_record([('Content-Length', str(len(body) + 6))],
                           body=body)
        with pytest.raises(StrictLengthError) as exc_info:
            read_all_records(data, strict_length=True)
        assert 'short read' in str(exc_info.value)

    def test_overlong_does_not_guess_next_record(self):
        body = b'five5'
        following = warc_record([('Content-Length', '2')], body=b'zz',
                                record_id=RECORD_ID_2)
        bad = warc_record([('Content-Length', str(len(body) + 10))],
                          body=body)
        with pytest.raises(StrictLengthError):
            read_all_records(bad + following, strict_length=True)

    def test_warc_marker_in_body_with_overlong_length(self):
        # an over-long declaration that lands on a WARC/1.1-looking line
        # must still be a framing error, not a guessed record boundary
        body = b'WARC/1.1\r\nrest'
        following = warc_record([('Content-Length', '1')], body=b'z',
                                record_id=RECORD_ID_2)
        data = warc_record([('Content-Length', str(len(body) + 4))],
                           body=body)
        with pytest.raises(StrictLengthError) as exc_info:
            read_all_records(data + following, strict_length=True)
        assert 'boundary mismatch' in str(exc_info.value)


# ==========================================================================
class TestStrictLengthArc(object):
    # the filedesc length equals the number of bytes consumed from the two
    # following lines (each line including its trailing newline)
    ARC_HEADER = (
        b'filedesc://bad.arc 127.0.0.1 20140301000000 text/plain 74\r\n'
        b'1 0 Bad Capture\r\n'
        b'URL IP-address Archive-date Content-type Archive-length\r\n\r\n')

    def _arc_record(self, length_field, body=b'x'):
        line = ('http://example.com/ 127.0.0.1 20140102000000 text/plain '
                '{0}\r\n').format(length_field)
        return line.encode('utf-8') + body + b'\r\n'

    def test_arc_negative_length_strict(self):
        data = self.ARC_HEADER + self._arc_record('-1')
        with pytest.raises(StrictLengthError) as exc_info:
            read_all_records(data, strict_length=True, arc2warc=False)
        err = exc_info.value
        assert 'negative record length' in str(err)
        assert err.record_id == 'http://example.com/'

    def test_arc_non_integer_length_strict(self):
        data = self.ARC_HEADER + self._arc_record('abc')
        with pytest.raises(StrictLengthError) as exc_info:
            read_all_records(data, strict_length=True, arc2warc=False)
        assert 'invalid length' in str(exc_info.value)

    def test_arc_zero_length_strict_accepted(self):
        data = self.ARC_HEADER + self._arc_record('0', body=b'')
        records = read_all_records(data, strict_length=True, arc2warc=False)
        types = [r.rec_type for r in records]
        assert types == ['arc_header', 'response']
        response = records[1]
        assert response.length == 0

    def test_arc_valid_strict(self):
        body = b'arc payload'
        record_line = ('http://example.com/ 127.0.0.1 20140102000000 '
                       'text/plain {0}\r\n').format(len(body)).encode('utf-8')
        # parse a single ARC data record directly with no_record_parse so the
        # plain body is not interpreted as an HTTP header block; the loader
        # still bounds the stream to the declared length in strict mode
        loader = ArcWarcRecordLoader(arc2warc=False, strict_length=True)
        stream = BytesIO(record_line + body)
        response = loader.parse_record_stream(stream, known_format='arc',
                                              no_record_parse=True)
        assert response.rec_type == 'response'
        assert response.length == len(body)
        assert response.raw_stream.read() == body

        # and the full two-record archive iterates cleanly in strict mode
        data = self.ARC_HEADER + record_line + body + b'\r\n'
        types = [r.rec_type for r in
                 ArchiveIterator(BytesIO(data), strict_length=True,
                                 arc2warc=False)]
        assert types == ['arc_header', 'response']


# ==========================================================================
class TestLenientBehaviorPreserved(object):
    def test_lenient_default_still_works_on_bad_arc(self):
        from . import get_test_file
        with open(get_test_file('bad.arc'), 'rb') as fh:
            types = [r.rec_type for r in ArchiveIterator(fh)]
        assert types == ['arc_header', 'response', 'response', 'response']

    def test_lenient_non_integer_becomes_zero_length(self):
        data = warc_record([('Content-Length', 'abc')], body=b'abc')
        assert first_record(data).length == 0

    def test_lenient_negative_becomes_zero_length(self):
        data = warc_record([('Content-Length', '-3')], body=b'abc')
        assert first_record(data).length == 0

    def test_lenient_missing_length_is_none(self):
        data = warc_record([], body=b'abc')
        assert first_record(data).length is None

    def test_lenient_conflicting_duplicates_becomes_zero(self):
        data = warc_record([('Content-Length', '3'),
                            ('Content-Length', '4')], body=b'abc')
        assert first_record(data).length == 0

    def test_lenient_truncated_archive_still_reads(self):
        from . import get_test_file
        with open(get_test_file('example-trunc.warc'), 'rb') as fh:
            it = ArchiveIterator(fh)
            types = [r.rec_type for r in it]
        assert types == ['warcinfo', 'warcinfo', 'response', 'request']
        assert it.err_count == 1

    def test_lenient_loader_default(self):
        assert ArcWarcRecordLoader().strict_length is False

    def test_strict_loader_flag(self):
        assert ArcWarcRecordLoader(strict_length=True).strict_length is True
