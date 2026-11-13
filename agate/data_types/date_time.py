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
        A ``tzinfo`` (such as :class:`zoneinfo.ZoneInfo`) to apply to each
        parsed value that does not carry its own offset. Values that already
        have an offset (for example ISO 8601 strings with a ``+06:00``
        suffix) are kept as-is and are never re-interpreted as local wall
        times in this timezone.
    :param ambiguous:
        How to handle local times that occur twice when clocks are turned
        back at the end of daylight saving time: ``'earlier'`` (the default)
        picks the first occurrence, ``'later'`` picks the second occurrence
        and ``'reject'`` raises a :class:`.CastError`.
    :param nonexistent:
        How to handle local times that never occur when clocks are turned
        forward at the start of daylight saving time: ``'reject'`` (the
        default) raises a :class:`.CastError`, ``'shift_forward'`` moves the
        time forward past the gap and ``'shift_backward'`` moves it backward
        before the gap.
    :param locale:
        A locale specification such as :code:`en_US` or :code:`de_DE` to use
        for parsing formatted datetimes.
    """
    def __init__(self, datetime_format=None, timezone=None, locale=None,
                 ambiguous='earlier', nonexistent='reject', **kwargs):
        super().__init__(**kwargs)

        if ambiguous not in ('earlier', 'later', 'reject'):
            raise ValueError('ambiguous must be "earlier", "later" or "reject".')
        if nonexistent not in ('reject', 'shift_forward', 'shift_backward'):
            raise ValueError('nonexistent must be "reject", "shift_forward" or "shift_backward".')

        self.datetime_format = datetime_format
        self.timezone = timezone
        self.locale = locale
        self.ambiguous = ambiguous
        self.nonexistent = nonexistent

        now = datetime.datetime.now()
        self._source_time = datetime.datetime(
            now.year, now.month, now.day, 0, 0, 0, 0, None
        )
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
        self._constants = parsedatetime.Constants(localeID=self.locale)
        self._parser = parsedatetime.Calendar(constants=self._constants, version=parsedatetime.VERSION_CONTEXT_STYLE)

    def _timezone_name(self):
        """
        A stable, machine-independent name for the configured timezone.
        """
        return getattr(self.timezone, 'key', None) or str(self.timezone)

    @staticmethod
    def _round_trips(candidate, wall_time):
        """
        Check whether an aware candidate maps back to the naive wall time.
        """
        return candidate.astimezone(datetime.timezone.utc).astimezone(candidate.tzinfo).replace(tzinfo=None) == wall_time

    def _apply_timezone(self, dt, text):
        """
        Attach the configured timezone to a parsed datetime.

        Aware values keep their own offset. Naive values are interpreted as
        local wall times in the configured timezone, resolving DST folds and
        gaps according to the ``ambiguous`` and ``nonexistent`` policies.
        """
        if self.timezone is None:
            return dt
        if dt.tzinfo is not None and dt.utcoffset() is not None:
            return dt

        earlier = dt.replace(tzinfo=self.timezone, fold=0)
        later = dt.replace(tzinfo=self.timezone, fold=1)

        if earlier.utcoffset() == later.utcoffset():
            return earlier

        if self._round_trips(earlier, dt) and self._round_trips(later, dt):
            if self.ambiguous == 'earlier':
                return earlier
            if self.ambiguous == 'later':
                return later
            raise CastError(
                'Can not parse value "%s" as datetime: the local time occurs twice in timezone "%s".'
                % (text, self._timezone_name())
            )

        if self.nonexistent == 'shift_forward':
            return self._apply_timezone(dt + (later.utcoffset() - earlier.utcoffset()), text)
        if self.nonexistent == 'shift_backward':
            return self._apply_timezone(dt - (later.utcoffset() - earlier.utcoffset()), text)
        raise CastError(
            'Can not parse value "%s" as datetime: the local time does not exist in timezone "%s".'
            % (text, self._timezone_name())
        )

    def cast(self, d):
        """
        Cast a single value to a :class:`datetime.datetime`.

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

            return self._apply_timezone(dt, d)

        try:
            (_, _, _, _, matched_text), = self._parser.nlp(d, sourceTime=self._source_time)
        except Exception:
            matched_text = None
        else:
            value, ctx = self._parser.parseDT(
                d,
                sourceTime=self._source_time
            )

            if matched_text == d and ctx.hasDate and ctx.hasTime:
                return self._apply_timezone(value, d)
            if matched_text == d and ctx.hasDate and not ctx.hasTime:
                return self._apply_timezone(datetime.datetime.combine(value.date(), datetime.time.min), d)

        try:
            dt = isodate.parse_datetime(d)

            return self._apply_timezone(dt, d)
        except Exception:
            pass

        raise CastError('Can not parse value "%s" as datetime.' % d)

    def csvify(self, d):
        if d is None:
            return None

        return d.isoformat()

    def jsonify(self, d):
        return self.csvify(d)
