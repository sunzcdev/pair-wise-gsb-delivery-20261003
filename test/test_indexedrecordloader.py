from warcio.archiveiterator import ArchiveIterator
from warcio.exceptions import ArchiveLoadFailed, RecordIDMismatch
from warcio.indexedrecordloader import load_indexed_record

import pytest

from . import get_test_file


#==============================================================================
class RangeTrackingReader(object):
    """ Wraps a file object and records the byte ranges actually read,
    to verify indexed reads stay within the range allowed by the index.
    """
    def __init__(self, fh):
        self.fh = fh
        self.ranges = []

    def _track(self, start, data):
        if data:
            self.ranges.append((start, start + len(data)))
        return data

    def read(self, size=-1):
        return self._track(self.fh.tell(), self.fh.read(size))

    def readline(self, size=-1):
        return self._track(self.fh.tell(), self.fh.readline(size))

    def seek(self, offset):
        return self.fh.seek(offset)

    def tell(self):
        return self.fh.tell()


#==============================================================================
class TestIndexedRecordLoader(object):
    def _build_index(self, filename):
        """ Build a correct index for the archive by iterating it once.
        """
        entries = []
        with open(get_test_file(filename), 'rb') as fh:
            it = ArchiveIterator(fh)
            for record in it:
                content = record.content_stream().read()
                entries.append({
                    'offset': it.get_record_offset(),
                    'length': it.get_record_length(),
                    'record_id': record.rec_headers.get_header('WARC-Record-ID'),
                    'rec_type': record.rec_type,
                    'content': content,
                })
        return entries

    def _load_tracked(self, filename, offset, length, **kwargs):
        fh = open(get_test_file(filename), 'rb')
        tracked = RangeTrackingReader(fh)
        try:
            record = load_indexed_record(tracked, offset, length, **kwargs)
        except Exception:
            fh.close()
            raise
        return record, tracked, fh

    def _assert_within_range(self, tracked, offset, length):
        assert tracked.ranges, 'expected some reads'
        for start, end in tracked.ranges:
            assert start >= offset
            assert end <= offset + length

    def _assert_index_matches(self, filename):
        entries = self._build_index(filename)
        assert entries

        for entry in entries:
            record, tracked, fh = self._load_tracked(filename,
                                                     entry['offset'],
                                                     entry['length'],
                                                     record_id=entry['record_id'])
            try:
                assert record.rec_type == entry['rec_type']
                assert (record.rec_headers.get_header('WARC-Record-ID') ==
                        entry['record_id'])
                assert record.content_stream().read() == entry['content']
                self._assert_within_range(tracked, entry['offset'], entry['length'])
            finally:
                fh.close()

        return entries

    def test_indexed_read_gz(self):
        self._assert_index_matches('example-resource.warc.gz')

    def test_indexed_read_uncompressed(self):
        self._assert_index_matches('example-iana.org-chunked.warc')

    def test_indexed_read_arc(self):
        self._assert_index_matches('example.arc')

    def test_indexed_read_does_not_scan_file(self):
        """ Reading a record in the middle of the archive must not
        require reading anything before its offset or after its end.
        """
        entries = self._build_index('example-resource.warc.gz')
        entry = entries[1]

        import os
        file_size = os.path.getsize(get_test_file('example-resource.warc.gz'))

        record, tracked, fh = self._load_tracked('example-resource.warc.gz',
                                                 entry['offset'],
                                                 entry['length'])
        try:
            assert record.rec_type == entry['rec_type']
            self._assert_within_range(tracked, entry['offset'], entry['length'])
            # no full-file scan: nothing read before the record,
            # nothing read past the end of the record
            assert min(start for start, end in tracked.ranges) == entry['offset']
            assert max(end for start, end in tracked.ranges) <= entry['offset'] + entry['length']
            assert entry['offset'] + entry['length'] < file_size
        finally:
            fh.close()

    def test_record_id_mismatch(self):
        entries = self._build_index('example-resource.warc.gz')
        entry = entries[2]

        with open(get_test_file('example-resource.warc.gz'), 'rb') as fh:
            with pytest.raises(RecordIDMismatch):
                load_indexed_record(fh, entry['offset'], entry['length'],
                                    record_id='<urn:uuid:not-the-record>')

        # RecordIDMismatch is also an ArchiveLoadFailed
        with open(get_test_file('example-resource.warc.gz'), 'rb') as fh:
            with pytest.raises(ArchiveLoadFailed):
                load_indexed_record(fh, entry['offset'], entry['length'],
                                    record_id='<urn:uuid:not-the-record>')

    def test_offset_middle_of_gzip_member(self):
        entries = self._build_index('example-resource.warc.gz')
        entry = entries[2]

        with open(get_test_file('example-resource.warc.gz'), 'rb') as fh:
            with pytest.raises(ArchiveLoadFailed):
                load_indexed_record(fh, entry['offset'] + 10, entry['length'] - 10)

    def test_offset_middle_of_record_uncompressed(self):
        entries = self._build_index('example-iana.org-chunked.warc')
        entry = entries[1]

        with open(get_test_file('example-iana.org-chunked.warc'), 'rb') as fh:
            with pytest.raises(ArchiveLoadFailed):
                load_indexed_record(fh, entry['offset'] + 100,
                                    entry['length'] - 100)

    def test_length_covers_two_records_gz(self):
        entries = self._build_index('example-resource.warc.gz')
        first, second = entries[0], entries[1]

        # stale length covering this record and the next one
        length = (second['offset'] + second['length']) - first['offset']

        with open(get_test_file('example-resource.warc.gz'), 'rb') as fh:
            with pytest.raises(ArchiveLoadFailed):
                load_indexed_record(fh, first['offset'], length)

    def test_length_covers_two_records_uncompressed(self):
        entries = self._build_index('example-iana.org-chunked.warc')
        first, second = entries[0], entries[1]

        length = (second['offset'] + second['length']) - first['offset']

        with open(get_test_file('example-iana.org-chunked.warc'), 'rb') as fh:
            with pytest.raises(ArchiveLoadFailed):
                load_indexed_record(fh, first['offset'], length)

    def test_length_too_short_gz(self):
        entries = self._build_index('example-resource.warc.gz')
        entry = entries[2]

        with open(get_test_file('example-resource.warc.gz'), 'rb') as fh:
            with pytest.raises(ArchiveLoadFailed):
                load_indexed_record(fh, entry['offset'], entry['length'] - 20)

    def test_length_too_short_uncompressed(self):
        entries = self._build_index('example-iana.org-chunked.warc')
        entry = entries[1]

        with open(get_test_file('example-iana.org-chunked.warc'), 'rb') as fh:
            with pytest.raises(ArchiveLoadFailed):
                load_indexed_record(fh, entry['offset'], entry['length'] - 20)

    def test_stale_index_from_other_file(self):
        """ Index entries created for a different archive must fail
        against this one.
        """
        other_entries = self._build_index('example.warc.gz')

        for entry in other_entries:
            with open(get_test_file('example-resource.warc.gz'), 'rb') as fh:
                with pytest.raises(ArchiveLoadFailed):
                    load_indexed_record(fh, entry['offset'], entry['length'])

    def test_stale_record_id_from_other_file(self):
        """ Correct offset/length, but a record id from another archive.
        """
        entries = self._build_index('example-resource.warc.gz')
        other_entries = self._build_index('example.warc.gz')

        entry = entries[0]
        other_id = other_entries[0]['record_id']
        assert other_id != entry['record_id']

        with open(get_test_file('example-resource.warc.gz'), 'rb') as fh:
            with pytest.raises(RecordIDMismatch):
                load_indexed_record(fh, entry['offset'], entry['length'],
                                    record_id=other_id)

    def test_offset_beyond_eof(self):
        with open(get_test_file('example-resource.warc.gz'), 'rb') as fh:
            with pytest.raises(ArchiveLoadFailed):
                load_indexed_record(fh, 100000, 100)

    def test_invalid_offset_length(self):
        with open(get_test_file('example-resource.warc.gz'), 'rb') as fh:
            with pytest.raises(ArchiveLoadFailed):
                load_indexed_record(fh, -1, 100)

            with pytest.raises(ArchiveLoadFailed):
                load_indexed_record(fh, 0, 0)

            with pytest.raises(ArchiveLoadFailed):
                load_indexed_record(fh, 0, -5)

    def test_sequential_iterator_unchanged(self):
        """ Indexed reads do not affect normal sequential iteration.
        """
        entries = self._build_index('example-resource.warc.gz')

        with open(get_test_file('example-resource.warc.gz'), 'rb') as fh:
            record = load_indexed_record(fh, entries[1]['offset'],
                                         entries[1]['length'])
            assert record.rec_type == entries[1]['rec_type']

        with open(get_test_file('example-resource.warc.gz'), 'rb') as fh:
            rec_types = [record.rec_type for record in ArchiveIterator(fh)]

        assert rec_types == [entry['rec_type'] for entry in entries]
