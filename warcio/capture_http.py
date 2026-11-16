import threading
import weakref

from io import BytesIO

from six.moves import http_client as httplib

from contextlib import contextmanager

from array import array

from warcio.utils import to_native_str, BUFF_SIZE, open
from warcio.warcwriter import WARCWriter, BufferWARCWriter

from tempfile import SpooledTemporaryFile


# ============================================================================
orig_connection = httplib.HTTPConnection


# ============================================================================
# A WARCWriter is not thread safe, and the same writer may be shared by
# several capture contexts (eg. an inner/outer capture, or a context that is
# re-opened on the same writer).  Keep one lock per writer so all writes to a
# given WARC file are serialized, no matter which context they come from.
_writer_locks = weakref.WeakKeyDictionary()
_writer_locks_guard = threading.Lock()


def _writer_lock(writer):
    try:
        with _writer_locks_guard:
            lock = _writer_locks.get(writer)
            if lock is None:
                lock = threading.Lock()
                _writer_locks[writer] = lock
            return lock
    except TypeError:  #pragma: no cover
        # writer is not weak referenceable or hashable
        return threading.Lock()


# ============================================================================
class _ThreadContextState(threading.local):
    """Per-thread stack of active capture contexts.

    ``threading.local`` subclasses are initialized once per thread, so every
    thread gets its own, independent ``stack``.  This is what makes nested
    ``capture_http()`` blocks local to the current thread: requesting from
    thread B never observes the capture that thread A is running.
    """

    def __init__(self):
        self.stack = []


def _current_context():
    """Return the innermost capture context active in this thread, if any."""
    stack = RecordingHTTPConnection.local.stack
    return stack[-1] if stack else None


def _push_context(context):
    RecordingHTTPConnection.local.stack.append(context)


def _pop_context(context):
    # The stack is only ever popped by the block that pushed it, but be
    # defensive in case the stack was modified out of order.
    stack = RecordingHTTPConnection.local.stack
    if stack and stack[-1] is context:
        stack.pop()
    elif context in stack:
        stack.remove(context)


class _CaptureContext(object):
    """State shared by every request captured within one ``capture_http()``.

    The context owns the writer, the lock used to serialize writes and the
    (optional) output file.  A fresh :class:`RequestRecorder` is created for
    each request from this context, so concurrent requests never share
    buffers and can be attributed to the writer that was active when the
    request was started.
    """

    def __init__(self, warc_writer, filter_func=None, record_ip=True, out=None):
        self.warc_writer = warc_writer
        self.filter_func = filter_func
        self.record_ip = record_ip

        self.out = out
        self.lock = _writer_lock(warc_writer)

        # number of requests started in this context which have not finished
        # writing their records yet
        self._pending = 0
        self._exited = False
        self._out_closed = False

    def new_recorder(self):
        # a recorder is bound to the capture context at the moment the
        # request is sent, so it always writes to the writer that was active
        # then, even if the response is read after the context exited
        with self.lock:
            self._pending += 1

        return RequestRecorder(self.warc_writer,
                               self.filter_func,
                               record_ip=self.record_ip,
                               lock=self.lock,
                               capture=self)

    def request_finished(self):
        """Called once a recorder has finished writing its records."""
        with self.lock:
            self._pending = max(self._pending - 1, 0)

        self._maybe_close_out()

    def exit(self):
        """Called when the ``capture_http()`` block exits."""
        with self.lock:
            self._exited = True

        self._maybe_close_out()

    def _maybe_close_out(self):
        """Close the output file once the context has exited and no request
        started within it is still pending.

        If a response is read only after the ``with capture_http(...)`` block
        has exited, its record is still written to the writer that was active
        when the request was made, and the file is closed afterwards.
        """
        out = None
        with self.lock:
            if not self._out_closed and self._exited and self._pending == 0:
                self._out_closed = True
                out, self.out = self.out, None

        if out:
            out.close()


# ============================================================================
class RecordingStream(object):
    def __init__(self, fp, recorder):
        self.fp = fp
        self.recorder = recorder
        self._recorder_done = False

        self.recorder.set_remote_ip(self._get_remote_ip())

    def _get_remote_ip(self):
        try:
            fp = self.fp
            # for python 3, need to get 'raw' fp
            if hasattr(fp, 'raw'):  #pragma: no cover
                fp = fp.raw

            socket = fp._sock

            # wrapped ssl socket
            if hasattr(socket, 'socket'):
                socket = socket.socket

            return socket.getpeername()[0]

        except Exception:  #pragma: no cover
            return None

    # Used in PY2 Only
    def read(self, amt=None):  #pragma: no cover
        buff = self.fp.read(amt)
        self.recorder.write_response(buff)
        return buff

    # Used in PY3 Only
    def readinto(self, buff):  #pragma: no cover
        res = self.fp.readinto(buff)
        if res:
            self.recorder.write_response(memoryview(buff)[:res])
        return res

    def read1(self, amt=-1):
        buff = self.fp.read1(amt)
        self.recorder.write_response(buff)
        return buff

    def readline(self, maxlen=-1):
        line = self.fp.readline(maxlen)
        self.recorder.write_response(line)
        return line

    def peek(self, amt=-1):
        # peeking does not consume data, nothing to record
        return self.fp.peek(amt)

    def close(self):
        if not self._recorder_done:
            self._recorder_done = True
            self.recorder.close_response()

        if self.fp:
            return self.fp.close()

    def flush(self):
        return self.fp.flush()


