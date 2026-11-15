"""By default, glom aims to safely return a transformed copy of your
data. But sometimes you really need to transform an existing object.

When you already have a large or complex bit of nested data that you
are sure you want to modify in-place, glom has you covered, with the
:func:`~glom.assign` function, and the :func:`~glom.Assign` specifier
type.

.. warning::

   glom's deep assignment is powerful, and incorrect use can result in 
   unintended assignments to global state, including class and module 
   attributes, as well as function defaults. 
   
   Be careful when writing assignment specs, and especially careful when 
   any part of the spec is data-driven or provided by an end user.

"""
import operator
from pprint import pprint

from .core import Path, T, S, Spec, glom, UnregisteredTarget, GlomError, PathAccessError, UP
from .core import TType, register_op, TargetRegistry, bbrepr, PathAssignError, arg_val, _assign_op


try:
    basestring
except NameError:
    basestring = str


if getattr(__builtins__, '__dict__', None) is not None:
    # pypy's __builtins__ is a module, as is CPython's REPL, but at
    # normal execution time it's a dict?
    __builtins__ = __builtins__.__dict__


class PathDeleteError(PathAssignError):
    """This :exc:`GlomError` subtype is raised when an assignment fails,
    stemming from an :func:`~glom.delete` call or other
    :class:`~glom.Delete` usage.

    One example would be deleting an out-of-range position in a list::

      >>> delete(["short", "list"], Path(5))
      Traceback (most recent call last):
      ...
      PathDeleteError: could not delete 5 on object at Path(), got error: IndexError(...

    Other assignment failures could be due to deleting a read-only
    ``@property`` or exception being raised inside a ``__delattr__()``.

    """
    def get_message(self):
        return ('could not delete %r on object at %r, got error: %r'
                % (self.dest_name, self.path, self.exc))


def _apply_for_each(func, path, val):
    layers = path.path_t.__stars__()
    if layers:
        for i in range(layers - 1):
            val = sum(val, [])  # flatten out the extra layers
        for inner in val:
            func(inner)
    else:
        func(val)


def _path_has_star(path):
    return bool(path.path_t.__stars__())


def _get_path_node(cur, op, arg, path, part_idx, scope):
    """Read a single segment of an assignment path, turning any access
    failure into a PathAccessError that carries the original *path*."""
    if op == '.':
        try:
            return getattr(cur, arg)
        except AttributeError as e:
            raise PathAccessError(e, path, part_idx)
    elif op == '[':
        try:
            return cur[arg]
        except (KeyError, IndexError, TypeError) as e:
            raise PathAccessError(e, path, part_idx)
    elif op == 'P':
        get = scope[TargetRegistry].get_handler(
            'get', cur, path=path.path_t.__ops__[2:2 * (part_idx + 1):2])
        try:
            return get(cur, arg)
        except Exception as e:
            raise PathAccessError(e, path, part_idx)
    raise TypeError('unsupported path operation %r for assignment'
                    % (op,))  # pragma: no cover


def _segment_present(cur, op, arg, scope):
    """Return True when *arg* is an existing attribute/key/index on
    *cur*, even if reading or assigning it would fail. Used to tell a
    genuinely missing node (which the missing factory may create) from
    an extant but unassignable one (which must raise instead)."""
    if op == '.':
        return hasattr(cur, arg)
    if op == '[':
        try:
            cur[arg]
        except KeyError:
            return False
        except IndexError:
            return False
        except TypeError:
            return False
        except Exception:
            return True
        return True
    # 'P' and any registered target: the membership/contains check
    try:
        contains = scope[TargetRegistry].get_handler('contains', cur, raise_exc=False)
    except Exception:
        contains = False
    if callable(contains):
        try:
            return contains(cur, arg)
        except Exception:
            pass
    try:
        return arg in cur
    except Exception:
        try:
            _get_path_node(cur, op, arg, Path(), 0, scope)
        except PathAccessError:
            return False
        return True


