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

from .core import Path, T, S, Spec, Val, glom, UnregisteredTarget, GlomError, PathAccessError, UP
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

        if missing is not None:
            if not callable(missing):
                raise TypeError(f'expected missing to be callable, not {missing!r}')
        self.missing = missing

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

            remaining_path = self._orig_path[pae.part_idx + 1:]
            val = scope[glom](self.missing(), Assign(remaining_path, val, missing=self.missing), scope)

            op, arg = self._orig_path.items()[pae.part_idx]
            path = self._orig_path[:pae.part_idx]
            dest = scope[glom](dest_target, path, scope)

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


class AtomicMutationError(GlomError, ValueError):
    """Raised when an :class:`~glom.Atomic` batch contains an operation
    which cannot be rolled back by the atomic mutation implementation.

    Atomic mutation deliberately supports only path assignment and
    deletion on built-in :class:`dict` and :class:`list` objects. In
    particular, attribute assignment on custom objects, dynamic path
    segments, and ``Assign(..., missing=...)`` are rejected before any
    part of the batch is applied.
    """
    def __init__(self, message):
        self.message = message

    def get_message(self):
        return self.message

    def __repr__(self):
        return f'{self.__class__.__name__}({self.message!r})'


class UnsupportedAtomicMutation(AtomicMutationError, TypeError):
    """A subtype of :exc:`AtomicMutationError` specifically for
    operation or target shapes outside :class:`~glom.Atomic`'s
    supported subset.
    """


_ATOMIC_DYNAMIC = object()


def _is_atomic_container(obj):
    return type(obj) is dict or type(obj) is list


def _is_atomic_dynamic_spec(obj):
    if isinstance(obj, Val):
        return False
    if isinstance(obj, TType):
        return True
    return callable(getattr(obj, 'glomit', None))


def _static_atomic_value(obj, memo=None):
    """Return a safe approximation of an Assign value for preflight.

    Values built solely from literals (and ``Val``) are reconstructed
    like glom's argument evaluator would do. Values which require
    evaluating a path or arbitrary glom spec are represented by an
    opaque marker. If a later path needs to walk through such a value,
    the batch is rejected before any mutation happens.
    """
    if isinstance(obj, Val):
        return obj.value
    if _is_atomic_dynamic_spec(obj):
        return _ATOMIC_DYNAMIC

    obj_type = type(obj)
    if obj_type in (list, dict):
        if memo is None:
            memo = {}
        obj_id = id(obj)
        if obj_id in memo:
            return memo[obj_id]
        result = obj_type()
        memo[obj_id] = result
        if obj_type is list:
            for val in obj:
                val = _static_atomic_value(val, memo)
                if val is _ATOMIC_DYNAMIC:
                    return _ATOMIC_DYNAMIC
                result.append(val)
        else:
            for key, val in obj.items():
                key = _static_atomic_value(key, memo)
                val = _static_atomic_value(val, memo)
                if key is _ATOMIC_DYNAMIC or val is _ATOMIC_DYNAMIC:
                    return _ATOMIC_DYNAMIC
                result[key] = val
        return result

    if obj_type in (tuple, set, frozenset):
        if memo is None:
            memo = {}
        vals = []
        for val in obj:
            val = _static_atomic_value(val, memo)
            if val is _ATOMIC_DYNAMIC:
                return _ATOMIC_DYNAMIC
            vals.append(val)
        return obj_type(vals)

    return obj


def _atomic_path_arg(arg, op, path, idx):
    if isinstance(arg, TType) or callable(getattr(arg, 'glomit', None)) or callable(arg):
        raise UnsupportedAtomicMutation(
            'atomic mutation does not support dynamic path segment %r '
            'in operation %d (%r)' % (arg, idx, path))
    return arg


def _atomic_list_index(arg, op, path, idx):
    _atomic_path_arg(arg, op, path, idx)
    if isinstance(arg, bool):
        return int(arg)
    if isinstance(arg, int):
        return arg
    if isinstance(arg, str):
        try:
            return int(arg)
        except ValueError:
            pass
    raise UnsupportedAtomicMutation(
        'atomic mutation requires integer list indexes, got %r '
        'in operation %d (%r)' % (arg, idx, path))


def _atomic_dict_key(arg, op, path, idx):
    _atomic_path_arg(arg, op, path, idx)
    try:
        hash(arg)
    except TypeError:
        raise UnsupportedAtomicMutation(
            'atomic mutation requires hashable dict keys, got %r '
            'in operation %d (%r)' % (arg, idx, path))
    return arg