# ============================================================================
class RecordingHTTPResponse(httplib.HTTPResponse):
    def __init__(self, recorder, *args, **kwargs):
        httplib.HTTPResponse.__init__(self, *args, **kwargs)
        self.fp = RecordingStream(self.fp, recorder)

    def begin(self):
        res = httplib.HTTPResponse.begin(self)
        # a status line was read: from here on this is a real exchange, even
        # if the connection is closed immediately afterwards
        self.fp.recorder.started_response = True
        return res


# ============================================================================
class RecordingHTTPConnection(httplib.HTTPConnection):
    local = _ThreadContextState()

    def __init__(self, *args, **kwargs):
        self.recorder = None
        self._in_tunnel = False

        orig_connection.__init__(self, *args, **kwargs)

    def _bind_recorder(self):
        """Bind this connection to the capture context active in this thread.

        The recorder is resolved for every request rather than once per
        connection: pooled connections may be created in one context and
        reused in another (or outside of any capture), and the request must
        be recorded by the writer that was active when it was sent.
        """
        # a recorder left over from a request that never produced a response
        # (eg. an aborted request) must not be attributed new data
        self._abandon_recorder()

        context = _current_context()
        self.recorder = context.new_recorder() if context else None
        return self.recorder

    def _abandon_recorder(self):
        recorder = self.recorder
        self.recorder = None

        if recorder and not recorder.started_response:
            recorder.abort()

        return recorder

    def send(self, data):
        recorder = self.recorder
        if not recorder or self._in_tunnel:
            orig_connection.send(self, data)
            return

        def send_request(buff):
            recorder.extract_url(buff, self.host, self.port, self.default_port,
                                 getattr(self, '_tunnel_host', None),
                                 getattr(self, '_tunnel_port', None))

            try:
                orig_connection.send(self, buff)
            except Exception:
                recorder.abort()
                raise

            recorder.write_request(buff)

        # if sending request body as stream
        # (supported via httplib but seems unused via higher-level apis)
        if hasattr(data, 'read') and not isinstance(data, array):  #pragma: no cover
            while True:
                buff = data.read(BUFF_SIZE)
                if not buff:
                    break

                send_request(buff)
        else:
            send_request(data)

    def _tunnel(self, *args, **kwargs):
        # the CONNECT exchange that opens a proxy tunnel is never recorded
        self._in_tunnel = True
        try:
            return orig_connection._tunnel(self, *args, **kwargs)
        finally:
            self._in_tunnel = False

    def putrequest(self, *args, **kwargs):
        res = orig_connection.putrequest(self, *args, **kwargs)

        recorder = self._bind_recorder()
        if recorder:
            recorder.start()

        return res

    def getresponse(self, *args, **kwargs):
        # urllib3 probes ``getresponse(buffering=True)`` on Python 2; accept
        # and ignore the keyword so the probe succeeds on all versions
        kwargs.pop('buffering', None)

        recorder = self.recorder
        if not recorder or self._in_tunnel:
            return orig_connection.getresponse(self, *args, **kwargs)

        created = []

        def make_recording_response(*rargs, **rkwargs):
            response = RecordingHTTPResponse(recorder, *rargs, **rkwargs)
            created.append(response)
            return response

        response_class = self.response_class
        self.response_class = make_recording_response
        try:
            return orig_connection.getresponse(self, *args, **kwargs)
        except Exception:
            if not created:
                # no response object at all: drop the partial exchange rather
                # than writing a request record with an empty response
                recorder.abort()
            raise
        finally:
            self.response_class = response_class

    def close(self):
        # a connection closed before any response was handed out (eg. the
        # request failed) leaves nothing to record
        self._abandon_recorder()

        return orig_connection.close(self)


