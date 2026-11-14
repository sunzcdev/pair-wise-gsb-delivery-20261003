import operator
import itertools
import copy
from collections.abc import Mapping
from pprint import pprint

from boltons.typeutils import make_sentinel

from .core import T, glom, GlomError, format_invocation, bbrepr, UnregisteredTarget, MODE
from .grouping import GROUP, target_iter, ACC_TREE, CUR_AGG

_MISSING = make_sentinel('_MISSING')


try:
    basestring
except NameError:
    basestring = str


class FoldError(GlomError):
    """Error raised when Fold() is called on non-iterable
    targets, and possibly other uses in the future."""
    pass


class Fold:
    """The `Fold` specifier type is glom's building block for reducing
    iterables in data, implementing the classic `fold
    <https://en.wikipedia.org/wiki/Fold_(higher-order_function)>`_
    from functional programming, similar to Python's built-in
    :func:`reduce`.

    Args:
       subspec: A spec representing the target to fold, which must be
          an iterable, or otherwise registered to 'iterate' (with
          :func:`~glom.register`).
       init (callable): A function or type which will be invoked to
          initialize the accumulator value.
       op (callable): A function to call on the accumulator value and
          every value, the result of which will become the new
          accumulator value. Defaults to :func:`operator.iadd`.

    Usage is as follows:

       >>> target = [set([1, 2]), set([3]), set([2, 4])]
       >>> result = glom(target, Fold(T, init=frozenset, op=frozenset.union))
       >>> result == frozenset([1, 2, 3, 4])
       True

    Note the required ``spec`` and ``init`` arguments. ``op`` is
    optional, but here must be used because the :class:`set` and
    :class:`frozenset` types do not work with addition.

    While :class:`~glom.Fold` is powerful, :class:`~glom.Flatten` and
    :class:`~glom.Sum` are subtypes with more convenient defaults for
    day-to-day use.
    """
    def __init__(self, subspec, init, op=operator.iadd):
        self.subspec = subspec
        self.init = init
        self.op = op
        if not callable(op):
            raise TypeError('expected callable for %s op param, not: %r' %
                            (self.__class__.__name__, op))
        if not callable(init):
            raise TypeError('expected callable for %s init param, not: %r' %
                            (self.__class__.__name__, init))

    def glomit(self, target, scope):
        is_agg = False
        if scope[MODE] is GROUP and scope.get(CUR_AGG) is None:
            scope[CUR_AGG] = self
            is_agg = True

        if self.subspec is not T:
            target = scope[glom](target, self.subspec, scope)

        if is_agg:
            return self._agg(target, scope[ACC_TREE])
        try:
            return self._fold(target_iter(target, scope))
        except UnregisteredTarget as ut:
            raise FoldError('can only %s on iterable targets, not %s type (%s)'
                            % (self.__class__.__name__, type(target).__name__, ut))

    def _fold(self, iterator):
        ret, op = self.init(), self.op

        for v in iterator:
            ret = op(ret, v)

        return ret

    def _agg(self, target, tree):
        if self not in tree:
            tree[self] = self.init()
        tree[self] = self.op(tree[self], target)
        return tree[self]

    def __repr__(self):
        cn = self.__class__.__name__
        kwargs = {'init': self.init}
        if self.op is not operator.iadd:
            kwargs['op'] = self.op
        return format_invocation(cn, (self.subspec,), kwargs, repr=bbrepr)


class Sum(Fold):
    """The `Sum` specifier type is used to aggregate integers and other
    numericals using addition, much like the :func:`sum()` builtin.

       >>> glom(range(5), Sum())
       10

    Note that this specifier takes a callable *init* parameter like
    its friends, so to change the start value, be sure to wrap it in a
    callable::

       >>> glom(range(5), Sum(init=lambda: 5.0))
       15.0

    To "sum" lists and other iterables, see the :class:`Flatten`
    spec. For other objects, see the :class:`Fold` specifier type.

    """
    def __init__(self, subspec=T, init=int):
        super().__init__(subspec=subspec, init=init, op=operator.iadd)

    def __repr__(self):
        cn = self.__class__.__name__
        args = () if self.subspec is T else (self.subspec,)
        kwargs = {'init': self.init} if self.init is not int else {}
        return format_invocation(cn, args, kwargs, repr=bbrepr)


