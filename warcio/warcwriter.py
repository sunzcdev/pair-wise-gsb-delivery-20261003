import struct
import sys
import zlib

from socket import gethostname

from warcio.utils import Digester
from warcio.recordbuilder import RecordBuilder

from warcio.statusandheaders import StatusAndHeadersParser


# ============================================================================
class BaseWARCWriter(RecordBuilder):

    def __init__(self, gzip=True, *args, **kwargs):
        # record-level providers, consumed by RecordBuilder
        id_provider = kwargs.pop('id_provider', None)
        date_provider = kwargs.pop('date_provider', None)

        # compression metadata, only relevant when gzip is enabled.
        # defaults keep the historical, already deterministic gzip header
        # (mtime 0, no original filename, unknown OS).
        self.gzip_mtime = kwargs.pop('gzip_mtime', 0)
        self.gzip_filename = kwargs.pop('gzip_filename', None)
        self.gzip_os = kwargs.pop('gzip_os', None)
        self.compress_level = kwargs.pop('compress_level', 9)

        super(BaseWARCWriter, self).__init__(
            warc_version=kwargs.get('warc_version'),
            header_filter=kwargs.get('header_filter'),
            id_provider=id_provider,
            date_provider=date_provider)
        self.gzip = gzip
        self.hostname = gethostname()

        self.parser = StatusAndHeadersParser([], verify=False)

    def write_request_response_pair(self, req, resp, params=None):
        url = resp.rec_headers.get_header('WARC-Target-URI')
        dt = resp.rec_headers.get_header('WARC-Date')

        req.rec_headers.replace_header('WARC-Target-URI', url)
        req.rec_headers.replace_header('WARC-Date', dt)

        resp_id = resp.rec_headers.get_header('WARC-Record-ID')
        if resp_id:
            req.rec_headers.add_header('WARC-Concurrent-To', resp_id)

        self._do_write_req_resp(req, resp, params)

    def write_record(self, record, params=None):  #pragma: no cover
        raise NotImplemented()

    def _do_write_req_resp(self, req, resp, params):  #pragma: no cover
        raise NotImplemented()

    def _write_warc_record(self, out, record):
        if self.gzip:
            out = GzippingWrapper(out,
                                  mtime=self.gzip_mtime,
                                  filename=self.gzip_filename,
                                  compress_level=self.compress_level,
                                  gzip_os=self.gzip_os)

        if record.http_headers:
            record.http_headers.compute_headers_buffer(self.header_filter)

        # Content-Length is None/unknown
        # Fix record by: buffering and recomputing all digests and length
        # (since no length, can't trust existing digests)
        # Also remove content-type for consistent header ordering
        if record.length is None:
            record.rec_headers.remove_header('WARC-Block-Digest')
            if record.rec_type != 'revisit':
                record.rec_headers.remove_header('WARC-Payload-Digest')
            record.rec_headers.remove_header('Content-Type')

            self.ensure_digest(record, block=True, payload=True)

            record.length = record.payload_length

        # ensure digests are set
        else:
            self.ensure_digest(record, block=True, payload=True)

        if record.content_type != None:
            # ensure proper content type
            record.rec_headers.replace_header('Content-Type', record.content_type)

        if record.rec_type == 'revisit':
            http_headers_only = True
        else:
            http_headers_only = False

        # compute Content-Length
        if record.http_headers and record.payload_length >= 0:
            actual_len = 0

            if record.http_headers:
                actual_len = len(record.http_headers.headers_buff)

            if not http_headers_only:
                actual_len += record.payload_length

            record.length = actual_len

        record.rec_headers.replace_header('Content-Length', str(record.length))

        # write record headers -- encoded as utf-8
        # WARC headers can be utf-8 per spec
        out.write(record.rec_headers.to_bytes(encoding='utf-8'))

        # write headers buffer, if any
        if record.http_headers:
            out.write(record.http_headers.headers_buff)

        if not http_headers_only:
            try:
                for buf in self._iter_stream(record.raw_stream):
                    out.write(buf)
            finally:
                if hasattr(record, '_orig_stream'):
                    record.raw_stream.close()
                    record.raw_stream = record._orig_stream

        # add two lines
        out.write(b'\r\n\r\n')

        out.flush()


