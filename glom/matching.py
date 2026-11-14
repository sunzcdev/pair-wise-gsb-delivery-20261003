"""
.. versionadded:: 20.7.0

Sometimes you want to confirm that your target data matches your
code's assumptions. With glom, you don't need a separate validation
step, you can do these checks inline with your glom spec, using
:class:`~glom.Match` and friends.
"""

import re
import sys
from contextlib import contextmanager
from pprint import pprint

from boltons.iterutils import is_iterable
from boltons.typeutils import make_sentinel

from .core import GlomError, glom, T, MODE, bbrepr, bbformat, format_invocation, Path, chain_child, Val, arg_val


_MISSING = make_sentinel('_MISSING')

#: scope key under which the active :class:`_MatchCollector` is stored when
#: a :class:`Match` is being evaluated in multi-error mode.
MATCH_COLLECTOR = make_sentinel('MATCH_COLLECTOR')

#: scope key holding a suspension depth. While positive, multi-error
#: collection is hidden inside speculative branches, restoring raise
#: semantics so failure can be detected via exceptions.
MATCH_SUSPEND = make_sentinel('MATCH_SUSPEND')

#: default cap on the number of problems collected before traversal stops.
DEFAULT_MAX_PROBLEMS = 10


# NOTE: it is important that MatchErrors be cheap to construct,
# because negative matches are part of normal control flow
# (e.g. often it is idiomatic to cascade from one possible match
# to the next and take the first one that works)
class MatchError(GlomError):
    """
    Raised when a :class:`Match` or :data:`M` check fails.

    >>> glom({123: 'a'}, Match({'id': int}))
    Traceback (most recent call last):
    ...
    MatchError: key 123 didn't match any of ['id']

    """
    def __init__(self, fmt, *args):
        super().__init__(fmt, *args)

    def get_message(self):
        fmt, args = self.args[0], self.args[1:]
        return bbformat(fmt, *args)


class TypeMatchError(MatchError, TypeError):
    """:exc:`MatchError` subtype raised when a
    :class:`Match` fails a type check.

    >>> glom({'id': 'a'}, Match({'id': int}))
    Traceback (most recent call last):
    ...
    TypeMatchError: error raised while processing.
     Target-spec trace, with error detail (most recent last):
     - Target: {'id': 'a'}
     - Spec: Match({'id': <type 'int'>})
     - Spec: {'id': <type 'int'>}
     - Target: 'a'
     - Spec: int
    TypeMatchError: expected type int, not str
    """

    def __init__(self, actual, expected):
        super().__init__(
            "expected type {0.__name__}, not {1.__name__}", expected, actual)

    def __copy__(self):
        # __init__ args = (actual, expected)
        # self.args = (fmt_str, expected, actual)
        return TypeMatchError(self.args[2], self.args[1])


class MatchProblem:
    """A single, structured description of why one location of a target
    did not match its :class:`Match` spec.

    Instances are produced in multi-error mode (see :meth:`Match.collect`
    and ``Match(..., multi_error=True)``). Only structural, non-sensitive
    information is retained:

    Args:
       path (tuple): location of the problem within the target, made up of
         dict keys and sequence indexes. An empty tuple means the root.
       expected (str): description of what the spec expected, such as a
         type name (``'int'``) or a safe rendition of a spec/key.
       actual_type (str): name of the type found at *path*, or
         ``'missing'`` when an expected key was absent.

    The actual target value is deliberately **not** stored, so sensitive
    payloads never leak into a structured report.

    >>> p = MatchProblem(('id',), 'int', 'str')
    >>> p.path
    ('id',)
    >>> p.expected, p.actual_type
    ('int', 'str')
    """
    __slots__ = ('path', 'expected', 'actual_type')

    def __init__(self, path, expected, actual_type):
        self.path = tuple(path)
        self.expected = expected
        self.actual_type = actual_type

    def as_dict(self):
        """Return a plain ``dict`` rendition, handy for serializing reports."""
        return {'path': list(self.path),
                'expected': self.expected,
                'actual_type': self.actual_type}

    def __iter__(self):
        # allow ``path, expected, actual_type = problem`` style unpacking
        yield self.path
        yield self.expected
        yield self.actual_type

    def __eq__(self, other):
        if type(other) is not MatchProblem:
            return NotImplemented
        return (self.path == other.path
                and self.expected == other.expected
                and self.actual_type == other.actual_type)

    def __hash__(self):
        return hash((self.path, self.expected, self.actual_type))

    def __repr__(self):
        return 'MatchProblem(path={0!r}, expected={1!r}, actual_type={2!r})'.format(
            list(self.path), self.expected, self.actual_type)


class MultiMatchError(MatchError):
    """:exc:`MatchError` subtype raised by ``Match(..., multi_error=True)``
    when one or more :class:`MatchProblem` objects were collected.

    The structured details live on :attr:`problems` (an ordered list) and
    :attr:`truncated` (a flag indicating that the problem cap was reached).
    The rendered message summarizes counts and locations without embedding
    actual target values.
    """
    def __init__(self, problems, truncated=False):
        self.problems = list(problems)
        self.truncated = truncated
        super().__init__("match failed with {0} problem(s)", len(self.problems))

    def get_message(self):
        lines = []
        for problem in self.problems:
            location = '.'.join([bbrepr(part) for part in problem.path]) or '<root>'
            lines.append('  - at {0}: expected {1}, got {2}'.format(
                location, problem.expected, problem.actual_type))
        suffix = ''
        if self.truncated:
            suffix = '\n  (problem limit reached; further checks were skipped)'
        return 'match failed with {0} problem(s):\n{1}{2}'.format(
            len(self.problems), '\n'.join(lines), suffix)


class MatchReport:
    """The result of :meth:`Match.collect`.

    Attributes:
       problems (list[MatchProblem]): ordered, structured problems found.
       truncated (bool): ``True`` when *max_problems* was reached and
         traversal was cut short.

    A report is truthy when the target matched without problems.
    """
    __slots__ = ('problems', 'truncated')

    def __init__(self, problems, truncated=False):
        self.problems = list(problems)
        self.truncated = truncated

    @property
    def ok(self):
        """``True`` when no problems were collected."""
        return not self.problems

    def as_dicts(self):
        """Return the problems as a list of plain ``dict`` objects."""
        return [problem.as_dict() for problem in self.problems]

    def __bool__(self):
        return self.ok

    def __iter__(self):
        return iter(self.problems)

    def __len__(self):
        return len(self.problems)

    def __repr__(self):
        return 'MatchReport({0!r}, truncated={1!r})'.format(
            self.problems, self.truncated)