def _assert_assignable(cur, op, arg, path, scope):
    """Raise :class:`~glom.PathAssignError` if *cur* cannot host a write
    at *arg*, without modifying *cur*.

    The check is structural and side-effect free. A ``'P'`` (fuzzy
    registry) segment is resolved to the registered assign handler, so
    the same logic applies to plain dict/list/object targets: a
    read-only property and targets without an instance dict or matching
    slot cannot take attributes, and anything without ``__setitem__``
    cannot take items.
    """
    if op == 'P':
        try:
            handler = scope[TargetRegistry].get_handler('assign', cur, raise_exc=False)
        except Exception:
            handler = None
        if not callable(handler):
            # nothing registered: let the real write produce the error
            return
        if handler is setattr or getattr(handler, '__name__', '') == 'setattr':
            op = '.'
        else:
            op = '['

    if op == '.':
        cls = type(cur)
        for klass in cls.__mro__:
            descriptor = klass.__dict__.get(arg)
            if descriptor is not None:
                if hasattr(descriptor, '__set__'):
                    return  # data descriptor: assignment goes through it
                # a getter without __set__ (e.g. a read-only property)
                raise PathAssignError(
                    AttributeError("can't set read-only attribute %r" % (arg,)),
                    path, arg)
        # no descriptor on the class: instance assignment needs somewhere
        # to live -- an instance dict or a matching __slots__ entry
        if hasattr(cur, '__dict__'):
            return
        slots = set()
        for klass in cls.__mro__:
            slots.update(getattr(klass, '__slots__', ()) or ())
        if arg in slots:
            return
        raise PathAssignError(
            AttributeError("'%s' object has no settable attribute %r"
                           % (cls.__name__, arg,)), path, arg)

    if op == '[':
        if not callable(getattr(cur, '__setitem__', None)):
            raise PathAssignError(
                TypeError("'%s' object does not support item assignment"
                          % (type(cur).__name__,)), path, arg)
        return


class MissingFactory:
    """Wraps a *context factory* for use as :class:`Assign`'s *missing*
    argument.

    Whereas a plain zero-argument *missing* callable (like ``dict``) has
    no information about where in the structure it is being invoked, a
    context factory is called with three positional arguments every time
    a genuinely missing node is encountered along an assignment path:

    Args:
       remaining_path (Path): The portion of the original assignment
          path that still has to be built, starting with the missing
          segment.
       parent (object): The node on which the missing segment would be
          set. This is the scope object for ``S``-based assignment
          paths.
       path_segment (object): The key, index, or attribute name that
          would be written next. Combined with the type of *parent*,
          this lets the factory decide whether to return a list, dict,
          or any other container.

    For example, the factory can inspect the upcoming segment (and the
    parent it will be written onto) to pick a suitable container::

      >>> def make_container(remaining_path, parent, path_segment):
      ...     return {'segment': path_segment, 'parent': type(parent).__name__}
      >>> target = {}
      >>> assign(target, Path('a', 'b'), 'value',
      ...        missing=MissingFactory(make_container))
      {'a': {'segment': 'a', 'parent': 'dict', 'b': 'value'}}

    A plain, zero-argument *missing* callable remains fully supported
    and needs no wrapping. The factory is only invoked for nodes that
    are actually missing (a failed lookup); an existing ``None``, empty
    list, or empty dict is treated as a real node and never replaced.
    """
    def __init__(self, factory):
        if not callable(factory):
            raise TypeError(
                'expected MissingFactory factory to be callable, not %r'
                % (factory,))
        self.factory = factory

    def __call__(self, remaining_path, parent, path_segment):
        return self.factory(remaining_path, parent, path_segment)

    def __repr__(self):
        return f'{self.__class__.__name__}({bbrepr(self.factory)})'


