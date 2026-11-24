# sql/_compiler_registry.py
# Copyright (C) 2005-2026 the SQLAlchemy authors and contributors
# <see AUTHORS file>
#
# This module is part of SQLAlchemy and is released under
# the MIT License: https://www.opensource.org/licenses/mit-license.php

"""Internal registry state for user-defined compilation handlers.

The :mod:`sqlalchemy.ext.compiler` extension allows the compiler for a custom
SQL element (or even a built-in one) to be registered and replaced at runtime.
A compiled statement cache however keys on the *structural* cache key of the
statement, which does not change merely because a new
:func:`~sqlalchemy.ext.compiler.compiles` handler was registered.  This module
is an import-cycle-free leaf holding the state needed to make hot-swapped
compilers visible to the compiled statement cache, so that:

* a statement already handed off to the DBAPI keeps running with the SQL it was
  compiled with, while executions started after a new handler is registered use
  the new handler;
* a statement embedding an element whose handler was re-registered is
  recompiled rather than served a stale entry from a shared
  ``compiled_cache``;
* statements that do not involve any custom element, or that involve other,
  untouched custom elements, continue to hit the same cache.

"""

from __future__ import annotations

from typing import Any
from typing import Callable
from typing import Dict
from typing import List
from typing import Tuple


# incremented each time a custom compilation handler is registered through
# :func:`sqlalchemy.ext.compiler.compiles` or removed through
# :func:`sqlalchemy.ext.compiler.deregister`.
_registry_generation = 0


def next_registry_generation() -> int:
    global _registry_generation
    _registry_generation += 1
    return _registry_generation


class CustomCompilerDispatcher:
    """Holds the user-defined compilation functions for one element class.

    The class lives in this import-cycle-free module so that both
    :mod:`sqlalchemy.ext.compiler` (which creates and mutates these
    dispatchers) and :mod:`sqlalchemy.sql.elements` (which needs to read them
    while building compiled statement cache keys) can depend on it without
    importing each other.

    """

    __slots__ = ("specs", "registry_generation")

    def __init__(self) -> None:
        self.specs: Dict[str, Callable[..., Any]] = {}
        # the value of the global registry generation at the time this
        # dispatcher was (re)registered.  Participates in the compiled
        # statement cache key, see
        # :meth:`.ClauseElement._compile_w_cache`.
        self.registry_generation: int = 0

    def __call__(self, element: Any, compiler: Any, **kw: Any) -> Any:
        # local imports to avoid a module-level dependency cycle; this method
        # is only invoked at actual compilation time, long after package
        # initialization has completed.
        from .. import exc
        from . import sqltypes

        # TODO: yes, this could also switch off of DBAPI in use.
        fn = self.specs.get(compiler.dialect.name, None)
        if not fn:
            try:
                fn = self.specs["default"]
            except KeyError as ke:
                raise exc.UnsupportedCompilationError(
                    compiler,
                    type(element),
                    message="%s construct has no default "
                    "compilation handler." % type(element),
                ) from ke

        # if compilation includes add_to_result_map, collect add_to_result_map
        # arguments from the user-defined callable, which are probably none
        # because this is not public API.  if it wasn't called, then call it
        # ourselves.
        arm = kw.get("add_to_result_map", None)
        if arm:
            arm_collection = []
            kw["add_to_result_map"] = (
                lambda *args: arm_collection.append(args)
            )

        expr = fn(element, compiler, **kw)

        if arm:
            if not arm_collection:
                arm_collection.append(
                    (None, None, (element,), sqltypes.NULLTYPE)
                )
            for tup in arm_collection:
                arm(*tup)
        return expr


def custom_compiler_dispatchers_from_key(
    cache_key: Tuple[Any, ...],
) -> Tuple[CustomCompilerDispatcher, ...]:
    """Collect every custom compiler dispatcher reachable from a structural
    cache key.

    Custom SQL elements compile through a :class:`.CustomCompilerDispatcher`
    that can be hot-swapped at any time.  Their structural cache key does not
    change when that happens, so the dispatchers involved in a statement are
    recorded separately and, together with their current registration
    generation, added to the compiled statement cache key: a re-registered
    (or deregistered) compiler forces the statements that embed such an
    element to be recompiled while unrelated statements keep hitting the same
    cache.

    The result is normally cached per statement through the
    :attr:`.ClauseElement._custom_compiler_dispatchers` memoized attribute;
    this function is the underlying implementation, kept in this module so
    that it has no dependency on the rest of the :mod:`sqlalchemy.sql`
    package.

    """

    dispatchers: set[CustomCompilerDispatcher] = set()
    stack: List[Any] = list(cache_key)

    while stack:
        token = stack.pop()

        if isinstance(token, tuple):
            stack.extend(token)
        elif isinstance(token, type):
            # attribute resolution walks the MRO: a subclass without its own
            # handler inherits the dispatcher of a custom-compiled superclass,
            # so re-registering a handler on the superclass must invalidate
            # statements built from the subclass as well.
            dispatcher = getattr(token, "_compiler_dispatcher", None)
            if isinstance(dispatcher, CustomCompilerDispatcher):
                dispatchers.add(dispatcher)

    return tuple(sorted(dispatchers, key=id))
