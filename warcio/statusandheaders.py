"""
Representation and parsing of HTTP-style status + headers
"""

from six.moves import range
from six import iteritems
from warcio.utils import to_native_str, headers_to_str_headers
import uuid

from six.moves.urllib.parse import quote
import re


#=================================================================
class StatusAndHeaders(object):
    ENCODE_HEADER_RX = re.compile(r'[=]["\']?([^;"]+)["\']?(?=[;]?)')
    """
    Representation of parsed http-style status line and headers
    Status Line if first line of request/response
    Headers is a list of (name, value) tuples
    An optional protocol which appears on first line may be specified
    If is_http_request is true, split http verb (instead of protocol) from start of statusline
    """
    def __init__(self, statusline, headers, protocol='', total_len=0, is_http_request=False):
        if is_http_request:
            protocol, statusline = statusline.split(' ', 1)

        self.statusline = statusline
        self.headers = headers_to_str_headers(headers)
        self.protocol = protocol
        self.total_len = total_len
        self.headers_buff = None
        # Raw bytes of any leading 1xx (informational) response header blocks,
        # in the order they were received and terminated by their blank lines.
        # Empty for an ordinary record with a single status line + headers.
        self.informational_headers = []

    def get_header(self, name, default_value=None):
        """
        return header (name, value)
        if found
        """
        name_lower = name.lower()
        for value in self.headers:
            if value[0].lower() == name_lower:
                return value[1]

        return default_value

    def add_header(self, name, value):
        self.headers.append((name, value))

    def replace_header(self, name, value):
        """
        replace header with new value or add new header
        return old header value, if any
        """
        name_lower = name.lower()
        for index in range(len(self.headers) - 1, -1, -1):
            curr_name, curr_value = self.headers[index]
            if curr_name.lower() == name_lower:
                self.headers[index] = (curr_name, value)
                return curr_value

        self.headers.append((name, value))
        return None

    def remove_header(self, name):
        """
        Remove header (case-insensitive)
        return True if header removed, False otherwise
        """
        name_lower = name.lower()
        for index in range(len(self.headers) - 1, -1, -1):
            if self.headers[index][0].lower() == name_lower:
                del self.headers[index]
                return True

        return False

    def get_statuscode(self):
        """
        Return the statuscode part of the status response line
        (Assumes no protocol in the statusline)
        """
        code = self.statusline.split(' ', 1)[0]
        return code

    def validate_statusline(self, valid_statusline):
        """
        Check that the statusline is valid, eg. starts with a numeric
        code. If not, replace with passed in valid_statusline
        """
        code = self.get_statuscode()
        try:
            code = int(code)
            assert(code > 0)
            return True
        except(ValueError, AssertionError):
            self.statusline = valid_statusline
            return False

    def add_range(self, start, part_len, total_len):
        """
        Add range headers indicating that this a partial response
        """
        content_range = 'bytes {0}-{1}/{2}'.format(start,
                                                   start + part_len - 1,
                                                   total_len)

        self.statusline = '206 Partial Content'
        self.replace_header('Content-Range', content_range)
        self.replace_header('Content-Length', str(part_len))
        self.replace_header('Accept-Ranges', 'bytes')
        return self

    def compute_headers_buffer(self, header_filter=None):
        """
        Set buffer representing headers
        """
        # HTTP headers %-encoded as ascii (see to_ascii_bytes for more info)
        self.headers_buff = self.to_ascii_bytes(header_filter)

    def __repr__(self):
        return "StatusAndHeaders(protocol = '{0}', statusline = '{1}', \
headers = {2})".format(self.protocol, self.statusline, self.headers)

    def __ne__(self, other):
        return not (self == other)

    def __eq__(self, other):
        if not other:
            return False

        return (self.statusline == other.statusline and
                self.headers == other.headers and
                self.protocol == other.protocol)

    def __str__(self, exclude_list=None):
        return self.to_str(exclude_list)

    def __bool__(self):
        return bool(self.statusline or self.headers)

    __nonzero__ = __bool__

    def to_str(self, filter_func=None):
        string = self.protocol

        if string and self.statusline:
            string += ' '

        if self.statusline:
            string += self.statusline

        if string:
            string += '\r\n'

        for h in self.headers:
            if filter_func:
                h = filter_func(h)
                if not h:
                    continue

            string += ': '.join(h) + '\r\n'

        return string

    def to_bytes(self, filter_func=None, encoding='utf-8'):
        return self.to_str(filter_func).encode(encoding) + b'\r\n'

    def to_ascii_bytes(self, filter_func=None):
        """ Attempt to encode the headers block as ascii
            If encoding fails, call percent_encode_non_ascii_headers()
            to encode any headers per RFCs
        """
        try:
            string = self.to_str(filter_func)
            string = string.encode('ascii')
        except (UnicodeEncodeError, UnicodeDecodeError):
            self.percent_encode_non_ascii_headers()
            string = self.to_str(filter_func)
            string = string.encode('ascii')

        return string + b'\r\n'

    def percent_encode_non_ascii_headers(self, encoding='UTF-8'):
        """ Encode any headers that are not plain ascii
            as UTF-8 as per:
            https://tools.ietf.org/html/rfc8187#section-3.2.3
            https://tools.ietf.org/html/rfc5987#section-3.2.2
        """
        def do_encode(m):
            return "*={0}''".format(encoding) + quote(to_native_str(m.group(1)))

        for index in range(len(self.headers) - 1, -1, -1):
            curr_name, curr_value = self.headers[index]
            try:
                # test if header is ascii encodable, no action needed
                curr_value.encode('ascii')
            except:
                # if single value header, (eg. no ';'), %-encode entire header
                if ';' not in curr_value:
                    new_value = quote(curr_value)

                else:
                # %-encode value in ; name="value"
                    new_value = self.ENCODE_HEADER_RX.sub(do_encode, curr_value)
                    if new_value == curr_value:
                        new_value = quote(curr_value)

                self.headers[index] = (curr_name, new_value)

    # act like a (case-insensitive) dictionary of headers, much like other
    # python http headers apis including http.client.HTTPMessage
    # and requests.structures.CaseInsensitiveDict
    get = get_header
    __getitem__ = get_header
    __setitem__ = replace_header
    __delitem__ = remove_header
    def __contains__(self, key):
        return bool(self[key])