class _AtomicShadow:
    """A shallow copy-on-write view used to validate a whole batch
    before touching the real target.

    Copy-on-write keeps aliases aliases: if two paths reach the same
    container, they reach the same shadow copy as well.
    """
    def __init__(self, root):
        if not _is_atomic_container(root):
            raise UnsupportedAtomicMutation(
                'atomic mutation only supports built-in dict/list targets, got %s'
                % type(root).__name__)
        self._views = {}
        # Keep originals alive so id() cannot be reused while the shadow
        # is being built.
        self._originals = {}
        self.root = self.view(root)

    def _clone(self, obj):
        if type(obj) is dict:
            return dict(obj)
        return list(obj)

    def view(self, obj):
        if not _is_atomic_container(obj):
            return obj
        obj_id = id(obj)
        try:
            return self._views[obj_id]
        except KeyError:
            clone = self._clone(obj)
            self._originals[obj_id] = obj
            self._views[obj_id] = clone
            return clone

    def _child(self, parent, path_op, arg, op, path, idx):
        if type(parent) is dict:
            if path_op not in ('P', '['):
                raise UnsupportedAtomicMutation(
                    'atomic mutation only supports dict key access, got %r '
                    'in operation %d (%r)' % (path_op, idx, path))
            key = _atomic_dict_key(arg, op, path, idx)
            try:
                return self.view(parent[key])
            except KeyError as exc:
                raise PathAccessError(exc, path, idx)
        if type(parent) is list:
            if path_op not in ('P', '['):
                raise UnsupportedAtomicMutation(
                    'atomic mutation only supports list index access, got %r '
                    'in operation %d (%r)' % (path_op, idx, path))
            index = _atomic_list_index(arg, op, path, idx)
            try:
                return self.view(parent[index])
            except IndexError as exc:
                raise PathAccessError(exc, path, idx)
        if parent is _ATOMIC_DYNAMIC:
            raise UnsupportedAtomicMutation(
                'atomic mutation cannot inspect a value produced by a '
                'dynamic spec at part %d of operation %d (%r)'
                % (idx, op, path))
        raise UnsupportedAtomicMutation(
            'atomic mutation does not support traversing %s objects '
            'at part %d of operation %d (%r)'
            % (type(parent).__name__, idx, op, path))

    def parent_for(self, operation, path):
        cur = self.root
        for idx, (path_op, arg) in enumerate(path.items()):
            cur = self._child(cur, path_op, arg, operation, path, idx)
        return cur

    def assign(self, operation, value):
        parent = self.parent_for(operation, operation.path)
        if parent is _ATOMIC_DYNAMIC:
            raise AtomicMutationError(
                'atomic mutation cannot inspect a value produced by a '
                'dynamic spec in %r' % (operation,))
        if not _is_atomic_container(parent):
            raise UnsupportedAtomicMutation(
                'atomic mutation only supports built-in dict/list '
                'destinations, got %s for %r'
                % (type(parent).__name__, operation))
        path_idx = len(operation.path)
        if type(parent) is dict:
            key = _atomic_dict_key(operation.arg, operation,
                                   operation._orig_path, path_idx)
            parent[key] = value
        else:
            index = _atomic_list_index(operation.arg, operation,
                                       operation._orig_path, path_idx)
            if index < -len(parent) or index >= len(parent):
                raise PathAssignError(
                    IndexError('list assignment index out of range'),
                    operation.path, operation.arg)
            parent[index] = value

    def delete(self, operation):
        try:
            parent = self.parent_for(operation, operation.path)
        except PathAccessError:
            if operation.ignore_missing:
                return
            raise
        if parent is _ATOMIC_DYNAMIC:
            raise AtomicMutationError(
                'atomic mutation cannot inspect a value produced by a '
                'dynamic spec in %r' % (operation,))
        if not _is_atomic_container(parent):
            raise UnsupportedAtomicMutation(
                'atomic mutation only supports built-in dict/list '
                'destinations, got %s for %r'
                % (type(parent).__name__, operation))
        path_idx = len(operation.path)
        if type(parent) is dict:
            key = _atomic_dict_key(operation.arg, operation,
                                   operation._orig_path, path_idx)
            try:
                del parent[key]
            except KeyError as exc:
                if not operation.ignore_missing:
                    raise PathDeleteError(exc, operation.path, operation.arg)
        else:
            index = _atomic_list_index(operation.arg, operation,
                                       operation._orig_path, path_idx)
            try:
                del parent[index]
            except IndexError as exc:
                if not operation.ignore_missing:
                    raise PathDeleteError(exc, operation.path, operation.arg)