class Assign:
    """*New in glom 18.3.0*

    The ``Assign`` specifier type enables glom to modify the target,
    performing a "deep-set" to mirror glom's original deep-get use
    case.

    ``Assign`` can be used to perform spot modifications of large data
    structures when making a copy is not desired::

      # deep assignment into a nested dictionary
      >>> target = {'a': {}}
      >>> spec = Assign('a.b', 'value')
      >>> _ = glom(target, spec)
      >>> pprint(target)
      {'a': {'b': 'value'}}

    The value to be assigned can also be a :class:`~glom.Spec`, which
    is useful for copying values around within the data structure::

      # copying one nested value to another
      >>> _ = glom(target, Assign('a.c', Spec('a.b')))
      >>> pprint(target)
      {'a': {'b': 'value', 'c': 'value'}}

    Another handy use of Assign is to deep-apply a function::

      # sort a deep nested list
      >>> target={'a':{'b':[3,1,2]}}
      >>> _ = glom(target, Assign('a.b', Spec(('a.b',sorted))))
      >>> pprint(target)
      {'a': {'b': [1, 2, 3]}}

    Like many other specifier types, ``Assign``'s destination path can be
    a :data:`~glom.T` expression, for maximum control::

      # changing the error message of an exception in an error list
      >>> err = ValueError('initial message')
      >>> target = {'errors': [err]}
      >>> _ = glom(target, Assign(T['errors'][0].args, ('new message',)))
      >>> str(err)
      'new message'

    ``Assign`` has built-in support for assigning to attributes of
    objects, keys of mappings (like dicts), and indexes of sequences
    (like lists). Additional types can be registered through
    :func:`~glom.register()` using the ``"assign"`` operation name.

    Attempting to assign to an immutable structure, like a
    :class:`tuple`, will result in a
    :class:`~glom.PathAssignError`. Attempting to assign to a path
    that doesn't exist will raise a :class:`~PathAccessError`.

    To automatically backfill missing structures, you can pass a
    callable to the *missing* argument. This callable will be called
    for each path segment along the assignment which is not
    present.

       >>> target = {}
       >>> assign(target, 'a.b.c', 'hi', missing=dict)
       {'a': {'b': {'c': 'hi'}}}

    A zero-argument callable like ``dict`` above receives no context.
    When the factory needs to know whether the next node should be a
    list or a dict, wrap a three-argument context factory in
    :class:`MissingFactory`. It receives the remaining path, the
    parent node, and the path segment about to be written::

       >>> target = {}
       >>> def _container(remaining_path, parent, segment):
       ...     return {'on': type(parent).__name__, 'segment': segment}
       >>> assign(target, Path('a', 'b'), 'hi',
       ...        missing=MissingFactory(_container))
       {'a': {'on': 'dict', 'segment': 'a', 'b': 'hi'}}

    Only genuinely missing nodes trigger the factory: existing
    ``None``, ``[]``, and ``{}`` values are kept as-is. If a factory
    returns an object that cannot hold the next step, the original
    assignment path is reported in the error and no half-built chain
    is left attached to the target.

    """
    def __init__(self, path, val, missing=None):
        # TODO: an option like require_preexisting or something to
        # ensure that a value is mutated, not just added. Current
        # workaround is to do a Check().
        if isinstance(path, basestring):
            path = Path.from_text(path)
        elif type(path) is TType:
            path = Path(path)
        elif not isinstance(path, Path):
            raise TypeError('path argument must be a .-delimited string, Path, T, or S')

        try:
            self.op, self.arg = path.items()[-1]
        except IndexError:
            raise ValueError('path must have at least one element')
        self._orig_path = path
        self.path = path[:-1]

        if self.op not in '[.P':
            # maybe if we add null-coalescing this should do something?
            raise ValueError('last part of path must be setattr or setitem')
        self.val = val

        if missing is not None and not callable(missing):
            raise TypeError(f'expected missing to be callable, not {missing!r}')
        self.missing = missing
        self._context_missing = isinstance(missing, MissingFactory)

    def _invoke_missing(self, remaining_path, parent, segment):
        if self._context_missing:
            return self.missing(remaining_path, parent, segment)
        return self.missing()

    def _backfill(self, dest_target, pae, val, scope):
        """Build the genuinely missing portion of an assignment path and
        return ``(anchor, op, arg, chain_head)`` for a single attachment.

        The chain is assembled fully detached from the target document:
        the missing factory is invoked once per genuinely missing
        segment and results are linked to one another only in local
        variables, with the terminal value written last. An extant node
        -- including ``None``, ``[]`` and ``{}``, and even an unreadable
        or unassignable one -- is followed rather than replaced, and its
        inability to host the next segment is raised structurally as a
        :class:`~glom.PathAssignError` with the original path. No write
        reaches the document until the entire chain, including the final
        value, has validated, so a bad factory result never leaves a
        half-built chain behind.
        """
        orig_path = self._orig_path
        items = orig_path.items()
        last_idx = len(items) - 1
        start_idx = pae.part_idx

        # the deepest node that genuinely exists on the target document
        anchor = dest_target
        for idx in range(start_idx):
            op, raw_arg = items[idx]
            arg = arg_val(dest_target, raw_arg, scope)
            anchor = _get_path_node(anchor, op, arg, orig_path, idx, scope)

        anchor_op, anchor_raw_arg = items[start_idx]
        anchor_arg = arg_val(dest_target, anchor_raw_arg, scope)
        # the anchor itself has to accept the head of the chain
        _assert_assignable(anchor, anchor_op, anchor_arg,
                           orig_path[:start_idx + 1], scope)

        head = None
        cur = None
        # extant nodes are validated only after all missing factories have
        # run, preserving eager factory invocation while keeping every
        # write detached from the target document
        deferred_checks = []
        for idx in range(start_idx, last_idx):
            op, raw_arg = items[idx]
            arg = arg_val(dest_target, raw_arg, scope)
            parent = anchor if cur is None else cur

            try:
                nxt = _get_path_node(parent, op, arg, orig_path, idx, scope)
                node_was_read = True
            except PathAccessError:
                node_was_read = False
                if _segment_present(parent, op, arg, scope):
                    # present but unreadable: do not backfill over an
                    # existing node; re-raise so the assignment attempt
                    # reports the real failure with the original path
                    raise
                remaining_path = orig_path[idx:]
                nxt = self._invoke_missing(remaining_path, parent, arg)

            next_op, next_raw = items[idx + 1]
            next_arg = arg_val(dest_target, next_raw, scope)
            next_path = orig_path if idx + 1 == last_idx else orig_path[:idx + 2]
            if node_was_read:
                deferred_checks.append((nxt, next_op, next_arg, next_path))
            else:
                # a freshly built container is validated immediately, so a
                # bad factory result raises while still fully detached
                _assert_assignable(nxt, next_op, next_arg, next_path, scope)
                if cur is not None:
                    _assign_op(dest=cur, op=op, arg=arg, val=nxt,
                               path=orig_path[:idx + 1], scope=scope)
            if head is None:
                head = nxt
            cur = nxt

        # surface extant nodes that cannot host their child (e.g. a
        # read-only property whose value is None) after factory calls
        for check_node, check_op, check_arg, check_path in deferred_checks:
            _assert_assignable(check_node, check_op, check_arg, check_path, scope)

        # write the terminal value on the detached final node
        final_op, final_raw_arg = items[last_idx]
        final_arg = arg_val(dest_target, final_raw_arg, scope)
        _assign_op(dest=cur, op=final_op, arg=final_arg, val=val,
                   path=orig_path, scope=scope)

        return anchor, anchor_op, anchor_arg, head

    def _backfill_star(self, dest_target, pae, val, scope):
        """Backfill for paths containing a star (``*``/``**``) segment.

        Star traversal resolves the already-reachable prefix and leaves
        the broadcast itself to the normal assignment flow, matching the
        original Assign behavior: a missing prefix is built once and any
        unmatched star simply produces nothing.
        """
        remaining_path = self._orig_path[pae.part_idx + 1:]
        built = scope[glom](
            self._invoke_missing(self._orig_path[pae.part_idx:], dest_target,
                                 self._orig_path.items()[pae.part_idx][1])
            if self._context_missing else self.missing(),
            Assign(remaining_path, val, missing=self.missing), scope)
        op, arg = self._orig_path.items()[pae.part_idx]
        path = self._orig_path[:pae.part_idx]
        anchor = scope[glom](dest_target, path, scope)
        _assign_op(dest=anchor, op=op, arg=arg, val=built,
                   path=self._orig_path[:pae.part_idx + 1], scope=scope)

    def glomit(self, target, scope):
        val = arg_val(target, self.val, scope)

        op, arg, path = self.op, self.arg, self.path
        if self.path.startswith(S):
            dest_target = scope[UP]
            dest_path = self.path.from_t()
        else:
            dest_target = target
            dest_path = self.path

        try:
            dest = scope[glom](dest_target, dest_path, scope)
        except PathAccessError as pae:
            if not self.missing:
                raise
            # a failure on the final segment is the assignment itself;
            # the factory only backfills missing intermediate containers
            if pae.part_idx >= len(self._orig_path.items()) - 1:
                raise
            if _path_has_star(self._orig_path):
                self._backfill_star(dest_target, pae, val, scope)
                return target
            anchor, anchor_op, anchor_arg, chain_head = self._backfill(
                dest_target, pae, val, scope)
            # the single write onto the target document; by this point
            # every container and the final value are validated
            attach_path = self._orig_path[:pae.part_idx + 1]
            _assign_op(dest=anchor, op=anchor_op, arg=anchor_arg,
                       val=chain_head, path=attach_path, scope=scope)
            return target

        # TODO: forward-detect immutable dest?
        _apply = lambda dest: _assign_op(
            dest=dest, op=op, arg=arg, val=val, path=path, scope=scope)
        _apply_for_each(_apply, path, dest)

        return target

    def __repr__(self):
        cn = self.__class__.__name__
        if self.missing is None:
            return f'{cn}({self._orig_path!r}, {self.val!r})'
        return f'{cn}({self._orig_path!r}, {self.val!r}, missing={bbrepr(self.missing)})'


