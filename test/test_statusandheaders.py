#!/usr/bin/env python
# -*- coding: utf-8 -*-

"""
>>> st1 = StatusAndHeadersParser(['HTTP/1.0']).parse(StringIO(status_headers_1))
>>> st1
StatusAndHeaders(protocol = 'HTTP/1.0', statusline = '200 OK', headers = [('Content-Type', 'ABC'), ('Some', 'Value'), ('Multi-Line', 'Value1    Also This')])

# add range (and byte headers)
>>> StatusAndHeaders(statusline = '200 OK', headers=[(b'Content-Type', b'text/plain')]).add_range(10, 4, 100)
StatusAndHeaders(protocol = '', statusline = '206 Partial Content', headers = [('Content-Type', 'text/plain'), ('Content-Range', 'bytes 10-13/100'), ('Content-Length', '4'), ('Accept-Ranges', 'bytes')])

# other protocol expected
>>> StatusAndHeadersParser(['Other']).parse(StringIO(status_headers_1))  # doctest: +IGNORE_EXCEPTION_DETAIL
Traceback (most recent call last):
StatusAndHeadersParserException: Expected Status Line starting with ['Other'] - Found: HTTP/1.0 200 OK

>>> StatusAndHeadersParser(['Other'], verify=False).parse(StringIO(status_headers_1))
StatusAndHeaders(protocol = 'HTTP/1.0', statusline = '200 OK', headers = [('Content-Type', 'ABC'), ('Some', 'Value'), ('Multi-Line', 'Value1    Also This')])


# verify protocol line
>>> StatusAndHeadersParser(['HTTP/1.0'], verify=True).parse(StringIO(unknown_protocol_headers))  # doctest: +IGNORE_EXCEPTION_DETAIL
Traceback (most recent call last):
StatusAndHeadersParserException: Expected Status Line starting with ['HTTP/1.0'] - Found: OtherBlah


# allow unexpected/invalid protocol line
>>> StatusAndHeadersParser(['HTTP/1.0'], verify=False).parse(StringIO(unknown_protocol_headers))
StatusAndHeaders(protocol = 'OtherBlah', statusline = '', headers = [('Foo', 'Bar')])



# test equality op
>>> st1 == StatusAndHeadersParser(['HTTP/1.0']).parse(StringIO(status_headers_1))
True

# replace header, print new headers
>>> st1.replace_header('some', 'Another-Value'); st1
'Value'
StatusAndHeaders(protocol = 'HTTP/1.0', statusline = '200 OK', headers = [('Content-Type', 'ABC'), ('Some', 'Another-Value'), ('Multi-Line', 'Value1    Also This')])


# replace header with dict-like api, print new headers
>>> st1['some'] = 'Yet-Another-Value'; st1
StatusAndHeaders(protocol = 'HTTP/1.0', statusline = '200 OK', headers = [('Content-Type', 'ABC'), ('Some', 'Yet-Another-Value'), ('Multi-Line', 'Value1    Also This')])


# remove header
>>> st1.remove_header('some')
True

# already removed
>>> st1.remove_header('Some')
False

# add header with dict-like api, print new headers
>>> st1['foo'] = 'bar'; st1
StatusAndHeaders(protocol = 'HTTP/1.0', statusline = '200 OK', headers = [('Content-Type', 'ABC'), ('Multi-Line', 'Value1    Also This'), ('foo', 'bar')])

# dict-like api existence and get value
>>> 'bar' in st1
False
>>> 'foo' in st1
True
>>> st1['bar']
>>> st1.get('bar')
>>> st1['foo']
'bar'
>>> st1.get('foo')
'bar'

# remove header with dict-like api, print new headers
>>> del st1['foo']; st1
StatusAndHeaders(protocol = 'HTTP/1.0', statusline = '200 OK', headers = [('Content-Type', 'ABC'), ('Multi-Line', 'Value1    Also This')])

# empty
>>> st2 = StatusAndHeadersParser(['HTTP/1.0']).parse(StringIO(status_headers_2)); x = st2.validate_statusline('204 No Content'); st2
StatusAndHeaders(protocol = '', statusline = '204 No Content', headers = [])


>>> StatusAndHeadersParser(['HTTP/1.0']).parse(StringIO(status_headers_3))
StatusAndHeaders(protocol = 'HTTP/1.0', statusline = '204 Empty', headers = [('Content-Type', 'Value'), ('Content-Length', '0')])

# case-insensitive match
>>> StatusAndHeadersParser(['HTTP/1.0']).parse(StringIO(status_headers_4))
StatusAndHeaders(protocol = 'HTTP/1.0', statusline = '204 empty', headers = [('Content-Type', 'Value'), ('Content-Length', '0')])


"""


