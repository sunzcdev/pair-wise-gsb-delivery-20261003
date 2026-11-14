from warcio.bufferedreaders import DecompressingBufferedReader

from warcio.exceptions import ArchiveLoadFailed, StrictLengthError
from warcio.recordloader import ArcWarcRecordLoader

from warcio.utils import BUFF_SIZE

import sys
import six

# ============================================================================
class UnseekableYetTellable:
    def __init__(self, fh):
        self.fh = fh
        self.offset = 0

    def tell(self):
        return self.offset

    def read(self, size=-1):
        result = self.fh.read(size)
        self.offset += len(result)
        return result

# ============================================================================
class ArchiveIterator(six.Iterator):
    """ Iterate over records in WARC and ARC files, both gzip chunk
    compressed and uncompressed

    The indexer will automatically detect format, and decompress
    if necessary.

    """

    GZIP_ERR_MSG = """
    ERROR: non-chunked gzip file detected, gzip block continues
    beyond single record.

    This file is probably not a multi-member gzip but a single gzip file.

    To allow seek, a gzipped {1} must have each record compressed into
    a single gzip member and concatenated together.

    This file is likely still valid and can be fixed by running:

    warcio recompress <path/to/file> <path/to/new_file>

"""

    INC_RECORD = """\
    WARNING: Record not followed by newline, perhaps Content-Length is invalid
    Offset: {0}
    Remainder: {1}
"""

    def __init__(self, fileobj, no_record_parse=False,
                 verify_http=False, arc2warc=False,
                 ensure_http_headers=False, block_size=BUFF_SIZE,
                 check_digests=False, strict_length=False):

        self.fh = fileobj

        self.loader = ArcWarcRecordLoader(verify_http=verify_http,
                                          arc2warc=arc2warc,
                                          strict_length=strict_length)
        self.known_format = None

        self.mixed_arc_warc = arc2warc

        self.member_info = None
        self.no_record_parse = no_record_parse
        self.ensure_http_headers = ensure_http_headers
        self.strict_length = strict_length

        try:
            self.offset = self.fh.tell()
        except:
            self.fh = UnseekableYetTellable(self.fh)
            self.offset = self.fh.tell()

        self.reader = DecompressingBufferedReader(self.fh,
                                                  block_size=block_size)

        self.next_line = None

        self.check_digests = check_digests
        self.err_count = 0
        self.record = None

        self.the_iter = self._iterate_records()

    def __iter__(self):
        return self.the_iter

    def __next__(self):
        return six.next(self.the_iter)

    def close(self):
        self.record = None
        if self.reader:
            self.reader.close_decompressor()
            self.reader = None

    def _iterate_records(self):
        """ iterate over each record
        """
        raise_invalid_gzip = False
        empty_record = False

        while True:
            try:
                self.record = self._next_record(self.next_line)
                if raise_invalid_gzip:
                    self._raise_invalid_gzip_err()

                yield self.record

            except EOFError:
                empty_record = True

            self.read_to_end()

            if self.reader.decompressor:
                # if another gzip member, continue
                if self.reader.read_next_member():
                    continue

                # if empty record, then we're done
                elif empty_record:
                    break

                # otherwise, probably a gzip
                # containing multiple non-chunked records
                # raise this as an error
                else:
                    raise_invalid_gzip = True

            # non-gzip, so we're done
            elif empty_record:
                break

        self.close()

    def _raise_invalid_gzip_err(self):
        """ A gzip file with multiple ARC/WARC records, non-chunked
        has been detected. This is not valid for replay, so notify user
        """
        frmt = 'warc/arc'
        if self.known_format:
            frmt = self.known_format

        frmt_up = frmt.upper()

        msg = self.GZIP_ERR_MSG.format(frmt, frmt_up)
        raise ArchiveLoadFailed(msg)

    def _consume_blanklines(self):
        """ Consume blank lines that are between records
        - For warcs, there are usually 2
        - For arcs, may be 1 or 0
        - For block gzipped files, these are at end of each gzip envelope
          and are included in record length which is the full gzip envelope
        - For uncompressed, they are between records and so are NOT part of
          the record length

          count empty_size so that it can be subtracted from
          the record length for uncompressed

          if first line read is not blank, likely error in WARC/ARC,
          display a warning

        :return: (next_line, empty_size, framing_line) where framing_line is
                 the first non-blank line encountered while blank lines were
                 expected (meaning the record boundary was not where the
                 declared length said it should be), or None.
        """
        empty_size = 0
        first_line = True
        framing_line = None

        while True:
            line = self.reader.readline()
            if len(line) == 0:
                return None, empty_size, framing_line

            stripped = line.rstrip()

            if len(stripped) == 0 or first_line:
                empty_size += len(line)

                if len(stripped) != 0:
                    if framing_line is None:
                        framing_line = line
                    # if first line is not blank,
                    # likely content-length was invalid, display warning
                    err_offset = self.fh.tell() - self.reader.rem_length() - empty_size
                    sys.stderr.write(self.INC_RECORD.format(err_offset, line))
                    self.err_count += 1

                first_line = False
                continue

            return line, empty_size, framing_line

    def read_to_end(self, record=None):
        """ Read remainder of the stream
        If a digester is included, update it
        with the data read
        """

        # no current record to read
        if not self.record:
            return None

        # already at end of this record, don't read until it is consumed
        if self.member_info:
            return None

        curr_offset = self.offset

        limit_reader = self._find_limit_reader(self.record.raw_stream)

        while True:
            b = self.record.raw_stream.read(BUFF_SIZE)
            if not b:
                break

        # strict mode: the declared length must exactly match the bytes
        # available. A short read means the stream ended before the whole
        # declared body could be read (truncation). Fail before any attempt
        # to locate the following record.
        if (self.strict_length and limit_reader is not None
                and limit_reader.truncated):
            rec_id = self._record_id(self.record)
            self.record = None
            raise StrictLengthError(
                'short read: record declared {0} body bytes but the stream '
                'ended first'.format(limit_reader._orig_limit),
                record_id=rec_id, offset=curr_offset)

        """
        - For compressed files, blank lines are consumed
          since they are part of record length
        - For uncompressed files, blank lines are read later,
          and not included in the record length
        """
        #if self.reader.decompressor:
        self.next_line, empty_size, framing_line = self._consume_blanklines()

        self.offset = self.fh.tell() - self.reader.rem_length()
        #if self.offset < 0:
        #    raise Exception('Not Gzipped Properly')

        if self.next_line:
            self.offset -= len(self.next_line)

        length = self.offset - curr_offset

        if not self.reader.decompressor:
            length -= empty_size

        # strict mode: after exactly the declared body bytes, only blank
        # separator lines are allowed. Anything else means the declared
        # length ran past the real record boundary (an over-long declared
        # length); the bytes seen may simply be WARC/1.1 text inside the
        # current body, not a real record separator, so do not guess.
        if self.strict_length and framing_line is not None:
            rec_id = self._record_id(self.record)
            self.record = None
            raise StrictLengthError(
                'record boundary mismatch: expected blank separator lines '
                'after the declared body but found: {0}'.format(
                    framing_line.rstrip()),
                record_id=rec_id, offset=curr_offset)

        self.member_info = (curr_offset, length)
        #return self.member_info
        #return next_line

    @staticmethod
    def _find_limit_reader(stream):
        """ Walk a chain of LimitReader-style wrappers to find the
        outermost one that tracks consumption of the declared length.
        Returns None when the stream is not length limited.
        """
        seen = set()
        while stream is not None and id(stream) not in seen:
            seen.add(id(stream))
            if hasattr(stream, 'truncated') and hasattr(stream, '_orig_limit'):
                return stream
            stream = getattr(stream, 'stream', None)
        return None

    @staticmethod
    def _record_id(record):
        return record.rec_headers.get_header('WARC-Record-ID')

    def get_record_offset(self):
        if not self.member_info:
            self.read_to_end()

        return self.member_info[0]

    def get_record_length(self):
        if not self.member_info:
            self.read_to_end()

        return self.member_info[1]

    def _next_record(self, next_line):
        """ Use loader to parse the record from the reader stream
        Supporting warc and arc records
        """
        try:
            record = self.loader.parse_record_stream(self.reader,
                                                     next_line,
                                                     self.known_format,
                                                     self.no_record_parse,
                                                     self.ensure_http_headers,
                                                     self.check_digests)
        except StrictLengthError as sle:
            # The loader only sees the block-buffered reader, whose tell()
            # is not an exact record boundary. Replace its offset with the
            # precise archive offset tracked by this iterator (which already
            # accounts for the pre-read status line, when there is one).
            if sle.offset is not None:
                sle.offset = self._current_record_offset()
            raise

        self.member_info = None

        # Track known format for faster parsing of other records
        if not self.mixed_arc_warc:
            self.known_format = record.format

        return record

    def _current_record_offset(self):
        """ Precise archive offset of the record currently being parsed.

        After the previous record was drained, ``self.offset`` points at the
        start of the next record's status line, whether that line is still in
        the buffered reader or has been pre-read into ``self.next_line``.
        Before the first record it is just the (initial) file position.
        """
        return self.offset


# ============================================================================
class WARCIterator(ArchiveIterator):
    def __init__(self, *args, **kwargs):
        super(WARCIterator, self).__init__(*args, **kwargs)
        self.known_format = 'warc'


# ============================================================================
class ARCIterator(ArchiveIterator):
    def __init__(self, *args, **kwargs):
        super(ARCIterator, self).__init__(*args, **kwargs)
        self.known_format = 'arc'