def _describe_expected(spec):
    """Safe, value-free description of what a spec expected."""
    if isinstance(spec, type):
        return spec.__name__
    if isinstance(spec, Or):
        return ' or '.join(_describe_expected(child) for child in spec.children)
    return bbrepr(spec)


def _type_name(value):
    return type(value).__name__


class _MatchCollector:
    """Accumulates :class:`MatchProblem` objects during a multi-error match.

    Also tracks the current target path (dict keys / sequence indexes) and
    provides checkpoints so branching specs (:class:`Or`, list alternative
    patterns) can discard problems from branches that ultimately do not
    apply. Actual values are never retained.
    """
    __slots__ = ('max_problems', 'problems', 'truncated', '_path')

    def __init__(self, max_problems=DEFAULT_MAX_PROBLEMS):
        self.max_problems = max_problems
        self.problems = []
        self.truncated = False
        self._path = []

    def full(self):
        return (self.max_problems is not None
                and len(self.problems) >= self.max_problems)

    def record(self, expected, actual_type):
        if self.full():
            self.truncated = True
            return False
        self.problems.append(
            MatchProblem(tuple(self._path), expected, actual_type))
        if self.full():
            self.truncated = True
        return True

    def record_error(self, spec, error):
        """Convert a raised :class:`GlomError` into a structured problem.

        :class:`TypeMatchError` carries type names directly; every other
        failure is described by its spec, since its free-form message may
        embed actual (sensitive) values.
        """
        if isinstance(error, TypeMatchError):
            # TypeMatchError args = (fmt, expected_type, actual_type)
            expected_type, actual_type = error.args[1], error.args[2]
            return self.record(expected_type.__name__, actual_type.__name__)
        # free-form MatchError messages may contain actual values, so only
        # describe the spec and mark the outcome as a generic mismatch.
        return self.record(_describe_expected(spec), 'mismatch')

    def checkpoint(self):
        return len(self.problems)

    def rollback(self, checkpoint):
        del self.problems[checkpoint:]

    @contextmanager
    def at(self, segment):
        self._path.append(segment)
        try:
            yield
        finally:
            self._path.pop()


@contextmanager
def _without_collector(scope):
    """Run a speculative branch with multi-error collection suspended.

    Restores the default raise-on-first-failure semantics inside the branch,
    so branching specs (Or, list alternatives, dict-key probes) can detect
    failure via exceptions without recording problems. The suspension is
    scoped to this context manager and unwinds cleanly however nested the
    scope becomes.
    """
    depth = scope.get(MATCH_SUSPEND, 0)
    scope[MATCH_SUSPEND] = depth + 1
    try:
        yield
    finally:
        if depth == 0:
            del scope.maps[0][MATCH_SUSPEND]
        else:
            scope.maps[0][MATCH_SUSPEND] = depth


def _collection_active(scope):
    """Return the active collector, or None when collection is suspended."""
    if scope.get(MATCH_SUSPEND, 0):
        return None
    return scope.get(MATCH_COLLECTOR)


