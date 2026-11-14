'''
Pure-python parsing backend.
'''
from json.decoder import scanstring
import re

from ijson import common, utils
import codecs


LEXEME_RE = re.compile(r'[a-z0-9eE\.\+-]+|\S')
INTEGER_RE = re.compile(r'-?[0-9]+')
UNARY_LEXEMES = set('[]{},')
EOF = -1, None

# Number of characters of input kept on each side of an error position
# when building the context snippet attached to errors
ERROR_CONTEXT_SIZE = 40


class UnexpectedSymbol(common.JSONError):
    def __init__(self, symbol, pos):
        super(UnexpectedSymbol, self).__init__(
            'Unexpected symbol %r at %d' % (symbol, pos)
        )


class ErrorTracker:
    '''
    Tracks the correspondence between positions in the decoded character
    stream and locations in the original utf-8 input (byte offsets, lines
    and columns), so that errors can be located in the original input.

    Only a bounded lookback window of the already-consumed input is kept
    around to build error context snippets; the full input is never copied.
    A `context_size` of 0 disables context snippets altogether.
    '''
    def __init__(self, context_size=ERROR_CONTEXT_SIZE):
        self.context_size = context_size
        self.discarded = 0   # decoded characters dropped from the buffer
        self.byte_base = 0   # input bytes corresponding to `discarded` chars
        self.line_base = 0   # newline count within the dropped text
        self.line_start = 0  # global char position of the current line start
        self.lookback = ''   # bounded tail of dropped text kept for context
        self.buf = ''        # the lexer's current buffer (shared, not copied)
        self.buf_bytes = 0   # input bytes decoded into the current buffer

    def extend(self, buf, byte_len):
        '''Accounts for a freshly decoded chunk of `byte_len` input bytes,
        after which the lexer's buffer is `buf`.'''
        self.buf = buf
        self.buf_bytes += byte_len

    def discard(self):
        '''Drops the current (fully consumed) buffer, keeping only a bounded
        lookback tail and the running byte/line counters.'''
        buf = self.buf
        self.byte_base += self.buf_bytes
        self.buf_bytes = 0
        self.line_base += buf.count('\n')
        last_newline = buf.rfind('\n')
        if last_newline >= 0:
            self.line_start = self.discarded + last_newline + 1
        self.discarded += len(buf)
        if self.context_size:
            self.lookback = (self.lookback + buf)[-self.context_size:]
        self.buf = ''

    @property
    def end(self):
        '''Character position of the end of the input received so far.'''
        return self.discarded + len(self.buf)

    def byte_position(self, pos):
        '''Zero-based byte offset in the original input for the decoded
        character at `pos`.'''
        return self.byte_base + len(self.buf[:pos - self.discarded].encode('utf-8'))

    def line_column(self, pos):
        '''One-based (line, column) of the decoded character at `pos`.'''
        local = pos - self.discarded
        line = self.line_base + self.buf.count('\n', 0, local) + 1
        last_newline = self.buf.rfind('\n', 0, local)
        if last_newline >= 0:
            line_start = self.discarded + last_newline + 1
        else:
            line_start = self.line_start
        return line, pos - line_start + 1

    def context(self, pos):
        '''A bounded snippet of the input surrounding `pos`, or None if
        context snippets are disabled.'''
        if not self.context_size:
            return None
        size = self.context_size
        text = self.lookback + self.buf
        index = pos - (self.discarded - len(self.lookback))
        return text[max(0, index - size):index + size]


def _locate(error, tracker, pos):
    '''Attaches the original-input location of `pos` (zero-based byte
    offset, one-based line and column, and a bounded context snippet)
    to `error`, and returns it.'''
    if tracker is None:
        return error
    byte_pos = tracker.byte_position(pos)
    line, column = tracker.line_column(pos)
    error.pos = byte_pos
    error.line = line
    error.column = column
    error.context = tracker.context(pos)
    error.args = ('%s (line %d, column %d, byte %d)' % (
        error.args[0], line, column, byte_pos
    ),)
    return error


def _incomplete_error(tracker, message='Incomplete JSON content'):
    '''Builds an IncompleteJSONError located at the end of the input.'''
    error = common.IncompleteJSONError(message)
    if tracker is not None:
        _locate(error, tracker, tracker.end)
    return error


@utils.coroutine
def utf8_encoder(target, tracker=None):
    decoder = codecs.getincrementaldecoder('utf-8')()
    decode = decoder.decode
    send = target.send
    bytes_fed = 0
    while True:
        try:
            final = False
            bdata = (yield)
        except GeneratorExit:
            final = True
            bdata = b''
        pending = len(decoder.getstate()[0])
        try:
            sdata = decode(bdata, final)
        except UnicodeDecodeError as e:
            try:
                target.close()
            except:
                pass
            if tracker is not None:
                # e.start is relative to the decoder's buffered input
                byte_pos = bytes_fed - pending + e.start
                line, column = tracker.line_column(tracker.end)
                error = common.IncompleteJSONError(
                    '%s (line %d, column %d, byte %d)' % (e, line, column, byte_pos)
                )
                error.pos = byte_pos
                error.line = line
                error.column = column
                error.context = tracker.context(tracker.end)
                raise error
            raise common.IncompleteJSONError(e)
        except:
            try:
                target.close()
            except:
                pass
            raise
        bytes_fed += len(bdata)
        if sdata:
            byte_len = pending + len(bdata) - len(decoder.getstate()[0])
            send((sdata, byte_len))
        elif not bdata:
            target.close()
            break


