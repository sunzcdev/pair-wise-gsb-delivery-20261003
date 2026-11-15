#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

"""Base class for the filters used by a :class:`~sqlparse.engine.FilterStack`.

Filters are stored once on a stack and may be driven through several
``run()`` calls -- those calls can even interleave, because each ``run()``
returns a generator that is consumed lazily.  A filter that keeps state while
it processes a stream therefore needs a fresh state for every run instead of
mutating the single instance registered on the stack.  Overriding
:meth:`Filter.begin` is the opt-in hook that provides that boundary.
"""


class Filter:
    """Base class for filters participating in run-level sessions.

    A *session* is the processing of one ``FilterStack.run()`` call.  When a
    run starts, the stack asks every registered filter for a session object
    via :meth:`begin`.  The returned object -- usually a newly constructed
    copy with reset state -- is used only for that run and is discarded when
    the generator is exhausted or closed.  Two interleaved runs therefore
    never share mutable state, and a run that fails or is abandoned partway
    through cannot leave indentation, a half-consumed generator or any other
    in-progress state behind for the next run.

    The default implementation returns ``self``, which keeps simple,
    stateless filters working unchanged.  Stateful filters must override
    :meth:`begin` to return a fresh instance.
    """

    def begin(self):
        """Return the filter instance to use for one run.

        Override this in a stateful filter and return a new object that
        starts from the filter's initial state.  The configuration stored on
        the registered (prototype) filter is typically copied to the new
        instance; no deep copy of caller-supplied data is performed.

        :returns: a filter with reset, run-private state.
        """
        return self