class Match:
    """glom's ``Match`` specifier type enables a new mode of glom usage:
    pattern matching.  In particular, this mode has been designed for
    nested data validation.

    Pattern specs are evaluated as follows:

      1. Spec instances are always evaluated first
      2. Types match instances of that type
      3. Instances of :class:`dict`, :class:`list`, :class:`tuple`,
         :class:`set`, and :class:`frozenset` are matched recursively
      4. Any other values are compared for equality to the target with
         ``==``

    By itself, this allows to assert that structures match certain
    patterns, and may be especially familiar to users of the `schema`_
    library.

    For example, let's load some data::

      >>> target = [
      ... {'id': 1, 'email': 'alice@example.com'},
      ... {'id': 2, 'email': 'bob@example.com'}]

    A :class:`Match` pattern can be used to ensure this data is in its expected form:

      >>> spec = Match([{'id': int, 'email': str}])

    This ``spec`` succinctly describes our data structure's pattern
    Specifically, a :class:`list` of :class:`dict` objects, each of
    which has exactly two keys, ``'id'`` and ``'email'``, whose values are
    an :class:`int` and :class:`str`, respectively. Now,
    :func:`~glom.glom` will ensure our ``target`` matches our pattern
    ``spec``:

      >>> result = glom(target, spec)
      >>> assert result == \\
      ... [{'id': 1, 'email': 'alice@example.com'}, {'id': 2, 'email': 'bob@example.com'}]

    With a more complex :class:`Match` spec, we can be more precise:

      >>> spec = Match([{'id': And(M > 0, int), 'email': Regex('[^@]+@[^@]+')}])

    :class:`~glom.And` allows multiple conditions to be applied.
    :class:`~glom.Regex` evaluates the regular expression against the
    target value under the ``'email'`` key.  In this case, we take a
    simple approach: an email has exactly one ``@``, with at least one
    character before and after.

    Finally, :attr:`~glom.M` is our stand-in for the current target
    we're matching against, allowing us to perform in-line comparisons
    using Python's native greater-than operator (as well as
    others). We apply our :class:`Match` pattern as before::

      >>> assert glom(target, spec) == \\
      ... [{'id': 1, 'email': 'alice@example.com'}, {'id': 2, 'email': 'bob@example.com'}]

    And as usual, upon a successful match, we get the matched result.

    .. note::

       For Python 3.6+ where dictionaries are ordered, keys in the target
       are matched against keys in the spec in their insertion order.

    .. _schema: https://github.com/keleshev/schema

    Args:
       spec: The glomspec representing the pattern to match data against.
       default: The default value to be returned if a match fails. If not
         set, a match failure will raise a :class:`MatchError`.
       multi_error: When ``True``, independent sibling fields are all
          checked instead of stopping at the first failure, and a
          :class:`MultiMatchError` (carrying an ordered list of
          :class:`MatchProblem` objects) is raised if anything fails.
          Defaults to ``False``, preserving the original
          first-failure behavior.
       max_problems: Optional cap on the number of problems collected in
          multi-error mode. Once the cap is reached, remaining traversal is
          skipped and the result is flagged as truncated. ``None`` removes
          the cap. Defaults to :data:`DEFAULT_MAX_PROBLEMS`.

    """
    def __init__(self, spec, default=_MISSING, multi_error=False,
                 max_problems=DEFAULT_MAX_PROBLEMS):
        self.spec = spec
        self.default = default
        self.multi_error = multi_error
        self.max_problems = max_problems

    def _run(self, target, scope):
        if not self.multi_error:
            return scope[glom](target, self.spec, scope), None
        collector = _collection_active(scope)
        if collector is None:
            # Write the collector into the current top map (which already
            # carries MODE and friends) rather than adding an empty child
            # map, which would shadow them for _glom's pmap lookups.
            collector = _MatchCollector(max_problems=self.max_problems)
            scope.maps[0][MATCH_COLLECTOR] = collector
        child_scope = scope
        try:
            result = child_scope[glom](target, self.spec, child_scope)
        except GlomError as error:
            # Only record here when nothing deeper handled the failure.
            # Container handlers record their own problems and should not
            # reach this path, but a leaf/scalar failure at the root does.
            if not collector.problems:
                collector.record_error(self.spec, error)
            result = target
        return result, collector

    def glomit(self, target, scope):
        scope[MODE] = _glom_match
        collector = None
        try:
            ret, collector = self._run(target, scope)
        except GlomError:
            if self.default is _MISSING:
                raise
            ret = arg_val(target, self.default, scope)
        if collector is not None and collector.problems:
            if self.default is not _MISSING:
                return arg_val(target, self.default, scope)
            raise MultiMatchError(collector.problems, collector.truncated)
        return ret

    def collect(self, target, max_problems=_MISSING):
        """Run the match in multi-error mode without raising.

        Args:
          target: Target value or data structure to match against.
          max_problems: Optional override for the maximum number of
            :class:`MatchProblem` objects to collect. Once reached, further
            traversal stops and the returned :class:`MatchReport` is marked
            truncated. Pass ``None`` to disable the cap.

        Returns:
          MatchReport: an ordered list of problems plus a truncation flag.
          Actual target values are never included.

        >>> report = Match({'id': int, 'name': str}).collect(
        ...     {'id': 'x', 'name': 1})
        >>> report.ok
        False
        >>> [p.as_dict() for p in report.problems]
        [{'path': ['id'], 'expected': 'int', 'actual_type': 'str'}, {'path': ['name'], 'expected': 'str', 'actual_type': 'int'}]
        """
        cap = self.max_problems if max_problems is _MISSING else max_problems
        spec = Match(self.spec, multi_error=True, max_problems=cap)
        collector = _MatchCollector(max_problems=cap)
        try:
            spec.glomit(target, _new_match_scope(target, collector))
        except MultiMatchError as error:
            return MatchReport(error.problems, error.truncated)
        return MatchReport(collector.problems, collector.truncated)

    def verify(self, target):
        """A convenience function a :class:`Match` instance which returns the
        matched value when *target* matches, or raises a
        :exc:`MatchError` when it does not.

        Args:
          target: Target value or data structure to match against.

        Raises:
          glom.MatchError

        """
        return glom(target, self)

    def matches(self, target):
        """A convenience method on a :class:`Match` instance, returns
        ``True`` if the *target* matches, ``False`` if not.

        >>> Match(int).matches(-1.0)
        False

        Args:
           target: Target value or data structure to match against.
        """
        try:
            glom(target, self)
        except GlomError:
            return False
        return True

    def __repr__(self):
        return f'{self.__class__.__name__}({bbrepr(self.spec)})'


def _new_match_scope(target, collector):
    # Mirror the scope that the top-level glom() builds, but start directly
    # in match mode so the collector is visible from the root. Imported
    # lazily because several of these names are defined later in core.
    from .core import (
        _DEFAULT_SCOPE, Path, Inspect, AUTO, MIN_MODE, CHILD_ERRORS,
        ScopeVars, UP, ROOT)
    scope = _DEFAULT_SCOPE.new_child({
        Path: [],
        Inspect: None,
        MODE: AUTO,
        MIN_MODE: None,
        CHILD_ERRORS: [],
        MATCH_COLLECTOR: collector,
        'globals': ScopeVars({}, {}),
    })
    scope[UP] = scope
    scope[ROOT] = scope
    scope[T] = target
    return scope


_RE_FULLMATCH = getattr(re, "fullmatch", None)
_RE_VALID_FUNCS = {_RE_FULLMATCH, None, re.search, re.match}
_RE_FUNC_ERROR = ValueError("'func' must be one of %s" % (", ".join(
    sorted(e and e.__name__ or "None" for e in _RE_VALID_FUNCS))))

_RE_TYPES = ()
try:   re.match("", "")
except Exception: pass  # pragma: no cover
else:  _RE_TYPES += (str,)
try:   re.match(b"", b"")
except Exception: pass  # pragma: no cover
else:  _RE_TYPES += (bytes,)


class Regex:
    """
    checks that target is a string which matches the passed regex pattern

    raises MatchError if there isn't a match; returns Target if match

    variables captures in regex are added to the scope so they can
    be used by downstream processes
    """
    __slots__ = ('flags', 'func', 'match_func', 'pattern')

    def __init__(self, pattern, flags=0, func=None):
        if func not in _RE_VALID_FUNCS:
            raise _RE_FUNC_ERROR
        regex = re.compile(pattern, flags)
        if func is re.match:
            match_func = regex.match
        elif func is re.search:
            match_func = regex.search
        else:
            if _RE_FULLMATCH:
                match_func = regex.fullmatch
            else:
                regex = re.compile(fr"(?:{pattern})\Z", flags)
                match_func = regex.match
        self.flags, self.func = flags, func
        self.match_func, self.pattern = match_func, pattern

    def glomit(self, target, scope):
        if type(target) not in _RE_TYPES:
            raise MatchError(
                "{0!r} not valid as a Regex target -- expected {1!r}", type(target), _RE_TYPES)
        match = self.match_func(target)
        if not match:
            raise MatchError("target did not match pattern {0!r}", self.pattern)
        scope.update(match.groupdict())
        return target

    def __repr__(self):
        args = '(' + bbrepr(self.pattern)
        if self.flags:
            args += ', flags=' + bbrepr(self.flags)
        if self.func is not None:
            args += ', func=' + self.func.__name__
        args += ')'
        return self.__class__.__name__ + args


