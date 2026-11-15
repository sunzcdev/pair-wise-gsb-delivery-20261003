import datetime
import math


# Retry decision actions.
RETRY_ABORT = 'abort'
RETRY_NOW = 'retry-now'
RETRY_AT = 'retry-at'
RETRY_AFTER = 'retry-after'


class RetryDecision(object):
    """
    Return value of a task's retry-policy callback describing what should
    happen after a task failure. Instances are created using the factory
    classmethods rather than the constructor:

    * :meth:`abort` give up and mark the task permanently failed.
    * :meth:`retry_now` re-enqueue the task immediately.
    * :meth:`retry_at` schedule the next attempt at an absolute datetime.
    * :meth:`retry_after` schedule the next attempt after a number of seconds.

    The Huey runtime re-validates every decision before it touches the
    scheduling queue, so a policy that returns a time in the past, an illegal
    interval or an unknown object can never poison the schedule -- the task
    fails definitively instead.
    """
    __slots__ = ('action', 'eta', 'delay')

    def __init__(self, action, eta=None, delay=None):
        self.action = action
        self.eta = eta
        self.delay = delay

    def __repr__(self):
        if self.action == RETRY_ABORT:
            return '<RetryDecision: abort>'
        elif self.action == RETRY_NOW:
            return '<RetryDecision: retry now>'
        elif self.action == RETRY_AFTER:
            return '<RetryDecision: retry after %s seconds>' % self.delay
        return '<RetryDecision: retry at %s>' % self.eta

    def __eq__(self, rhs):
        return (isinstance(rhs, RetryDecision) and
                self.action == rhs.action and self.eta == rhs.eta and
                self.delay == rhs.delay)

    def __ne__(self, rhs):
        return not self.__eq__(rhs)

    def __hash__(self):
        return hash((self.action, self.eta, self.delay))

    @classmethod
    def abort(cls):
        """Give up: the task enters its final failed state."""
        return cls(RETRY_ABORT)

    @classmethod
    def retry_now(cls):
        """Re-enqueue the task for an immediate retry."""
        return cls(RETRY_NOW)

    @classmethod
    def retry_at(cls, eta):
        """Schedule the next attempt for the given absolute datetime."""
        if not isinstance(eta, datetime.datetime):
            raise TypeError('eta must be a datetime instance, got %r' %
                            type(eta))
        return cls(RETRY_AT, eta=eta)

    @classmethod
    def retry_after(cls, delay):
        """Schedule the next attempt after ``delay`` seconds (or timedelta)."""
        if isinstance(delay, datetime.timedelta):
            delay = delay.total_seconds()
        if isinstance(delay, bool) or not isinstance(delay, (int, float)):
            raise TypeError('delay must be a number of seconds, got %r' %
                            type(delay))
        if not math.isfinite(delay):
            raise ValueError('delay must be a finite number of seconds')
        if delay <= 0:
            raise ValueError('delay must be greater than zero')
        return cls(RETRY_AFTER, delay=float(delay))