class Count(Fold):
    """
    takes a count of how many values occurred

    >>> glom([1, 2, 3], Count())
    3
    """
    __slots__ = ()

    def __init__(self):
        super().__init__(
            subspec=T, init=int, op=lambda cur, val: cur + 1)

    def __repr__(self):
        return '%s()' % self.__class__.__name__


class Flatten(Fold):
    """The `Flatten` specifier type is used to combine iterables. By
    default it flattens an iterable of iterables into a single list
    containing items from all iterables.

    >>> target = [[1], [2, 3]]
    >>> glom(target, Flatten())
    [1, 2, 3]

    You can also set *init* to ``"lazy"``, which returns a generator
    instead of a list. Use this to avoid making extra lists and other
    collections during intermediate processing steps.
    """
    def __init__(self, subspec=T, init=list):
        if init == 'lazy':
            self.lazy = True
            init = list
        else:
            self.lazy = False
        super().__init__(subspec=subspec, init=init, op=operator.iadd)

    def _fold(self, iterator):
        if self.lazy:
            return itertools.chain.from_iterable(iterator)
        return super()._fold(iterator)

    def __repr__(self):
        cn = self.__class__.__name__
        args = () if self.subspec is T else (self.subspec,)
        kwargs = {}
        if self.lazy:
            kwargs['init'] = 'lazy'
        elif self.init is not list:
            kwargs['init'] = self.init
        return format_invocation(cn, args, kwargs, repr=bbrepr)


def flatten(target, **kwargs):
    """At its most basic, ``flatten()`` turns an iterable of iterables
    into a single list. But it has a few arguments which give it more
    power:

    Args:

       init (callable): A function or type which gives the initial
          value of the return. The value must support addition. Common
          values might be :class:`list` (the default), :class:`tuple`,
          or even :class:`int`. You can also pass ``init="lazy"`` to
          get a generator.
       levels (int): A positive integer representing the number of
          nested levels to flatten. Defaults to 1.
       spec: The glomspec to fetch before flattening. This defaults to the
          the root level of the object.

    Usage is straightforward.

      >>> target = [[1, 2], [3], [4]]
      >>> flatten(target)
      [1, 2, 3, 4]

    Because integers themselves support addition, we actually have two
    levels of flattening possible, to get back a single integer sum:

      >>> flatten(target, init=int, levels=2)
      10

    However, flattening a non-iterable like an integer will raise an
    exception:

      >>> target = 10
      >>> flatten(target)
      Traceback (most recent call last):
      ...
      FoldError: can only Flatten on iterable targets, not int type (...)

    By default, ``flatten()`` will add a mix of iterables together,
    making it a more-robust alternative to the built-in
    ``sum(list_of_lists, list())`` trick most experienced Python
    programmers are familiar with using:

      >>> list_of_iterables = [range(2), [2, 3], (4, 5)]
      >>> sum(list_of_iterables, [])
      Traceback (most recent call last):
      ...
      TypeError: can only concatenate list (not "tuple") to list

    Whereas flatten() handles this just fine:

      >>> flatten(list_of_iterables)
      [0, 1, 2, 3, 4, 5]

    The ``flatten()`` function is a convenient wrapper around the
    :class:`Flatten` specifier type. For embedding in larger specs,
    and more involved flattening, see :class:`Flatten` and its base,
    :class:`Fold`.

    """
    subspec = kwargs.pop('spec', T)
    init = kwargs.pop('init', list)
    levels = kwargs.pop('levels', 1)
    if kwargs:
        raise TypeError('unexpected keyword args: %r' % sorted(kwargs.keys()))

    if levels == 0:
        return target
    if levels < 0:
        raise ValueError('expected levels >= 0, not %r' % levels)
    spec = (subspec,)
    spec += (Flatten(init="lazy"),) * (levels - 1)
    spec += (Flatten(init=init),)

    return glom(target, spec)