#TODO: combine this with other functionality elsewhere?
def _bool_child_repr(child):
    if child is M:
        return repr(child)
    elif isinstance(child, _MExpr):
        return "(" + bbrepr(child) + ")"
    return bbrepr(child)


class _Bool:
    def __init__(self, *children, **kw):
        self.children = children
        if not children:
            raise ValueError("need at least one operand for {}".format(
                self.__class__.__name__))
        self.default = kw.pop('default', _MISSING)
        if kw:
            raise TypeError('got unexpected kwargs: %r' % list(kw.keys()))

    def __and__(self, other):
        return And(self, other)

    def __or__(self, other):
        return Or(self, other)

    def __invert__(self):
        return Not(self)

    def glomit(self, target, scope):
        try:
            return self._glomit(target, scope)
        except GlomError:
            if self.default is not _MISSING:
                return arg_val(target, self.default, scope)
            raise

    def _m_repr(self):
        """should this Or() repr as M |?"""
        # only used by And() and Or(), not Not(), so len(children) >= 1
        if isinstance(self.children[0], (_MType, _MExpr)):
            return True
        if type(self.children[0]) in (And, Or, Not):
            return self.children[0]._m_repr()
        return False

    def __repr__(self):
        child_reprs = [_bool_child_repr(c) for c in self.children]
        if self._m_repr() and self.default is _MISSING:
            return f" {self.OP} ".join(child_reprs)
        if self.default is not _MISSING:
            child_reprs.append("default=" + repr(self.default))
        return self.__class__.__name__ + "(" + ", ".join(child_reprs) + ")"


class And(_Bool):
    """
    Applies child specs one after the other to the target; if none of the
    specs raises `GlomError`, returns the last result.
    """
    OP = "&"
    __slots__ = ('children',)

    def _glomit(self, target, scope):
        # all children must match without exception
        result = target  # so that And() == True, similar to all([]) == True
        collector = _collection_active(scope)
        if collector is not None:
            # And short-circuits like the default raise mode: the first
            # failing child is reported once, remaining children are skipped.
            for child in self.children:
                try:
                    with _without_collector(scope):
                        result = scope[glom](target, child, scope)
                except GlomError:
                    collector.record(_describe_expected(child),
                                     type(target).__name__)
                    return target
            return result
        for child in self.children:
            result = scope[glom](target, child, scope)
        return result

    def __and__(self, other):
        # reduce number of layers of spec
        return And(*(self.children + (other,)))


class Or(_Bool):
    """
    Tries to apply the first child spec to the target, and return the result.
    If `GlomError` is raised, try the next child spec until there are no
    all child specs have been tried, then raise `MatchError`.
    """
    OP = "|"
    __slots__ = ('children',)

    def _glomit(self, target, scope):
        collector = _collection_active(scope)
        if collector is not None:
            # An Or is a single decision at this path: if any child succeeds,
            # the whole Or succeeds and no branch's failures may leak. Probe
            # each child with the collector hidden (restoring raise
            # semantics); if all fail, record one summary problem here.
            successful = False
            for child in self.children:
                try:
                    with _without_collector(scope):
                        result = scope[glom](target, child, scope)
                except GlomError:
                    continue
                successful = True
                break
            if successful:
                return result
            collector.record(_describe_expected(self), _type_name(target))
            return target
        for child in self.children[:-1]:
            try:  # one child must match without exception
                return scope[glom](target, child, scope)
            except GlomError:
                pass
        return scope[glom](target, self.children[-1], scope)

    def __or__(self, other):
        # reduce number of layers of spec
        return Or(*(self.children + (other,)))


class Not(_Bool):
    """
    Inverts the *child*. Child spec will be expected to raise
    :exc:`GlomError` (or subtype), in which case the target will be returned.

    If the child spec does not raise :exc:`GlomError`, :exc:`MatchError`
    will be raised.
    """
    __slots__ = ('child',)

    def __init__(self, child):
        self.child = child

    def glomit(self, target, scope):
        try:  # one child must match without exception
            scope[glom](target, self.child, scope)
        except GlomError:
            return target
        else:
            raise GlomError("child shouldn't have passed", self.child)

    def _m_repr(self):
        if isinstance(self.child, (_MType, _MExpr)):
            return True
        if type(self.child) not in (And, Or, Not):
            return False
        return self.child._m_repr()

    def __repr__(self):
        if self.child is M:
            return '~M'
        if self._m_repr():  # is in M repr
            return "~(" + bbrepr(self.child) + ")"
        return "Not(" + bbrepr(self.child) + ")"


_M_OP_MAP = {'=': '==', '!': '!=', 'g': '>=', 'l': '<='}


class _MSubspec:
    """used by MType.__call__ to wrap a sub-spec for comparison"""
    __slots__ = ('spec')

    def __init__(self, spec):
        self.spec = spec

    def __eq__(self, other):
        return _MExpr(self, '=', other)

    def __ne__(self, other):
        return _MExpr(self, '!', other)

    def __gt__(self, other):
        return _MExpr(self, '>', other)

    def __lt__(self, other):
        return _MExpr(self, '<', other)

    def __ge__(self, other):
        return _MExpr(self, 'g', other)

    def __le__(self, other):
        return _MExpr(self, 'l', other)

    def __repr__(self):
        return f'M({bbrepr(self.spec)})'

    def glomit(self, target, scope):
        match = scope[glom](target, self.spec, scope)
        if match:
            return target
        raise MatchError('expected truthy value from {0!r}, got {1!r}', self.spec, match)


