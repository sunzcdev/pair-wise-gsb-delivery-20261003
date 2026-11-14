"""
Group mode
"""

import hashlib
import random

from boltons.typeutils import make_sentinel

from .core import glom, MODE, SKIP, STOP, TargetRegistry, Path, T, BadSpec, _MISSING


STATE_FORMAT_VERSION = 1

# node "kind" tags used inside an exported state document
_NODE_DICT = 'dict'
_NODE_COUNT = 'count'
_NODE_SUM = 'sum'
_NODE_AVG = 'avg'

# explicit type tags so numeric/key types round-trip losslessly
_TAG_BOOL = 'bool'
_TAG_INT = 'int'
_TAG_FLOAT = 'float'
_TAG_STR = 'str'
_TAG_BYTES = 'bytes'
_TAG_NONE = 'none'
_TAG_TUPLE = 'tuple'


class GroupStateError(Exception):
    """Raised when a :class:`Group` cannot export or restore its
    aggregation state.

    This covers unsupported aggregations or key types (which are
    rejected up front, before any data is processed), malformed state
    documents, format-version mismatches, and attempts to restore a
    state produced by a different grouping definition.
    """

ACC_TREE = make_sentinel('ACC_TREE')
ACC_TREE.__doc__ = """
tree of accumulators for aggregation;
structure roughly corresponds to the result,
but is not 1:1; instead the main purpose is to ensure
data is kept until the Group() finishes executing
"""

CUR_AGG = make_sentinel('CUR_AGG')
CUR_AGG.__doc__ = """
the spec which is currently performing aggregation --
useful for specs that want to work in either "aggregate"
mode, or "spec" mode depending on if they are in Group mode
or not; this sentinel in the Scope allows a spec to decide
if it is "closest" to the Group and so should behave
like an aggregate, or if it is further away and so should
have normal spec behavior.
"""

ACC_BRANCH = make_sentinel('ACC_BRANCH')
ACC_BRANCH.__doc__ = """
side-table mapping a dict grouping spec (by id) to the index of the
keyspec/valspec pair that created each data key, preserving branch
provenance for state export when several keyspecs yield overlapping keys
"""


def target_iter(target, scope):
    iterate = scope[TargetRegistry].get_handler('iterate', target, path=scope[Path])

    try:
        iterator = iterate(target)
    except Exception as e:
        raise TypeError('failed to iterate on instance of type %r at %r (got %r)'
                        % (target.__class__.__name__, Path(*scope[Path]), e))
    return iterator