# ============================================================================
class GzippingWrapper(object):
    """Per-record gzip member writer.

    The gzip header is emitted explicitly (rather than via zlib's gzip
    wrapper) so that compression metadata -- modification time and original
    filename -- can be fixed, allowing byte-for-byte reproducible archives.

    With the default arguments the produced bytes are identical to the
    previous zlib gzip output: mtime 0, no extra fields and the same
    platform-specific OS byte zlib itself emits (10 on Windows, 255 on
    other platforms). Pass ``gzip_os`` to pin the OS byte for fully
    cross-platform reproducible output.
    """
    # OS byte used by zlib's own gzip wrapper on this platform.
    # zlib writes 10 (NTFS) on Windows and 255 (unknown) elsewhere.
    DEFAULT_GZIP_OS = 10 if sys.platform.startswith('win') else 255

    def __init__(self, out, mtime=0, filename=None, compress_level=9,
                 gzip_os=None):
        if not isinstance(mtime, int) or not (0 <= mtime <= 0xffffffff):
            raise ValueError('gzip mtime must be an unsigned 32-bit integer')
        if filename is not None and not isinstance(filename, str):
            raise ValueError('gzip filename must be a string')
        if gzip_os is None:
            # default reproduces zlib's platform-specific output exactly
            gzip_os = self.DEFAULT_GZIP_OS
        if not isinstance(gzip_os, int) or not (0 <= gzip_os <= 0xff):
            raise ValueError('gzip_os must be an unsigned 8-bit integer')

        self.out = out
        self.mtime = mtime
        self.filename = filename
        self.gzip_os = gzip_os

        # raw deflate: the gzip envelope is built manually below
        self.compressor = zlib.compressobj(compress_level,
                                           zlib.DEFLATED,
                                           -zlib.MAX_WBITS)
        self._crc = 0
        self._raw_len = 0
        self._write_header(compress_level)

    def _write_header(self, compress_level):
        flags = 0
        if self.filename:
            # FNAME field, which must not contain NUL bytes
            try:
                name_bytes = self.filename.encode('latin-1')
            except UnicodeEncodeError:
                raise ValueError('gzip filename must be latin-1 encodable')
            if b'\x00' in name_bytes:
                raise ValueError('gzip filename must not contain NUL bytes')
            flags |= 0x08

        # XFL: 2 for max compression (9), 4 for fastest (1), else 0
        if compress_level == 9:
            xfl = 2
        elif compress_level == 1:
            xfl = 4
        else:
            xfl = 0

        header = b'\x1f\x8b'          # gzip magic
        header += b'\x08'             # compression method: deflate
        header += struct.pack('B', flags)
        header += struct.pack('<I', self.mtime)
        header += struct.pack('B', xfl)
        header += struct.pack('B', self.gzip_os)  # OS field
        if self.filename:
            header += name_bytes + b'\x00'

        self.out.write(header)

    def write(self, buff):
        #if isinstance(buff, str):
        #    buff = buff.encode('utf-8')
        self._crc = zlib.crc32(buff, self._crc)
        self._raw_len += len(buff)
        buff = self.compressor.compress(buff)
        self.out.write(buff)

    def flush(self):
        buff = self.compressor.flush()
        self.out.write(buff)
        # gzip trailer: crc32 of uncompressed data and its length mod 2^32
        self.out.write(struct.pack('<I', self._crc & 0xffffffff))
        self.out.write(struct.pack('<I', self._raw_len & 0xffffffff))
        self.out.flush()


# ============================================================================
class WARCWriter(BaseWARCWriter):
    def __init__(self, filebuf, *args, **kwargs):
        super(WARCWriter, self).__init__(*args, **kwargs)
        self.out = filebuf

    def write_record(self, record, params=None):
        self._write_warc_record(self.out, record)

    def _do_write_req_resp(self, req, resp, params):
        self._write_warc_record(self.out, resp)
        self._write_warc_record(self.out, req)


# ============================================================================
class BufferWARCWriter(WARCWriter):
    def __init__(self, *args, **kwargs):
        out = self._create_temp_file()
        super(BufferWARCWriter, self).__init__(out, *args, **kwargs)

    def get_contents(self):
        pos = self.out.tell()
        self.out.seek(0)
        buff = self.out.read()
        self.out.seek(pos)
        return buff

    def get_stream(self):
        self.out.seek(0)
        return self.out