class _MExpr:
    __slots__ = ('lhs', 'op', 'rhs')

    def __init__(self, lhs, op, rhs):
        self.lhs, self.op, self.rhs = lhs, op, rhs

    def __and__(self, other):
        return And(self, other)

    __rand__ = __and__

    def __or__(self, other):
        return Or(self, other)

    def __invert__(self):
        return Not(self)

    def glomit(self, target, scope):
        lhs, op, rhs = self.lhs, self.op, self.rhs
        if lhs is M:
            lhs = target
        if rhs is M:
            rhs = target
        if type(lhs) is _MSubspec:
            lhs = scope[glom](target, lhs.spec, scope)
        if type(rhs) is _MSubspec:
            rhs = scope[glom](target, rhs.spec, scope)
        matched = (
            (op == '=' and lhs == rhs) or
            (op == '!' and lhs != rhs) or
            (op == '>' and lhs > rhs) or
            (op == '<' and lhs < rhs) or
            (op == 'g' and lhs >= rhs) or
            (op == 'l' and lhs <= rhs)
        )
        if matched:
            return target
        raise MatchError("{0!r} {1} {2!r}", lhs, _M_OP_MAP.get(op, op), rhs)

    def __repr__(self):
        op = _M_OP_MAP.get(self.op, self.op)
        return f"{self.lhs!r} {op} {self.rhs!r}"


class _MType:
    """:attr:`~glom.M` is similar to :attr:`~glom.T`, a stand-in for the
    current target, but where :attr:`~glom.T` allows for attribute and
    key access and method calls, :attr:`~glom.M` allows for comparison
    operators.

    If a comparison succeeds, the target is returned unchanged.
    If a comparison fails, :class:`~glom.MatchError` is thrown.

    Some examples:

    >>> glom(1, M > 0)
    1
    >>> glom(0, M == 0)
    0
    >>> glom('a', M != 'b') == 'a'
    True

    :attr:`~glom.M` by itself evaluates the current target for truthiness.
    For example, `M | Val(None)` is a simple idiom for normalizing all falsey values to None:

    >>> from glom import Val
    >>> glom([0, False, "", None], [M | Val(None)])
    [None, None, None, None]

    For convenience, ``&`` and ``|`` operators are overloaded to
    construct :attr:`~glom.And` and :attr:`~glom.Or` instances.

    >>> glom(1.0, (M > 0) & float)
    1.0

    .. note::

       Python's operator overloading may make for concise code,
       but it has its limits.

       Because bitwise operators (``&`` and ``|``) have higher precedence
       than comparison operators (``>``, ``<``, etc.), expressions must
       be parenthesized.

       >>> M > 0 & float
       Traceback (most recent call last):
       ...
       TypeError: unsupported operand type(s) for &: 'int' and 'type'

       Similarly, because of special handling around ternary
       comparisons (``1 < M < 5``) are implemented via
       short-circuiting evaluation, they also cannot be captured by
       :data:`M`.

    """
    __slots__ = ()

    def __call__(self, spec):
        """wrap a sub-spec in order to apply comparison operators to the result"""
        if not isinstance(spec, type(T)):
            # TODO: open this up for other specs so we can do other
            # checks, like function calls
            raise TypeError("M() only accepts T-style specs, not %s" % type(spec).__name__)
        return _MSubspec(spec)

    def __eq__(self, other):
        return _MExpr(self, '=', other)

    def __ne__(self, other):
        return _MExpr(self, '!', other)

    def __gt__(self, other):
        return _MExpr(self, '>', other)

    def __lt__(self, other):
        return _MExpr(self, '<', other)

    def __ge__(self, other):
        return _MExpr(self, 'g', other)

    def __le__(self, other):
        return _MExpr(self, 'l', other)

    def __and__(self, other):
        return And(self, other)

    __rand__ = __and__

    def __or__(self, other):
        return Or(self, other)

    def __invert__(self):
        return Not(self)

    def __repr__(self):
        return "M"

    def glomit(self, target, spec):
        if target:
            return target
        raise MatchError("{0!r} not truthy", target)


M = _MType()



class Optional:
    """Used as a :class:`dict` key in a :class:`~glom.Match()` spec,
    marks that a value match key which would otherwise be required is
    optional and should not raise :exc:`~glom.MatchError` even if no
    keys match.

    For example::

      >>> spec = Match({Optional("name"): str})
      >>> glom({"name": "alice"}, spec)
      {'name': 'alice'}
      >>> glom({}, spec)
      {}
      >>> spec = Match({Optional("name", default=""): str})
      >>> glom({}, spec)
      {'name': ''}

    """
    __slots__ = ('key', 'default')

    def __init__(self, key, default=_MISSING):
        if type(key) in (Required, Optional):
            raise TypeError("double wrapping of Optional")
        hash(key)  # ensure is a valid key
        if _precedence(key) != 0:
            raise ValueError(f"Optional() keys must be == match constants, not {key!r}")
        self.key, self.default = key, default

    def glomit(self, target, scope):
        if target != self.key:
            raise MatchError("target {0} != spec {1}", target, self.key)
        return target

    def __repr__(self):
        return f'{self.__class__.__name__}({bbrepr(self.key)})'


class Required:
    """Used as a :class:`dict` key in :class:`~glom.Match()` mode, marks
    that a key which might otherwise not be required should raise
    :exc:`~glom.MatchError` if the key in the target does not match.

    For example::

      >>> spec = Match({object: object})

    This spec will match any dict, because :class:`object` is the base
    type of every object::

      >>> glom({}, spec)
      {}

    ``{}`` will also match because match mode does not require at
    least one match by default. If we want to require that a key
    matches, we can use :class:`~glom.Required`::

      >>> spec = Match({Required(object): object})
      >>> glom({}, spec)
      Traceback (most recent call last):
      ...
      MatchError: error raised while processing.
       Target-spec trace, with error detail (most recent last):
       - Target: {}
       - Spec: Match({Required(object): <type 'object'>})
       - Spec: {Required(object): <type 'object'>}
      MatchError: target missing expected keys Required(object)

    Now our spec requires at least one key of any type. You can refine
    the spec by putting more specific subpatterns inside of
    :class:`~glom.Required`.

    """
    __slots__ = ('key',)

    def __init__(self, key):
        if type(key) in (Required, Optional):
            raise TypeError("double wrapping of Required")
        hash(key)  # ensure is a valid key
        if _precedence(key) == 0:
            raise ValueError("== match constants are already required: " + bbrepr(key))
        self.key = key

    def __repr__(self):
        return f'{self.__class__.__name__}({bbrepr(self.key)})'


