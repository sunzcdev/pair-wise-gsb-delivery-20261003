from warcio.archiveiterator import ArchiveIterator
from warcio.exceptions import ArchiveLoadFailed
from warcio.memberextract import MemberPreservingExtractor, extract_members
from warcio.statusandheaders import StatusAndHeaders
from warcio.warcwriter import BufferWARCWriter

from contextlib import closing
from io import BytesIO

import gzip
import pytest

from . import get_test_file


# ============================================================================
class TestMemberPreservingExtractor(object):
    def _make_warc(self, gzip_flag=True, num_records=5):
        writer = BufferWARCWriter(gzip=gzip_flag)

        record = writer.create_warcinfo_record('test.warc',
                                               {'software': 'memberextract-test'})
        writer.write_record(record)

        for i in range(num_records):
            payload = self._make_payload(i)
            http_headers = StatusAndHeaders(
                '200 OK',
                [('Content-Type', 'text/plain'),
                 ('Content-Length', str(len(payload)))],
                protocol='HTTP/1.0')
            record = writer.create_warc_record('http://example.com/{0}'.format(i),
                                               'response',
                                               payload=BytesIO(payload),
                                               length=len(payload),
                                               http_headers=http_headers)
            writer.write_record(record)

        return writer.get_contents()

    @staticmethod
    def _make_payload(i):
        return ('payload {0}\n'.format(i) * 20).encode('utf-8')

    def _get_members(self, contents):
        """ list of (WARC-Target-URI, rec_type, raw gzip member bytes) per record """
        members = []
        with closing(ArchiveIterator(BytesIO(contents))) as it:
            for record in it:
                uri = record.rec_headers.get_header('WARC-Target-URI')
                offset = it.get_record_offset()
                length = it.get_record_length()
                members.append((uri, record.rec_type, contents[offset:offset + length]))

        return members

    def _load_uris(self, contents):
        uris = []
        with closing(ArchiveIterator(BytesIO(contents))) as it:
            for record in it:
                uris.append(record.rec_headers.get_header('WARC-Target-URI'))

        return uris

    def test_copy_members_byte_identical(self):
        contents = self._make_warc(gzip_flag=True)
        members = self._get_members(contents)

        selected = ('http://example.com/1', 'http://example.com/3')

        out = BytesIO()
        extractor = MemberPreservingExtractor(
            BytesIO(contents), out,
            lambda record: record.rec_headers.get_header('WARC-Target-URI') in selected)

        assert extractor.extract() == 2
        assert extractor.num_copied == 2
        assert extractor.num_rewritten == 0

        # the compressed bytes of the selected members are identical to the source
        expected = b''.join(member for uri, rec_type, member in members if uri in selected)
        assert out.getvalue() == expected

        # output is readable by a regular ArchiveIterator
        assert self._load_uris(out.getvalue()) == list(selected)

    def test_copy_all_members_reproduces_source(self):
        contents = self._make_warc(gzip_flag=True)

        out = BytesIO()
        extractor = MemberPreservingExtractor(BytesIO(contents), out, lambda record: True)

        assert extractor.extract() == 6
        assert extractor.num_copied == 6
        assert extractor.num_rewritten == 0

        # selecting all records copies every member verbatim
        assert out.getvalue() == contents

    def test_copy_none_selected(self):
        contents = self._make_warc(gzip_flag=True)

        out = BytesIO()
        extractor = MemberPreservingExtractor(BytesIO(contents), out, lambda record: False)

        assert extractor.extract() == 0
        assert out.getvalue() == b''

    def test_example_warc_gz(self):
        with open(get_test_file('example.warc.gz'), 'rb') as fh:
            contents = fh.read()

        members = self._get_members(contents)

        out = BytesIO()
        extractor = MemberPreservingExtractor(BytesIO(contents), out,
                                              lambda record: record.rec_type == 'response')

        assert extractor.extract() == 1
        assert extractor.num_copied == 1

        expected = b''.join(member for uri, rec_type, member in members
                            if rec_type == 'response')
        assert out.getvalue() == expected

        rec_types = [record.rec_type for record in ArchiveIterator(BytesIO(out.getvalue()))]
        assert rec_types == ['response']

    def test_uncompressed_input_uses_read_path(self):
        contents = self._make_warc(gzip_flag=False)

        out = BytesIO()
        extractor = MemberPreservingExtractor(BytesIO(contents), out,
                                              lambda record: record.rec_type == 'response')

        assert extractor.extract() == 5
        assert extractor.num_copied == 0
        assert extractor.num_rewritten == 5

        # uncompressed input is rewritten uncompressed, readable as usual
        payloads = []
        for record in ArchiveIterator(BytesIO(out.getvalue())):
            assert record.rec_type == 'response'
            payloads.append(record.content_stream().read())

        assert payloads == [self._make_payload(i) for i in range(5)]

    def test_non_chunked_gzip_rejected(self):
        # single gzip member containing all records
        contents = gzip.compress(self._make_warc(gzip_flag=False))

        out = BytesIO()
        extractor = MemberPreservingExtractor(BytesIO(contents), out, lambda record: True)

        with pytest.raises(ArchiveLoadFailed):
            extractor.extract()

        # no partial member was written
        assert out.getvalue() == b''

    def test_non_chunked_gzip_file_rejected(self):
        out = BytesIO()
        with open(get_test_file('example-bad-non-chunked.warc.gz'), 'rb') as fh:
            extractor = MemberPreservingExtractor(fh, out, lambda record: True)

            with pytest.raises(ArchiveLoadFailed):
                extractor.extract()

    def test_valid_members_before_invalid_member_stay_intact(self):
        # one valid single-record member, followed by a gzip member
        # containing two records
        valid = self._make_warc(gzip_flag=True, num_records=1)
        contents = valid + gzip.compress(self._make_warc(gzip_flag=False, num_records=2))

        out = BytesIO()
        extractor = MemberPreservingExtractor(BytesIO(contents), out, lambda record: True)

        with pytest.raises(ArchiveLoadFailed):
            extractor.extract()

        # the valid members copied before the invalid member remain intact
        assert out.getvalue() == valid

        # output remains readable by a regular ArchiveIterator
        assert len(list(ArchiveIterator(BytesIO(out.getvalue())))) == 2

    def test_non_chunked_gzip_fallback(self):
        # single gzip member containing all records
        contents = gzip.compress(self._make_warc(gzip_flag=False))

        out = BytesIO()
        extractor = MemberPreservingExtractor(BytesIO(contents), out,
                                              lambda record: record.rec_type == 'response',
                                              allow_fallback=True)

        assert extractor.extract() == 5
        assert extractor.num_copied == 0
        assert extractor.num_rewritten == 5

        payloads = []
        for record in ArchiveIterator(BytesIO(out.getvalue())):
            assert record.rec_type == 'response'
            payloads.append(record.content_stream().read())

        assert payloads == [self._make_payload(i) for i in range(5)]

    def test_fallback_mode_still_copies_valid_members(self):
        contents = self._make_warc(gzip_flag=True)

        out = BytesIO()
        extractor = MemberPreservingExtractor(BytesIO(contents), out,
                                              lambda record: True,
                                              allow_fallback=True)

        assert extractor.extract() == 6
        assert extractor.num_copied == 6
        assert extractor.num_rewritten == 0

        # valid one-record members are still copied verbatim in fallback mode
        assert out.getvalue() == contents

    def test_selector_exception_leaves_output_valid(self):
        contents = self._make_warc(gzip_flag=True)
        members = self._get_members(contents)

        selected = ('http://example.com/0',
                    'http://example.com/1',
                    'http://example.com/2')

        def selector(record):
            uri = record.rec_headers.get_header('WARC-Target-URI')
            if uri == 'http://example.com/2':
                raise ValueError('selector boom')

            return uri in selected

        out = BytesIO()
        extractor = MemberPreservingExtractor(BytesIO(contents), out, selector)

        with pytest.raises(ValueError):
            extractor.extract()

        # the records selected before the exception were copied as
        # complete members, nothing partial was written for the
        # current record
        expected = b''.join(member for uri, rec_type, member in members
                            if uri in selected[:2])
        assert out.getvalue() == expected

        # output remains readable by a regular ArchiveIterator
        assert self._load_uris(out.getvalue()) == list(selected[:2])

    def test_unseekable_input_rejected(self):
        contents = self._make_warc(gzip_flag=True)

        class Unseekable(object):
            def __init__(self, fh):
                self.fh = fh

            def read(self, size=-1):
                return self.fh.read(size)

            def tell(self):
                raise Exception('not tellable')

        out = BytesIO()
        extractor = MemberPreservingExtractor(Unseekable(BytesIO(contents)), out,
                                              lambda record: True)

        with pytest.raises(ArchiveLoadFailed):
            extractor.extract()

    def test_unseekable_input_fallback(self):
        contents = self._make_warc(gzip_flag=True)

        class Unseekable(object):
            def __init__(self, fh):
                self.fh = fh

            def read(self, size=-1):
                return self.fh.read(size)

            def tell(self):
                raise Exception('not tellable')

        out = BytesIO()
        extractor = MemberPreservingExtractor(Unseekable(BytesIO(contents)), out,
                                              lambda record: record.rec_type == 'response',
                                              allow_fallback=True)

        assert extractor.extract() == 5
        assert extractor.num_rewritten == 5

        assert len(list(ArchiveIterator(BytesIO(out.getvalue())))) == 5

    def test_extract_members_convenience_func(self):
        contents = self._make_warc(gzip_flag=True)

        out = BytesIO()
        count = extract_members(BytesIO(contents), out,
                                lambda record: record.rec_type == 'warcinfo')

        assert count == 1
        assert self._load_uris(out.getvalue()) == [None]