class Group:
    """supports nesting grouping operations --
    think of a glom-style recursive boltons.iterutils.bucketize

    the "branches" of a Group spec are dicts;
    the leaves are lists, or an Aggregation object
    an Aggregation object is any object that defines the
    method agg(target, accumulator)

    For example, here we get a map of even and odd counts::

    >>> glom(range(10), Group({T % 2: T}))
    {0: 8, 1: 9}

    And here we create a `"bucketized"
    <https://boltons.readthedocs.io/en/latest/iterutils.html#boltons.iterutils.bucketize>`_
    map of even and odd numbers::

    >>> glom(range(10), Group({T % 2: [T]}))
    {0: [0, 2, 4, 6, 8], 1: [1, 3, 5, 7, 9]}

    target is the current target, accumulator is a dict
    maintained by Group mode

    unlike Iter(), Group() converts an iterable target
    into a single result; Iter() converts an iterable
    target into an iterable result

    A Group keeps its running aggregation state on the instance, so the
    same Group can be fed data in multiple batches. :meth:`export_state`
    and :meth:`restore_state` make that state portable across processes
    and runs, which makes long-running, multi-batch collection possible.

    """
    def __init__(self, spec):
        self.spec = spec
        # persistent accumulator support structure; reused across
        # glomit() calls so feeding data in batches is equivalent to
        # feeding it all at once.
        self._acc_tree = {}

    def glomit(self, target, scope):
        scope[MODE] = GROUP
        scope[CUR_AGG] = None  # reset aggregation tripwire for sub-specs
        scope[ACC_TREE] = self._acc_tree

        # seed the result from any state carried over from earlier
        # batches, so an empty batch (or a leaf aggregator that has
        # already stopped) returns the accumulated value rather than
        # starting over.
        ret = self._initial_result()

        for t in target_iter(target, scope):
            last, ret = ret, scope[glom](t, self.spec, scope)
            if ret is STOP:
                return last
        return ret

    def _initial_result(self):
        spec_type = type(self.spec)
        if spec_type in (dict, list):
            existing = self._acc_tree.get(id(self.spec))
            if existing is not None and existing is not STOP:
                return existing
            return spec_type()
        # leaf aggregations (Avg, Count, Sum, ...) keep their value
        # keyed by the aggregator instance. A stored STOP marks an
        # aggregator that has already finished (e.g. First); surface None
        # as the baseline just like a fresh Group.
        existing = self._acc_tree.get(self.spec)
        return None if existing is STOP else existing

    def export_state(self):
        """Return a portable, JSON-safe snapshot of this Group's current
        aggregation state.

        Only the built-in count (:class:`~glom.Count`), sum
        (:class:`~glom.Sum`) and average (:class:`Avg`) aggregations and
        nested dict groupings are supported; any other aggregation (or an
        unsupported key type) raises :class:`GroupStateError` before any
        data is dropped.

        The returned object is a fresh, self-contained snapshot built only
        from JSON-compatible types; continuing to feed this Group after
        exporting does not alter a previously returned state. It includes a
        format version and a fingerprint of the grouping spec, so restoring
        it into a Group with a different definition is rejected.
        """
        validate_spec(self.spec)
        return {
            'format_version': STATE_FORMAT_VERSION,
            'spec_fingerprint': spec_fingerprint(self.spec),
            'state': _export_node(self.spec, self._acc_tree),
        }

    def restore_state(self, state):
        """Restore aggregation state previously produced by
        :meth:`export_state` (possibly in another process).

        The *state* document's format version and grouping-spec
        fingerprint are validated, so state from a different grouping
        definition raises :class:`GroupStateError`. On success this
        Group's accumulator is rebuilt and further data can be fed with
        results identical to processing every input in one go.
        """
        if not isinstance(state, dict):
            raise GroupStateError('expected a state mapping, not %r'
                                  % type(state).__name__)
        version = state.get('format_version')
        if version != STATE_FORMAT_VERSION:
            raise GroupStateError(
                'unsupported state format version %r; this build supports %r'
                % (version, STATE_FORMAT_VERSION))
        fingerprint = state.get('spec_fingerprint')
        expected = spec_fingerprint(self.spec)
        if fingerprint != expected:
            raise GroupStateError(
                'state spec fingerprint %r does not match this Group (%r); '
                'state can only be restored into a Group with the same '
                'grouping definition' % (fingerprint, expected))
        validate_spec(self.spec)
        if 'state' not in state:
            raise GroupStateError('state document is missing its "state" node')
        self._acc_tree = _restore_node(self.spec, state['state'])
        return self

    def __repr__(self):
        cn = self.__class__.__name__
        return f'{cn}({self.spec!r})'


def GROUP(target, spec, scope):
    """
    Group mode dispatcher; also sentinel for current mode = group
    """
    recurse = lambda spec: scope[glom](target, spec, scope)
    tree = scope[ACC_TREE]  # current accumulator support structure
    if callable(getattr(spec, "agg", None)):
        return spec.agg(target, tree)
    elif callable(spec):
        return spec(target)
    _spec_type = type(spec)
    if _spec_type not in (dict, list):
        raise BadSpec("Group mode expected dict, list, callable, or"
                      " aggregator, not: %r" % (spec,))
    _spec_id = id(spec)
    try:
        acc = tree[_spec_id]  # current accumulator
    except KeyError:
        acc = tree[_spec_id] = _spec_type()
    if _spec_type is dict:
        done = True
        branch_index = tree.get(ACC_BRANCH)
        if not isinstance(branch_index, dict):
            branch_index = tree[ACC_BRANCH] = {}
        index_by_spec = branch_index.setdefault(_spec_id, {})
        for branch, (keyspec, valspec) in enumerate(spec.items()):
            if tree.get(keyspec, None) is STOP:
                continue
            key = recurse(keyspec)
            if key is SKIP:
                done = False  # SKIP means we still want more vals
                continue
            if key is STOP:
                tree[keyspec] = STOP
                continue
            if key not in acc:
                # TODO: guard against key == id(spec)
                tree[key] = {}
                index_by_spec[key] = branch
            scope[ACC_TREE] = tree[key]
            result = recurse(valspec)
            if result is STOP:
                tree[keyspec] = STOP
                continue
            done = False  # SKIP or returning a value means we still want more vals
            if result is not SKIP:
                acc[key] = result
        if done:
            return STOP
        return acc
    elif _spec_type is list:
        for valspec in spec:
            if type(valspec) is dict:
                # doesn't make sense due to arity mismatch. did you mean [Auto({...})] ?
                raise BadSpec('dicts within lists are not'
                              ' allowed while in Group mode: %r' % spec)
            result = recurse(valspec)
            if result is STOP:
                return STOP
            if result is not SKIP:
                acc.append(result)
        return acc
    raise ValueError(f"{_spec_type} not a valid spec type for Group mode")  # pragma: no cover


