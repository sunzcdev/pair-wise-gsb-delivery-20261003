from warcio.archiveiterator import ArchiveIterator
from warcio.exceptions import ArchiveLoadFailed
from warcio.utils import BUFF_SIZE
from warcio.warcwriter import WARCWriter

import tempfile


# ============================================================================
class _MemberTolerantArchiveIterator(ArchiveIterator):
    """ An ArchiveIterator which does not raise when a gzip member
    contains more than one record.

    Used only for the explicit fallback mode: records from such members
    can not be copied as raw members and are rewritten through the
    regular read/write path instead.
    """
    def _raise_invalid_gzip_err(self):
        pass


# ============================================================================
class MemberPreservingExtractor(object):
    """ Extract selected records from an ARC/WARC stream into an output
    stream, choosing records by their metadata via a selector callable.

    For gzip input which is confirmed to be chunked -- exactly one record
    per gzip member -- the complete, original gzip member of each selected
    record is copied to the output verbatim: the compressed bytes are
    identical to the source and no decompress/recompress round trip is
    needed.

    Uncompressed input continues through the regular ArchiveIterator read
    path and selected records are written out with a WARCWriter.

    Gzip input in which a member does not contain exactly one record is
    rejected with ArchiveLoadFailed, unless allow_fallback is set, in
    which case such records are decompressed and rewritten through the
    regular read/write path.  A gzip member containing multiple records
    is never mistaken for a single record.

    The selector is called with each parsed record before anything is
    written for that record.  If the selector raises, no (partial) member
    is emitted for the current record and any members already copied to
    the output remain intact, so the output stays a valid archive.
    """

    GZIP_MAGIC = b'\x1f\x8b\x08'

    COPY_BUFF_SIZE = BUFF_SIZE * 4

    SPOOL_MAX_SIZE = 512 * 1024

    INVALID_MEMBER_MSG = """
    ERROR: can not extract record at offset {0} as a raw gzip member.

    Member-preserving extraction requires a chunked gzipped warc/arc,
    with each record compressed into a single gzip member, read from
    a seekable input stream.

    To allow member-preserving extraction, rechunk the input by running:

    warcio recompress <path/to/file> <path/to/new_file>

    Or use allow_fallback=True to decompress and rewrite such records
    through the regular read path.
"""

    def __init__(self, fileobj, out, selector=None, allow_fallback=False,
                 **iterator_kwargs):
        self.fh = fileobj
        self.out = out
        self.selector = selector
        self.allow_fallback = allow_fallback
        self.iterator_kwargs = iterator_kwargs

        self.iterator = None
        self.writer = None

        self.num_selected = 0
        self.num_copied = 0
        self.num_rewritten = 0

    def extract(self):
        """ Iterate over all records, writing the selected ones to the
        output stream.  Returns the number of selected records.
        """
        if self.allow_fallback:
            iterator_cls = _MemberTolerantArchiveIterator
        else:
            iterator_cls = ArchiveIterator

        self.iterator = iterator_cls(self.fh, **self.iterator_kwargs)

        for record in self.iterator:
            self._extract_record(record)

        return self.num_selected

    def _extract_record(self, record):
        # the selector runs before anything is written for this record:
        # if it raises, no (partial) member is emitted for this record
        # and previously copied members remain intact
        if self.selector is not None and not self.selector(record):
            return

        self.num_selected += 1

        iterator = self.iterator

        if iterator.reader.decompressor is None:
            # uncompressed input: continue through the regular read
            # path, rewriting the record to the output
            self._write_record(record, None, gzip=False)
            iterator.read_to_end()
            return

        spooled = None
        if self.allow_fallback:
            # buffer the record content up-front, so that the record can
            # still be rewritten if its gzip member turns out to not
            # contain exactly this record
            spooled = tempfile.SpooledTemporaryFile(max_size=self.SPOOL_MAX_SIZE)
            self._spool_content(record, spooled)
            spooled.seek(0)

        # consume the remainder of the record and compute the
        # boundaries of the gzip member it was read from
        iterator.read_to_end()
        offset = iterator.get_record_offset()
        length = iterator.get_record_length()

        if self._is_single_record_member() and self._copy_member(offset, length):
            self.num_copied += 1
            return

        if not self.allow_fallback:
            raise ArchiveLoadFailed(self.INVALID_MEMBER_MSG.format(offset))

        # explicit fallback: decompress and rewrite via the regular path
        self._write_record(record, spooled, gzip=True)

    def _is_single_record_member(self):
        """ Confirm that the record just read occupies its gzip member
        exactly: no next record line was read ahead, the decompressed
        buffer is fully consumed, and the decompressor has reached the
        end of the current gzip member.
        """
        iterator = self.iterator
        reader = iterator.reader
        decomp = reader.decompressor
        if decomp is None:
            return False

        # a non-blank line was read ahead: the member continues past
        # this record (eg. multiple records in a single member)
        if iterator.next_line is not None:
            return False

        # any decompressed data still buffered means the member
        # continues past the record boundary
        if reader.buff is not None and reader.buff.tell() < reader.buff_size:
            return False

        # the decompressor must have reached the end of the gzip member
        return bool(getattr(decomp, 'eof', False))

    def _copy_member(self, offset, length):
        """ Copy the raw gzip member at offset/length from the input to
        the output.  Returns False if the input is not seekable or no
        gzip member starts at offset.
        """
        fh = self.iterator.fh

        try:
            if not fh.seekable():
                return False
        except AttributeError:
            return False

        pos = fh.tell()
        try:
            fh.seek(offset)

            # sanity check: a gzip member must start at the record offset,
            # verified before anything is written to the output
            if fh.read(len(self.GZIP_MAGIC)) != self.GZIP_MAGIC:
                return False

            self.out.write(self.GZIP_MAGIC)

            remaining = length - len(self.GZIP_MAGIC)
            while remaining > 0:
                buff = fh.read(min(self.COPY_BUFF_SIZE, remaining))
                if not buff:
                    raise ArchiveLoadFailed(
                        'Unexpected end of input while copying gzip member at offset {0}'.format(offset))

                self.out.write(buff)
                remaining -= len(buff)
        finally:
            fh.seek(pos)

        return True

    def _write_record(self, record, spooled, gzip):
        """ Write the record through the regular read/write path """
        if spooled is not None:
            record.raw_stream = spooled

        if self.writer is None or self.writer.gzip != gzip:
            self.writer = WARCWriter(filebuf=self.out, gzip=gzip)

        self.writer.write_record(record)
        self.num_rewritten += 1

    @staticmethod
    def _spool_content(record, spooled):
        while True:
            buff = record.raw_stream.read(BUFF_SIZE)
            if not buff:
                break

            spooled.write(buff)


# ============================================================================
def extract_members(fileobj, out, selector=None, allow_fallback=False,
                    **iterator_kwargs):
    """ Convenience function for MemberPreservingExtractor, see its
    docstring for details.
    """
    extractor = MemberPreservingExtractor(fileobj, out, selector,
                                          allow_fallback=allow_fallback,
                                          **iterator_kwargs)
    return extractor.extract()
