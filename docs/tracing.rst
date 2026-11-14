Provenance Tracing
==================

When glom produces a new document, it is often useful to know exactly
where every value in the output came from. The :func:`glom.trace`
entrypoint evaluates a spec exactly like :func:`glom.glom`, but returns
a :class:`~glom.TraceResult` carrying both the normal result and a
*source map*.

Tracing is fully opt-in. Ordinary :func:`glom.glom` calls are unchanged
and take on no tracing overhead; the tracing evaluator only runs when
:func:`glom.trace` (or :meth:`glom.Glommer.trace`) is called directly.

A first example
---------------

>>> from glom import trace, Val, Coalesce
>>> target = {'product': {'name': 'widget', 'price': 9.5}}
>>> result = trace(target, {
...     'name': 'product.name',
...     'price': 'product.price',
...     'tax_inclusive': Val(True),
...     'discount': Coalesce('product.discount', default=Val(0)),
... })
>>> result.value
{'name': 'widget', 'price': 9.5, 'tax_inclusive': True, 'discount': 0}
>>> result[('name',)]
PathSource(('name',), 'widget', input_path=Path('product', 'name'))

The source map keys are tuples describing output locations: ``()`` for
the root, ``('name',)`` for a dictionary field, and
``('items', 0, 'id')`` for an element nested inside a list.

Source types
------------

Every entry in the source map is one of three types:

* :class:`glom.PathSource` -- the value was read from a concrete path
  on the input. The exact input path is available on ``input_path``.
* :class:`glom.LiteralSource` -- the value came from a literal embedded
  in the spec, such as :class:`glom.Val`, not from the input.
* :class:`glom.SyntheticSource` -- the value was produced by a
  computation. Its ``inputs`` attribute lists the sources the
  computation is known to have depended on, and ``origin`` describes
  the kind of step (a callable, a ``T`` expression, ...).

>>> result[('tax_inclusive',)]
LiteralSource(('tax_inclusive',), True)
>>> synth = trace(target, lambda t: t['product']['price'] * 1.2)[()]
>>> synth  # doctest: +ELLIPSIS
SyntheticSource((), ..., origin='callable ...', inputs=[PathSource((), ..., input_path=Path())])

Callables are treated as opaque: glom records the target the callable
received, but never guesses which fields the function accessed
internally. Values without a real source are never attributed to the
current target just to fill the map.

Nested lists and branching
--------------------------

Each element of a nested list is traced separately, so the source map
distinguishes the origin of every output element:

>>> rows = {'rows': [{'id': 1}, {'id': 2}]}
>>> result = trace(rows, {'ids': ('rows', [{'id': 'id'}])})
>>> result[('ids', 0, 'id')].input_path
Path('rows', 0, 'id')
>>> result[('ids', 1, 'id')].input_path
Path('rows', 1, 'id')

:class:`glom.Coalesce` only records the branch that was actually
adopted; the branches that were tried and skipped leave nothing in the
source map. Likewise, :data:`glom.SKIP` values never produce ghost
output paths, and :data:`glom.STOP` truncates a list cleanly:

>>> result = trace({'a': 1, 'b': 2},
...                {'keep': 'a', 'gone': lambda t: glom.SKIP})
>>> sorted(result.sources)
[(), ('keep',)]

Errors and partial results
--------------------------

If evaluation fails, :func:`glom.trace` does not raise. The returned
:class:`~glom.TraceResult` reports ``complete=False``, keeps the
original error on ``error``, retains every source that had been
confirmed before the failure, and exposes the best-effort partial
output value (or the :data:`glom.INCOMPLETE` sentinel when nothing
could be assembled). Use :meth:`~glom.TraceResult.raise_on_error` to
re-raise when strict behavior is needed:

>>> result = trace({'a': 1}, {'a': 'a', 'bad': 'missing'})
>>> result.complete
False
>>> result.value
{'a': 1}
>>> sorted(result.sources)
[(), ('a',)]
>>> type(result.error).__name__
'PathAccessError'

API reference
-------------

.. autofunction:: glom.trace

.. autoclass:: glom.TraceResult
   :members:

.. autoclass:: glom.Source
.. autoclass:: glom.PathSource
.. autoclass:: glom.LiteralSource
.. autoclass:: glom.SyntheticSource

.. autodata:: glom.INCOMPLETE