# ---------------------------------------------------------------------------
# aggregation state export / restore
# ----------------------------------------------------------------
#
# The accumulator tree (ACC_TREE) is keyed by spec id() for dict/list
# branches, by aggregator *instance* for leaf aggregations, and by data
# keys for group buckets. The walkers below traverse the user-facing spec
# in parallel with that tree, so the exported document never relies on
# id()/object identity and remains portable across processes.


def _encode_value(value):
    # lossless, JSON-safe encoding of a scalar group key or numeric value
    if isinstance(value, bool):
        return [_TAG_BOOL, value]
    if isinstance(value, int):
        # str() keeps arbitrarily large ints exact across JSON
        return [_TAG_INT, str(value)]
    if isinstance(value, float):
        return [_TAG_FLOAT, value]
    if isinstance(value, str):
        return [_TAG_STR, value]
    if isinstance(value, bytes):
        return [_TAG_BYTES, value.hex()]
    if value is None:
        return [_TAG_NONE]
    if isinstance(value, tuple):
        return [_TAG_TUPLE, [_encode_value(item) for item in value]]
    raise GroupStateError(
        'group keys of type %r cannot be serialized into aggregation state; '
        'supported key types are bool, int, float, str, bytes, None and '
        'tuples thereof' % type(value).__name__)


def _decode_value(node):
    if not isinstance(node, list) or not node:
        raise GroupStateError('malformed encoded value: %r' % (node,))
    tag = node[0]
    if tag == _TAG_BOOL:
        return bool(node[1])
    if tag == _TAG_INT:
        return int(node[1])
    if tag == _TAG_FLOAT:
        return float(node[1])
    if tag == _TAG_STR:
        return str(node[1])
    if tag == _TAG_BYTES:
        return bytes.fromhex(node[1])
    if tag == _TAG_NONE:
        return None
    if tag == _TAG_TUPLE:
        return tuple(_decode_value(item) for item in node[1])
    raise GroupStateError('unknown value type tag in state: %r' % (tag,))


def _is_supported_leaf(spec):
    from .reduction import Count, Sum
    if isinstance(spec, Avg):
        return True
    if isinstance(spec, Count):
        return True
    if isinstance(spec, Sum):
        # a custom init callable cannot be reconstructed from state
        return spec.init in (int, float)
    return False


def validate_spec(spec):
    """Recursively confirm *spec* only uses groupings and aggregations that
    can be represented in portable state, raising :class:`GroupStateError`
    before processing begins otherwise."""
    spec_type = type(spec)
    if spec_type is dict:
        if not spec:
            raise GroupStateError('empty dict grouping specs have no state to export')
        for keyspec, valspec in spec.items():
            validate_spec(valspec)
        return
    if _is_supported_leaf(spec):
        return
    raise GroupStateError(
        'aggregation %r is not supported for state export/restore; only '
        'Count, Sum (int/float), Avg and nested dict groupings are '
        'supported, and arbitrary callables cannot be restored' % (spec,))


def _callable_token(func):
    # a best-effort stable identity for a key spec across processes
    qualname = getattr(func, '__qualname__', None)
    if qualname and '<lambda>' not in qualname:
        return '%s.%s' % (getattr(func, '__module__', '?'), qualname)
    try:
        import inspect
        source = inspect.getsource(func).strip()
    except (OSError, TypeError):
        return 'repr:' + repr(func)
    return 'src:' + hashlib.sha256(source.encode('utf-8')).hexdigest()