def assign(obj, path, val, missing=None):
    """*New in glom 18.3.0*

    The ``assign()`` function provides convenient "deep set"
    functionality, modifying nested data structures in-place::

      >>> target = {'a': [{'b': 'c'}, {'d': None}]}
      >>> _ = assign(target, 'a.1.d', 'e')  # let's give 'd' a value of 'e'
      >>> pprint(target)
      {'a': [{'b': 'c'}, {'d': 'e'}]}

    Missing structures can also be automatically created with the
    *missing* parameter. For more information and examples, see the
    :class:`~glom.Assign` specifier type, which this function wraps.
    """
    return glom(obj, Assign(path, val, missing=missing))


_ALL_BUILTIN_TYPES = [v for v in __builtins__.values() if isinstance(v, type)]
_BUILTIN_BASE_TYPES = [v for v in _ALL_BUILTIN_TYPES
                       if not issubclass(v, tuple([t for t in _ALL_BUILTIN_TYPES
                                                   if t not in (v, type, object)]))]
_UNASSIGNABLE_BASE_TYPES = tuple(set(_BUILTIN_BASE_TYPES)
                                 - {dict, list, BaseException, object, type})


def _set_sequence_item(target, idx, val):
    target[int(idx)] = val


def _assign_autodiscover(type_obj):
    # TODO: issubclass or "in"?
    if issubclass(type_obj, _UNASSIGNABLE_BASE_TYPES):
        return False

    if callable(getattr(type_obj, '__setitem__', None)):
        if callable(getattr(type_obj, 'index', None)):
            return _set_sequence_item
        return operator.setitem

    return setattr