from warcio.statusandheaders import StatusAndHeadersParser, StatusAndHeaders
from warcio.statusandheaders import StatusAndHeadersParserException
from warcio.statusandheaders import (
    MissingFinalStatusAndHeadersException,
    TruncatedStatusAndHeadersException,
    ProtocolSwitchStatusAndHeadersException,
)
from six import StringIO
from io import BytesIO
import pytest


status_headers_1 = "\
HTTP/1.0 200 OK\r\n\
Content-Type: ABC\r\n\
HTTP/1.0 200 OK\r\n\
Some: Value\r\n\
Multi-Line: Value1\r\n\
    Also This\r\n\
\r\n\
Body"


status_headers_2 = """

"""

status_headers_3 = "\
HTTP/1.0 204 Empty\r\n\
Content-Type: Value\r\n\
%Invalid%\r\n\
\tMultiline\r\n\
Content-Length: 0\r\n\
\r\n"

status_headers_4 = "\
http/1.0 204 empty\r\n\
Content-Type: Value\r\n\
%Invalid%\r\n\
\tMultiline\r\n\
Content-Length: 0\r\n\
\r\n"

unknown_protocol_headers = "\
OtherBlah\r\n\
Foo: Bar\r\n\
\r\n"


req_headers = "\
GET / HTTP/1.0\r\n\
Foo: Bar\r\n\
Content-Length: 0\r\n"


if __name__ == "__main__":
    import doctest
    doctest.testmod()



def test_to_str_1():
    res = str(StatusAndHeadersParser(['HTTP/1.0']).parse(StringIO(status_headers_1)))

    exp = "\
HTTP/1.0 200 OK\r\n\
Content-Type: ABC\r\n\
Some: Value\r\n\
Multi-Line: Value1    Also This\r\n\
"
    assert(res == exp)


def test_to_str_exclude():
    def exclude(h):
        if h[0].lower() == 'multi-line':
            return None

        return h

    sah = StatusAndHeadersParser(['HTTP/1.0']).parse(StringIO(status_headers_1))
    res = sah.to_str(exclude)

    exp = "\
HTTP/1.0 200 OK\r\n\
Content-Type: ABC\r\n\
Some: Value\r\n\
"
    assert(res == exp)

    assert(sah.to_bytes(exclude) == (exp.encode('latin-1') + b'\r\n'))


def test_to_str_2():
    res = str(StatusAndHeadersParser(['GET']).parse(StringIO(req_headers)))

    assert(res == req_headers)

    res = str(StatusAndHeadersParser(['GET']).parse(StringIO(req_headers + '\r\n')))

    assert(res == req_headers)


def test_to_str_with_remove():
    res = StatusAndHeadersParser(['GET']).parse(StringIO(req_headers))
    res.remove_header('Foo')

    exp = "\
GET / HTTP/1.0\r\n\
Content-Length: 0\r\n"

    assert(str(res) == exp)


def test_status_empty():
    with pytest.raises(EOFError):
        StatusAndHeadersParser([], verify=False).parse(StringIO(''))


def test_status_one_word():
    res = StatusAndHeadersParser(['GET'], verify=False).parse(StringIO('A'))
    assert(str(res) == 'A\r\n')

def test_validate_status():
    assert StatusAndHeaders('200 OK', []).validate_statusline('204 No Content')
    assert not StatusAndHeaders('Bad OK', []).validate_statusline('204 No Content')


def test_non_ascii():
    st = StatusAndHeaders('200 OK', [('Custom-Header', 'attachment; filename="Éxamplè"')])
    res = st.to_ascii_bytes().decode('ascii')
    assert res == "\
200 OK\r\n\
Custom-Header: attachment; filename*=UTF-8''%C3%89xampl%C3%A8\r\n\
\r\n\
"