def _key_descriptor(keyspec):
    if callable(keyspec):
        return ('callable', _callable_token(keyspec))
    return ('spec', repr(keyspec))


def _init_descriptor(init):
    if init in (int, float):
        return init.__name__
    return _callable_token(init)


def _spec_descriptor(spec):
    spec_type = type(spec)
    if spec_type is dict:
        return ('dict', tuple(
            (_key_descriptor(keyspec), _spec_descriptor(valspec))
            for keyspec, valspec in spec.items()))
    from .reduction import Count, Sum
    if isinstance(spec, Count):
        return ('agg', 'Count')
    if isinstance(spec, Sum):
        return ('agg', 'Sum', _init_descriptor(spec.init))
    if isinstance(spec, Avg):
        return ('agg', 'Avg')
    return ('obj', repr(spec))


def spec_fingerprint(spec):
    """Stable hash of a grouping definition's structure and aggregations,
    used to reject state restored into an incompatible Group."""
    descriptor = repr(_spec_descriptor(spec)).encode('utf-8')
    return hashlib.sha256(descriptor).hexdigest()


def _export_node(spec, tree):
    from .reduction import Count, Sum
    if not isinstance(tree, dict):
        tree = {}
    spec_type = type(spec)
    if spec_type is dict:
        acc = tree.get(id(spec))
        items = list(spec.items())
        index_by_spec = tree.get(ACC_BRANCH, {}).get(id(spec), {})
        groups = []
        if isinstance(acc, dict):
            for key in acc:
                child_tree = tree.get(key)
                index = index_by_spec.get(key)
                if index is None:
                    # state produced before provenance tracking; fall back
                    # to the first branch (correct for a single keyspec)
                    index = 0
                valspec = items[index][1]
                groups.append({
                    'key': _encode_value(key),
                    'valspec': index,
                    'node': _export_node(valspec, child_tree),
                })
        return {'kind': _NODE_DICT, 'groups': groups}
    if isinstance(spec, Count):
        return {'kind': _NODE_COUNT, 'value': _encode_value(tree.get(spec, 0))}
    if isinstance(spec, Sum):
        value = tree.get(spec, spec.init())
        return {'kind': _NODE_SUM, 'value': _encode_value(value)}
    if isinstance(spec, Avg):
        running = tree.get(spec, [0.0, 0])
        return {'kind': _NODE_AVG,
                'sum': _encode_value(running[0]),
                'count': _encode_value(running[1])}
    raise GroupStateError('cannot export unsupported aggregation: %r' % (spec,))


def _require_kind(node, kind):
    if not isinstance(node, dict) or node.get('kind') != kind:
        raise GroupStateError(
            'expected a %r state node, got %r' % (kind, node))


def _current_result(spec, child_tree):
    if isinstance(spec, Avg):
        total, count = child_tree[spec]
        return total / count if count else 0.0
    if type(spec) is dict:
        return child_tree[id(spec)]
    return child_tree[spec]


def _restore_node(spec, node):
    from .reduction import Count, Sum
    spec_type = type(spec)
    if spec_type is dict:
        _require_kind(node, _NODE_DICT)
        items = list(spec.items())
        tree = {}
        acc = {}
        tree[id(spec)] = acc
        index_by_spec = {}
        tree[ACC_BRANCH] = {id(spec): index_by_spec}
        for group in node['groups']:
            index = group['valspec']
            if not isinstance(index, int) or not (0 <= index < len(items)):
                raise GroupStateError('state references invalid grouping branch %r'
                                      % (index,))
            valspec = items[index][1]
            key = _decode_value(group['key'])
            child_tree = _restore_node(valspec, group['node'])
            tree[key] = child_tree
            index_by_spec[key] = index
            acc[key] = _current_result(valspec, child_tree)
        return tree
    if isinstance(spec, Count):
        _require_kind(node, _NODE_COUNT)
        return {spec: _decode_value(node['value'])}
    if isinstance(spec, Sum):
        _require_kind(node, _NODE_SUM)
        value = _decode_value(node['value'])
        if spec.init is float and not isinstance(value, float):
            value = float(value)
        return {spec: value}
    if isinstance(spec, Avg):
        _require_kind(node, _NODE_AVG)
        return {spec: [_decode_value(node['sum']), _decode_value(node['count'])]}
    raise GroupStateError('cannot restore unsupported aggregation: %r' % (spec,))


