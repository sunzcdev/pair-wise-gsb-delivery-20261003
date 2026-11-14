import gzip
import pytest

from io import BytesIO

from warcio.archiveiterator import ArchiveIterator
from warcio.recordbuilder import RecordBuilder
from warcio.statusandheaders import StatusAndHeaders
from warcio.warcwriter import BufferWARCWriter


# ============================================================================
class NonSeekableStream(object):
    def __init__(self, buff):
        self.buff = buff

    def read(self, size=-1):
        return self.buff.read(size)


class FailingStream(object):
    def read(self, size=-1):
        raise IOError('read failed')


# ============================================================================
class TestRecordBuilder(object):
    def _make_response_record(self, writer, body, content_encoding=None):
        headers = [('Content-Type', 'text/html; charset=utf-8'),
                   ('Content-Length', str(len(body)))]
        if content_encoding:
            headers.append(('Content-Encoding', content_encoding))

        http_headers = StatusAndHeaders('200 OK', headers=headers,
                                        protocol='HTTP/1.1')

        return writer.create_warc_record('http://example.com/', 'response',
                                         payload=BytesIO(body),
                                         length=len(body),
                                         http_headers=http_headers)

    def _roundtrip(self, record):
        writer = BufferWARCWriter(gzip=False)
        writer.write_record(record)
        return writer.get_contents()

    def test_replace_body_decoded(self):
        orig_body = gzip.compress(b'<html>original</html>')
        new_body = b'<html>cleaned replacement body</html>'

        writer = BufferWARCWriter(gzip=False)
        record = self._make_response_record(writer, orig_body,
                                            content_encoding='gzip')

        builder = RecordBuilder()
        new_record = builder.create_replaced_body_record(
            record, BytesIO(new_body), content_encoded=False)

        # source uri, date and record association are preserved
        for name in ('WARC-Target-URI', 'WARC-Date', 'WARC-Record-ID',
                     'WARC-Type'):
            assert (new_record.rec_headers.get_header(name) ==
                    record.rec_headers.get_header(name))

        # stale metadata not carried over
        assert new_record.http_headers.get_header('Content-Encoding') is None
        assert new_record.http_headers.get_header('Transfer-Encoding') is None
        assert (new_record.http_headers.get_header('Content-Length') ==
                str(len(new_body)))
        assert new_record.length == len(new_body)

        # digests recomputed for the new body
        assert (new_record.rec_headers.get_header('WARC-Payload-Digest') !=
                record.rec_headers.get_header('WARC-Payload-Digest'))

        # original record still readable
        assert record.content_stream().read() == b'<html>original</html>'

        # write out and read back: content and digests verify
        buff = self._roundtrip(new_record)
        loaded = next(iter(ArchiveIterator(BytesIO(buff),
                                           check_digests='raise')))
        assert loaded.content_stream().read() == new_body
        assert (loaded.rec_headers.get_header('WARC-Target-URI') ==
                'http://example.com/')

    def test_replace_body_still_encoded(self):
        orig_body = gzip.compress(b'<html>original</html>')
        new_body = gzip.compress(b'<html>cleaned</html>')

        writer = BufferWARCWriter(gzip=False)
        record = self._make_response_record(writer, orig_body,
                                            content_encoding='gzip')

        builder = RecordBuilder()
        new_record = builder.create_replaced_body_record(
            record, BytesIO(new_body), content_encoded=True)

        # content-encoding kept, as the new body is still encoded
        assert new_record.http_headers.get_header('Content-Encoding') == 'gzip'

        buff = self._roundtrip(new_record)
        loaded = next(iter(ArchiveIterator(BytesIO(buff),
                                           check_digests='raise')))
        assert loaded.content_stream().read() == b'<html>cleaned</html>'

    def test_replace_body_unknown_length(self):
        orig_body = b'<html>original</html>'
        new_body = b'<html>cleaned replacement body</html>'

        writer = BufferWARCWriter(gzip=False)
        record = self._make_response_record(writer, orig_body)

        builder = RecordBuilder()
        new_record = builder.create_replaced_body_record(
            record, NonSeekableStream(BytesIO(new_body)),
            content_encoded=False)

        assert new_record.length == len(new_body)

        buff = self._roundtrip(new_record)
        loaded = next(iter(ArchiveIterator(BytesIO(buff),
                                           check_digests='raise')))
        assert loaded.content_stream().read() == new_body

        # original record still readable
        assert record.content_stream().read() == orig_body

    def test_replace_body_known_length(self):
        orig_body = b'<html>original</html>'
        new_body = b'<html>cleaned replacement body</html>'

        writer = BufferWARCWriter(gzip=False)
        record = self._make_response_record(writer, orig_body)

        builder = RecordBuilder()
        new_record = builder.create_replaced_body_record(
            record, BytesIO(new_body), content_encoded=False,
            length=len(new_body))

        buff = self._roundtrip(new_record)
        loaded = next(iter(ArchiveIterator(BytesIO(buff),
                                           check_digests='raise')))
        assert loaded.content_stream().read() == new_body

    def test_replace_body_read_failure(self):
        orig_body = b'<html>original</html>'

        writer = BufferWARCWriter(gzip=False)
        record = self._make_response_record(writer, orig_body)

        builder = RecordBuilder()
        with pytest.raises(IOError):
            builder.create_replaced_body_record(record, FailingStream(),
                                                content_encoded=False)

        # original record still readable
        assert record.content_stream().read() == orig_body
