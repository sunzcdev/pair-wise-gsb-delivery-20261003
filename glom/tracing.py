"""Explicit provenance tracking for :func:`glom.glom` evaluations.

The :func:`trace` entrypoint evaluates a glom spec just like
:func:`glom.glom`, but additionally returns a *source map* describing
where every value in the output came from:

* :class:`PathSource` -- a value read from a concrete path on the
  input target,
* :class:`LiteralSource` -- a literal value embedded in the spec
  (e.g. :class:`~glom.Val`), and
* :class:`SyntheticSource` -- a value produced by a computation, such
  as a callable, method call, arithmetic, or any other opaque
  specifier type. A synthetic source records the sources it actually
  depended on, without guessing at internal field accesses.

Tracing is a fully opt-in feature: importing this module or calling
:func:`trace` never changes the behavior, return value, or hot path of
ordinary :func:`glom.glom` calls.
"""

from boltons.typeutils import make_sentinel

from glom.core import (AUTO,
                       FILL,
                       CHILD_ERRORS,
                       LAST_CHILD_SCOPE,
                       MODE,
                       MIN_MODE,
                       ROOT,
                       SKIP,
                       STOP,
                       UP,
                       Coalesce,
                       CoalesceError,
                       GlomError,
                       Inspect,
                       PathAccessError,
                       Path,
                       ScopeVars,
                       S,
                      Spec,
                       T,
                       TType,
                       TargetRegistry,
                       Val,
                       _DEFAULT_SCOPE,
                       _MISSING,
                       _ArgValuator,
                       _glom,
                       _has_callable_glomit,
                      bbrepr,
                      chain_child,
                      glom)


class Source:
    """Base class for provenance markers returned by :func:`trace`.

    Every source knows the :class:`~glom.Path` (relative to the input
    target) at which it was emitted and the value that was produced
    there.
    """
    def __init__(self, path, value):
        self.path = tuple(path)
        self.value = value

    def __repr__(self):
        return '%s(%r, %s)' % (type(self).__name__, self.path, bbrepr(self.value))

    def __eq__(self, other):
        if type(self) is not type(other):
            return NotImplemented
        return self.__dict__ == other.__dict__

    def __ne__(self, other):
        result = self.__eq__(other)
        if result is NotImplemented:
            return result
        return not result

    __hash__ = None


class PathSource(Source):
    """A value copied directly from a concrete input path.

    Attributes:
       path: the output path at which the value appears.
       value: the value read from the input.
       input_path: a :class:`~glom.Path` rooted at the input target
         (``T``) that was accessed to obtain *value*.
    """
    def __init__(self, path, value, input_path):
        super().__init__(path, value)
        self.input_path = input_path

    def __repr__(self):
        return 'PathSource(%r, %s, input_path=%r)' % (
            self.path, bbrepr(self.value), self.input_path)


class LiteralSource(Source):
    """A value that came from a literal embedded in the spec rather than
    from the input target (e.g. :class:`~glom.Val('x') <glom.Val>`).
    """


class SyntheticSource(Source):
    """A value computed by an opaque step (a callable, arithmetic, a
    method call, a custom specifier type, ...).

    Attributes:
       path: the output path at which the value appears.
       value: the computed value.
       inputs: tuple of :class:`Source` objects that the computation
         is known to have depended on.
       origin: short label identifying the kind of computation
         (e.g. ``'callable'``, ``'T expression'``, the repr of a
         custom specifier type).
    """
    def __init__(self, path, value, inputs=(), origin='synthetic'):
        super().__init__(path, value)
        self.inputs = tuple(inputs)
        self.origin = origin

    def __repr__(self):
        return 'SyntheticSource(%r, %s, origin=%r, inputs=%r)' % (
            self.path, bbrepr(self.value), self.origin, list(self.inputs))