def _precedence(match):
    """
    in a dict spec, target-keys may match many
    spec-keys (e.g. 1 will match int, M > 0, and 1);
    therefore we need a precedence for which order to try
    keys in; higher = later
    """
    if type(match) in (Required, Optional):
        match = match.key
    if type(match) in (tuple, frozenset):
        if not match:
            return 0
        return max([_precedence(item) for item in match])
    if isinstance(match, type):
        return 2
    if hasattr(match, "glomit"):
        return 1
    return 0  # == match


def _handle_dict(target, spec, scope):
    collector = _collection_active(scope)
    if not isinstance(target, dict):
        if collector is not None:
            # Record the parent type problem and stop descending: inventing
            # missing-child errors for a non-dict parent would be misleading.
            collector.record('dict', type(target).__name__)
            return target
        raise TypeMatchError(type(target), dict)
    spec_keys = spec  # cheating a little bit here, list-vs-dict, but saves an object copy sometimes

    required = {
        key for key in spec_keys
        if _precedence(key) == 0 and type(key) is not Optional
        or type(key) is Required}
    defaults = {  # pre-load result with defaults
        key.key: key.default for key in spec_keys
        if type(key) is Optional and key.default is not _MISSING}
    result = {}
    if collector is not None:
        return _handle_dict_collect(
            target, spec, scope, spec_keys, required, defaults, result, collector)
    for key, val in target.items():
        for maybe_spec_key in spec_keys:
            # handle Required as a special case here rather than letting it be a stand-alone spec
            if type(maybe_spec_key) is Required:
                spec_key = maybe_spec_key.key
            else:
                spec_key = maybe_spec_key
            try:
                key = scope[glom](key, spec_key, scope)
            except GlomError:
                pass
            else:
                result[key] = scope[glom](val, spec[maybe_spec_key], chain_child(scope))
                required.discard(maybe_spec_key)
                break
        else:
            raise MatchError("key {0!r} didn't match any of {1!r}", key, spec_keys)
    for key in set(defaults) - set(result):
        result[key] = arg_val(target, defaults[key], scope)
    if required:
        raise MatchError("target missing expected keys: {0}", ', '.join([bbrepr(r) for r in required]))
    return result


def _handle_dict_collect(target, spec, scope, spec_keys, required,
                         defaults, result, collector):
    if collector.full():
        collector.truncated = True
        return result
    # Each target key and each required spec key is an independent sibling,
    # so failures are recorded and matching continues with the next sibling.
    for key, val in target.items():
        if collector.full():
            collector.truncated = True
            break
        matched = False
        for maybe_spec_key in spec_keys:
            spec_key = maybe_spec_key.key if type(maybe_spec_key) is Required else maybe_spec_key
            try:
                with _without_collector(scope):
                    matched_key = scope[glom](key, spec_key, scope)
            except GlomError:
                continue
            with collector.at(matched_key):
                try:
                    matched_val = scope[glom](
                        val, spec[maybe_spec_key], scope)
                except GlomError as error:
                    collector.record_error(spec[maybe_spec_key], error)
                    result[matched_key] = val
                else:
                    result[matched_key] = matched_val
            required.discard(maybe_spec_key)
            matched = True
            break
        if not matched:
            collector.record('one of ' + bbrepr(list(spec_keys)), type(key).__name__)
    if not collector.full():
        for key in set(defaults) - set(result):
            result[key] = arg_val(target, defaults[key], scope)
    if not collector.full():
        for missing in spec_keys:
            if missing not in required:
                continue
            key = missing.key if type(missing) is Required else missing
            with collector.at(key):
                collector.record(_describe_expected(key), 'missing')
            if collector.full():
                collector.truncated = True
                break
    return result


def _glom_match(target, spec, scope):
    collector = _collection_active(scope)
    if isinstance(spec, type):
        if not isinstance(target, spec):
            if collector is not None:
                collector.record(spec.__name__, type(target).__name__)
                return target
            raise TypeMatchError(type(target), spec)
    elif isinstance(spec, dict):
        return _handle_dict(target, spec, scope)
    elif isinstance(spec, (list, set, frozenset)):
        if not isinstance(target, type(spec)):
            if collector is not None:
                collector.record(type(spec).__name__, type(target).__name__)
                return target
            raise TypeMatchError(type(target), type(spec))
        result = []
        if collector is not None:
            for index, item in enumerate(target):
                if collector.full():
                    collector.truncated = True
                    break
                with collector.at(index):
                    matched = False
                    children = list(spec)
                    successful_child = None
                    for child in children:
                        try:
                            with _without_collector(scope):
                                scope[glom](item, child, scope)
                            successful_child = child
                            break
                        except GlomError:
                            pass
                    if successful_child is not None:
                        # Re-run the clean effective branch with collection
                        # active so its nested problems still get reported.
                        result.append(scope[glom](item, successful_child, scope))
                        matched = True
                    if not matched:
                        if not spec:
                            collector.record(
                                'non-empty ' + type(spec).__name__, type(item).__name__)
                        else:
                            # Every candidate failed under raise semantics.
                            # For a single structural (container) candidate,
                            # re-run with collection active so its internal
                            # problems are reported; otherwise emit one
                            # summary without leaking each alternative.
                            first_child = children[0]
                            if len(children) == 1 and isinstance(
                                    first_child, (dict, list, tuple,
                                                  set, frozenset)):
                                before = collector.checkpoint()
                                try:
                                    result.append(scope[glom](
                                        item, first_child, scope))
                                except GlomError as error:
                                    collector.rollback(before)
                                    collector.record_error(first_child, error)
                                if collector.checkpoint() == before:
                                    result.append(item)
                                    collector.record(
                                        'one of ' + bbrepr(children),
                                        type(item).__name__)
                            else:
                                result.append(item)
                                collector.record(
                                    'one of ' + bbrepr(children),
                                    type(item).__name__)
        else:
            for item in target:
                for child in spec:
                    try:
                        result.append(scope[glom](item, child, scope))
                        break
                    except GlomError as e:
                        last_error = e
                else:  # did not break, something went wrong
                    if target and not spec:
                        raise MatchError(
                            "{0!r} does not match empty {1}", target, type(spec).__name__)
                    # NOTE: unless error happens above, break will skip else branch
                    # so last_error will have been assigned
                    raise last_error
        if type(spec) is not list:
            return type(spec)(result)
        return result
    elif isinstance(spec, tuple):
        if not isinstance(target, tuple):
            if collector is not None:
                collector.record('tuple', type(target).__name__)
                return target
            raise TypeMatchError(type(target), tuple)
        if len(target) != len(spec):
            if collector is not None:
                collector.record(
                    'tuple of length %d' % len(spec),
                    'tuple of length %d' % len(target))
                return target
            raise MatchError("{0!r} does not match {1!r}", target, spec)
        result = []
        if collector is not None:
            for index, (sub_target, sub_spec) in enumerate(zip(target, spec)):
                if collector.full():
                    collector.truncated = True
                    break
                with collector.at(index):
                    try:
                        result.append(scope[glom](sub_target, sub_spec, scope))
                    except GlomError as error:
                        collector.record_error(sub_spec, error)
                        result.append(sub_target)
        else:
            for sub_target, sub_spec in zip(target, spec):
                result.append(scope[glom](sub_target, sub_spec, scope))
        return tuple(result)
    elif callable(spec):
        try:
            if spec(target):
                return target
        except Exception as e:
            if collector is not None:
                collector.record(_describe_expected(spec), type(target).__name__)
                return target
            raise MatchError(
                "{0}({1!r}) did not validate (got exception {2!r})", spec.__name__, target, e)
        if collector is not None:
            collector.record(_describe_expected(spec), type(target).__name__)
            return target
        raise MatchError(
            "{0}({1!r}) did not validate (non truthy return)", spec.__name__, target)
    elif target != spec:
        if collector is not None:
            collector.record(_describe_expected(spec), type(target).__name__)
            return target
        raise MatchError("{0!r} does not match {1!r}", target, spec)
    return target


