"""Call level capability policies for JMESPath expressions.

A capability policy limits the set of functions that an expression is
allowed to invoke.  Rules come from multiple sources, so a service needs a
way to restrict which functions a caller can reach.

Policies are pure data: they only hold function *names* (the names under
which functions are actually registered, e.g. ``"length"`` rather than the
``_func_length`` implementation).  They never modify the global function
table and never load or execute code.  The built in functions that only
perform data manipulation keep working as long as their registered name is
permitted.
"""

from jmespath.exceptions import FunctionNotAllowedError


class CapabilityPolicy(object):
    """Restrict which functions may be called from an expression.

    A policy works in one of two modes:

    * ``allowed_functions``: an allow list.  Only calls whose registered
      name is in the collection are permitted; everything else is rejected.
    * ``denied_functions``: a deny list.  Every call is permitted except
      those whose registered name is in the collection.

    Exactly one of the two arguments must be provided.  Names are matched
    against the function's actual registered name, never against the
    repr of the underlying callable, so overriding a built in function in a
    custom :class:`~jmespath.functions.Functions` subclass cannot bypass a
    restriction on that name.
    """

    def __init__(self, allowed_functions=None, denied_functions=None):
        if allowed_functions is not None and denied_functions is not None:
            raise ValueError(
                "A capability policy must specify either "
                "allowed_functions or denied_functions, not both.")
        if allowed_functions is None and denied_functions is None:
            raise ValueError(
                "A capability policy must specify either "
                "allowed_functions or denied_functions.")
        if allowed_functions is not None:
            self._mode = 'allow'
            self._functions = frozenset(allowed_functions)
        else:
            self._mode = 'deny'
            self._functions = frozenset(denied_functions)

    @property
    def mode(self):
        # Either 'allow' or 'deny'.
        return self._mode

    @property
    def functions(self):
        return self._functions

    def is_allowed(self, function_name):
        """Return True if a call to ``function_name`` is permitted."""
        if self._mode == 'allow':
            return function_name in self._functions
        return function_name not in self._functions

    def check(self, function_name):
        """Raise if a call to ``function_name`` is not permitted."""
        if not self.is_allowed(function_name):
            raise FunctionNotAllowedError(function_name)
