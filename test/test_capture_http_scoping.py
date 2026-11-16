from warcio.capture_http import capture_http   # must be imported before requests/urllib3
import socket
import threading
import time

from wsgiref.simple_server import make_server, WSGIServer, WSGIRequestHandler
from socketserver import ThreadingMixIn
from pytest import raises
import requests

from warcio.archiveiterator import ArchiveIterator
from warcio.warcwriter import BufferWARCWriter


class _QuietHandler(WSGIRequestHandler):
    def log_message(self, *args):
        pass


class _ThreadingWSGIServer(ThreadingMixIn, WSGIServer):
    daemon_threads = True


class _ClosingServer(threading.Thread):
    """Accepts connections and closes them without sending a response."""

    def __init__(self, reply=b''):
        super().__init__(daemon=True)
        self.reply = reply
        self.sock = socket.socket()
        self.sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self.sock.bind(('127.0.0.1', 0))
        self.sock.listen(5)
        self.port = self.sock.getsockname()[1]

    def run(self):
        while True:
            try:
                conn, _ = self.sock.accept()
            except OSError:  # pragma: no cover
                return

            if self.reply:
                fp = conn.makefile('rb')
                try:
                    fp.readline()
                    while fp.readline() not in (b'\r\n', b''):
                        pass
                except OSError:  # pragma: no cover
                    pass
                conn.sendall(self.reply)

            conn.close()

    def stop(self):
        self.sock.close()


class LocalHTTPServer(object):
    """Local HTTP stand-in, so these tests never hit a public site."""

    def __init__(self):
        self.requests_seen = []
        self._lock = threading.Lock()

        def app(env, start_response):
            path = env['PATH_INFO']
            with self._lock:
                self.requests_seen.append(path)

            if path.startswith('/slow'):
                time.sleep(0.2)

            if path == '/chunked':
                start_response('200 OK', [('Content-Type', 'text/plain')])
                return [b'multi-', b'part', b'-body']

            if path == '/status500':
                body = b'server-error'
                start_response('500 Internal Server Error',
                               [('Content-Length', str(len(body)))])
                return [body]

            body = ('reply:' + path).encode('utf-8')
            start_response('200 OK', [('Content-Length', str(len(body)))])
            return [body]

        self.server = make_server('127.0.0.1', 0, app,
                                  server_class=_ThreadingWSGIServer,
                                  handler_class=_QuietHandler)
        self.port = self.server.server_address[1]
        self.thread = threading.Thread(target=self.server.serve_forever)
        self.thread.daemon = True
        self.thread.start()

    def url(self, path):
        return 'http://127.0.0.1:{0}{1}'.format(self.port, path)

    def close(self):
        self.server.shutdown()
        self.server.server_close()


def _uris(writer):
    return [rec.rec_headers.get_header('WARC-Target-URI')
            for rec in ArchiveIterator(writer.get_stream())]


def _records(writer):
    return [(rec.rec_type, rec.rec_headers.get_header('WARC-Target-URI'))
            for rec in ArchiveIterator(writer.get_stream())]