class First:
    """
    holds onto the first value

    >>> glom([1, 2, 3], Group(First()))
    1
    """
    __slots__ = ()

    def agg(self, target, tree):
        if self not in tree:
            tree[self] = STOP
            return target
        return STOP

    def __repr__(self):
        return '%s()' % self.__class__.__name__


class Avg:
    """
    takes the numerical average of all values;
    raises exception on non-numeric value

    >>> glom([1, 2, 3], Group(Avg()))
    2.0
    """
    __slots__ = ()

    def agg(self, target, tree):
        try:
            avg_acc = tree[self]
        except KeyError:
            # format is [sum, count]
            avg_acc = tree[self] = [0.0, 0]
        avg_acc[0] += target
        avg_acc[1] += 1
        return avg_acc[0] / avg_acc[1]

    def __repr__(self):
        return '%s()' % self.__class__.__name__


class Max:
    """
    takes the maximum of all values;
    raises exception on values that are not comparable

    >>> glom([1, 2, 3], Group(Max()))
    3
    """
    __slots__ = ()

    def agg(self, target, tree):
        if self not in tree or target > tree[self]:
            tree[self] = target
        return tree[self]

    def __repr__(self):
        return '%s()' % self.__class__.__name__


class Min:
    """
    takes the minimum of all values;
    raises exception on values that are not comparable

    >>> glom([1, 2, 3], Group(Min()))
    1
    """
    __slots__ = ()

    def agg(self, target, tree):
        if self not in tree or target < tree[self]:
            tree[self] = target
        return tree[self]

    def __repr__(self):
        return '%s()' % self.__class__.__name__


class Sample:
    """takes a random sample of the values

    >>> glom([1, 2, 3], Group(Sample(2)))  # doctest: +SKIP
    [1, 3]
    >>> glom(range(5000), Group(Sample(2)))  # doctest: +SKIP
    [272, 2901]

    The advantage of this over :func:`random.sample` is that this can
    take an arbitrarily-sized, potentially-very-long streaming input
    and returns a fixed-size output. Note that this does not stream
    results out, so your streaming input must have finite length.
    """
    __slots__ = ('size',)

    def __init__(self, size):
        self.size = size

    def agg(self, target, tree):
        # simple reservoir sampling scheme
        # https://en.wikipedia.org/wiki/Reservoir_sampling#Simple_algorithm
        if self not in tree:
            tree[self] = [0, []]
        num_seen, sample = tree[self]
        if len(sample) < self.size:
            sample.append(target)
        else:
            pos = random.randint(0, num_seen)
            if pos < self.size:
                sample[pos] = target
        tree[self][0] += 1
        return sample

    def __repr__(self):
        return f'{self.__class__.__name__}({self.size!r})'



class Limit:
    """
    Limits the number of values passed to sub-accumulator

    >>> glom([1, 2, 3], Group(Limit(2)))
    [1, 2]

    To override the default untransformed list output, set the subspec kwarg:

    >>> glom(range(10), Group(Limit(3, subspec={(lambda x: x % 2): [T]})))
    {0: [0, 2], 1: [1]}

    You can even nest Limits in other ``Group`` specs:

    >>> glom(range(10), Group(Limit(5, {(lambda x: x % 2): Limit(2)})))
    {0: [0, 2], 1: [1, 3]}

    """
    __slots__ = ('n', 'subspec')

    def __init__(self, n, subspec=_MISSING):
        if subspec is _MISSING:
            subspec = [T]
        self.n = n
        self.subspec = subspec

    def glomit(self, target, scope):
        if scope[MODE] is not GROUP:
            raise BadSpec("Limit() only valid in Group mode")
        tree = scope[ACC_TREE]  # current accumulator support structure
        if self not in tree:
            tree[self] = [0, {}]
        scope[ACC_TREE] = tree[self][1]
        tree[self][0] += 1
        if tree[self][0] > self.n:
            return STOP
        return scope[glom](target, self.subspec, scope)

    def __repr__(self):
        return f'{self.__class__.__name__}({self.n!r}, {self.subspec!r})'
