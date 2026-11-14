import zlib

from copy import deepcopy
from socket import gethostname

from warcio.utils import Digester
from warcio.recordbuilder import RecordBuilder
from warcio.recordloader import ArcWarcRecord

from warcio.statusandheaders import StatusAndHeadersParser


# ============================================================================
class BaseWARCWriter(RecordBuilder):

    def __init__(self, gzip=True, *args, **kwargs):
        super(BaseWARCWriter, self).__init__(warc_version=kwargs.get('warc_version'),
                                             header_filter=kwargs.get('header_filter'))
        self.gzip = gzip
        self.hostname = gethostname()

        self.parser = StatusAndHeadersParser([], verify=False)

    def write_request_response_pair(self, req, resp, params=None):
        # legacy, direct write: the request record headers are mutated in place
        # and the two records are written without any rollback guarantee
        url = resp.rec_headers.get_header('WARC-Target-URI')
        dt = resp.rec_headers.get_header('WARC-Date')

        req.rec_headers.replace_header('WARC-Target-URI', url)
        req.rec_headers.replace_header('WARC-Date', dt)

        resp_id = resp.rec_headers.get_header('WARC-Record-ID')
        if resp_id:
            req.rec_headers.add_header('WARC-Concurrent-To', resp_id)

        self._do_write_req_resp(req, resp, params)

    def write_request_response_pair_atomic(self, req, resp, params=None):
        """ Write a request/response pair as a single all-or-nothing commit.

        The response is written first, followed by the request, so a failure
        while writing the request would otherwise leave an unpaired response
        behind. Here the position before the pair is captured and, if writing
        either record raises, the stream is seeked back to that position and
        truncated, removing any partially written bytes. Content that existed
        before the pair is never truncated.

        The output must be positionable (support tell(), seek() and
        truncate()); non-positionable outputs are rejected before anything
        is written. The caller's records are not modified: pairing metadata
        is prepared on private copies that require matching WARC-Date and
        WARC-Target-URI, plus a WARC-Concurrent-To referencing the response
        record.

        The original, non-atomic write_request_response_pair() behavior is
        unchanged.
        """
        out = getattr(self, 'out', None)
        if not self._is_positionable(out):
            raise ValueError('Atomic request/response pair writes require an '
                             'output stream supporting tell(), seek() and '
                             'truncate()')

        req_copy, resp_copy = self._prepare_pair_records(req, resp)

        resume_pos = out.tell()
        try:
            self._do_write_req_resp(req_copy, resp_copy, params)
        except BaseException:
            # any failure, including a partial write, undoes the whole pair
            self._rollback_output(out, resume_pos)
            raise

    def _prepare_pair_records(self, req, resp):
        """ Return private copies of req and resp with consistent pair metadata.

        The two records are paired on the response's WARC-Target-URI and
        WARC-Date, with the request carrying a WARC-Concurrent-To that
        references the response WARC-Record-ID. Any request value already
        present that contradicts the response is rejected before anything
        is written. If the response itself lacks a URI/date, the request's
        value is used instead so that the written pair stays consistent.
        Neither input record is modified.
        """
        resp_url = resp.rec_headers.get_header('WARC-Target-URI')
        resp_dt = resp.rec_headers.get_header('WARC-Date')
        resp_id = resp.rec_headers.get_header('WARC-Record-ID')

        req_url = req.rec_headers.get_header('WARC-Target-URI')
        if resp_url is not None and req_url is not None and req_url != resp_url:
            raise ValueError('Request WARC-Target-URI {0} does not match '
                             'response WARC-Target-URI {1}'.format(req_url,
                                                                  resp_url))

        req_dt = req.rec_headers.get_header('WARC-Date')
        if resp_dt is not None and req_dt is not None and req_dt != resp_dt:
            raise ValueError('Request WARC-Date {0} does not match '
                             'response WARC-Date {1}'.format(req_dt, resp_dt))

        req_concurrent = req.rec_headers.get_header('WARC-Concurrent-To')
        if req_concurrent is not None and resp_id is not None \
                and req_concurrent != resp_id:
            raise ValueError('Request WARC-Concurrent-To {0} does not reference '
                             'response WARC-Record-ID {1}'.format(req_concurrent,
                                                                  resp_id))

        url = resp_url if resp_url is not None else req_url
        dt = resp_dt if resp_dt is not None else req_dt

        req_copy = self._copy_record(req)
        resp_copy = self._copy_record(resp)

        for record in (req_copy, resp_copy):
            if url is not None:
                record.rec_headers.replace_header('WARC-Target-URI', url)

            if dt is not None:
                record.rec_headers.replace_header('WARC-Date', dt)

        if resp_id:
            req_copy.rec_headers.replace_header('WARC-Concurrent-To', resp_id)

        return req_copy, resp_copy

    @staticmethod
    def _copy_record(record):
        # payload streams are shared, not duplicated: only the headers (the
        # part the pairing metadata touches) are copied, so the caller's
        # records keep their own header state intact
        return ArcWarcRecord(record.format,
                             record.rec_type,
                             deepcopy(record.rec_headers),
                             record.raw_stream,
                             deepcopy(record.http_headers),
                             record.content_type,
                             record.length,
                             payload_length=record.payload_length)

    @staticmethod
    def _is_positionable(out):
        for name in ('tell', 'seek', 'truncate'):
            if not callable(getattr(out, name, None)):
                return False

        try:
            pos = out.tell()
            out.seek(pos)
        except (OSError, ValueError, AttributeError):
            return False

        return True

    def _rollback_output(self, out, pos):
        out.seek(pos)
        try:
            out.truncate(pos)
        except TypeError:
            # streams whose truncate() takes no size argument
            out.truncate()

    def write_record(self, record, params=None):  #pragma: no cover
        raise NotImplemented()

    def _do_write_req_resp(self, req, resp, params):  #pragma: no cover
        raise NotImplemented()

    def _write_warc_record(self, out, record):
        if self.gzip:
            out = GzippingWrapper(out)

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
    def __init__(self, out):
        self.compressor = zlib.compressobj(9, zlib.DEFLATED, zlib.MAX_WBITS + 16)
        self.out = out

    def write(self, buff):
        #if isinstance(buff, str):
        #    buff = buff.encode('utf-8')
        buff = self.compressor.compress(buff)
        self.out.write(buff)

    def flush(self):
        buff = self.compressor.flush()
        self.out.write(buff)
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