def test_non_ascii_2():
    st = StatusAndHeaders('200 OK', [('Custom-Header', 'value; filename="Éxamplè"; param; other=испытание; another')])
    res = st.to_ascii_bytes().decode('ascii')
    assert res == "\
200 OK\r\n\
Custom-Header: value; filename*=UTF-8''%C3%89xampl%C3%A8; param; other*=UTF-8''%D0%B8%D1%81%D0%BF%D1%8B%D1%82%D0%B0%D0%BD%D0%B8%D0%B5; another\r\n\
\r\n\
"

def test_non_ascii_3():
    st = StatusAndHeaders('200 OK', [('Custom-Header', '“max-age=31536000″')])
    res = st.to_ascii_bytes().decode('ascii')
    assert res == "\
200 OK\r\n\
Custom-Header: %E2%80%9Cmax-age%3D31536000%E2%80%B3\r\n\
\r\n\
"


# ============================================================================
# Leading 1xx (informational) response handling
# ============================================================================

HTTP_TYPES = ['HTTP/1.0', 'HTTP/1.1']


def _response_parser(verify=True):
    return StatusAndHeadersParser(HTTP_TYPES, verify=verify,
                                  is_http_response=True)


def test_single_100_then_final_response():
    data = (b'HTTP/1.1 100 Continue\r\n\r\n'
            b'HTTP/1.1 200 OK\r\n'
            b'Content-Type: text/plain\r\n'
            b'Content-Length: 5\r\n\r\n'
            b'hello')

    stream = BytesIO(data)
    sh = _response_parser().parse(stream)

    assert sh.protocol == 'HTTP/1.1'
    assert sh.statusline == '200 OK'
    assert sh.headers == [('Content-Type', 'text/plain'),
                          ('Content-Length', '5')]

    # raw informational block is preserved verbatim, in order
    assert sh.informational_headers == [b'HTTP/1.1 100 Continue\r\n\r\n']

    # body starts exactly after the final header block
    assert stream.read() == b'hello'
    assert sh.total_len == len(data) - len(b'hello')


def test_multiple_informational_responses_in_order():
    data = (b'HTTP/1.1 100 Continue\r\n\r\n'
            b'HTTP/1.1 103 Early Hints\r\n'
            b'Link: </style.css>; rel=preload\r\n\r\n'
            b'HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\n'
            b'body')

    stream = BytesIO(data)
    sh = _response_parser().parse(stream)

    assert sh.statusline == '200 OK'
    assert sh.headers == [('Content-Length', '4')]
    assert sh.informational_headers == [
        b'HTTP/1.1 100 Continue\r\n\r\n',
        b'HTTP/1.1 103 Early Hints\r\n'
        b'Link: </style.css>; rel=preload\r\n\r\n',
    ]
    assert stream.read() == b'body'


def test_informational_with_headers_and_continuation_lines():
    data = (b'HTTP/1.1 102 Processing\r\n'
            b'X-Progress: step one\r\n'
            b'    continued\r\n\r\n'
            b'HTTP/1.0 204 No Content\r\n\r\n')

    stream = BytesIO(data)
    sh = _response_parser().parse(stream)

    assert sh.protocol == 'HTTP/1.0'
    assert sh.statusline == '204 No Content'
    assert sh.informational_headers == [
        b'HTTP/1.1 102 Processing\r\n'
        b'X-Progress: step one\r\n'
        b'    continued\r\n\r\n'
    ]
    assert stream.read() == b''


def test_no_informational_response_is_unchanged():
    # an ordinary single response must produce exactly the old result
    data = b'HTTP/1.0 200 OK\r\nContent-Type: ABC\r\n\r\nBody'

    new = _response_parser().parse(BytesIO(data))
    old = StatusAndHeadersParser(HTTP_TYPES).parse(BytesIO(data))

    assert new.statusline == old.statusline
    assert new.protocol == old.protocol
    assert new.headers == old.headers
    assert new.total_len == old.total_len
    assert new.informational_headers == []

    stream = BytesIO(data)
    sh = _response_parser().parse(stream)
    assert stream.read() == b'Body'