def _atomic_actual_parent(target, operation):
    cur = target
    for idx, (path_op, arg) in enumerate(operation.path.items()):
        if type(cur) is dict:
            if path_op not in ('P', '['):
                raise UnsupportedAtomicMutation(
                    'atomic mutation only supports dict key access in %r'
                    % operation)
            key = _atomic_dict_key(arg, operation, operation.path, idx)
            try:
                cur = cur[key]
            except KeyError as exc:
                raise PathAccessError(exc, operation.path, idx)
        elif type(cur) is list:
            if path_op not in ('P', '['):
                raise UnsupportedAtomicMutation(
                    'atomic mutation only supports list index access in %r'
                    % operation)
            index = _atomic_list_index(arg, operation, operation.path, idx)
            try:
                cur = cur[index]
            except IndexError as exc:
                raise PathAccessError(exc, operation.path, idx)
        else:
            raise UnsupportedAtomicMutation(
                'atomic mutation does not support traversing %s objects '
                'at %r' % (type(cur).__name__, operation))
    if not _is_atomic_container(cur):
        raise UnsupportedAtomicMutation(
            'atomic mutation only supports built-in dict/list destinations, '
            'got %s for %r' % (type(cur).__name__, operation))
    return cur


def _restore_atomic_snapshots(snapshots):
    for obj, obj_type, snapshot in reversed(snapshots):
        if obj_type is dict:
            obj.clear()
            obj.update(snapshot)
        else:
            obj[:] = snapshot


def _coerce_atomic_operation(operation, op_idx):
    if isinstance(operation, (Assign, Delete)):
        return operation

    if isinstance(operation, (tuple, list)):
        op_name = operation[0] if operation else None
        if op_name in ('delete', 'del', Delete):
            if len(operation) != 2:
                raise UnsupportedAtomicMutation(
                    'delete operation %d must be ("delete", path)' % op_idx)
            return Delete(operation[1])
        if op_name in ('assign', 'set', Assign):
            if len(operation) != 3:
                raise UnsupportedAtomicMutation(
                    'assign operation %d must be ("assign", path, value)' % op_idx)
            return Assign(operation[1], operation[2])

    raise UnsupportedAtomicMutation(
        'atomic mutation operation %d must be an Assign or Delete, got %r'
        % (op_idx, operation))


def _normalize_atomic_operations(operations):
    if len(operations) == 1:
        first = operations[0]
        if not isinstance(first, (Assign, Delete)):
            if isinstance(first, (list, tuple)):
                operations = tuple(first)
            elif hasattr(first, '__iter__') and not isinstance(first, (str, bytes)):
                operations = tuple(first)
    return tuple(_coerce_atomic_operation(operation, idx)
                 for idx, operation in enumerate(operations))