# ============================================================================
class RequestRecorder(object):
    def __init__(self, writer, filter_func=None, record_ip=True,
                 lock=None, capture=None):
        self.writer = writer
        self.filter_func = filter_func
        self.request_out = None
        self.response_out = None
        self.url = None
        self.started_req = False
        self.first_line_read = False
        # a single recorder used to own its own lock, but a WARC writer is not
        # thread safe: writes from one capture context must be serialized
        self.lock = lock or threading.Lock()
        self.warc_headers = {}
        self.record_ip = record_ip

        self.capture = capture
        self.finished = False
        self.released = False
        self.started_response = False

    def start(self):
        # one recorder is created per request, so this runs at most once
        self.request_out = self._create_buffer()
        self.response_out = self._create_buffer()
        self.url = None
        self.started_req = True
        self.first_line_read = False

    def _create_buffer(self):
        return SpooledTemporaryFile(BUFF_SIZE)

    def set_remote_ip(self, remote_ip):
        if self.record_ip and remote_ip:  #pragma: no cover
            self.warc_headers['WARC-IP-Address'] = remote_ip

    def write_request(self, buff):
        # buffers may already be released if the response was closed before
        # a slow request body finished being sent
        if self.started_req and self.request_out:
            self.request_out.write(buff)

    def write_response(self, buff):
        if self.started_req and self.response_out and buff:
            self.response_out.write(buff)

    def _create_record(self, out, record_type):
        length = out.tell()
        out.seek(0)
        return self.writer.create_warc_record(
                warc_headers_dict=self.warc_headers,
                uri=self.url,
                record_type=record_type,
                payload=out,
                length=length)

    def done(self):
        """Write this request/response pair to the writer of its context.

        Safe to call more than once: only the first call writes records.
        """
        if self.finished or not self.started_req or not self.request_out:
            self._release()
            return

        self.finished = True
        try:
            request = self._create_record(self.request_out, 'request')
            response = self._create_record(self.response_out, 'response')

            if self.filter_func:
                request, response = self.filter_func(request, response, self)
                if not request or not response:
                    return

            with self.lock:
                self.writer.write_request_response_pair(request, response)
        finally:
            self._release()

    def close_response(self):
        """Called when the response stream is closed.

        A response that never started (the request failed before any status
        line was received) is dropped rather than written as an empty
        response; anything else is written normally.
        """
        if not self.started_response and not self.finished:
            self.abort()
        else:
            self.done()

    def abort(self):
        """Drop this request without writing anything.

        Used when the request fails before a response is available, so a
        partial exchange is never written and the buffers are not leaked.
        """
        self.finished = True
        self.started_req = False
        self._release()

    def _release(self):
        if self.released:
            return

        self.released = True
        self.started_req = False

        for name in ('request_out', 'response_out'):
            out = getattr(self, name, None)
            setattr(self, name, None)
            if out:
                out.close()

        if self.capture:
            self.capture.request_finished()

    def extract_url(self, data, host, port, default_port,
                    tunnel_host=None, tunnel_port=None):
        if self.first_line_read:
            return

        self.first_line_read = True
        buff = BytesIO(data)
        line = to_native_str(buff.readline(), 'latin-1')

        parts = line.split(' ', 2)
        path = parts[1]

        if path.startswith(('http:', 'https:')):
            # absolute-form request (plain HTTP through a proxy)
            self.warc_headers['WARC-Proxy-Host'] = "http://{0}:{1}".format(host, port)
            self.url = path
            return

        scheme = 'https' if default_port == 443 else 'http'

        if tunnel_host:
            # origin-form request sent through a CONNECT tunnel: the socket is
            # connected to the proxy, but the request is for the tunnel target
            self.warc_headers['WARC-Proxy-Host'] = "https://{0}:{1}".format(host, port)
            host = tunnel_host
            port = tunnel_port
            scheme = 'https' if tunnel_port == 443 else 'http'

        self.url = scheme + '://' + host
        if port != default_port:
            self.url += ':' + str(port)

        self.url += path


# ============================================================================
httplib.HTTPConnection = RecordingHTTPConnection


# ============================================================================
@contextmanager
def capture_http(warc_writer=None, filter_func=None, append=True,
                 record_ip=True, **kwargs):
    """Capture HTTP requests made within this block into a WARC.

    Multiple ``capture_http()`` blocks may be active at once.  Each block is
    bound to the calling thread only: nesting blocks in one thread makes the
    inner block take over for that thread (and restore the outer one on exit),
    while other threads keep using the context that is active in *their* own
    thread.  Requests therefore always record to the writer of the context
    that was active in the requesting thread when the request was sent, even
    if the response body is read later on.
    """
    out = None
    if warc_writer == None:
        if 'gzip' not in kwargs:
            kwargs['gzip'] = False

        warc_writer = BufferWARCWriter(**kwargs)

    if isinstance(warc_writer, str):
        out = open(warc_writer, 'ab' if append else 'xb')
        warc_writer = WARCWriter(out, **kwargs)

    context = _CaptureContext(warc_writer, filter_func,
                              record_ip=record_ip, out=out)

    _push_context(context)
    try:
        yield warc_writer
    finally:
        _pop_context(context)
        context.exit()