# ==================================================================
class TestCaptureHttpScoping(object):
    def setup_class(cls):
        cls.server = LocalHTTPServer()

    def teardown_class(cls):
        cls.server.close()

    def test_single_request_recorded_in_context(self):
        url = self.server.url('/one')
        writer = BufferWARCWriter(gzip=False)

        with capture_http(writer):
            res = requests.get(url)

        assert res.content == b'reply:/one'
        assert _records(writer) == [('response', url), ('request', url)]

    def test_requests_outside_capture_not_recorded(self):
        writer = BufferWARCWriter(gzip=False)
        url = self.server.url('/inside')

        with capture_http(writer):
            requests.get(url)

        # nothing active now
        res = requests.get(self.server.url('/outside'))
        assert res.content == b'reply:/outside'

        assert _uris(writer) == [url, url]

    def test_nested_context_inner_overrides_then_restores(self):
        outer_url = self.server.url('/outer')
        inner_url = self.server.url('/inner')
        outer = BufferWARCWriter(gzip=False)
        inner = BufferWARCWriter(gzip=False)

        with capture_http(outer):
            requests.get(outer_url + '/before')

            with capture_http(inner):
                requests.get(inner_url + '/nested')

            requests.get(outer_url + '/after')

        assert _uris(outer) == [outer_url + '/before',
                                outer_url + '/before',
                                outer_url + '/after',
                                outer_url + '/after']
        assert _uris(inner) == [inner_url + '/nested',
                                inner_url + '/nested']

    def test_deeply_nested_contexts_restore_in_order(self):
        w1 = BufferWARCWriter(gzip=False)
        w2 = BufferWARCWriter(gzip=False)
        w3 = BufferWARCWriter(gzip=False)
        u1 = self.server.url('/l1')
        u2 = self.server.url('/l2')
        u3 = self.server.url('/l3')

        with capture_http(w1):
            requests.get(u1 + '/a')
            with capture_http(w2):
                requests.get(u2 + '/a')
                with capture_http(w3):
                    requests.get(u3 + '/a')
                requests.get(u2 + '/b')
            requests.get(u1 + '/b')

        assert _uris(w1) == [u1 + '/a', u1 + '/a', u1 + '/b', u1 + '/b']
        assert _uris(w2) == [u2 + '/a', u2 + '/a', u2 + '/b', u2 + '/b']
        assert _uris(w3) == [u3 + '/a', u3 + '/a']

    def test_threads_are_independent(self):
        threads = 4
        per_thread = 3
        writers = [BufferWARCWriter(gzip=False) for _ in range(threads)]
        barrier = threading.Barrier(threads)
        errors = []

        def worker(index):
            try:
                with capture_http(writers[index]):
                    barrier.wait(timeout=10)
                    for i in range(per_thread):
                        requests.get(
                            self.server.url('/t{0}/{1}'.format(index, i)))
            except Exception as e:  # pragma: no cover
                errors.append(e)

        workers = [threading.Thread(target=worker, args=(i,))
                   for i in range(threads)]
        for t in workers:
            t.start()
        for t in workers:
            t.join(60)

        assert errors == []
        for index, writer in enumerate(writers):
            expected = []
            for i in range(per_thread):
                url = self.server.url('/t{0}/{1}'.format(index, i))
                expected.extend([url, url])

            assert _uris(writer) == expected

    def test_thread_without_capture_is_not_affected(self):
        writer = BufferWARCWriter(gzip=False)
        captured = []

        def capturing():
            with capture_http(writer):
                for i in range(3):
                    captured.append(
                        requests.get(self.server.url('/cap/{0}'.format(i))))

        t = threading.Thread(target=capturing)
        t.start()

        # this thread has no capture context at all
        for i in range(3):
            res = requests.get(self.server.url('/uncap/{0}'.format(i)))
            assert res.content.startswith(b'reply:/uncap/')

        t.join(60)

        uris = _uris(writer)
        assert len(uris) == 6
        assert all('/cap/' in uri for uri in uris)

    def test_outer_capture_unaffected_by_other_thread(self):
        outer = BufferWARCWriter(gzip=False)
        other = BufferWARCWriter(gzip=False)

        with capture_http(outer):
            requests.get(self.server.url('/main/before'))

            def other_thread():
                with capture_http(other):
                    requests.get(self.server.url('/other/inside'))

            t = threading.Thread(target=other_thread)
            t.start()
            t.join(60)

            requests.get(self.server.url('/main/after'))

        assert len(_uris(outer)) == 4
        assert all('/main/' in uri for uri in _uris(outer))
        assert _uris(other) == [self.server.url('/other/inside'),
                                self.server.url('/other/inside')]

    def test_response_read_after_context_exit(self):
        url = self.server.url('/late-read')
        writer = BufferWARCWriter(gzip=False)

        with capture_http(writer):
            res = requests.get(url, stream=True)

        # the body is consumed only now, after the block has exited
        assert res.content == b'reply:/late-read'
        assert _uris(writer) == [url, url]

    def test_response_read_after_exit_belongs_to_its_writer(self):
        url = self.server.url('/owned-response')
        first = BufferWARCWriter(gzip=False)
        second = BufferWARCWriter(gzip=False)

        with capture_http(first):
            res = requests.get(url, stream=True)

        # a different capture is active while the earlier response is read
        with capture_http(second):
            assert res.content == b'reply:/owned-response'
            requests.get(self.server.url('/second'))

        assert _uris(first) == [url, url]
        assert _uris(second) == [self.server.url('/second'),
                                 self.server.url('/second')]

    def test_capture_to_file_read_after_exit(self, tmpdir):
        path = str(tmpdir.join('after-exit.warc.gz'))
        url = self.server.url('/file-late')

        with capture_http(path):
            res = requests.get(url, stream=True)

        assert res.content == b'reply:/file-late'

        with open(path, 'rb') as fh:
            uris = [rec.rec_headers.get_header('WARC-Target-URI')
                    for rec in ArchiveIterator(fh)]

        assert uris == [url, url]

    def test_exception_leaves_no_context_behind(self):
        writer = BufferWARCWriter(gzip=False)
        before = self.server.url('/exc/before')

        with raises(RuntimeError):
            with capture_http(writer):
                requests.get(before)
                raise RuntimeError('boom')

        # nothing is captured after the context aborted
        after = self.server.url('/exc/after')
        requests.get(after)

        assert _uris(writer) == [before, before]

    def test_exception_in_nested_context_restores_outer(self):
        outer = BufferWARCWriter(gzip=False)
        inner = BufferWARCWriter(gzip=False)
        outer_url = self.server.url('/restore/outer')
        inner_url = self.server.url('/restore/inner')

        with capture_http(outer):
            with raises(ValueError):
                with capture_http(inner):
                    requests.get(inner_url)
                    raise ValueError('boom')

            requests.get(outer_url)

        assert _uris(inner) == [inner_url, inner_url]
        assert _uris(outer) == [outer_url, outer_url]

    def test_connection_pooled_before_capture_is_recorded(self):
        session = requests.Session()
        try:
            # opens and pools a connection outside any capture
            session.get(self.server.url('/warmup'))

            writer = BufferWARCWriter(gzip=False)
            with capture_http(writer):
                session.get(self.server.url('/reuse/1'))
                session.get(self.server.url('/reuse/2'))
        finally:
            session.close()

        assert _uris(writer) == [self.server.url('/reuse/1'),
                                 self.server.url('/reuse/1'),
                                 self.server.url('/reuse/2'),
                                 self.server.url('/reuse/2')]

    def test_connection_pooled_inside_capture_not_recorded_later(self):
        session = requests.Session()
        writer = BufferWARCWriter(gzip=False)
        try:
            with capture_http(writer):
                session.get(self.server.url('/pooled/inside'))

            # reuses the pooled connection, but no capture is active
            session.get(self.server.url('/pooled/after'))
        finally:
            session.close()

        assert _uris(writer) == [self.server.url('/pooled/inside'),
                                 self.server.url('/pooled/inside')]

    def test_error_status_is_still_captured(self):
        url = self.server.url('/status500')
        writer = BufferWARCWriter(gzip=False)

        with capture_http(writer):
            res = requests.get(url)

        assert res.status_code == 500

        records = list(ArchiveIterator(writer.get_stream()))
        assert records[0].rec_type == 'response'
        assert records[0].http_headers.get_statuscode() == '500'
        assert records[1].rec_type == 'request'

    def test_chunked_response_captured(self):
        url = self.server.url('/chunked')
        writer = BufferWARCWriter(gzip=False)

        with capture_http(writer):
            res = requests.get(url)

        assert res.content == b'multi-part-body'
        assert _uris(writer) == [url, url]

    def test_failed_connection_records_nothing_and_does_not_leak(self):
        writer = BufferWARCWriter(gzip=False)

        with capture_http(writer):
            with raises(requests.exceptions.RequestException):
                requests.get('http://127.0.0.1:1/refused', timeout=2)

            # the capture is still usable after the failure
            url = self.server.url('/after-refused')
            requests.get(url)

        assert _uris(writer) == [url, url]

    def test_server_closing_connection_records_nothing(self):
        # a server that accepts and immediately closes the socket never
        # produces a status line, so there is no response to record
        closer = _ClosingServer()
        closer.start()
        writer = BufferWARCWriter(gzip=False)
        try:
            with capture_http(writer):
                with raises(requests.exceptions.RequestException):
                    requests.get(
                        'http://127.0.0.1:{0}/closed'.format(closer.port),
                        timeout=3)

                # the capture still works after that failure
                url = self.server.url('/after-closed')
                requests.get(url)
        finally:
            closer.stop()

        assert _uris(writer) == [url, url]

    def test_invalid_response_records_nothing(self):
        # a non-HTTP reply makes getresponse() fail after the request was
        # sent: no half-written pair may be left in the WARC
        garbage = _ClosingServer(reply=b'NOT-HTTP\r\n\r\n')
        garbage.start()
        writer = BufferWARCWriter(gzip=False)
        try:
            with capture_http(writer):
                with raises(requests.exceptions.RequestException):
                    requests.get(
                        'http://127.0.0.1:{0}/garbage'.format(garbage.port),
                        timeout=3)

                url = self.server.url('/after-garbage')
                requests.get(url)
        finally:
            garbage.stop()

        assert _uris(writer) == [url, url]

    def test_default_arguments_and_path_writer(self, tmpdir):
        first = str(tmpdir.join('first.warc.gz'))
        url = self.server.url('/default-args')

        # a filename writes a gzipped WARC by default
        with capture_http(first):
            requests.get(url)

        with open(first, 'rb') as fh:
            uris = [rec.rec_headers.get_header('WARC-Target-URI')
                    for rec in ArchiveIterator(fh)]
        assert uris == [url, url]

        # no writer means an in-memory BufferWARCWriter
        with capture_http() as buffer_writer:
            requests.get(url)

        assert isinstance(buffer_writer, BufferWARCWriter)
        assert _uris(buffer_writer) == [url, url]

    def test_reopening_capture_on_same_writer_appends(self):
        writer = BufferWARCWriter(gzip=False)
        first = self.server.url('/reopen/1')
        second = self.server.url('/reopen/2')

        with capture_http(writer):
            requests.get(first)

        with capture_http(writer):
            requests.get(second)

        assert _uris(writer) == [first, first, second, second]

    def test_nested_capture_on_same_writer(self):
        writer = BufferWARCWriter(gzip=False)
        url = self.server.url('/same-writer')

        with capture_http(writer):
            requests.get(url + '/a')
            with capture_http(writer):
                requests.get(url + '/b')
            requests.get(url + '/c')

        assert _uris(writer) == [url + '/a', url + '/a',
                                 url + '/b', url + '/b',
                                 url + '/c', url + '/c']

    def test_threads_sharing_one_writer_serialize_writes(self):
        threads = 6
        per_thread = 3
        writer = BufferWARCWriter(gzip=False)
        barrier = threading.Barrier(threads)
        errors = []

        def worker(index):
            try:
                # each thread opens its own context on the *same* writer
                with capture_http(writer):
                    barrier.wait(timeout=10)
                    for i in range(per_thread):
                        requests.get(
                            self.server.url('/shared{0}/{1}'.format(index, i)))
            except Exception as e:  # pragma: no cover
                errors.append(e)

        workers = [threading.Thread(target=worker, args=(i,))
                   for i in range(threads)]
        for t in workers:
            t.start()
        for t in workers:
            t.join(60)

        assert errors == []

        uris = _uris(writer)
        expected = set()
        for index in range(threads):
            for i in range(per_thread):
                expected.add(self.server.url(
                    '/shared{0}/{1}'.format(index, i)))

        # every request/response pair was written exactly once
        assert len(uris) == len(expected) * 2
        assert set(uris) == expected

    def test_concurrent_requests_in_one_context(self):
        # a single capture pushed in the main thread is *not* inherited by
        # other threads, so those requests are not recorded
        writer = BufferWARCWriter(gzip=False)
        barrier = threading.Barrier(3)

        def worker(index):
            barrier.wait(timeout=10)
            requests.get(self.server.url('/unscoped/{0}'.format(index)))

        workers = [threading.Thread(target=worker, args=(i,))
                   for i in range(3)]
        with capture_http(writer):
            for t in workers:
                t.start()
            for t in workers:
                t.join(60)

            requests.get(self.server.url('/scoped'))

        assert _uris(writer) == [self.server.url('/scoped'),
                                 self.server.url('/scoped')]

    def test_records_are_response_then_request_pairs(self):
        writer = BufferWARCWriter(gzip=False)
        url = self.server.url('/pairs')

        with capture_http(writer):
            requests.get(url)

        records = list(ArchiveIterator(writer.get_stream()))
        assert [rec.rec_type for rec in records] == ['response', 'request']

        response, request = records
        assert response.rec_headers.get_header('WARC-Target-URI') == url
        assert request.rec_headers.get_header('WARC-Target-URI') == url
        assert request.http_headers.statusline == '/pairs HTTP/1.1'
        assert request.http_headers.get_header('Host')

    def test_no_capture_does_not_touch_request(self):
        # the local server behaves identically without wrapping
        res = requests.get(self.server.url('/plain'))
        assert res.status_code == 200
        assert res.content == b'reply:/plain'
