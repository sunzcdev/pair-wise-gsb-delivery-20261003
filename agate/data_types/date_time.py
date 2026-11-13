import datetime
import locale

import isodate
import parsedatetime

from agate.data_types.base import DataType
from agate.exceptions import CastError


class DateTime(DataType):
    """
    Data representing dates with times.

    :param datetime_format:
        A formatting string for :meth:`datetime.datetime.strptime` to use
        instead of using regex-based parsing.
    :param timezone:
        A ``ZoneInfo`` timezone to apply to each parsed date.
    :param locale:
        A locale specification such as :code:`en_US` or :code:`de_DE` to use
        for parsing formatted datetimes.
    :param source_time:
        An optional reference date or datetime to use when interpreting
        relative dates and times such as "yesterday" or "next Monday". If a
        timezone is specified, an aware source time is converted to that
        timezone before being interpreted.
    """
    def __init__(self, datetime_format=None, timezone=None, locale=None, source_time=None, reference_time=None,
                 **kwargs):
        if source_time is None:
            source_time = reference_time

        super().__init__(**kwargs)

        self.datetime_format = datetime_format
        self.timezone = timezone
        self.locale = locale

        self.source_time = source_time
        if source_time is None:
            now = datetime.datetime.now()
            self._source_time = datetime.datetime(
                now.year, now.month, now.day, 0, 0, 0, 0, None
            )
        else:
            self._source_time = source_time
        self._constants = parsedatetime.Constants(localeID=self.locale)
        self._parser = parsedatetime.Calendar(constants=self._constants, version=parsedatetime.VERSION_CONTEXT_STYLE)

    def __getstate__(self):
        """
        Return state values to be pickled. Exclude _parser because parsedatetime
        cannot be pickled.
        """
        odict = self.__dict__.copy()
        del odict['_constants']
        del odict['_parser']
        return odict

    def __setstate__(self, ndict):
        """
        Restore state from the unpickled state values. Set _constants to an instance
        of the parsedatetime Constants class, and _parser to an instance
        of the parsedatetime Calendar class.
        """
        self.__dict__.update(ndict)
        if 'source_time' not in self.__dict__:
            self.source_time = None
        self._constants = parsedatetime.Constants(localeID=self.locale)
        self._parser = parsedatetime.Calendar(constants=self._constants, version=parsedatetime.VERSION_CONTEXT_STYLE)

    def cast(self, d, source_time=None, reference_time=None):
        """
        Cast a single value to a :class:`datetime.datetime`.

        A reference date or datetime may be passed to override the value
        configured on this instance for relative dates.

        If both `date_format` and `locale` have been specified
        in the `agate.DateTime` instance, the `cast()` function
        is not thread-safe.
        :returns: :class:`datetime.datetime` or :code:`None`.
        """
        if isinstance(d, datetime.datetime) or d is None:
            return d
        if isinstance(d, datetime.date):
            return datetime.datetime.combine(d, datetime.time(0, 0, 0))
        if isinstance(d, str):
            d = d.strip()

            if d.lower() in self.null_values:
                return None
        else:
            raise CastError('Can not parse value "%s" as datetime.' % d)

        if self.datetime_format:
            orig_locale = None
            if self.locale:
                orig_locale = locale.getlocale(locale.LC_TIME)
                locale.setlocale(locale.LC_TIME, (self.locale, 'UTF-8'))

            try:
                dt = datetime.datetime.strptime(d, self.datetime_format)
            except (ValueError, TypeError):
                raise CastError('Value "%s" does not match date format.' % d)
            finally:
                if orig_locale:
                    locale.setlocale(locale.LC_TIME, orig_locale)

            return dt

        source_time = self._get_source_time(source_time, reference_time)

        try:
            (_, _, _, _, matched_text), = self._parser.nlp(d, sourceTime=source_time)
        except Exception:
            matched_text = None
        else:
            value, ctx = self._parser.parseDT(
                d,
                sourceTime=source_time,
                tzinfo=self.timezone
            )

            if matched_text == d and ctx.hasDate and ctx.hasTime:
                return value
            if matched_text == d and ctx.hasDate and not ctx.hasTime:
                return datetime.datetime.combine(value.date(), datetime.time.min)

        try:
            dt = isodate.parse_datetime(d)

            return dt
        except Exception:
            pass

        raise CastError('Can not parse value "%s" as datetime.' % d)

    def _get_source_time(self, source_time=None, reference_time=None):
        if source_time is None:
            source_time = reference_time

        if source_time is None:
            source_time = self.source_time

        if source_time is None:
            return self._source_time

        if isinstance(source_time, datetime.datetime):
            if self.timezone is not None and source_time.tzinfo is not None:
                source_time = source_time.astimezone(self.timezone)

            return source_time

        if isinstance(source_time, datetime.date):
            return datetime.datetime.combine(source_time, datetime.time.min)

        raise TypeError('source_time must be a date or datetime.')

    def csvify(self, d):
        if d is None:
            return None

        return d.isoformat()

    def jsonify(self, d):
        return self.csvify(d)
