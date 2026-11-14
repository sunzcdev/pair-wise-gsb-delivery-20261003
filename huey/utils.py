from collections import namedtuple
import calendar
import contextlib
import datetime
import os
import sys
import threading
import time
import warnings
try:
    import fcntl
except ImportError:
    fcntl = None


if sys.version_info < (3, 12):
    utcnow = datetime.datetime.utcnow
else:
    def utcnow():
        return (datetime.datetime
                .now(datetime.timezone.utc)
                .replace(tzinfo=None))

try:
    from zoneinfo import ZoneInfo
except ImportError:
    ZoneInfo = None


Error = namedtuple('Error', ('metadata',))


class _Skipped(object):
    def __repr__(self):
        return 'SKIPPED'
    def __reduce__(self):
        return 'SKIPPED'

SKIPPED = _Skipped()

ChordConfig = namedtuple('ChordConfig', ('cid', 'size', 'idx', 'callback'))


def load_class(s):
    path, klass = s.rsplit('.', 1)
    __import__(path)
    mod = sys.modules[path]
    return getattr(mod, klass)


def is_naive(dt):
    """
    Determines if a given datetime.datetime is naive.
    The concept is defined in Python's docs:
    http://docs.python.org/library/datetime.html#datetime.tzinfo
    Assuming value.tzinfo is either None or a proper datetime.tzinfo,
    value.utcoffset() implements the appropriate logic.
    """
    return dt.utcoffset() is None


def make_naive(dt):
    """
    Makes an aware datetime.datetime naive in local time zone.
    """
    tt = dt.utctimetuple()
    ts = calendar.timegm(tt)
    local_tt = time.localtime(ts)
    return datetime.datetime(*local_tt[:6])


def aware_to_utc(dt):
    """
    Converts an aware datetime.datetime in UTC time zone.
    """
    return dt.astimezone(datetime.timezone.utc).replace(tzinfo=None)


def local_to_utc(dt):
    """
    Converts a naive local datetime.datetime in UTC time zone.
    """
    return datetime.datetime(*time.gmtime(time.mktime(dt.timetuple()))[:6])


def resolve_timezone(tz):
    """
    Resolve a timezone given as an IANA key (e.g. "America/New_York") or a
    ``datetime.tzinfo`` instance into a ``tzinfo``. ``None`` is returned as-is.
    """
    if tz is None or isinstance(tz, datetime.tzinfo):
        return tz
    elif isinstance(tz, str):
        if ZoneInfo is None:
            raise ValueError('zoneinfo is not available, cannot resolve '
                             'timezone %r' % tz)
        return ZoneInfo(tz)
    raise ValueError('timezone must be None, a string, or a tzinfo instance, '
                     'got %r' % (tz,))


def is_ambiguous_wall_time(wall, tz):
    """
    Whether the naive wall-clock time occurs twice (due to a DST transition
    setting the clock back) in the given timezone.
    """
    return (wall.replace(tzinfo=tz, fold=0).utcoffset() !=
            wall.replace(tzinfo=tz, fold=1).utcoffset())


def periodic_occurrences(validate_date, tz, now_utc, on_ambiguous='once',
                         on_nonexistent='skip', lookback=60):
    """
    Determine which occurrences of a periodic schedule fall due at the given
    instant, evaluating ``validate_date`` against the wall-clock time in the
    timezone ``tz``.

    :param validate_date: predicate accepting a datetime and returning bool.
    :param tz: tzinfo the schedule is expressed in.
    :param now_utc: aware UTC datetime for the current instant.
    :param on_ambiguous: 'once' to fire only on the first occurrence of a
        repeated wall-clock time, 'twice' to fire on both.
    :param on_nonexistent: 'skip' to ignore wall-clock times that never occur,
        'shift' to fire them at the first valid minute after the gap.
    :param lookback: seconds since the previous check, used to detect
        nonexistent wall-clock minutes that were skipped over.
    :return: list of ``(wall_minute, fold)`` tuples identifying each due
        occurrence, where ``wall_minute`` is the naive scheduled wall-clock
        time (truncated to the minute) in ``tz``.
    """
    now_utc = now_utc.replace(second=0, microsecond=0)
    curr = now_utc.astimezone(tz)
    curr_wall = curr.replace(tzinfo=None)
    occurrences = []

    if validate_date(curr):
        fold = curr.fold
        if fold and not is_ambiguous_wall_time(curr_wall, tz):
            fold = 0
        if fold == 0 or on_ambiguous == 'twice':
            occurrences.append((curr_wall, fold))

    if on_nonexistent == 'shift':
        # Detect wall-clock minutes that never occurred because the clock
        # jumped forward between the previous check and this one.
        prev = (now_utc - datetime.timedelta(seconds=lookback)).astimezone(tz)
        prev_wall = prev.replace(tzinfo=None)
        missing = prev_wall + datetime.timedelta(minutes=1)
        while missing < curr_wall:
            if validate_date(missing):
                occurrences.append((missing, 0))
            missing += datetime.timedelta(minutes=1)

    return occurrences


def normalize_expire_time(expires, utc=True):
    if isinstance(expires, datetime.datetime):
        return normalize_time(eta=expires, utc=utc)
    return normalize_time(delay=expires, utc=utc)


def normalize_time(eta=None, delay=None, utc=True):
    if (delay is None) == (eta is None):
        raise ValueError('Specify either an eta (datetime) or delay (seconds)')
    elif delay is not None:
        method = (utc and utcnow or
                  datetime.datetime.now)
        if not isinstance(delay, datetime.timedelta):
            delay = datetime.timedelta(seconds=delay)
        return method() + delay
    elif not isinstance(eta, datetime.datetime):
        raise ValueError('eta must be a datetime instance')
    else:
        has_tz = not is_naive(eta)
        if utc:
            if not has_tz:
                eta = local_to_utc(eta)
            else:
                eta = aware_to_utc(eta)
        elif has_tz:
            # Convert TZ-aware into naive localtime.
            eta = make_naive(eta)
        return eta


def encode(s):
    if isinstance(s, bytes):
        return s
    elif isinstance(s, str):
        return s.encode('utf8')
    elif s is not None:
        return str(s).encode('utf8')


class FileLock(object):
    def __init__(self, filename):
        if fcntl is None:
            warnings.warn('FileLock not supported on this platform. Please '
                          'use a different storage implementation.')
        self.filename = filename
        # Each thread flocks its own fd. A shared fd is clobbered under
        # thread contention, leaking the flock and deadlocking every process.
        self._local = threading.local()

        dirname = os.path.dirname(filename)
        if not os.path.exists(dirname):
            os.makedirs(dirname)

    def acquire(self):
        flags = os.O_CREAT | os.O_TRUNC | os.O_RDWR
        self._local.fd = fd = os.open(self.filename, flags)
        if fcntl is not None:
            fcntl.flock(fd, fcntl.LOCK_EX)

    def release(self):
        fd = getattr(self._local, 'fd', None)
        if fd is not None:
            self._local.fd = None
            if fcntl is not None:
                fcntl.flock(fd, fcntl.LOCK_UN)
            os.close(fd)

    def __enter__(self):
        self.acquire()
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        self.release()


@contextlib.contextmanager
def noop_context():
    yield
