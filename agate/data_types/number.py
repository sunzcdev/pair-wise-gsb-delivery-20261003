import warnings
from decimal import Decimal, InvalidOperation

from babel.core import Locale
from babel.numbers import parse_grouping

from agate.data_types.base import DataType
from agate.exceptions import CastError

#: A list of currency symbols sourced from `Xe <https://www.xe.com/symbols/>`_.
DEFAULT_CURRENCY_SYMBOLS = ['؋', '$', 'ƒ', '៛', '¥', '₡', '₱', '£', '€', '¢', '﷼', '₪', '₩', '₭', '₮',
                            '₦', '฿', '₤', '₫']

POSITIVE = Decimal('1')
NEGATIVE = Decimal('-1')


class Number(DataType):
    """
    Data representing numbers.

    :param locale:
        A locale specification such as :code:`en_US` or :code:`de_DE` to use
        for parsing formatted numbers.
    :param group_symbol:
        A grouping symbol used in the numbers. Overrides the value provided by
        the specified :code:`locale`.
    :param decimal_symbol:
        A decimal separate symbol used in the numbers. Overrides the value
        provided by the specified :code:`locale`.
    :param currency_symbols:
        A sequence of currency symbols to strip from numbers.
    :param no_leading_zeroes:
        Whether to disallow leading zeroes.
    :param strict_grouping:
        Whether to require digit grouping to follow the locale's primary and
        secondary grouping lengths. By default, grouping symbols are removed
        without validation, so incorrectly grouped values such as
        :code:`1,23,456` are still parsed. When enabled, only correctly grouped
        (e.g. :code:`1,23,456` for an Indian locale) and ungrouped (e.g.
        :code:`123456`) values are accepted. Grouping symbols are never
        allowed in the fractional or exponent portion of a number.
    """
    def __init__(self, locale='en_US', group_symbol=None, decimal_symbol=None,
                 currency_symbols=DEFAULT_CURRENCY_SYMBOLS, no_leading_zeroes=None,
                 strict_grouping=False, **kwargs):
        super().__init__(**kwargs)

        self.locale = Locale.parse(locale)
        self.currency_symbols = currency_symbols
        self.no_leading_zeroes = no_leading_zeroes
        self.strict_grouping = strict_grouping

        # Suppress Babel warning on Python 3.6
        # See #665
        with warnings.catch_warnings():
            warnings.simplefilter("ignore")

            # Babel 2.14 support.
            # https://babel.pocoo.org/en/latest/changelog.html#possibly-backwards-incompatible-changes
            number_symbols = self.locale.number_symbols.get('latn', self.locale.number_symbols)
            self.group_symbol = group_symbol or number_symbols.get('group', ',')
            self.decimal_symbol = decimal_symbol or number_symbols.get('decimal', '.')

            # The grouping sizes are taken from the integer portion of the
            # locale decimal pattern, e.g. "#,##0" -> (3, 3) and
            # "#,##,##0" -> (3, 2) for Indian-style grouping.
            decimal_pattern = self.locale.decimal_formats[None].pattern.split('.')[0]
            self._primary_grouping, self._secondary_grouping = parse_grouping(decimal_pattern)

    def cast(self, d):
        """
        Cast a single value to a :class:`decimal.Decimal`.

        :returns:
            :class:`decimal.Decimal` or :code:`None`.
        """
        if isinstance(d, Decimal) or d is None:
            return d

        t = type(d)

        if t is int:
            return Decimal(d)
        if t is float:
            return Decimal(repr(d))
        if d is False:
            return Decimal(0)
        if d is True:
            return Decimal(1)
        if not isinstance(d, str):
            raise CastError('Can not parse value "%s" as Decimal.' % d)

        d = d.strip()

        if d.lower() in self.null_values:
            return None

        if self.strict_grouping:
            d, sign = self._parse_strict(d)
        else:
            d, sign = self._parse_lenient(d)

        if self.no_leading_zeroes and len(d) > 1 and d[0] == '0' and d[1] != '.':
            raise CastError('Can not parse value "%s" as Decimal without leading zeroes' % d)

        try:
            return Decimal(d) * sign
        # The Decimal class will return an InvalidOperation exception on most Python implementations,
        # but PyPy3 may return a ValueError if the string is not translatable to ASCII
        except (InvalidOperation, ValueError):
            pass

        raise CastError('Can not parse value "%s" as Decimal.' % d)

    def _parse_lenient(self, d):
        d = d.strip('%')

        if len(d) > 0 and d[0] == '-':
            d = d[1:]
            sign = NEGATIVE
        else:
            sign = POSITIVE

        for symbol in self.currency_symbols:
            d = d.strip(symbol)

        d = d.replace(self.group_symbol, '')
        d = d.replace(self.decimal_symbol, '.')

        return d, sign

    def _parse_strict(self, d):
        value = d

        # Percent symbols may only wrap the entire value, at most one on each
        # side, and never appear within the numeric body.
        leading_percent = 0
        while d.startswith('%'):
            leading_percent += 1
            d = d[1:]

        trailing_percent = 0
        while d.endswith('%'):
            trailing_percent += 1
            d = d[:-1]
        has_percent_on_both_sides = leading_percent and trailing_percent
        if '%' in d or leading_percent > 1 or trailing_percent > 1 or has_percent_on_both_sides:
            raise CastError('Can not parse value "%s" as Decimal.' % value)

        # Currency symbols may only wrap the value, and at most one sign may
        # lead the number, either before or after a leading currency symbol.
        # Symbols are stripped positionally so the structure is validated
        # before anything is removed from the middle.
        d = d.strip()
        sign = POSITIVE
        sign_seen = False

        def take_sign():
            nonlocal d, sign, sign_seen
            if sign_seen or not d or d[0] not in '+-':
                return False
            if d[0] == '-':
                sign = NEGATIVE
            sign_seen = True
            d = d[1:].lstrip()
            return True

        take_sign()

        leading_currency = next((symbol for symbol in self.currency_symbols if d.startswith(symbol)), None)
        if leading_currency is not None:
            d = d[len(leading_currency):].lstrip()
            take_sign()

        trailing_currency = next((symbol for symbol in self.currency_symbols if d.endswith(symbol)), None)
        if trailing_currency is not None:
            d = d[:-len(trailing_currency)].rstrip()

        for symbol in self.currency_symbols:
            if symbol in d:
                raise CastError('Can not parse value "%s" as Decimal.' % value)

        if not d:
            raise CastError('Can not parse value "%s" as Decimal.' % value)

        # Interior whitespace is never valid except as the locale grouping
        # symbol (e.g. the narrow no-break space used in the French locale).
        if any(char.isspace() and char != self.group_symbol for char in d):
            raise CastError('Can not parse value "%s" as Decimal.' % value)

        # Only ASCII digits and the expected locale symbols may make up the
        # numeric body. This rejects underscores, Unicode digits and stray
        # signs before Decimal applies its own lenient parsing. The sign of
        # an exponent is validated separately.
        allowed = set('0123456789eE+-') | {self.group_symbol, self.decimal_symbol}
        if any(char not in allowed for char in d):
            raise CastError('Can not parse value "%s" as Decimal.' % value)

        exponent_marker = next((marker for marker in ('e', 'E') if marker in d), None)

        if exponent_marker:
            mantissa, exponent = d.split(exponent_marker, 1)
            self._validate_exponent(exponent, value)
        else:
            mantissa = d

        if '+' in mantissa or '-' in mantissa:
            raise CastError('Can not parse value "%s" as Decimal.' % value)

        self._validate_mantissa(mantissa, value)

        d = d.replace(self.group_symbol, '').replace(self.decimal_symbol, '.').replace('E', 'e')

        return d, sign

    def _validate_exponent(self, exponent, original):
        # An exponent is an optional sign followed by plain ASCII digits. It
        # may contain neither grouping nor decimal symbols.
        if not exponent:
            raise CastError('Can not parse value "%s" as Decimal.' % original)
        if exponent[0] in '+-':
            digits = exponent[1:]
        else:
            digits = exponent
        if not digits or not digits.isascii() or not digits.isdigit():
            raise CastError('Can not parse value "%s" as Decimal.' % original)

    def _validate_mantissa(self, mantissa, original):
        if not mantissa:
            raise CastError('Can not parse value "%s" as Decimal.' % original)

        if self.decimal_symbol in mantissa:
            integer_part, fractional_part = mantissa.split(self.decimal_symbol, 1)
            if self.decimal_symbol in fractional_part or (not integer_part and not fractional_part):
                raise CastError('Can not parse value "%s" as Decimal.' % original)
        else:
            integer_part, fractional_part = mantissa, None

        # Grouping symbols are only valid in the integer portion.
        if fractional_part is not None and self.group_symbol in fractional_part:
            raise CastError('Can not parse value "%s" as Decimal.' % original)

        if not integer_part or self.group_symbol not in integer_part:
            return

        groups = integer_part.split(self.group_symbol)

        if any(not group or not group.isascii() or not group.isdigit() for group in groups):
            raise CastError('Can not parse value "%s" as Decimal.' % original)

        # An ungrouped integer is always valid, even when its length does not
        # match the grouping sizes of the locale.
        if len(groups) == 1:
            return

        if len(groups[-1]) != self._primary_grouping:
            raise CastError('Can not parse value "%s" as Decimal.' % original)

        for group in groups[1:-1]:
            if len(group) != self._secondary_grouping:
                raise CastError('Can not parse value "%s" as Decimal.' % original)

        if len(groups) <= 3:
            if len(groups[0]) > self._secondary_grouping:
                raise CastError('Can not parse value "%s" as Decimal.' % original)
        elif len(groups[0]) != self._secondary_grouping:
            raise CastError('Can not parse value "%s" as Decimal.' % original)

    def csvify(self, d):
        return d

    def jsonify(self, d):
        if d is None:
            return d

        return float(d)