def _unpack_chunk(received):
    '''Normalizes a chunk of decoded data into a (text, byte length) tuple.
    Plain strings are accepted for backwards compatibility.'''
    if isinstance(received, tuple):
        return received
    return received, len(received.encode('utf-8'))


@utils.coroutine
def Lexer(target, tracker=None):
    """
    Parses lexemes out of the incoming content, and sends them to parse_value.
    A special EOF result is sent when the data source has been exhausted to
    give parse_value the possibility of raising custom exceptions due to missing
    content.
    """
    try:
        received = (yield)
    except GeneratorExit:
        received = None
    data, byte_len = _unpack_chunk(received) if received is not None else ('', 0)
    buf = data
    pos = 0
    discarded = 0
    send = target.send
    if tracker is not None:
        tracker.extend(buf, byte_len)
    while True:
        match = LEXEME_RE.search(buf, pos)
        if match:
            lexeme = match.group()
            if lexeme == '"':
                pos = match.start()
                start = pos + 1
                while True:
                    try:
                        end = buf.index('"', start)
                        escpos = end - 1
                        while buf[escpos] == '\\':
                            escpos -= 1
                        if (end - escpos) % 2 == 0:
                            start = end + 1
                        else:
                            break
                    except ValueError:
                        try:
                            received = (yield)
                        except GeneratorExit:
                            received = None
                        if received is None:
                            raise _incomplete_error(tracker, 'Incomplete string lexeme')
                        data, byte_len = _unpack_chunk(received)
                        buf += data
                        if tracker is not None:
                            tracker.extend(buf, byte_len)
                send((discarded + pos, buf[pos:end + 1]))
                pos = end + 1
            else:
                while lexeme not in UNARY_LEXEMES and match.end() == len(buf):
                    try:
                        received = (yield)
                    except GeneratorExit:
                        received = None
                    if received is None:
                        data = ''
                        break
                    data, byte_len = _unpack_chunk(received)
                    buf += data
                    if tracker is not None:
                        tracker.extend(buf, byte_len)
                    match = LEXEME_RE.search(buf, pos)
                    lexeme = match.group()
                send((discarded + match.start(), lexeme))
                pos = match.end()
        else:
            # Don't ask data from an already exhausted source
            if data:
                try:
                    received = (yield)
                except GeneratorExit:
                    received = None
                data, byte_len = _unpack_chunk(received) if received is not None else ('', 0)
            if not data:
                # Normally should raise StopIteration, but can raise
                # IncompleteJSONError too, which is the point of sending EOF
                try:
                    target.send(EOF)
                except StopIteration:
                    pass
                break
            discarded += len(buf)
            if tracker is not None:
                tracker.discard()
            buf = data
            if tracker is not None:
                tracker.extend(buf, byte_len)
            pos = 0


# Parsing states
_PARSE_VALUE = 0
_PARSE_ARRAY_ELEMENT_END = 1
_PARSE_OBJECT_KEY = 2
_PARSE_OBJECT_END = 3

# infinity singletons for overflow checks
inf = float("inf")
neg_inf = float("-inf")

