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


class UnexpectedSymbol(common.JSONError):
    def __init__(self, symbol, pos):
        super(UnexpectedSymbol, self).__init__(
            'Unexpected symbol %r at %d' % (symbol, pos)
        )


@utils.coroutine
def utf8_encoder(target):
    decoder = codecs.getincrementaldecoder('utf-8')()
    decode = decoder.decode
    send = target.send
    while True:
        try:
            final = False
            bdata = (yield)
        except GeneratorExit:
            final = True
            bdata = b''
        try:
            sdata = decode(bdata, final)
        except UnicodeDecodeError as e:
            try:
                target.close()
            except:
                pass
            raise common.IncompleteJSONError(e)
        except:
            try:
                target.close()
            except:
                pass
            raise
        if sdata:
            send(sdata)
        elif not bdata:
            target.close()
            break

@utils.coroutine
def Lexer(target):
    """
    Parses lexemes out of the incoming content, and sends them to parse_value.
    A special EOF result is sent when the data source has been exhausted to
    give parse_value the possibility of raising custom exceptions due to missing
    content.
    """
    try:
        data = (yield)
    except GeneratorExit:
        data = ''
    buf = data
    pos = 0
    discarded = 0
    send = target.send
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
                            data = (yield)
                        except GeneratorExit:
                            data = ''
                        if not data:
                            raise common.IncompleteJSONError('Incomplete string lexeme')
                        buf += data
                send((discarded + pos, buf[pos:end + 1]))
                pos = end + 1
            else:
                while lexeme not in UNARY_LEXEMES and match.end() == len(buf):
                    try:
                        data = (yield)
                    except GeneratorExit:
                        data = ''
                    if not data:
                        break
                    buf += data
                    match = LEXEME_RE.search(buf, pos)
                    lexeme = match.group()
                send((discarded + match.start(), lexeme))
                pos = match.end()
        else:
            # Don't ask data from an already exhausted source
            if data:
                try:
                    data = (yield)
                except GeneratorExit:
                    data = ''
            if not data:
                # Normally should raise StopIteration, but can raise
                # IncompleteJSONError too, which is the point of sending EOF
                try:
                    target.send(EOF)
                except StopIteration:
                    pass
                break
            discarded += len(buf)
            buf = data
            pos = 0


@utils.coroutine
def CommentableLexer(target):
    """
    Like ``Lexer``, but transparently skips JavaScript-style line comments
    (``// ...``) and block comments (``/* ... */``) wherever whitespace is
    allowed. Comments are only recognized outside of strings, and their bytes
    are never joined into surrounding lexemes (so two number fragments can't
    be merged into a single number). Positions reported for the surviving
    lexemes are the same positions they would have in the original input.
    """
    try:
        data = (yield)
    except GeneratorExit:
        data = ''
    buf = data
    pos = 0
    discarded = 0
    send = target.send
    # Whether the source has already produced an empty chunk. The first chunk
    # is obtained above, so an empty first chunk means exhaustion from the
    # start; later chunks (and a GeneratorExit on close) update this as data
    # flows through the coroutine.
    exhausted = not data

    while True:
        match = LEXEME_RE.search(buf, pos)
        if not match:
            # Everything before pos has already been consumed (whitespace or
            # a comment), so drop it rather than growing the buffer forever
            if pos:
                discarded += pos
                buf = buf[pos:]
                pos = 0
            if not exhausted:
                try:
                    data = (yield)
                except GeneratorExit:
                    data = ''
                exhausted = not data
            if not data:
                try:
                    target.send(EOF)
                except StopIteration:
                    pass
                break
            buf += data
            continue

        if match.group() == '/':
            start = match.start()
            if start + 1 >= len(buf):
                if exhausted:
                    raise common.JSONError(
                        'Unexpected symbol %r at %d' % ('/', discarded + start))
                try:
                    data = (yield)
                except GeneratorExit:
                    data = ''
                exhausted = not data
                if not data:
                    raise common.JSONError(
                        'Unexpected symbol %r at %d' % ('/', discarded + start))
                buf += data
            if buf[start + 1] == '/':
                # Line comment: everything up to (but excluding) the newline
                search_from = start + 2
                end = buf.find('\n', search_from)
                while end == -1:
                    if exhausted:
                        # A line comment running into EOF closes normally
                        try:
                            target.send(EOF)
                        except StopIteration:
                            pass
                        return
                    try:
                        data = (yield)
                    except GeneratorExit:
                        data = ''
                    exhausted = not data
                    if not data:
                        try:
                            target.send(EOF)
                        except StopIteration:
                            pass
                        return
                    search_from = len(buf)
                    buf += data
                    end = buf.find('\n', search_from)
                else:
                    pos = end
                continue
            elif buf[start + 1] == '*':
                # Block comment: must be closed before the stream ends
                end = buf.find('*/', start + 2)
                while end == -1:
                    if exhausted:
                        raise common.IncompleteJSONError('Incomplete block comment')
                    try:
                        data = (yield)
                    except GeneratorExit:
                        data = ''
                    exhausted = not data
                    if not data:
                        raise common.IncompleteJSONError('Incomplete block comment')
                    # A trailing '*' in the old buffer could start the closing
                    # '*/' together with the first byte of the incoming data
                    # (but the '*' that opens the comment can't also close it)
                    trailing_star = buf.endswith('*') and not (
                        len(buf) == start + 2 and buf[start:start + 2] == '/*')
                    previous_len = len(buf)
                    buf += data
                    if trailing_star and buf[previous_len:previous_len + 1] == '/':
                        end = previous_len - 1
                    else:
                        end = buf.find('*/', previous_len)
                pos = end + 2
                continue
            else:
                # A lone slash is not the start of a comment
                send((discarded + start, '/'))
                pos = start + 1
                continue

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
                    if exhausted:
                        raise common.IncompleteJSONError('Incomplete string lexeme')
                    try:
                        data = (yield)
                    except GeneratorExit:
                        data = ''
                    exhausted = not data
                    if not data:
                        raise common.IncompleteJSONError('Incomplete string lexeme')
                    buf += data
            send((discarded + pos, buf[pos:end + 1]))
            pos = end + 1
        else:
            while lexeme not in UNARY_LEXEMES and match.end() == len(buf):
                if exhausted:
                    break
                try:
                    data = (yield)
                except GeneratorExit:
                    data = ''
                exhausted = not data
                if not data:
                    break
                buf += data
                match = LEXEME_RE.search(buf, pos)
                lexeme = match.group()
            send((discarded + match.start(), lexeme))
            pos = match.end()


