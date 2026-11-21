"""Key functions for memoizing decorators."""

__all__ = ("hashkey", "methodkey", "typedkey", "typedmethodkey")

import inspect


class _HashedTuple(tuple):
    """A tuple subclass formerly used for cache keys.

    Keys are now plain tuples: the key functions return their argument
    tuple as is, saving a copy into a subclass instance on every call.
    The class remains as the kwargs separator and for loading keys
    pickled by earlier versions, which compare equal to plain tuples.

    """


# A sentinel for separating args from kwargs.  Using the class itself
# ensures uniqueness and preserves identity when pickling/unpickling.
_kwmark = (_HashedTuple,)


def hashkey(*args, **kwargs):
    """Return a cache key for the specified hashable arguments."""

    if kwargs:
        return args + _kwmark + tuple(sorted(kwargs.items()))
    else:
        return args


def methodkey(self, /, *args, **kwargs):
    """Return a cache key for use with cached methods."""

    if kwargs:
        return args + _kwmark + tuple(sorted(kwargs.items()))
    else:
        return args


def typedkey(*args, **kwargs):
    """Return a typed cache key for the specified hashable arguments."""

    if kwargs:
        sorted_kwargs = tuple(sorted(kwargs.items()))
        key = args + _kwmark + sorted_kwargs
        key += tuple([type(v) for _, v in sorted_kwargs])
    else:
        key = args
    return key + tuple(map(type, args))


def typedmethodkey(self, /, *args, **kwargs):
    """Return a typed cache key for use with cached methods."""

    if kwargs:
        sorted_kwargs = tuple(sorted(kwargs.items()))
        key = args + _kwmark + sorted_kwargs
        key += tuple([type(v) for _, v in sorted_kwargs])
    else:
        key = args
    return key + tuple(map(type, args))


# Marker separating the method namespace from its normalized arguments, so
# keys created by boundkey() cannot collide with plain tuple keys returned by
# the other key functions.
_boundmark = ("__cachetools_boundkey__",)


def boundkey(method, namespace):
    """Create a signature-aware key function for a cached method.

    The returned function binds the receiver and all call arguments to the
    signature of *method*, applies parameter defaults and normalizes the
    result into a cache key.  Calls that Python treats as equivalent, i.e.
    positional vs. keyword arguments and explicitly passed vs. defaulted
    values, therefore share a single cache entry.  The receiver itself is
    excluded from the key, and *namespace* keeps the keys of distinct methods
    apart when several methods share one cache.

    """

    signature = inspect.signature(method)
    try:
        receiver = next(iter(signature.parameters))
    except StopIteration:
        raise TypeError(
            "bind requires a method with at least one receiver parameter"
        ) from None

    def key(obj, /, *args, **kwargs):
        bound = signature.bind(obj, *args, **kwargs)
        bound.apply_defaults()
        arguments = dict(bound.arguments)
        arguments.pop(receiver, None)
        items = []
        for name, value in arguments.items():
            kind = signature.parameters[name].kind
            if kind is inspect.Parameter.VAR_KEYWORD:
                # normalize the order of arbitrary keyword arguments
                items.extend(sorted(value.items()))
            elif kind is inspect.Parameter.VAR_POSITIONAL:
                items.extend((name, item) for item in value)
            else:
                items.append((name, value))
        return _boundmark + (namespace, tuple(items))

    key.__qualname__ = f"boundkey({namespace!r})"
    return key