@utils.coroutine
def parse_value(target, multivalue, use_float, tracker=None):
    """
    Parses results coming out of the Lexer into ijson events, which are sent to
    `target`. A stack keeps track of the type of object being parsed at the time
    (a value, and object or array -- the last two being values themselves).

    A special EOF result coming from the Lexer indicates that no more content is
    expected. This is used to check for incomplete content and raise the
    appropriate exception, which wouldn't be possible if the Lexer simply closed
    this co-routine (either explicitly via .close(), or implicitly by itself
    finishing and decreasing the only reference to the co-routine) since that
    causes a GeneratorExit exception that cannot be replaced with a custom one.
    """

    state_stack = [_PARSE_VALUE]
    pop = state_stack.pop
    push = state_stack.append
    send = target.send
    prev_pos, prev_symbol = None, None
    to_number = common.integer_or_float if use_float else common.integer_or_decimal
    while True:

        if prev_pos is None:
            pos, symbol = (yield)
            if (pos, symbol) == EOF:
                if state_stack:
                    raise _incomplete_error(tracker)
                break
        else:
            pos, symbol = prev_pos, prev_symbol
            prev_pos, prev_symbol = None, None
        try:
            state = state_stack[-1]
        except IndexError:
            if multivalue:
                state = _PARSE_VALUE
                push(state)
            else:
                raise _locate(common.JSONError('Additional data found'), tracker, pos)
        assert state_stack

        if state == _PARSE_VALUE:
            # Simple, common cases
            if symbol == 'null':
                send(('null', None))
                pop()
            elif symbol == 'true':
                send(('boolean', True))
                pop()
            elif symbol == 'false':
                send(('boolean', False))
                pop()
            elif symbol[0] == '"':
                send(('string', parse_string(symbol)))
                pop()
            # Array start
            elif symbol == '[':
                send(('start_array', None))
                pos, symbol = (yield)
                if (pos, symbol) == EOF:
                    raise _incomplete_error(tracker)
                if symbol == ']':
                    send(('end_array', None))
                    pop()
                else:
                    prev_pos, prev_symbol = pos, symbol
                    push(_PARSE_ARRAY_ELEMENT_END)
                    push(_PARSE_VALUE)
            # Object start
            elif symbol == '{':
                send(('start_map', None))
                pos, symbol = (yield)
                if (pos, symbol) == EOF:
                    raise _incomplete_error(tracker)
                if symbol == '}':
                    send(('end_map', None))
                    pop()
                else:
                    prev_pos, prev_symbol = pos, symbol
                    push(_PARSE_OBJECT_KEY)
            # A number
            else:
                # JSON numbers can't contain leading zeros
                if ((len(symbol) > 1 and symbol[0] == '0' and symbol[1] not in ('e', 'E', '.')) or
                    (len(symbol) > 2 and symbol[0:2] == '-0' and symbol[2] not in ('e', 'E', '.'))):
                    raise _locate(common.JSONError('Invalid JSON number: %s' % (symbol,)), tracker, pos)
                # JSON numbers can't start with a plus sign
                if symbol[0] == '+':
                    raise _locate(common.JSONError(f'Invalid JSON number: {symbol}'), tracker, pos)
                # Fractions need a leading digit and must be followed by a digit
                dot = symbol.find('.')
                if dot != -1 and not (symbol[dot - 1:dot].isdigit() and symbol[dot + 1:dot + 2].isdigit()):
                    raise _locate(common.JSONError('Invalid JSON number: %s' % (symbol,)), tracker, pos)
                try:
                    number = to_number(symbol)
                    if number == inf or number == neg_inf:
                        raise _locate(common.JSONError("float overflow: %s" % (symbol,)), tracker, pos)
                except:
                    if 'true'.startswith(symbol) or 'false'.startswith(symbol) or 'null'.startswith(symbol):
                        raise _incomplete_error(tracker)
                    # a well-formed integer that int() still refused is one
                    # longer than sys.get_int_max_str_digits(); let that
                    # error through as the other backends do
                    if INTEGER_RE.fullmatch(symbol):
                        raise
                    raise _locate(UnexpectedSymbol(symbol, pos), tracker, pos)
                else:
                    send(('number', number))
                    pop()

        elif state == _PARSE_OBJECT_KEY:
            if symbol[0] != '"':
                raise _locate(UnexpectedSymbol(symbol, pos), tracker, pos)
            send(('map_key', parse_string(symbol)))
            pos, symbol = (yield)
            if (pos, symbol) == EOF:
                raise _incomplete_error(tracker)
            if symbol != ':':
                raise _locate(UnexpectedSymbol(symbol, pos), tracker, pos)
            state_stack[-1] = _PARSE_OBJECT_END
            push(_PARSE_VALUE)

        elif state == _PARSE_OBJECT_END:
            if symbol == ',':
                state_stack[-1] = _PARSE_OBJECT_KEY
            elif symbol != '}':
                raise _locate(UnexpectedSymbol(symbol, pos), tracker, pos)
            else:
                send(('end_map', None))
                pop()
                pop()

        elif state == _PARSE_ARRAY_ELEMENT_END:
            if symbol == ',':
                state_stack[-1] = _PARSE_ARRAY_ELEMENT_END
                push(_PARSE_VALUE)
            elif symbol != ']':
                raise _locate(UnexpectedSymbol(symbol, pos), tracker, pos)
            else:
                send(('end_array', None))
                pop()
                pop()


def parse_string(symbol):
    return scanstring(symbol, 1)[0]


def basic_parse_basecoro(target, multiple_values=False, allow_comments=False,
                         use_float=False, error_context=True):
    '''
    Iterator yielding unprefixed events.

    Parameters:

    - file: a readable file-like object with JSON input
    - error_context: whether errors should carry a context snippet of the
      input surrounding the error location
    '''
    if allow_comments:
        raise ValueError("Comments are not supported by the python backend")
    tracker = ErrorTracker(ERROR_CONTEXT_SIZE if error_context else 0)
    return utf8_encoder(
        Lexer(
            parse_value(target, multiple_values, use_float, tracker),
            tracker
        ),
        tracker
    )


common.enrich_backend(globals(), c_comments=False)