#=================================================================
def _strip_count(string, total_read):
    length = len(string)
    return string.rstrip(), total_read + length


#=================================================================
class StatusAndHeadersParser(object):
    """
    Parser which consumes a stream support readline() to read
    status and headers and return a StatusAndHeaders object
    """
    def __init__(self, statuslist, verify=True, is_http_response=False):
        self.statuslist = statuslist
        self.verify = verify
        # When true, treat leading 1xx (informational) status lines as
        # preliminary response header blocks and keep parsing until the
        # final (non-1xx) response. Only meaningful for HTTP responses.
        self.is_http_response = is_http_response

    def parse(self, stream, full_statusline=None):
        """
        parse stream for status line and headers
        return a StatusAndHeaders object

        support continuation headers starting with space or tab

        For HTTP responses (is_http_response=True), any number of leading
        1xx informational response header blocks are consumed and preserved
        in order; the last non-1xx response is returned as the main
        StatusAndHeaders and the body begins after it. The informational
        blocks are kept verbatim for digest verification.
        """

        # status line w newlines intact
        if full_statusline is None:
            full_statusline = stream.readline()

        full_statusline = self.decode_header(full_statusline)

        statusline, total_read = _strip_count(full_statusline, 0)

        headers = []

        # at end of stream
        if total_read == 0:
            raise EOFError()
        elif not statusline:
            return StatusAndHeaders(statusline=statusline,
                                    headers=headers,
                                    protocol='',
                                    total_len=total_read)

        # Collapse any leading 1xx informational responses (e.g. HTTP/1.1 100
        # Continue, 103 Early Hints) so that the final response is parsed as
        # the main response. Only the raw header bytes are retained; parsing
        # resumes strictly at the status line that follows each blank line.
        informational = []

        if self.is_http_response:
            while True:
                protocol_status = self._parse_statusline(statusline)
                status_code = self._get_status_code(protocol_status)

                # A 101 Switching Protocols response is terminal: the
                # remainder of the stream is no longer HTTP framing, so it
                # must not be parsed as another response (nor treated as a
                # preliminary response to be collapsed).
                if status_code == 101:
                    raise ProtocolSwitchStatusAndHeadersException(
                        'HTTP 101 Switching Protocols response terminates the '
                        'HTTP header stream; the tunneled data cannot be '
                        'parsed as another response',
                        statusline,
                        b''.join(informational))

                if not self._is_informational(protocol_status):
                    break

                # preserve the exact bytes of the informational status line
                informational_buff = self._raw_line(full_statusline)

                # consume the informational header block up to its blank line
                blank_seen = True
                while True:
                    raw_line = stream.readline()
                    if not raw_line:
                        blank_seen = False
                        break

                    total_read += len(raw_line)
                    informational_buff += raw_line

                    if not self.decode_header(raw_line).rstrip():
                        break

                if not blank_seen:
                    raise TruncatedStatusAndHeadersException(
                        'Informational response header block is truncated '
                        '(missing terminating blank line)',
                        statusline,
                        informational_buff)

                informational.append(informational_buff)

                # read the next status line; this must be another response,
                # never body bytes (body parsing only starts after the final
                # header block is returned)
                full_statusline = stream.readline()
                total_read += len(full_statusline)

                if not full_statusline:
                    raise MissingFinalStatusAndHeadersException(
                        'Informational response(s) not followed by a final '
                        'response status line',
                        statusline,
                        b''.join(informational))

                full_statusline = self.decode_header(full_statusline)
                statusline = full_statusline.rstrip()

        # validate only if verify is set
        if self.verify:
            protocol_status = self.split_prefix(statusline, self.statuslist)

            if not protocol_status:
                msg = 'Expected Status Line starting with {0} - Found: {1}'
                msg = msg.format(self.statuslist, statusline)
                raise StatusAndHeadersParserException(msg, full_statusline)
        else:
            protocol_status = statusline.split(' ', 1)

        line, total_read = _strip_count(self.decode_header(stream.readline()), total_read)
        while line:
            result = line.split(':', 1)
            if len(result) == 2:
                name = result[0].rstrip(' \t')
                value = result[1].lstrip()
            else:
                name = result[0]
                value = None

            next_line, total_read = _strip_count(self.decode_header(stream.readline()),
                                                 total_read)

            # append continuation lines, if any
            while next_line and next_line.startswith((' ', '\t')):
                if value is not None:
                    value += next_line
                next_line, total_read = _strip_count(self.decode_header(stream.readline()),
                                                     total_read)

            if value is not None:
                header = (name, value)
                headers.append(header)

            line = next_line

        if len(protocol_status) > 1:
            statusline = protocol_status[1].strip()
        else:
            statusline = ''

        parsed = StatusAndHeaders(statusline=statusline,
                                  headers=headers,
                                  protocol=protocol_status[0],
                                  total_len=total_read)

        # keep the verbatim informational response header blocks in order
        parsed.informational_headers = informational

        return parsed

    def _parse_statusline(self, statusline):
        """
        split a status line into (protocol, remainder) without raising,
        using the configured prefixes when verifying, else a simple split
        """
        if self.verify:
            protocol_status = self.split_prefix(statusline, self.statuslist)
            if not protocol_status:
                return None
            return protocol_status
        return statusline.split(' ', 1)

    @staticmethod
    def _get_status_code(protocol_status):
        """
        extract the numeric status code from a (protocol, remainder) tuple,
        returning None when it is missing or not numeric
        """
        if not protocol_status or len(protocol_status) < 2:
            return None

        code = protocol_status[1].strip().split(' ', 1)[0]
        try:
            return int(code)
        except ValueError:
            return None

    def _is_informational(self, protocol_status):
        """
        True when the status line holds a 1xx informational response
        """
        code = self._get_status_code(protocol_status)
        return code is not None and 100 <= code < 200

    @staticmethod
    def _raw_line(line):
        """
        return the original bytes for a (possibly decoded) readline() result
        """
        if isinstance(line, bytes):
            return line
        return line.encode('iso-8859-1')

    @staticmethod
    def split_prefix(key, prefixs):
        """
        split key string into prefix and remainder
        for first matching prefix from a list
        """
        key_upper = key.upper()
        for prefix in prefixs:
            if key_upper.startswith(prefix):
                plen = len(prefix)
                return (key_upper[:plen], key[plen:])

    @staticmethod
    def make_warc_id(id_=None):
        if not id_:
            id_ = uuid.uuid4()
        return '<urn:uuid:{0}>'.format(id_)


    @staticmethod
    def decode_header(line):
        try:
            # attempt to decode as utf-8 first
            return to_native_str(line, 'utf-8')
        except:
            # if fails, default to ISO-8859-1
            return to_native_str(line, 'iso-8859-1')