class Merge(Fold):
    """By default, Merge turns an iterable of mappings into a single,
    merged :class:`dict`, leveraging the behavior of the
    :meth:`~dict.update` method. The start state can be customized
    with *init*, as well as the update operation, with *op*.

    Args:
       subspec: The location of the iterable of mappings. Defaults to ``T``.
       init (callable): A type or callable which returns a base
          instance into which all other values will be merged.
       op (callable): A callable, which takes two arguments, and
          performs a merge of the second into the first. Can also be
          the string name of a method to fetch on the instance created
          from *init*. Defaults to ``"update"``.

    .. note::

       Besides the differing defaults, the primary difference between
       :class:`Merge` and other :class:`Fold` subtypes is that its
       *op* argument is assumed to be a two-argument function which
       has no return value and modifies the left parameter
       in-place. Because the initial state is a new object created with
       the *init* parameter, none of the target values are modified.

    """
    def __init__(self, subspec=T, init=dict, op=None):
        if op is None:
            op = 'update'
        if isinstance(op, basestring):
            test_init = init()
            op = getattr(type(test_init), op, None)
        if not callable(op):
            raise ValueError('expected callable "op" arg or an "init" with an .update()'
                             ' method not %r and %r' % (op, init))
        super().__init__(subspec=subspec, init=init, op=op)

    def _fold(self, iterator):
        # the difference here is that ret is mutated in-place, the
        # variable not being reassigned, as in base Fold.
        ret, op = self.init(), self.op

        for v in iterator:
            op(ret, v)

        return ret


    def _agg(self, target, tree):
        if self not in tree:
            acc = tree[self] = self.init()
        else:
            acc = tree[self]
        self.op(acc, target)
        return acc


def merge(target, **kwargs):
    """By default, ``merge()`` turns an iterable of mappings into a
    single, merged :class:`dict`, leveraging the behavior of the
    :meth:`~dict.update` method. A new mapping is created and none of
    the passed mappings are modified.

    >>> target = [{'a': 'alpha'}, {'b': 'B'}, {'a': 'A'}]
    >>> res = merge(target)
    >>> pprint(res)
    {'a': 'A', 'b': 'B'}

    Args:
       target: The list of dicts, or some other iterable of mappings.

    The start state can be customized with the *init* keyword
    argument, as well as the update operation, with the *op* keyword
    argument. For more on those customizations, see the :class:`Merge`
    spec.

    """
    subspec = kwargs.pop('spec', T)
    init = kwargs.pop('init', dict)
    op = kwargs.pop('op', None)
    if kwargs:
        raise TypeError('unexpected keyword args: %r' % sorted(kwargs.keys()))
    spec = Merge(subspec, init, op)
    return glom(target, spec)


class DeepMergeError(FoldError):
    """Base error raised for invalid or impossible deep merges."""

    def __init__(self, message, path=None, **context):
        self.path = path
        self.context = context
        super().__init__(message)


class DeepMergeConflictError(DeepMergeError):
    """Raised when scalar values conflict and ``conflict='error'``."""


class DeepMergeTypeError(DeepMergeError):
    """Raised when mapping, list, and scalar structures cannot be merged."""


class DeepMergeCycleError(DeepMergeError):
    """Raised when a circular reference is found at a specific path."""

    def __init__(self, path, first_path=None):
        if first_path is None:
            super().__init__(path)
        else:
            super().__init__(
                'circular reference at %s (already seen at %s)'
                % (path, first_path), path, first_path=first_path)

    def __str__(self):
        # Rendering glom's target trace would recurse into the very
        # cycle this error is reporting.
        try:
            return super().__str__()
        except RecursionError:
            return '%s: %s' % (
                type(self).__name__, Exception.__str__(self))


class DeepMergeKeyError(DeepMergeError):
    """Raised for invalid element keys during keyed list merging."""


_SCALAR_POLICIES = {
    'first': 'first',
    'keep_first': 'first',
    'keep-first': 'first',
    'prefer_first': 'first',
    'first_wins': 'first',
    'last': 'last',
    'keep_last': 'last',
    'keep-last': 'last',
    'prefer_last': 'last',
    'last_wins': 'last',
    'error': 'error',
    'raise': 'error',
}

_LIST_POLICIES = {
    'replace': 'replace',
    'concat': 'concat',
    'concatenate': 'concat',
    'append': 'concat',
    'extend': 'concat',
    'by_key': 'by_key',
    'by-key': 'by_key',
    'keyed': 'by_key',
    'merge': 'by_key',
    'merge_by_key': 'by_key',
    'merge-by-key': 'by_key',
}

_MISSING_KEY_POLICIES = {
    'error': 'error',
    'raise': 'error',
    'append': 'append',
    'keep': 'append',
    'preserve': 'append',
    'skip': 'skip',
    'exclude': 'skip',
    'ignore': 'skip',
}

_DUPLICATE_KEY_POLICIES = {
    'merge': 'merge',
    'error': 'error',
    'raise': 'error',
    'first': 'first',
    'keep_first': 'first',
    'keep-first': 'first',
    'last': 'last',
    'replace': 'last',
    'keep_last': 'last',
    'keep-last': 'last',
}