def test_http_status_line_in_body_is_not_reparsed():
    # an HTTP/1.1 line inside the body must never start another parse
    body = b'HTTP/1.1 200 OK\r\nX-Not-A-Header: value\r\n\r\n'
    data = (b'HTTP/1.1 200 OK\r\n'
            b'Content-Length: ' + str(len(body)).encode('ascii') +
            b'\r\n\r\n' + body)

    stream = BytesIO(data)
    sh = _response_parser().parse(stream)

    assert sh.statusline == '200 OK'
    assert sh.informational_headers == []
    assert stream.read() == body


def test_second_non_1xx_response_is_treated_as_body():
    body = b'HTTP/1.1 404 Not Found\r\n\r\n'
    data = (b'HTTP/1.1 200 OK\r\n'
            b'Content-Length: ' + str(len(body)).encode('ascii') +
            b'\r\n\r\n' + body)

    stream = BytesIO(data)
    sh = _response_parser().parse(stream)

    assert sh.statusline == '200 OK'
    assert sh.informational_headers == []
    assert stream.read() == body


def test_missing_final_response_raises():
    data = b'HTTP/1.1 100 Continue\r\nFoo: bar\r\n\r\n'

    with pytest.raises(MissingFinalStatusAndHeadersException) as exc:
        _response_parser().parse(BytesIO(data))

    # the consumed informational bytes are retained for diagnostics/digests
    assert exc.value.raw_headers == data
    assert issubclass(MissingFinalStatusAndHeadersException,
                      StatusAndHeadersParserException)


def test_missing_final_after_multiple_responses_keeps_all_blocks():
    first = b'HTTP/1.1 100 Continue\r\n\r\n'
    second = b'HTTP/1.1 103 Early Hints\r\nLink: </a>\r\n\r\n'

    with pytest.raises(MissingFinalStatusAndHeadersException) as exc:
        _response_parser().parse(BytesIO(first + second))

    assert exc.value.raw_headers == first + second


def test_truncated_informational_header_raises():
    data = b'HTTP/1.1 100 Continue\r\nFoo: bar\r\n'

    with pytest.raises(TruncatedStatusAndHeadersException) as exc:
        _response_parser().parse(BytesIO(data))

    assert exc.value.raw_headers == data
    assert issubclass(TruncatedStatusAndHeadersException,
                      StatusAndHeadersParserException)


def test_101_switching_protocols_as_first_response_raises():
    data = (b'HTTP/1.1 101 Switching Protocols\r\n'
            b'Upgrade: websocket\r\n\r\n'
            b'raw-tunnel-bytes')

    with pytest.raises(ProtocolSwitchStatusAndHeadersException):
        _response_parser().parse(BytesIO(data))


def test_101_switching_protocols_after_informational_raises():
    info = b'HTTP/1.1 100 Continue\r\n\r\n'
    data = (info +
            b'HTTP/1.1 101 Switching Protocols\r\n'
            b'Upgrade: websocket\r\n\r\n'
            b'raw-tunnel-bytes')

    with pytest.raises(ProtocolSwitchStatusAndHeadersException) as exc:
        _response_parser().parse(BytesIO(data))

    # the leading informational block that was read is retained
    assert exc.value.raw_headers == info
    assert issubclass(ProtocolSwitchStatusAndHeadersException,
                      StatusAndHeadersParserException)


def test_informational_verify_false():
    data = (b'HTTP/1.1 100 Continue\r\n\r\n'
            b'HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\nbody')

    stream = BytesIO(data)
    sh = StatusAndHeadersParser([], verify=False,
                                is_http_response=True).parse(stream)

    assert sh.statusline == '200 OK'
    assert sh.informational_headers == [b'HTTP/1.1 100 Continue\r\n\r\n']
    assert stream.read() == b'body'


def test_response_flag_does_not_affect_non_response_parser():
    # a parser without is_http_response keeps the legacy behaviour: a leading
    # 1xx status line is simply parsed as the (single) response
    data = b'HTTP/1.1 100 Continue\r\nFoo: bar\r\n\r\n'
    sh = StatusAndHeadersParser(HTTP_TYPES).parse(BytesIO(data))

    assert sh.statusline == '100 Continue'
    assert sh.headers == [('Foo', 'bar')]
    assert sh.informational_headers == []