#=================================================================
class StatusAndHeadersParserException(Exception):
    """
    status + headers parsing exception
    """
    def __init__(self, msg, statusline):
        super(StatusAndHeadersParserException, self).__init__(msg)
        self.statusline = statusline


#=================================================================
class MissingFinalStatusAndHeadersException(StatusAndHeadersParserException):
    """
    Raised when one or more 1xx informational responses are present but the
    stream ends before a final (non-1xx) response status line.
    """
    def __init__(self, msg, statusline, raw_headers=b''):
        super(MissingFinalStatusAndHeadersException, self).__init__(msg, statusline)
        self.raw_headers = raw_headers


#=================================================================
class TruncatedStatusAndHeadersException(StatusAndHeadersParserException):
    """
    Raised when an informational response header block is cut off before its
    terminating blank line.
    """
    def __init__(self, msg, statusline, raw_headers=b''):
        super(TruncatedStatusAndHeadersException, self).__init__(msg, statusline)
        self.raw_headers = raw_headers


#=================================================================
class ProtocolSwitchStatusAndHeadersException(StatusAndHeadersParserException):
    """
    Raised when an HTTP 101 Switching Protocols response is encountered among
    leading responses. The tunneled bytes are not HTTP framing and must not be
    parsed as another response.
    """
    def __init__(self, msg, statusline, raw_headers=b''):
        super(ProtocolSwitchStatusAndHeadersException, self).__init__(msg, statusline)
        self.raw_headers = raw_headers