def _normalize_choice(value, choices, option_name):
    if isinstance(value, str):
        normalized = choices.get(value.lower())
        if normalized is not None:
            return normalized
    allowed = sorted(set(choices.values()))
    raise ValueError('expected %s to be one of %r, not %r'
                     % (option_name, allowed, value))


def _resolve_option(name, value, default, aliases, kwargs):
    alias_values = []
    for alias in aliases:
        if alias in kwargs:
            alias_values.append((alias, kwargs.pop(alias)))

    if alias_values:
        first_alias, first_value = alias_values[0]
        for alias, alias_value in alias_values[1:]:
            if alias_value != first_value:
                raise TypeError('conflicting values for %s and %s'
                                % (first_alias, alias))
        if value != default and value != first_value:
            raise TypeError('conflicting values for %s and %s'
                            % (name, first_alias))
        value = first_value
    return value


def _path_key(path, key):
    if isinstance(key, str) and key.isidentifier():
        return '%s.%s' % (path, key)
    if path == '$':
        return '$[%r]' % (key,)
    return '%s[%r]' % (path, key)


def _path_index(path, index):
    return '%s[%d]' % (path, index)


class DeepMerge(Fold):
    """Recursively merge an iterable of mapping documents.

    Unlike :class:`Merge`, which performs a shallow
    :meth:`~dict.update`, ``DeepMerge`` descends into nested mappings
    and lists. Every input layer is copied, so the result never shares
    mutable containers with the source documents.

    Args:
       subspec: A spec for locating the iterable of documents. Defaults
          to ``T``.
       conflict (str): What to do when two scalar values differ:
          ``"last"`` (the default), ``"first"``, or ``"error"``.
       lists (str): How to merge two lists. There is deliberately no
          default: callers must choose ``"replace"``, ``"concat"``, or
          ``"by_key"`` when lists are present.
       key: The mapping key used to identify list elements when
          ``lists="by_key"``. May also be a callable returning an
          element's identity.
       missing_key (str): Policy for keyed list elements without
          *key*: ``"error"`` (the default), ``"append"``, or ``"skip"``.
       duplicate_key (str): Policy for repeated element keys:
          ``"merge"`` (the default), ``"error"``, ``"first"``, or
          ``"last"``.
       reject_cycles (bool): Circular references are rejected by
          default, with the offending and original paths included in
          the error.

    Keyed merges retain the order in which element keys first appear.
    Repeated keys merge recursively in place, so their original list
    position is retained.

    A mapping value may never be merged into a list value, or vice
    versa. Such structural changes raise :class:`DeepMergeTypeError`
    with the full path, rather than being silently coerced.

    Circular references are always rejected with
    :class:`DeepMergeCycleError`, which includes both the path at which
    the cycle was detected and the path where the container was first
    seen.
    """

    def __init__(self, subspec=T, conflict='last', lists=None, key=None,
                 missing_key='error', duplicate_key='merge',
                 reject_cycles=True, **kwargs):
        conflict = _resolve_option(
            'conflict', conflict, 'last',
            ('on_conflict', 'on_scalar_conflict', 'scalar_conflict',
             'scalar_policy', 'scalar'), kwargs)
        lists = _resolve_option(
            'lists', lists, None,
            ('list_strategy', 'list_merge', 'list_mode', 'on_list',
             'list_policy'),
            kwargs)
        key = _resolve_option(
            'key', key, None,
            ('list_key', 'element_key', 'key_field', 'by_key'), kwargs)
        missing_key = _resolve_option(
            'missing_key', missing_key, 'error',
            ('on_missing_key', 'on_missing', 'missing_key_policy',
             'missing'), kwargs)
        duplicate_key = _resolve_option(
            'duplicate_key', duplicate_key, 'merge',
            ('on_duplicate_key', 'on_duplicate', 'duplicate_key_policy',
             'duplicate', 'duplicates'), kwargs)
        reject_cycles = _resolve_option(
            'reject_cycles', reject_cycles, True,
            ('reject_cycle',), kwargs)
        if kwargs:
            raise TypeError('unexpected keyword args: %r'
                            % sorted(kwargs.keys()))

        self.subspec = subspec
        self.conflict = _normalize_choice(
            conflict, _SCALAR_POLICIES, 'conflict')
        if lists is not None:
            lists = _normalize_choice(lists, _LIST_POLICIES, 'lists')
        self.lists = lists
        self.key = key
        self.missing_key = _normalize_choice(
            missing_key, _MISSING_KEY_POLICIES, 'missing_key')
        self.duplicate_key = _normalize_choice(
            duplicate_key, _DUPLICATE_KEY_POLICIES, 'duplicate_key')
        if reject_cycles is not True:
            raise ValueError(
                'reject_cycles must be True; circular references are '
                'always rejected')

        if lists == 'by_key' and key is None:
            raise ValueError('lists="by_key" requires an element key')
        if lists != 'by_key' and key is not None:
            raise ValueError('the element key is only used with lists="by_key"')

    def glomit(self, target, scope):
        is_agg = False
        if scope[MODE] is GROUP and scope.get(CUR_AGG) is None:
            scope[CUR_AGG] = self
            is_agg = True

        if self.subspec is not T:
            target = scope[glom](target, self.subspec, scope)

        if is_agg:
            return self._agg(target, scope[ACC_TREE])

        if isinstance(target, Mapping):
            iterator = (target,)
        else:
            try:
                iterator = target_iter(target, scope)
            except UnregisteredTarget as ut:
                raise FoldError(
                    'can only DeepMerge on iterable targets, not %s type (%s)'
                    % (type(target).__name__, ut))
        return self._fold(iterator)

    def _fold(self, iterator):
        result = {}
        for document in iterator:
            if not isinstance(document, Mapping):
                raise DeepMergeError(
                    'expected each document to be a mapping, not %s'
                    % type(document).__name__, '$')
            self._merge_mapping(result, document, '$', {})
        return result

    def _agg(self, target, tree):
        if self not in tree:
            tree[self] = {}
        if not isinstance(target, Mapping):
            raise DeepMergeError(
                'expected each document to be a mapping, not %s'
                % type(target).__name__, '$')
        self._merge_mapping(tree[self], target, '$', {})
        return tree[self]

    def _enter(self, value, path, active):
        first_path = active.get(id(value))
        if first_path is not None:
            raise DeepMergeCycleError(path, first_path)
        active[id(value)] = path

    def _clone(self, value, path, active):
        if isinstance(value, Mapping):
            self._enter(value, path, active)
            try:
                result = {}
                for key, child in value.items():
                    result[key] = self._clone(
                        child, _path_key(path, key), active)
                return result
            finally:
                del active[id(value)]

        if isinstance(value, list):
            return self._merge_lists([], value, path, active)

        if isinstance(value, tuple):
            self._enter(value, path, active)
            try:
                return tuple(self._clone(
                    child, _path_index(path, index), active)
                    for index, child in enumerate(value))
            finally:
                del active[id(value)]

        try:
            return copy.deepcopy(value)
        except Exception as exc:
            raise DeepMergeError(
                'could not copy value at %s: %s' % (path, exc),
                path) from exc

    def _merge_mapping(self, destination, source, path, active):
        self._enter(source, path, active)
        try:
            for key, source_value in source.items():
                child_path = _path_key(path, key)
                if key not in destination:
                    destination[key] = self._clone(
                        source_value, child_path, active)
                else:
                    destination[key] = self._merge_value(
                        destination[key], source_value, child_path, active)
        finally:
            del active[id(source)]
        return destination

    def _merge_value(self, left, right, path, active):
        if isinstance(left, Mapping):
            if isinstance(right, Mapping):
                return self._merge_mapping(left, right, path, active)
            raise DeepMergeTypeError(
                'cannot merge %s into mapping at %s'
                % (type(right).__name__, path), path)

        if isinstance(left, list):
            if isinstance(right, list):
                return self._merge_lists(left, right, path, active)
            raise DeepMergeTypeError(
                'cannot merge %s into list at %s'
                % (type(right).__name__, path), path)

        if isinstance(right, (Mapping, list)):
            raise DeepMergeTypeError(
                'cannot merge %s into %s at %s'
                % (type(right).__name__, type(left).__name__, path), path)

        return self._merge_scalars(left, right, path, active)

    def _merge_scalars(self, left, right, path, active):
        if type(left) is type(right) and _values_equal(left, right):
            return left
        if self.conflict == 'first':
            return left
        if self.conflict == 'last':
            return self._clone(right, path, active)
        raise DeepMergeConflictError(
            'scalar conflict at %s: %r != %r' % (path, left, right),
            path, left=left, right=right)

    def _merge_lists(self, left, right, path, active):
        if self.lists is None:
            raise DeepMergeError(
                'list merge strategy required at %s; pass lists="replace", '
                '"concat", or "by_key"' % path, path)

        self._enter(right, path, active)
        try:
            if self.lists == 'replace':
                return [self._clone(
                    item, _path_index(path, index), active)
                    for index, item in enumerate(right)]

            if self.lists == 'concat':
                result = list(left)
                result.extend(
                    self._clone(item, _path_index(path, index), active)
                    for index, item in enumerate(right))
                return result

            return self._merge_keyed_lists(left, right, path, active)
        finally:
            del active[id(right)]

    def _element_key(self, item, path):
        if not isinstance(item, Mapping):
            raise DeepMergeTypeError(
                'keyed list elements must be mappings, not %s at %s'
                % (type(item).__name__, path), path)
        try:
            if callable(self.key):
                return self.key(item)
            return item[self.key]
        except KeyError:
            return _MISSING

    def _check_element_key(self, key, path):
        try:
            hash(key)
        except TypeError:
            raise DeepMergeKeyError(
                'unhashable element key %r at %s' % (key, path), path)

    def _merge_keyed_lists(self, left, right, path, active):
        result = list(left)
        positions = {}
        for index, item in enumerate(result):
            item_path = _path_index(path, index)
            key = self._element_key(item, item_path)
            if key is _MISSING:
                raise DeepMergeKeyError(
                    'element at %s is missing key %r'
                    % (item_path, self.key), item_path, key=self.key)
            self._check_element_key(key, item_path)
            positions[key] = index

        for source_index, item in enumerate(right):
            item_path = _path_index(path, source_index)
            key = self._element_key(item, item_path)
            if key is _MISSING:
                if self.missing_key == 'error':
                    raise DeepMergeKeyError(
                        'element at %s is missing key %r'
                        % (item_path, self.key), item_path, key=self.key)
                if self.missing_key == 'skip':
                    continue
                result.append(self._clone(item, item_path, active))
                continue

            self._check_element_key(key, item_path)
            if key not in positions:
                positions[key] = len(result)
                result.append(self._clone(item, item_path, active))
                continue

            destination_index = positions[key]
            destination_path = _path_index(path, destination_index)
            if self.duplicate_key == 'error':
                raise DeepMergeKeyError(
                    'duplicate element key %r at %s'
                    % (key, item_path), item_path, key=key)
            if self.duplicate_key == 'first':
                continue
            if self.duplicate_key == 'last':
                result[destination_index] = self._clone(
                    item, destination_path, active)
                continue

            destination = result[destination_index]
            if not isinstance(destination, Mapping):
                raise DeepMergeTypeError(
                    'cannot merge repeated key %r into %s at %s'
                    % (key, type(destination).__name__, destination_path),
                    destination_path)
            result[destination_index] = self._merge_mapping(
                destination, item, destination_path, active)

        return result

    def __repr__(self):
        cn = self.__class__.__name__
        args = () if self.subspec is T else (self.subspec,)
        kwargs = {}
        if self.conflict != 'last':
            kwargs['conflict'] = self.conflict
        if self.lists is not None:
            kwargs['lists'] = self.lists
        if self.key is not None:
            kwargs['key'] = self.key
        if self.missing_key != 'error':
            kwargs['missing_key'] = self.missing_key
        if self.duplicate_key != 'merge':
            kwargs['duplicate_key'] = self.duplicate_key
        return format_invocation(cn, args, kwargs, repr=bbrepr)


def _values_equal(left, right):
    try:
        return bool(left == right)
    except Exception:
        return False


def deep_merge(target, *documents, **kwargs):
    """Recursively merge mappings without modifying any input value.

    The *target* is an iterable of mapping documents. Additional
    mappings may be passed positionally for convenience.

    >>> docs = [{'user': {'name': 'A', 'tags': ['x']}},
    ...         {'user': {'id': 1, 'tags': ['y']}}]
    >>> deep_merge(docs, lists='concat')
    {'user': {'name': 'A', 'tags': ['x', 'y'], 'id': 1}}

    For scalar conflict handling, list strategies, and keyed list
    options, see :class:`DeepMerge`.
    """
    subspec = kwargs.pop('spec', T)
    if documents:
        if subspec is not T:
            raise TypeError('spec cannot be combined with positional documents')
        target = [target]
        target.extend(documents)
    spec = DeepMerge(subspec, **kwargs)
    return glom(target, spec)