# Parsing states
_PARSE_VALUE = 0
_PARSE_ARRAY_ELEMENT_END = 1
_PARSE_OBJECT_KEY = 2
_PARSE_OBJECT_END = 3

# infinity singletons for overflow checks
inf = float("inf")
neg_inf = float("-inf")

@utils.coroutine
def parse_value(target, multivalue, use_float):
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
                    raise common.IncompleteJSONError('Incomplete JSON content')
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
                raise common.JSONError('Additional data found')
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
                    raise common.IncompleteJSONError('Incomplete JSON content')
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
                    raise common.IncompleteJSONError('Incomplete JSON content')
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
                    raise common.JSONError('Invalid JSON number: %s' % (symbol,))
                # JSON numbers can't start with a plus sign
                if symbol[0] == '+':
                    raise common.JSONError(f'Invalid JSON number: {symbol}')
                # Fractions need a leading digit and must be followed by a digit
                dot = symbol.find('.')
                if dot != -1 and not (symbol[dot - 1:dot].isdigit() and symbol[dot + 1:dot + 2].isdigit()):
                    raise common.JSONError('Invalid JSON number: %s' % (symbol,))
                try:
                    number = to_number(symbol)
                    if number == inf or number == neg_inf:
                        raise common.JSONError("float overflow: %s" % (symbol,))
                except:
                    if 'true'.startswith(symbol) or 'false'.startswith(symbol) or 'null'.startswith(symbol):
                        raise common.IncompleteJSONError('Incomplete JSON content')
                    # a well-formed integer that int() still refused is one
                    # longer than sys.get_int_max_str_digits(); let that
                    # error through as the other backends do
                    if INTEGER_RE.fullmatch(symbol):
                        raise
                    raise UnexpectedSymbol(symbol, pos)
                else:
                    send(('number', number))
                    pop()

        elif state == _PARSE_OBJECT_KEY:
            if symbol[0] != '"':
                raise UnexpectedSymbol(symbol, pos)
            send(('map_key', parse_string(symbol)))
            pos, symbol = (yield)
            if (pos, symbol) == EOF:
                raise common.IncompleteJSONError('Incomplete JSON content')
            if symbol != ':':
                raise UnexpectedSymbol(symbol, pos)
            state_stack[-1] = _PARSE_OBJECT_END
            push(_PARSE_VALUE)

        elif state == _PARSE_OBJECT_END:
            if symbol == ',':
                state_stack[-1] = _PARSE_OBJECT_KEY
            elif symbol != '}':
                raise UnexpectedSymbol(symbol, pos)
            else:
                send(('end_map', None))
                pop()
                pop()

        elif state == _PARSE_ARRAY_ELEMENT_END:
            if symbol == ',':
                state_stack[-1] = _PARSE_ARRAY_ELEMENT_END
                push(_PARSE_VALUE)
            elif symbol != ']':
                raise UnexpectedSymbol(symbol, pos)
            else:
                send(('end_array', None))
                pop()
                pop()


def parse_string(symbol):
    return scanstring(symbol, 1)[0]


def basic_parse_basecoro(target, multiple_values=False, allow_comments=False,
                         use_float=False):
    '''
    Iterator yielding unprefixed events.

    Parameters:

    - file: a readable file-like object with JSON input
    '''
    lexer = CommentableLexer if allow_comments else Lexer
    return utf8_encoder(lexer(parse_value(target, multiple_values, use_float)))


common.enrich_backend(globals())