class TraceResult:
    """The result of a :func:`trace` evaluation.

    Attributes:
       value: the normal glom return value (identical to what
         :func:`glom.glom` would return). On failure this is a
         best-effort partial result, or the sentinel :data:`INCOMPLETE`.
       sources: an ordered mapping of output path tuples to
         :class:`Source` instances. Only paths that actually made it
         into the output are present.
       complete: ``True`` when the whole spec evaluated successfully,
         ``False`` when an error cut the evaluation short.
       error: the underlying :class:`~glom.GlomError` when
         *complete* is ``False``, otherwise ``None``.

    ``TraceResult`` supports ``result[path_tuple]`` lookup and direct
    iteration over its ``(path, source)`` entries.
    """
    def __init__(self, value, sources, error=None):
        self.value = value
        self.sources = dict(sources)
        self.error = error
        self.complete = error is None

    def get_source(self, *path):
        """Return the :class:`Source` recorded at *path* (given as separate
        key/index arguments), or ``None`` if the path was not emitted."""
        return self.sources.get(tuple(path))

    def __getitem__(self, path):
        if not isinstance(path, tuple):
            path = (path,)
        return self.sources[path]

    def __iter__(self):
        return iter(self.sources.items())

    def __len__(self):
        return len(self.sources)

    def raise_on_error(self):
        """Re-raise the underlying error if the trace was incomplete;
        return ``self`` otherwise (for fluent chaining)."""
        if self.error is not None:
            raise self.error
        return self

    def __repr__(self):
        state = 'complete' if self.complete else 'incomplete'
        return 'TraceResult(%s, %d sources)' % (state, len(self.sources))

    def __eq__(self, other):
        if type(other) is not TraceResult:
            return NotImplemented
        return (self.value == other.value
                and self.sources == other.sources
                and self.complete == other.complete)

    def __ne__(self, other):
        result = self.__eq__(other)
        if result is NotImplemented:
            return result
        return not result

    __hash__ = None


INCOMPLETE = make_sentinel('INCOMPLETE')
INCOMPLETE.__doc__ = """
Sentinel stored on :attr:`TraceResult.value` when an error prevented
even a partial output value from being assembled.
"""

def _dedup(items):
    seen = []
    for item in items:
        if item not in seen:
            seen.append(item)
    return seen