class Switch:
    r"""The :class:`Switch` specifier type routes data processing based on
    matching keys, much like the classic switch statement.

    Here is a spec which differentiates between lowercase English
    vowel and consonant characters:

      >>> switch_spec = Match(Switch([(Or('a', 'e', 'i', 'o', 'u'), Val('vowel')),
      ...                             (And(str, M, M(T[2:]) == ''), Val('consonant'))]))

    The constructor accepts a :class:`dict` of ``{keyspec: valspec}``
    or a list of items, ``[(keyspec, valspec)]``. Keys are tried
    against the current target in order. If a keyspec raises
    :class:`GlomError`, the next keyspec is tried.  Once a keyspec
    succeeds, the corresponding valspec is evaluated and returned.
    Let's try it out:

      >>> glom('a', switch_spec)
      'vowel'
      >>> glom('z', switch_spec)
      'consonant'

    If no keyspec succeeds, a :class:`MatchError` is raised. Our spec
    only works on characters (strings of length 1). Let's try a
    non-character, the integer ``3``:

      >>> glom(3, switch_spec)
      Traceback (most recent call last):
      ...
      glom.matching.MatchError: error raised while processing, details below.
       Target-spec trace (most recent last):
       - Target: 3
       - Spec: Match(Switch([(Or('a', 'e', 'i', 'o', 'u'), Val('vowel')), (And(str, M, (M(T[2:]) == '')), Val('...
       + Spec: Switch([(Or('a', 'e', 'i', 'o', 'u'), Val('vowel')), (And(str, M, (M(T[2:]) == '')), Val('conson...
       |\ Spec: Or('a', 'e', 'i', 'o', 'u')
       ||\ Spec: 'a'
       ||X glom.matching.MatchError: 3 does not match 'a'
       ||\ Spec: 'e'
       ||X glom.matching.MatchError: 3 does not match 'e'
       ||\ Spec: 'i'
       ||X glom.matching.MatchError: 3 does not match 'i'
       ||\ Spec: 'o'
       ||X glom.matching.MatchError: 3 does not match 'o'
       ||\ Spec: 'u'
       ||X glom.matching.MatchError: 3 does not match 'u'
       |X glom.matching.MatchError: 3 does not match 'u'
       |\ Spec: And(str, M, (M(T[2:]) == ''))
       || Spec: str
       |X glom.matching.TypeMatchError: expected type str, not int
      glom.matching.MatchError: no matches for target in Switch


    .. note::

       :class:`~glom.Switch` is one of several *branching* specifier
       types in glom. See ":ref:`branched-exceptions`" for details on
       interpreting its exception messages.

    A *default* value can be passed to the spec to be returned instead
    of raising a :class:`MatchError`.

    .. note::

      Switch implements control flow similar to the switch statement
      proposed in `PEP622 <https://www.python.org/dev/peps/pep-0622/>`_.

    """
    def __init__(self, cases, default=_MISSING):
        if type(cases) is dict:
            cases = list(cases.items())
        if type(cases) is not list:
            raise TypeError(
                "expected cases argument to be of format {{keyspec: valspec}}"
                " or [(keyspec, valspec)] not: {}".format(type(cases)))
        self.cases = cases
        # glom.match(cases, Or([(object, object)], dict))
        # start dogfooding ^
        self.default = default
        if not cases:
            raise ValueError('expected at least one case in %s, got: %r'
                             % (self.__class__.__name__, self.cases))
        return


    def glomit(self, target, scope):
        collector = _collection_active(scope)
        for keyspec, valspec in self.cases:
            try:
                if collector is not None:
                    with _without_collector(scope):
                        scope[glom](target, keyspec, scope)
                else:
                    scope[glom](target, keyspec, scope)
            except GlomError as ge:
                continue
            return scope[glom](target, valspec, chain_child(scope))
        if self.default is not _MISSING:
            return arg_val(target, self.default, scope)
        if collector is not None:
            collector.record(_describe_expected(self), type(target).__name__)
            return target
        raise MatchError("no matches for target in %s"  % self.__class__.__name__)

    def __repr__(self):
        return f'{self.__class__.__name__}({bbrepr(self.cases)})'


RAISE = make_sentinel('RAISE')  # flag object for "raise on check failure"