register_op('assign', auto_func=_assign_autodiscover, exact=False)


class Delete:
    """
    In addition to glom's core "deep-get" and ``Assign``'s "deep-set",
    the ``Delete`` specifier type performs a "deep-del", which can
    remove items from larger data structures by key, attribute, and
    index.

    >>> target = {'dict': {'x': [5, 6, 7]}}
    >>> glom(target, Delete('dict.x.1'))
    {'dict': {'x': [5, 7]}}
    >>> glom(target, Delete('dict.x'))
    {'dict': {}}

    If a target path is missing, a :exc:`PathDeleteError` will be
    raised. To ignore missing targets, use the ``ignore_missing``
    flag:

    >>> glom(target, Delete('does_not_exist', ignore_missing=True))
    {'dict': {}}

    ``Delete`` has built-in support for deleting attributes of
    objects, keys of dicts, and indexes of sequences
    (like lists). Additional types can be registered through
    :func:`~glom.register()` using the ``"delete"`` operation name.

    .. versionadded:: 20.5.0
    """
    def __init__(self, path, ignore_missing=False):
        if isinstance(path, basestring):
            path = Path.from_text(path)
        elif type(path) is TType:
            path = Path(path)
        elif not isinstance(path, Path):
            raise TypeError('path argument must be a .-delimited string, Path, T, or S')

        try:
            self.op, self.arg = path.items()[-1]
        except IndexError:
            raise ValueError('path must have at least one element')
        self._orig_path = path
        self.path = path[:-1]

        if self.op not in '[.P':
            raise ValueError('last part of path must be an attribute or index')

        self.ignore_missing = ignore_missing

    def _del_one(self, dest, op, arg, scope):
        if op == '[':
            try:
                del dest[arg]
            except IndexError as e:
                if not self.ignore_missing:
                    raise PathDeleteError(e, self.path, arg)
        elif op == '.':
            try:
                delattr(dest, arg)
            except AttributeError as e:
                if not self.ignore_missing:
                    raise PathDeleteError(e, self.path, arg)
        elif op == 'P':
            _delete = scope[TargetRegistry].get_handler('delete', dest)
            try:
                _delete(dest, arg)
            except Exception as e:
                if not self.ignore_missing:
                    raise PathDeleteError(e, self.path, arg)

    def glomit(self, target, scope):
        op, arg, path = self.op, self.arg, self.path
        if self.path.startswith(S):
            dest_target = scope[UP]
            dest_path = self.path.from_t()
        else:
            dest_target = target
            dest_path = self.path
        try:
            dest = scope[glom](dest_target, dest_path, scope)
        except PathAccessError as pae:
            if not self.ignore_missing:
                raise
        else:
            _apply_for_each(lambda dest: self._del_one(dest, op, arg, scope), path, dest)

        return target

    def __repr__(self):
        cn = self.__class__.__name__
        return f'{cn}({self._orig_path!r})'


def delete(obj, path, ignore_missing=False):
    """
    The ``delete()`` function provides "deep del" functionality,
    modifying nested data structures in-place::

      >>> target = {'a': [{'b': 'c'}, {'d': None}]}
      >>> delete(target, 'a.0.b')
      {'a': [{}, {'d': None}]}

    Attempting to delete missing keys, attributes, and indexes will
    raise a :exc:`PathDeleteError`. To ignore these errors, use the
    *ignore_missing* argument::

      >>> delete(target, 'does_not_exist', ignore_missing=True)
      {'a': [{}, {'d': None}]}

    For more information and examples, see the :class:`~glom.Delete`
    specifier type, which this convenience function wraps.

    .. versionadded:: 20.5.0
    """
    return glom(obj, Delete(path, ignore_missing=ignore_missing))


def _del_sequence_item(target, idx):
    del target[int(idx)]


def _delete_autodiscover(type_obj):
    if issubclass(type_obj, _UNASSIGNABLE_BASE_TYPES):
        return False

    if callable(getattr(type_obj, '__delitem__', None)):
        if callable(getattr(type_obj, 'index', None)):
            return _del_sequence_item
        return operator.delitem
    return delattr


register_op('delete', auto_func=_delete_autodiscover, exact=False)