class _Tracer:
    """Runs a glom evaluation through a tracing version of the
    ``scope[glom]`` entrypoint while building the source map."""

    def __init__(self, scope):
        self.scope = scope
        self.scope[glom] = self._intercept
        self.entries = []
        # stack of (output_path, target_source, collect_inputs or None)
        self.frames = [([], PathSource([], None, Path(T)), None)]
        self.argval_depth = 0
        self.suppress_literals = False
        self._current_scope = scope

    # -- frame / entry bookkeeping -------------------------------------

    @property
    def frame(self):
        return self.frames[-1]

    def _push(self, out_path, target_source, collect):
        self.frames.append((list(out_path), target_source, [] if collect else None))

    def _pop(self):
        return self.frames.pop()

    def _emit(self, source):
        self.entries.append((tuple(source.path), source))
        frame = self.frame
        if frame[2] is not None:
            frame[2].append(source)

    def _snapshot(self):
        return len(self.entries)

    def _rollback(self, snapshot):
        del self.entries[snapshot:]

    def _drop_prefix(self, prefix_tuple):
        self.entries = [(path, source) for (path, source) in self.entries
                        if not path[:len(prefix_tuple)] == prefix_tuple]

    # -- the tracing glom entrypoint ------------------------------------

    def _intercept(self, target, spec, scope=None):
        # While an opaque step is being collected, nested scope[glom]
        # calls fall straight through to the real evaluator; each still
        # gets its own collection frame so its source is recorded once,
        # as a dependency of the opaque step.
        if scope is None:
            scope = self._current_scope
        self._current_scope = scope
        if scope.get(MIN_MODE) is not None and not self.argval_depth:
            self.argval_depth += 1
            try:
                return self._intercept(target, spec, scope)
            finally:
                self.argval_depth -= 1
        if self.frame[2] is not None:
            # opaque internals: evaluate for real and fold the source
            # into the active collection frame instead of re-driving
            # foreign modes (Match, Group, ...) through tracing
            value, source = self._eval_opaque_child(target, spec, scope)
            if source is not None:
                self.frame[2].append(source)
            return value
        value, _ = self._call(target, spec, scope)
        return value

    def _eval_opaque_child(self, target, spec, scope):
        out_path, target_source, _ = self.frame
        mode = (_ArgValuator().mode if self.argval_depth
                else scope.get(MODE, AUTO))
        if (self.suppress_literals and self.argval_depth
                and not self._is_spec_like(spec)):
            # path keys / constants inside a native T expression:
            # they are not standalone output values or input reads
            return spec, None
        if mode is AUTO or mode is FILL or self.argval_depth:
            return self._eval(target, spec, scope)
        value = _glom(target, spec, scope)
        return value, SyntheticSource(out_path, value, [target_source],
                                      '%s mode' % getattr(mode, '__name__', mode))

    @staticmethod
    def _is_spec_like(spec):
        return (isinstance(spec, TType)
                or _has_callable_glomit(spec)
                or callable(spec)
                or type(spec) in (dict, list, tuple, set, frozenset))

    def _collect(self, target, spec, scope):
        out_path, target_source, _ = self.frame
        self._push(out_path, target_source, True)
        try:
            if scope.get(MIN_MODE) is not None:
                self.argval_depth += 1
                try:
                    value, source = self._eval(target, spec, scope)
                finally:
                    self.argval_depth -= 1
            else:
                value, source = self._eval(target, spec, scope)
        finally:
            _, _, collected = self._pop()
        collected.append(source)
        return value

    def _call(self, target, spec, scope):
        out_path, target_source, collect = self.frame
        self._push(out_path, target_source, collect is not None)
        try:
            value, source = self._eval(target, spec, scope)
        finally:
            self._pop()
        if collect is None and value is not SKIP and value is not STOP:
            self._emit(source)
        return value, source

    def _opaque(self, target, spec, scope, origin, inputs=None):
        """Evaluate *spec* with the real (non-tracing) evaluator; nested
        ``scope[glom]`` calls land in collection mode so their sources
        become recorded dependencies rather than guessed internals."""
        out_path, target_source, _ = self.frame
        self._push(out_path, target_source, True)
        # keep the tracing intercept active (spec types like Match swap
        # MODE and drive their own nested glom calls), but every nested
        # call sees the collection frame above and is folded into inputs
        try:
            value = _glom(target, spec, scope)
        finally:
            _, _, collected = self._pop()
        all_inputs = list(collected)
        if inputs:
            all_inputs.extend(inputs)
        return value, SyntheticSource(out_path, value, _dedup(all_inputs), origin)

    # -- spec dispatch --------------------------------------------------

    def _eval(self, target, spec, scope):
        out_path, target_source, collect = self.frame
        min_mode = _ArgValuator().mode if self.argval_depth else None
        mode = min_mode or scope.get(MODE, AUTO)

        if type(spec) is TType:
            return self._eval_t(target, spec, scope, target_source)
        if type(spec) is Path:
            return self._eval_t(target, spec.path_t, scope, target_source)
        if isinstance(spec, Val):
            return spec.value, LiteralSource(out_path, spec.value)
        if isinstance(spec, Coalesce):
            return self._eval_coalesce(target, spec, scope)

        if min_mode is not None:
            # _ArgValuator: literals and literal structures inside arguments
            if type(spec) in (dict, list, tuple, set, frozenset):
                return self._eval_arg_container(target, spec, scope)
            return spec, LiteralSource(out_path, spec)

        if type(spec) is str and mode is AUTO:
            return self._eval_t(target, Path.from_text(spec).path_t,
                                scope, target_source)

        if mode is AUTO:
            if isinstance(spec, dict):
                return self._eval_dict(target, spec, scope)
            if isinstance(spec, list):
                return self._eval_list(target, spec, scope)
            if isinstance(spec, tuple):
                return self._eval_tuple(target, spec, scope)
            if callable(spec):
                return self._opaque(target, spec, scope,
                                    'callable %s' % bbrepr(spec),
                                    inputs=[target_source])
        elif mode is FILL:
            if type(spec) in (dict, list, tuple, set, frozenset):
                return self._eval_fill_container(target, spec, scope)
            if callable(spec):
                return self._opaque(target, spec, scope,
                                    'callable %s' % bbrepr(spec),
                                    inputs=[target_source])
            return target if spec is T else spec, LiteralSource(out_path, spec)
        elif mode is not AUTO:
            # foreign dialects (Match, Group, ...) drive their own
            # semantics through MODE: treat the subtree opaquely
            return self._opaque(target, spec, scope,
                                '%s mode' % getattr(mode, '__name__', mode))

        if _has_callable_glomit(spec):
            return self._opaque(target, spec, scope,
                                '%s spec' % type(spec).__name__)
        return spec, LiteralSource(out_path, spec)

    # -- structural handlers -------------------------------------------

    def _eval_dict(self, target, spec, scope):
        out_path, target_source, _ = self.frame
        result = type(spec)()
        container = SyntheticSource(out_path, result, [target_source], 'dict')
        self._emit(container)
        for field, subspec in spec.items():
            key = field
            if type(field) in (Spec, TType):
                key = _glom(target, field, scope)
            field_path = tuple(out_path) + (key,)
            self._push(field_path, target_source, False)
            try:
                val, source = self._eval(target, subspec, scope)
            except Exception:
                self._pop()
                self._drop_prefix(field_path)
                raise
            else:
                self._pop()
            if val is SKIP:
                self._drop_prefix(field_path)
                continue
            self._emit(source)
            result[key] = val
        return result, container

    def _eval_list(self, target, spec, scope):
        out_path, target_source, _ = self.frame
        subspec = spec[0]
        iterate = scope[TargetRegistry].get_handler(
            'iterate', target, path=Path(*scope[Path]))
        try:
            iterator = iterate(target)
        except Exception as e:
            raise TypeError(
                'failed to iterate on instance of type %r at %r (got %r)'
                % (target.__class__.__name__, Path(*scope[Path]), e))
        result = []
        container = SyntheticSource(out_path, result, [target_source], 'list')
        self._emit(container)
        base_path = scope[Path]
        for index, item in enumerate(iterator):
            scope[Path] = base_path + [index]
            item_source = self._index_target(target_source, index)
            item_path = tuple(out_path) + (len(result),)
            snapshot = self._snapshot()
            self._push(item_path, item_source, False)
            try:
                val, source = self._eval(item, subspec, scope)
            except Exception:
                self._pop()
                self._rollback(snapshot)
                raise
            else:
                self._pop()
            if val is SKIP:
                self._rollback(snapshot)
                continue
            if val is STOP:
                self._rollback(snapshot)
                break
            self._emit(source)
            result.append(val)
        return result, container

    def _eval_tuple(self, target, spec, scope):
        # mirrors _handle_tuple: chain subspecs, threading sources through
        out_path, target_source, _ = self.frame
        current = target
        current_source = target_source
        last_source = None
        for subspec in spec:
            scope = chain_child(scope)
            self._push(out_path, current_source, False)
            try:
                nxt, source = self._eval(current, subspec, scope)
            finally:
                self._pop()
            if nxt is SKIP:
                continue
            if nxt is STOP:
                break
            current = nxt
            last_source = source
            current_source = self._retarget(source, target_source)
            if not isinstance(subspec, list):
                scope[Path] += [getattr(subspec, '__name__', subspec)]
        if last_source is None:
            if isinstance(target_source, PathSource):
                last_source = PathSource(out_path, current,
                                         target_source.input_path)
            else:
                last_source = target_source
        return current, last_source

    def _eval_fill_container(self, target, spec, scope):
        out_path, target_source, _ = self.frame
        kind = type(spec)
        container_source = SyntheticSource(
            out_path, None, [target_source], '%s (Fill)' % kind.__name__)
        self._emit(container_source)
        items = spec.items() if kind is dict else enumerate(spec)
        values = []
        for key, subspec in items:
            child_path = tuple(out_path) + ((key,) if kind is dict else (key,))
            self._push(child_path, target_source, False)
            try:
                val, source = self._eval(target, subspec, scope)
            finally:
                self._pop()
            if val is SKIP:
                self._drop_prefix(child_path)
                continue
            self._emit(source)
            values.append((key, val) if kind is dict else val)
        if kind is dict:
            result = dict(values)
        elif kind is list:
            result = values
        else:
            result = kind(values)
        container_source.value = result
        return result, container_source

    def _eval_arg_container(self, target, spec, scope):
        # structures nested inside call arguments / Coalesce defaults:
        # evaluated structurally but collected as one synthetic source
        out_path, target_source, _ = self.frame
        kind = type(spec)
        collected_frame = self.frame[2]
        before = len(collected_frame)

        def run(child):
            value, source = self._eval(target, child, scope)
            collected_frame.append(source)
            return value, source

        if kind is dict:
            result = kind()
            for key, subspec in spec.items():
                eval_key = (run(key)[0] if type(key) in (Spec, TType) else key)
                val, _ = run(subspec)
                result[eval_key] = val
        elif kind is list:
            result = [run(subspec)[0] for subspec in spec]
        else:
            result = kind([run(subspec)[0]
                           for subspec in spec])
        collected = collected_frame[before:]
        source = SyntheticSource(out_path, result, _dedup(collected),
                                 '%s (argument)' % kind.__name__)
        return result, source

    # -- T / Path expressions ------------------------------------------

    def _pure_path(self, t_path):
        """Return the accessed path parts if *t_path* is a plain read of
        the target (attribute/item/fuzzy access only), else ``None``."""
        if t_path[0] is not T:
            return None
        parts = []
        for index in range(1, len(t_path), 2):
            op, arg = t_path[index], t_path[index + 1]
            if op not in ('.', '[', 'P') or isinstance(arg, TType):
                return None
            parts.append(arg)
        return parts

    def _eval_t(self, target, t_spec, scope, target_source):
        out_path, _, _ = self.frame
        t_path = t_spec.__ops__
        parts = self._pure_path(t_path)
        if parts is not None:
            # always read through the tracer's own accessor so (a)
            # errors are native PathAccessErrors and (b) pure paths
            # nested inside opaque arguments are collected as inputs
            # rather than having their key literals leak out
            value = self._read_pure_path(target, t_path, scope)
            input_path = self._extend_input_path(target_source, parts)
            return value, PathSource(out_path, value, input_path)
        # calls, arithmetic, star traversal, scope access, ...: an opaque
        # computation; record only the arguments it provably consumed
        inputs = []
        prefix_parts = self._leading_access_parts(t_path)
        if prefix_parts:
            prefix_path = self._extend_input_path(target_source, prefix_parts)
            inputs.append(PathSource(out_path, None, prefix_path))
        for index in range(1, len(t_path), 2):
            arg = t_path[index + 1]
            if isinstance(arg, TType):
                dep_value = self._collect(target, arg, scope)
                dep_parts = self._pure_path(arg.__ops__)
                if dep_parts is not None:
                    dep_source = PathSource(
                        self.frame[0], dep_value,
                        self._extend_input_path(target_source, dep_parts))
                else:
                    dep_source = SyntheticSource(
                        self.frame[0], dep_value, [target_source],
                        'T expression %s' % bbrepr(arg))
                inputs.append(dep_source)
        if not inputs:
            inputs = [target_source]
        self.suppress_literals = True
        try:
            value, source = self._opaque(
                target, t_spec, scope, 'T expression %s' % bbrepr(t_spec),
                inputs=inputs)
        finally:
            self.suppress_literals = False
        if t_path[0] is S:
            source.origin = 'scope expression %s' % bbrepr(t_spec)
        return value, source

    def _leading_access_parts(self, t_path):
        """The leading run of plain attribute/item/fuzzy accesses on the
        T root, before the first computation (call, arithmetic, ...)."""
        if t_path[0] is not T:
            return []
        parts = []
        for index in range(1, len(t_path), 2):
            op, arg = t_path[index], t_path[index + 1]
            if op not in ('.', '[', 'P') or isinstance(arg, TType):
                break
            parts.append(arg)
        return parts

    def _read_pure_path(self, target, t_path, scope):
        # core _t_eval access semantics for read-only paths, raising
        # native PathAccessErrors while staying inside the tracer so
        # nested collection frames still see the dependency
        current = target
        index = 1
        while index < len(t_path):
            op, arg = t_path[index], t_path[index + 1]
            if op == '.':
                try:
                    current = getattr(current, arg)
                except AttributeError as e:
                    raise PathAccessError(e, Path(t_path), index // 2)
            elif op == '[':
                try:
                    current = current[arg]
                except (KeyError, IndexError, TypeError) as e:
                    raise PathAccessError(e, Path(t_path), index // 2)
            else:
                get = scope[TargetRegistry].get_handler(
                    'get', current, path=t_path[2:index + 2:2])
                try:
                    current = get(current, arg)
                except Exception as e:
                    raise PathAccessError(e, Path(t_path), index // 2)
            index += 2
        return current

    # -- Coalesce -------------------------------------------------------

    def _eval_coalesce(self, target, spec, scope):
        out_path, target_source, _ = self.frame
        skipped = []
        for subspec in spec.subspecs:
            snapshot = self._snapshot()
            self._push(out_path, target_source, False)
            try:
                ret, source = self._eval(target, subspec, scope)
            except spec.skip_exc as e:
                self._pop()
                self._rollback(snapshot)
                skipped.append(e)
                continue
            self._pop()
            if not spec.skip_func(ret):
                self._emit(source)
                return ret, source
            self._rollback(snapshot)
            skipped.append(ret)
        if spec.default is not _MISSING:
            ret, source = self._call_arg(target, spec.default, scope,
                                         'Coalesce default')
        elif spec.default_factory is not _MISSING:
            ret = spec.default_factory()
            source = SyntheticSource(out_path, ret, (), 'Coalesce default_factory')
        else:
            raise CoalesceError(spec, skipped, scope[Path])
        self._emit(source)
        return ret, source

    def _call_arg(self, target, arg, scope, origin):
        # like arg_val(), but producing a source instead of a bare value
        out_path, target_source, _ = self.frame
        self._push(out_path, target_source, True)
        self.argval_depth += 1
        try:
            value, source = self._eval(target, arg, scope)
        finally:
            self.argval_depth -= 1
            _, _, collected = self._pop()
        # fold anything the argument evaluation gathered (e.g. literal
        # containers) into one source describing the whole argument
        if collected:
            extra = [item for item in collected if item is not source]
            if isinstance(source, SyntheticSource):
                source.inputs = tuple(_dedup(list(source.inputs) + extra))
                if source.origin == 'synthetic':
                    source.origin = origin
            elif extra:
                source = SyntheticSource(out_path, value,
                                         _dedup([source] + extra), origin)
        return value, source

    # -- source path helpers -------------------------------------------

    def _extend_input_path(self, target_source, parts):
        if isinstance(target_source, PathSource):
            base = target_source.input_path
        else:
            base = Path(T)
        path = base
        for part in parts:
            path = Path(path, part)
        return path

    def _index_target(self, target_source, index):
        if isinstance(target_source, PathSource):
            return PathSource(
                target_source.path, None,
                self._extend_input_path(target_source, [index]))
        deps = list(target_source.inputs) if isinstance(target_source, SyntheticSource) else [target_source]
        return SyntheticSource(target_source.path, None, deps,
                               'list element %d' % index)

    def _retarget(self, step_source, fallback):
        # the target seen by the next chain step comes from this step's
        # output; only pure input reads can be located precisely
        if isinstance(step_source, PathSource):
            return PathSource(step_source.path, None, step_source.input_path)
        if isinstance(step_source, LiteralSource):
            return step_source
        inputs = list(step_source.inputs) or [fallback]
        return SyntheticSource(step_source.path, None, inputs, step_source.origin)


def trace(target, spec, scope=None):
    """Evaluate *spec* against *target* with provenance tracking.

    The evaluation is performed exactly as it would be by
    :func:`glom.glom`, and the returned :class:`TraceResult` carries
    the identical return value in :attr:`TraceResult.value`, alongside
    a source map in :attr:`TraceResult.sources`.

    Each key of the source map is a tuple describing an output path
    (``()`` for the root, ``('name',)`` for a dict field,
    ``('items', 0, 'id')`` for a nested list element, ...), and each
    value is a :class:`PathSource`, :class:`LiteralSource`, or
    :class:`SyntheticSource`.

    On error, the function does not raise: it returns a
    :class:`TraceResult` with ``complete=False``, the original
    :class:`~glom.GlomError` on :attr:`TraceResult.error`, a best-effort
    partial value (or :data:`INCOMPLETE`), and every source confirmed
    before the failure. Call :meth:`TraceResult.raise_on_error` to
    re-raise.

    Args:
       target: the input data, same as :func:`~glom.glom`'s *target*.
       spec: any glom spec, same as :func:`~glom.glom`'s *spec*.
       scope: optional extra scope variables, same as the *scope*
         keyword of :func:`~glom.glom`.

    >>> from glom import trace, Val
    >>> result = trace({'a': {'b': 1}}, {'x': 'a.b', 'y': Val(2)})
    >>> result.value
    {'x': 1, 'y': 2}
    >>> result[('x',)]
    PathSource(('x',), 1, input_path=Path('a', 'b'))
    """
    root_scope = _DEFAULT_SCOPE.new_child({
        Path: [],
        Inspect: None,
        MODE: AUTO,
        MIN_MODE: None,
        CHILD_ERRORS: [],
        'globals': ScopeVars({}, {}),
    })
    root_scope[UP] = root_scope
    root_scope[ROOT] = root_scope
    root_scope[T] = target
    if scope:
        root_scope.update(scope)

    tracer = _Tracer(root_scope)
    error = None
    value = INCOMPLETE
    try:
        value, _ = tracer._call(target, spec, root_scope)
    except Exception as e:
        if isinstance(e, GlomError):
            error = e
        else:
            error = GlomError.wrap(e)
        try:
            if getattr(error, '_scope', None) is None:
                error._finalize(root_scope.get(LAST_CHILD_SCOPE))
        except Exception:
            pass
        root_entry = dict(tracer.entries).get(())
        if root_entry is not None:
            value = root_entry.value

    sources = [(path, source) for (path, source) in tracer.entries
               if source.value is not SKIP and source.value is not STOP]
    return TraceResult(value, sources, error)
