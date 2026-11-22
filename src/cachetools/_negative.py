"""Negative result caching helpers.

A negative result is an exception raised by the wrapped function,
cached for a short, exception-specific time so that repeated lookups of
something known to be absent do not keep hitting the underlying
storage.  Negative results are kept separately from regular results and
re-raised as fresh exceptions, so a caller never sees the traceback of
an earlier invocation.

"""

__all__ = ()


class _NegativeResult:
    """A cached exception together with its expiration time."""

    __slots__ = ("exception", "expires")

    def __init__(self, exception, expires):
        self.exception = exception
        self.expires = expires


class _NegativeCache:
    """Timer-driven store for short-lived negative results."""

    def __init__(self, policy, timer):
        self.__policy = policy
        self.__timer = timer
        self.__data = {}

    def get(self, key):
        """Return a live negative result for key, or None.

        Expired results are removed on access, so an expired key behaves
        exactly like one that was never cached.

        """
        result = self.__data.get(key)
        if result is None:
            return None
        if self.__timer() < result.expires:
            return result
        del self.__data[key]
        return None

    def store(self, key, exception):
        """Cache exception for key if its type is part of the policy."""
        for exc_type, ttl in self.__policy.items():
            if isinstance(exception, exc_type):
                expires = self.__timer() + ttl
                self.__data[key] = _NegativeResult(exception, expires)
                return exc_type
        return None

    def discard(self, key):
        """Drop any negative result for key, e.g. once a value is cached."""
        self.__data.pop(key, None)

    def clear(self):
        self.__data.clear()

    @staticmethod
    def raise_result(result):
        """Re-raise a cached exception without chaining the original."""
        exception = result.exception
        raise type(exception)(*exception.args) from None