class Atomic:
    """Apply a batch of :class:`~glom.Assign` and
    :class:`~glom.Delete` operations atomically.

    Unlike a plain sequence of assignments and deletions, ``Atomic``
    validates the whole batch before making any changes. If any step
    fails, every touched container is restored in place, so both the
    root object and any external aliases to child objects keep their
    original identity and contents.

    The supported subset is intentionally small: path assignment and
    deletion on built-in :class:`dict` and :class:`list` objects.
    Attribute assignment, custom ``__setitem__``/``__delitem__``
    implementations, ``Assign(..., missing=...)``, scope paths (``S``),
    and dynamic path segments are rejected before mutation starts.
    Values computed by glom specs are supported as final assigned
    values, but a later operation may not use such a computed value as
    an intermediate container. As with ``Assign``, side effects of
    user callables invoked by a value spec are outside the rollback
    guarantee.

    ``Atomic`` accepts either an iterable of operations or several
    operations as positional arguments. Existing :class:`~glom.Assign`
    and :class:`~glom.Delete` behavior is unchanged.

    >>> target = {'doc': {'status': 'old', 'tags': ['a', 'b']}}
    >>> doc_alias = target['doc']
    >>> atomic(target, [Assign('doc.status', 'new'), Delete('doc.tags.0')])
    {'doc': {'status': 'new', 'tags': ['b']}}
    >>> target['doc'] is doc_alias
    True

    On failure, the original references are restored:

    >>> old_status = target['doc']['status']
    >>> try:
    ...     atomic(target, [Assign('doc.status', 'broken'), Delete('missing')])
    ... except PathDeleteError:
    ...     pass
    >>> target['doc'] is doc_alias
    True
    >>> target['doc']['status'] == 'new'
    True
    >>> target['doc']['status'] is old_status
    True

    .. versionadded:: 25.12.1
    """
    def __init__(self, *operations, ops=None):
        if ops is not None:
            if operations:
                raise TypeError('pass either operations or ops=, not both')
            operations = (ops,)
        self.operations = _normalize_atomic_operations(operations)

    def glomit(self, target, scope):
        if not self.operations:
            return target
        shadow = _AtomicShadow(target)

        for op_idx, operation in enumerate(self.operations):
            if isinstance(operation, Assign):
                if operation.missing is not None:
                    raise UnsupportedAtomicMutation(
                        'atomic mutation does not support Assign(missing=...) '
                        'in operation %d (%r)' % (op_idx, operation))
            if operation._orig_path.path_t.__ops__[0] is not T:
                raise UnsupportedAtomicMutation(
                    'atomic mutation only supports paths rooted at T '
                    'in operation %d (%r)' % (op_idx, operation))
            for path_idx, (path_op, arg) in enumerate(operation._orig_path.items()):
                if path_op not in ('P', '['):
                    raise UnsupportedAtomicMutation(
                        'atomic mutation only supports dict/list key and '
                        'index paths, got %r at part %d of operation %d (%r)'
                        % (path_op, path_idx, op_idx, operation))
                _atomic_path_arg(arg, operation, operation._orig_path, path_idx)

            if isinstance(operation, Assign):
                shadow.assign(operation, _static_atomic_value(operation.val))
            else:
                shadow.delete(operation)

        snapshots = []
        seen_ids = set()
        try:
            for operation in self.operations:
                if isinstance(operation, Assign):
                    val = arg_val(target, operation.val, scope)
                    parent = _atomic_actual_parent(target, operation)
                    parent_id = id(parent)
                    if parent_id not in seen_ids:
                        seen_ids.add(parent_id)
                        snapshots.append((parent, type(parent),
                                          dict(parent) if type(parent) is dict
                                          else list(parent)))
                    path_idx = len(operation.path)
                    if type(parent) is dict:
                        key = _atomic_dict_key(operation.arg, operation,
                                               operation._orig_path, path_idx)
                        try:
                            parent[key] = val
                        except Exception as exc:
                            raise PathAssignError(exc, operation.path,
                                                  operation.arg)
                    else:
                        index = _atomic_list_index(operation.arg, operation,
                                                   operation._orig_path, path_idx)
                        try:
                            parent[index] = val
                        except Exception as exc:
                            raise PathAssignError(exc, operation.path,
                                                  operation.arg)
                    continue

                try:
                    parent = _atomic_actual_parent(target, operation)
                except PathAccessError:
                    if operation.ignore_missing:
                        continue
                    raise
                parent_id = id(parent)
                if parent_id not in seen_ids:
                    seen_ids.add(parent_id)
                    snapshots.append((parent, type(parent),
                                      dict(parent) if type(parent) is dict
                                      else list(parent)))
                path_idx = len(operation.path)
                if type(parent) is dict:
                    key = _atomic_dict_key(operation.arg, operation,
                                           operation._orig_path, path_idx)
                    try:
                        del parent[key]
                    except KeyError as exc:
                        if not operation.ignore_missing:
                            raise PathDeleteError(exc, operation.path,
                                                  operation.arg)
                else:
                    index = _atomic_list_index(operation.arg, operation,
                                               operation._orig_path, path_idx)
                    try:
                        del parent[index]
                    except IndexError as exc:
                        if not operation.ignore_missing:
                            raise PathDeleteError(exc, operation.path,
                                                  operation.arg)
        except BaseException:
            _restore_atomic_snapshots(snapshots)
            raise

        return target

    def __repr__(self):
        cn = self.__class__.__name__
        return '%s([%s])' % (cn, ', '.join([bbrepr(op)
                                            for op in self.operations]))


def atomic(obj, *operations, ops=None):
    """Apply a set of :class:`~glom.Assign` and :class:`~glom.Delete`
    operations atomically to *obj*.

    This is the convenience wrapper around :class:`~glom.Atomic`, and
    accepts the same operation forms. See :class:`~glom.Atomic` for the
    supported subset and rollback guarantees.

    >>> target = {'a': {'b': 1}}
    >>> alias = target['a']
    >>> atomic(target, [Assign('a.b', 2)]) is target
    True
    >>> target['a'] is alias
    True
    >>> target
    {'a': {'b': 2}}

    .. versionadded:: 25.12.1
    """
    return glom(obj, Atomic(*operations, ops=ops))


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