class Check:
    """Check objects are used to make assertions about the target data,
    and either pass through the data or raise exceptions if there is a
    problem.

    If any check condition fails, a :class:`~glom.CheckError` is raised.

    Args:

       spec: a sub-spec to extract the data to which other assertions will
          be checked (defaults to applying checks to the target itself)
       type: a type or sequence of types to be checked for exact match
       equal_to: a value to be checked for equality match ("==")
       validate: a callable or list of callables, each representing a
          check condition. If one or more return False or raise an
          exception, the Check will fail.
       instance_of: a type or sequence of types to be checked with isinstance()
       one_of: an iterable of values, any of which can match the target ("in")
       default: an optional default value to replace the value when the check fails
                (if default is not specified, GlomCheckError will be raised)

    Aside from *spec*, all arguments are keyword arguments. Each
    argument, except for *default*, represent a check
    condition. Multiple checks can be passed, and if all check
    conditions are left unset, Check defaults to performing a basic
    truthy check on the value.

    """
    # TODO: the next level of Check would be to play with the Scope to
    # allow checking to continue across the same level of
    # dictionary. Basically, collect as many errors as possible before
    # raising the unified CheckError.
    def __init__(self, spec=T, **kwargs):
        self.spec = spec
        self._orig_kwargs = dict(kwargs)
        self.default = kwargs.pop('default', RAISE)

        def _get_arg_val(name, cond, func, val, can_be_empty=True):
            if val is _MISSING:
                return ()
            if not is_iterable(val):
                val = (val,)
            elif not val and not can_be_empty:
                raise ValueError('expected %r argument to contain at least one value,'
                                 ' not: %r' % (name, val))
            for v in val:
                if not func(v):
                    raise ValueError('expected %r argument to be %s, not: %r'
                                     % (name, cond, v))
            return val

        # if there are other common validation functions, maybe a
        # small set of special strings would work as valid arguments
        # to validate, too.
        def truthy(val):
            return bool(val)

        validate = kwargs.pop('validate', _MISSING if kwargs else truthy)
        type_arg = kwargs.pop('type', _MISSING)
        instance_of = kwargs.pop('instance_of', _MISSING)
        equal_to = kwargs.pop('equal_to', _MISSING)
        one_of = kwargs.pop('one_of', _MISSING)
        if kwargs:
            raise TypeError('unexpected keyword arguments: %r' % kwargs.keys())

        self.validators = _get_arg_val('validate', 'callable', callable, validate)
        self.instance_of = _get_arg_val('instance_of', 'a type',
                                        lambda x: isinstance(x, type), instance_of, False)
        self.types = _get_arg_val('type', 'a type',
                                  lambda x: isinstance(x, type), type_arg, False)

        if equal_to is not _MISSING:
            self.vals = (equal_to,)
            if one_of is not _MISSING:
                raise TypeError('expected "one_of" argument to be unset when'
                                ' "equal_to" argument is passed')
        elif one_of is not _MISSING:
            if not is_iterable(one_of):
                raise ValueError('expected "one_of" argument to be iterable'
                                 ' , not: %r' % one_of)
            if not one_of:
                raise ValueError('expected "one_of" to contain at least'
                                 ' one value, not: %r' % (one_of,))
            self.vals = one_of
        else:
            self.vals = ()
        return

    class _ValidationError(Exception):
        "for internal use inside of Check only"
        pass

    def glomit(self, target, scope):
        ret = target
        errs = []
        if self.spec is not T:
            target = scope[glom](target, self.spec, scope)
        if self.types and type(target) not in self.types:
            if self.default is not RAISE:
                return arg_val(target, self.default, scope)
            errs.append('expected type to be %r, found type %r' %
                        (self.types[0].__name__ if len(self.types) == 1
                         else tuple([t.__name__ for t in self.types]),
                         type(target).__name__))

        if self.vals and target not in self.vals:
            if self.default is not RAISE:
                return arg_val(target, self.default, scope)
            if len(self.vals) == 1:
                errs.append(f"expected {self.vals[0]}, found {target}")
            else:
                errs.append(f'expected one of {self.vals}, found {target}')

        if self.validators:
            for i, validator in enumerate(self.validators):
                try:
                    res = validator(target)
                    if res is False:
                        raise self._ValidationError
                except Exception as e:
                    msg = ('expected %r check to validate target'
                           % getattr(validator, '__name__', None) or ('#%s' % i))
                    if type(e) is self._ValidationError:
                        if self.default is not RAISE:
                            return self.default
                    else:
                        msg += ' (got exception: %r)' % e
                    errs.append(msg)

        if self.instance_of and not isinstance(target, self.instance_of):
            # TODO: can these early returns be done without so much copy-paste?
            # (early return to avoid potentially expensive or even error-causeing
            # string formats)
            if self.default is not RAISE:
                return arg_val(target, self.default, scope)
            errs.append('expected instance of %r, found instance of %r' %
                        (self.instance_of[0].__name__ if len(self.instance_of) == 1
                         else tuple([t.__name__ for t in self.instance_of]),
                         type(target).__name__))

        if errs:
            raise CheckError(errs, self, scope[Path])
        return ret

    def __repr__(self):
        cn = self.__class__.__name__
        posargs = (self.spec,) if self.spec is not T else ()
        return format_invocation(cn, posargs, self._orig_kwargs, repr=bbrepr)


class CheckError(GlomError):
    """This :exc:`GlomError` subtype is raised when target data fails to
    pass a :class:`Check`'s specified validation.

    An uncaught ``CheckError`` looks like this::

       >>> target = {'a': {'b': 'c'}}
       >>> glom(target, {'b': ('a.b', Check(type=int))})
       Traceback (most recent call last):
       ...
       CheckError: target at path ['a.b'] failed check, got error: "expected type to be 'int', found type 'str'"

    If the ``Check`` contains more than one condition, there may be
    more than one error message. The string rendition of the
    ``CheckError`` will include all messages.

    You can also catch the ``CheckError`` and programmatically access
    messages through the ``msgs`` attribute on the ``CheckError``
    instance.

    """
    def __init__(self, msgs, check, path):
        self.msgs = msgs
        self.check_obj = check
        self.path = path

    def get_message(self):
        msg = 'target at path %s failed check,' % self.path
        if self.check_obj.spec is not T:
            msg += f' subtarget at {self.check_obj.spec!r}'
        if len(self.msgs) == 1:
            msg += f' got error: {self.msgs[0]!r}'
        else:
            msg += f' got {len(self.msgs)} errors: {self.msgs!r}'
        return msg

    def __repr__(self):
        cn = self.__class__.__name__
        return f'{cn}({self.msgs!r}, {self.check_obj!r}, {self.path!r})'
