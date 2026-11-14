from warcio.bufferedreaders import DecompressingBufferedReader, BufferedReader
from warcio.exceptions import ArchiveLoadFailed, RecordIDMismatch
from warcio.limitreader import LimitReader
from warcio.recordloader import ArcWarcRecordLoader
from warcio.utils import BUFF_SIZE

from io import BytesIO


# ============================================================================
GZIP_MAGIC = b'\x1f\x8b'


# ============================================================================
def load_indexed_record(fh, offset, length, record_id=None,
                        no_record_parse=False,
                        ensure_http_headers=False,
                        arc2warc=False,
                        verify_http=False):
    """ Load a single record from an archive using an offset/length
    index entry, without scanning the rest of the file.

    Since an index entry may be stale (created for a different version
    of the archive), the record is validated before it is returned:

    - the record must start exactly at ``offset`` and parse as a
      valid ARC/WARC record
    - the record must end exactly at ``offset + length`` -- for gzipped
      archives ``length`` is the compressed gzip member length, for
      plain archives it is the uncompressed record length
    - the declared Content-Length must be satisfiable within the
      indexed range
    - if ``record_id`` is given, it must match the WARC-Record-ID header

    No bytes are read outside of the physical range
    [offset, offset + length).

    The record content is buffered while validating, so the returned
    record is fully in-memory and safe to read after this call.

    Raises ArchiveLoadFailed if the index entry does not match the
    archive, and RecordIDMismatch (a subclass of ArchiveLoadFailed)
    if the record id does not match.
    """
    try:
        offset = int(offset)
        length = int(length)
    except (ValueError, TypeError):
        raise ArchiveLoadFailed('Invalid index offset/length: ' +
                                str((offset, length)))

    if offset < 0 or length <= 0:
        raise ArchiveLoadFailed('Invalid index offset/length: ' +
                                str((offset, length)))

    fh.seek(offset)

    # bound all reads to the physical range allowed by the index
    limit_reader = LimitReader(fh, length)

    reader, compressed = _make_bounded_reader(limit_reader, offset, length)

    loader = ArcWarcRecordLoader(verify_http=verify_http, arc2warc=arc2warc)

    try:
        record = loader.parse_record_stream(reader,
                                            no_record_parse=no_record_parse,
                                            ensure_http_headers=ensure_http_headers)
    except EOFError:
        raise ArchiveLoadFailed('No record found at offset ' + str(offset))

    if record_id is not None:
        actual_id = record.rec_headers.get_header('WARC-Record-ID')
        if actual_id != record_id:
            raise RecordIDMismatch(record_id, actual_id)

    content = _consume_record_content(record, offset, length)

    if compressed:
        _consume_trailing_blanklines(reader, offset)

    _validate_record_boundary(reader, limit_reader, compressed, offset, length)

    # validation passed: expose the record content buffered above
    record.raw_stream = BytesIO(content)

    return record


# ============================================================================
def _make_bounded_reader(limit_reader, offset, length):
    """ Create a buffered reader over the index-bounded stream,
    detecting gzip compression by the member magic. An offset pointing
    into the middle of a gzip member will not have the magic and will
    fail to parse as a record later on.
    """
    starting_data = limit_reader.read(len(GZIP_MAGIC))

    if len(starting_data) < len(GZIP_MAGIC):
        raise ArchiveLoadFailed('Unable to read record at offset {0}, '
                                'index range [{0}, {1}) extends past '
                                'end of file'.format(offset, offset + length))

    if starting_data == GZIP_MAGIC:
        reader = DecompressingBufferedReader(limit_reader,
                                             starting_data=starting_data)
        return reader, True

    reader = BufferedReader(limit_reader, starting_data=starting_data)
    return reader, False


# ============================================================================
def _consume_record_content(record, offset, length):
    """ Read the remainder of the record content. If the declared
    Content-Length can not be satisfied within the indexed range,
    the index length is too short (or the record is truncated).

    Returns the buffered content, which is only exposed to the caller
    once the record has been fully validated.
    """
    stream = record.raw_stream

    all_buffs = []

    while True:
        buff = stream.read(BUFF_SIZE)
        if not buff:
            break
        all_buffs.append(buff)

    remaining = getattr(stream, 'limit', 0)
    if remaining:
        raise ArchiveLoadFailed('Record at offset {0} with declared '
                                'Content-Length extends beyond index '
                                'length {1}'.format(offset, length))

    return b''.join(all_buffs)


# ============================================================================
def _consume_trailing_blanklines(reader, offset):
    """ For gzipped records, the blank lines following a record are
    part of the gzip member (and of the index length). Consume them,
    requiring anything after the record to be blank.
    """
    while True:
        line = reader.readline()
        if not line:
            return

        if line.rstrip():
            raise ArchiveLoadFailed('Record at offset {0} not followed '
                                    'by newline within index '
                                    'range'.format(offset))


# ============================================================================
def _validate_record_boundary(reader, limit_reader, compressed, offset, length):
    """ The record must end exactly at the end of the indexed range:
    no unread data may remain within the range (eg. an index length
    covering two records), and a gzip member must not continue past
    the end of the range.
    """
    if reader.rem_length() > 0 or limit_reader.limit > 0:
        raise ArchiveLoadFailed('Index length {0} at offset {1} extends '
                                'beyond end of record'.format(length, offset))

    if compressed:
        decompressor = reader.decompressor
        if decompressor is not None and not getattr(decompressor, 'eof', True):
            raise ArchiveLoadFailed('Gzip member at offset {0} continues '
                                    'beyond index length '
                                    '{1}'.format(offset, length))
