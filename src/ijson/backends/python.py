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


def _string_limit_info(limits):
    if not limits or 'max_string_length' not in limits:
        return None, 'max_string_length'
    return limits['max_string_length']


def _scan_string_length(buf, pos, end, decoded_len, pending_high_surrogate,
                        limit, limit_name, token_pos):
    '''
    Advance over the raw contents of a string token, keeping track of the
    number of *decoded* characters seen so far.

    The scan stops in front of an escape sequence that is not complete yet so
    it can be resumed when more input arrives. Surrogate pairs expressed as
    two ``\\uXXXX`` escapes count as a single character.
    '''
    i = pos
    while i < end:
        char = buf[i]
        if char != '\\':
            decoded_len += 1
            pending_high_surrogate = False
            i += 1
        else:
            if i + 1 >= end:
                break
            escape = buf[i + 1]
            if escape == 'u':
                if i + 6 > end:
                    break
                try:
                    codepoint = int(buf[i + 2:i + 6], 16)
                except ValueError:
                    # Leave the actual validation to scanstring(); for the
                    # purpose of the budget this is still one character.
                    decoded_len += 1
                    pending_high_surrogate = False
                    i += 2
                    continue
                if 0xD800 <= codepoint <= 0xDBFF:
                    decoded_len += 1
                    pending_high_surrogate = True
                elif 0xDC00 <= codepoint <= 0xDFFF:
                    if not pending_high_surrogate:
                        decoded_len += 1
                    pending_high_surrogate = False
                else:
                    decoded_len += 1
                    pending_high_surrogate = False
                i += 6
            else:
                decoded_len += 1
                pending_high_surrogate = False
                i += 2

        if limit is not None and decoded_len > limit:
            raise common.JSONLimitError(
                limit_name, token_pos, limit, actual=decoded_len
            )

    return i, decoded_len, pending_high_surrogate


def _check_number_length(lexeme, pos, limit, limit_name):
    if limit is None or not lexeme:
        return
    # Only numeric-looking tokens are subject to this budget. Keywords such
    # as true/false/null share the lexer's character class but aren't numbers.
    if lexeme[0].isdigit() or lexeme[0] in '-+':
        if len(lexeme) > limit:
            raise common.JSONLimitError(
                limit_name, pos, limit, actual=len(lexeme)
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
def Lexer(target, limits=None):
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
    string_limit, string_limit_name = _string_limit_info(limits)
    number_limit, number_limit_name = (None, 'max_number_length')
    if limits and 'max_number_length' in limits:
        number_limit, number_limit_name = limits['max_number_length']
    while True:
        match = LEXEME_RE.search(buf, pos)
        if match:
            lexeme = match.group()
            if lexeme == '"':
                pos = match.start()
                start = pos + 1
                token_pos = discarded + pos
                scan_pos = start
                decoded_len = 0
                pending_high_surrogate = False
                while True:
                    try:
                        end = buf.index('"', start)
                        escpos = end - 1
                        while buf[escpos] == '\\':
                            escpos -= 1
                        escaped = (end - escpos) % 2 == 0
                    except ValueError:
                        end = len(buf)
                        escaped = None

                    if string_limit is not None:
                        scan_pos, decoded_len, pending_high_surrogate = \
                            _scan_string_length(
                                buf, scan_pos, end, decoded_len,
                                pending_high_surrogate, string_limit,
                                string_limit_name, token_pos
                            )

                    if escaped is None:
                        try:
                            data = (yield)
                        except GeneratorExit:
                            data = ''
                        if not data:
                            raise common.IncompleteJSONError('Incomplete string lexeme')
                        buf += data
                    elif escaped:
                        start = end + 1
                    else:
                        break
                send((discarded + pos, buf[pos:end + 1]))
                pos = end + 1
            else:
                if number_limit is not None:
                    _check_number_length(
                        lexeme, discarded + match.start(),
                        number_limit, number_limit_name
                    )
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
                    if number_limit is not None:
                        _check_number_length(
                            lexeme, discarded + match.start(),
                            number_limit, number_limit_name
                        )
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


# Parsing states
_PARSE_VALUE = 0
_PARSE_ARRAY_ELEMENT_END = 1
_PARSE_OBJECT_KEY = 2
_PARSE_OBJECT_END = 3

# infinity singletons for overflow checks
inf = float("inf")
neg_inf = float("-inf")

@utils.coroutine
def parse_value(target, multivalue, use_float, limits=None):
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
    depth_limit, depth_limit_name = (None, 'max_depth')
    if limits and 'max_depth' in limits:
        depth_limit, depth_limit_name = limits['max_depth']
    container_depth = 0
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
                new_depth = container_depth + 1
                if depth_limit is not None and new_depth > depth_limit:
                    raise common.JSONLimitError(
                        depth_limit_name, pos, depth_limit, actual=new_depth
                    )
                container_depth = new_depth
                send(('start_array', None))
                pos, symbol = (yield)
                if (pos, symbol) == EOF:
                    raise common.IncompleteJSONError('Incomplete JSON content')
                if symbol == ']':
                    send(('end_array', None))
                    container_depth -= 1
                    pop()
                else:
                    prev_pos, prev_symbol = pos, symbol
                    push(_PARSE_ARRAY_ELEMENT_END)
                    push(_PARSE_VALUE)
            # Object start
            elif symbol == '{':
                new_depth = container_depth + 1
                if depth_limit is not None and new_depth > depth_limit:
                    raise common.JSONLimitError(
                        depth_limit_name, pos, depth_limit, actual=new_depth
                    )
                container_depth = new_depth
                send(('start_map', None))
                pos, symbol = (yield)
                if (pos, symbol) == EOF:
                    raise common.IncompleteJSONError('Incomplete JSON content')
                if symbol == '}':
                    send(('end_map', None))
                    container_depth -= 1
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
                container_depth -= 1
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
                container_depth -= 1
                pop()
                pop()


def parse_string(symbol):
    return scanstring(symbol, 1)[0]


def basic_parse_basecoro(target, multiple_values=False, allow_comments=False,
                         use_float=False, max_string_length=None,
                         max_number_length=None, max_depth=None, **config):
    '''
    Iterator yielding unprefixed events.

    Parameters:

    - file: a readable file-like object with JSON input
    '''
    if allow_comments:
        raise ValueError("Comments are not supported by the python backend")

    limits = common._resolve_resource_limits(config)
    if config:
        unexpected = next(iter(config))
        raise TypeError(
            "basic_parse_basecoro() got an unexpected keyword argument %r"
            % (unexpected,)
        )
    for name, value in (
        ('max_string_length', max_string_length),
        ('max_number_length', max_number_length),
        ('max_depth', max_depth),
    ):
        if value is not None:
            value = common._validate_limit_value(name, value)
            previous = limits.get(name)
            if previous is not None and previous[0] != value:
                raise ValueError(
                    'Conflicting values for resource limit %s' % (name,)
                )
            limits[name] = (value, name)

    return utf8_encoder(
        Lexer(
            parse_value(target, multiple_values, use_float, limits),
            limits
        )
    )


common.enrich_backend(globals(), c_comments=False, resource_limits=True,
                      limits=True)
