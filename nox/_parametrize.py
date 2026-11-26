# Copyright 2017 Alethea Katherine Flowers
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

from __future__ import annotations

__lazy_modules__ = {"functools", "inspect", "itertools"}

import functools
import inspect
import itertools
from collections.abc import Iterable, Iterator, Mapping
from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    from collections.abc import Callable, Sequence

    # A single matrix cell: its parameter combination and its interpreter.
    Cell = tuple[Param, str | None]
    MatrixFilter = Mapping[str, Any] | Callable[[Mapping[str, Any]], bool]

__all__ = [
    "MatrixError",
    "Param",
    "axis_token",
    "cell_inputs",
    "describe_cell",
    "format_inputs",
    "matrix_conflicts",
    "matrix_identity",
    "matrix_short_name",
    "parametrize_decorator",
    "parametrize_exclude_decorator",
    "select_cells",
    "update_param_specs",
]


def __dir__() -> list[str]:
    return __all__


class MatrixError(Exception):
    """Raised when a filtered matrix is internally inconsistent.

    ``problems`` holds one human-readable message per problem so that Nox can
    print all of them before executing anything, instead of stopping at the
    first traceback.
    """

    def __init__(self, problems: Iterable[str]) -> None:
        self.problems = list(problems)
        super().__init__("\n".join(self.problems))


# Characters that would make a matrix selector ambiguous if they appeared in an
# axis value token, since a selector is ``name[axis=token,axis=token]``.
_UNSAFE_TOKEN_CHARS = frozenset(",=[]")


def _format_token(text: str) -> str:
    """Render an axis value token so it can be embedded in a selector."""
    if not text or _UNSAFE_TOKEN_CHARS.intersection(text):
        return repr(text)
    return text


def axis_token(value: Any) -> str:
    """The stable, order-independent name of a single axis value."""
    return _format_token(str(value))


def cell_axes(param_spec: Param, python: str | None = None) -> dict[str, Any]:
    """The axis values that make up a single cell of a parametrized session.

    The ``python`` axis names the interpreter when it is not itself a
    parametrized argument, so that cells which only differ by interpreter do
    not share an identity.
    """
    axes = dict(param_spec.call_spec)
    if python and "python" not in axes:
        axes["python"] = python
    return axes


def matrix_identity(
    name: str, param_spec: Param, python: str | None = None
) -> str:
    """The stable selection identity of a single matrix cell.

    Unlike the display signature, the axes are sorted by name, so the identity
    does not depend on the order in which ``@nox.parametrize`` decorators (or
    the values within them) were declared. It also depends only on the values
    themselves, so hiding a different combination cannot renumber it.
    """
    axes = cell_axes(param_spec, python)
    if not axes:
        return name
    pairs = ",".join(
        f"{axis}={axis_token(value)}" for axis, value in sorted(axes.items())
    )
    return f"{name}[{pairs}]"


def matrix_short_name(param_spec: Param) -> str | None:
    """The user-chosen short name of a cell, or ``None`` if it has none.

    Only explicit ids are included.  ``str(param_spec)`` also contains
    generated ``axis=value`` fragments, but the stable identity already shows
    every axis value; appending those generated fragments as the "short name"
    would neither be short nor the name the user chose.
    """
    fragments = [fragment for fragment in param_spec.id_fragments if fragment]
    if not fragments:
        return None
    return ", ".join(fragments)


def cell_inputs(param_spec: Param, python: str | None = None) -> dict[str, Any]:
    """The inputs of a cell, sorted by axis, for human-readable reporting."""
    return dict(sorted(cell_axes(param_spec, python).items()))


def format_inputs(inputs: Mapping[str, Any]) -> str:
    """Render a cell's inputs like ``a=1, b='x'``."""
    return ", ".join(f"{axis}={value!r}" for axis, value in inputs.items())


def describe_cell(param_spec: Param, python: str | None) -> str:
    """Describe a cell so two cells with the same identity stay distinct.

    A cell may still differ by interpreter when ``python`` is itself a
    parametrized axis; say so instead of rendering both cells identically.
    """
    inputs = cell_inputs(param_spec, python)
    text = format_inputs(inputs)
    if python and inputs.get("python") != python:
        text = f"{text}, python={python!r}" if text else f"python={python!r}"
    return text


def matrix_conflicts(
    name: str, cells: Iterable[tuple[Param, str | None]]
) -> list[str]:
    """Report distinct cells that would be indistinguishable to the user.

    Two different combinations are a problem when they declare the same custom
    short name, because the short name is what a reader uses to tell cells
    apart. Identical stable identities are also reported: selecting one would
    silently pick the other.

    Args:
        name: The session name, used to phrase the message.
        cells: ``(param_spec, python)`` pairs for every cell that will run.

    Returns:
        One message per conflict, each naming both groups of inputs.
    """
    by_short: dict[str, dict[str, str]] = {}
    by_identity: dict[str, list[str]] = {}
    for param_spec, python in cells:
        identity = matrix_identity(name, param_spec, python)
        described = describe_cell(param_spec, python)
        by_identity.setdefault(identity, []).append(described)
        short = matrix_short_name(param_spec)
        if short is not None:
            by_short.setdefault(short, {}).setdefault(identity, described)

    problems: list[str] = []
    for short, identities in by_short.items():
        if len(identities) > 1:
            detail = "; ".join(
                f"{identity} ({described})"
                for identity, described in identities.items()
            )
            problems.append(
                f"Session {name!r}: the short name {short!r} is declared by"
                f" different combinations: {detail}. Give each combination a"
                " unique id, or exclude all but one."
            )
    for identity, described_list in by_identity.items():
        if len(described_list) > 1:
            detail = "; ".join(described_list)
            problems.append(
                f"Session {name!r}: different combinations share the identity"
                f" {identity!r}: {detail}. Give each combination distinct axis"
                " values, or exclude all but one so that each identity names"
                " exactly one cell."
            )
    return problems


def _values_equal(left: Any, right: Any) -> bool:
    try:
        if left == right:
            return True
    except Exception:  # noqa: BLE001 - exotic values may not be comparable
        pass
    # Filters are usually written by hand, so accept the string form too.
    return str(left) == str(right)


def _filter_matches(axes: Mapping[str, Any], wanted: Mapping[str, Any]) -> bool:
    for axis, wanted_value in wanted.items():
        if axis not in axes:
            return False
        axis_value = axes[axis]
        # A sequence in a filter means "match any of these values". Keep an
        # actual sequence axis value on exact-match semantics.
        if isinstance(wanted_value, (list, tuple, set, frozenset)) and not isinstance(
            axis_value, (list, tuple, set, frozenset)
        ):
            if not any(_values_equal(axis_value, item) for item in wanted_value):
                return False
        elif not _values_equal(axis_value, wanted_value):
            return False
    return True


def select_cells(
    cells: Iterable[Cell],
    *,
    excludes: Sequence[MatrixFilter],
    axis_names: Iterable[str],
) -> list[Cell]:
    """Apply explicit ``exclude`` filters to a set of matrix cells.

    Filtering is done over *every* cell at once, including the interpreter, so
    a single mapping filter can span the ``python`` axis and the parametrized
    axes. A callable filter receives the sorted axis mapping and returns
    ``True`` for cells that should be removed.

    Args:
        cells: ``(param_spec, python)`` pairs for every combination.
        excludes: Mapping filters and/or predicate callables. A mapping matches
            a cell when all of its axis/value pairs match; a cell matching any
            filter is removed.
        axis_names: The axis names the session declares, used to reject typos.

    Returns:
        The surviving cells, in their original order.

    Raises:
        MatrixError: If a mapping filter names an axis the session does not
            declare, or does not match any combination. These are almost always
            typos, and silently ignoring them would quietly run a cell that was
            meant to be excluded.
    """
    cell_list = list(cells)
    known = set(axis_names)
    if any(python and "python" not in spec.call_spec for spec, python in cell_list):
        known.add("python")

    errors: list[str] = []
    valid_excludes: list[MatrixFilter] = []
    for wanted in excludes:
        if callable(wanted):
            valid_excludes.append(wanted)
            continue
        unknown = sorted(set(wanted) - known)
        if unknown:
            errors.append(
                "Unknown axis"
                f" {', '.join(repr(name) for name in unknown)} in exclude filter"
                f" {dict(wanted)!r}; this session declares"
                f" {', '.join(repr(name) for name in sorted(known)) or 'no axes'}."
            )
            continue
        valid_excludes.append(wanted)

    removed: set[int] = set()
    for wanted in valid_excludes:
        if callable(wanted):
            matched = {
                n
                for n, (spec, python) in enumerate(cell_list)
                if wanted(cell_axes(spec, python))
            }
        else:
            matched = {
                n
                for n, (spec, python) in enumerate(cell_list)
                if _filter_matches(cell_axes(spec, python), wanted)
            }
            # An empty matrix has no cell any filter could match; it is already a
            # no-op placeholder session, so demanding a match would be noise.
            if not matched and cell_list:
                errors.append(
                    f"Exclude filter {dict(wanted)!r} does not match any combination."
                )
        removed |= matched

    if errors:
        raise MatrixError(errors)
    return [cell for n, cell in enumerate(cell_list) if n not in removed]


class Param:
    """A class that encapsulates a single set of parameters to a parametrized
    session.

    Args:
        args (List[Any]): The list of args to pass to the invoked function.
        arg_names (Sequence[str]): The names of the args.
        id (str): An optional ID for this set of parameters. If unspecified,
            it will be generated from the parameters.
        tags (Sequence[str]): Optional tags to associate with this set of
            parameters.
    """

    def __init__(
        self,
        *args: Any,
        arg_names: Sequence[str] | None = None,
        id: str | None = None,
        tags: Sequence[str] | None = None,
    ) -> None:
        self.args = args
        self.id = id
        # The fragments of ``id`` that the user declared explicitly, rather
        # than composed from the argument values. A cell with none of these has
        # no custom short name; otherwise the rendered id (which may also carry
        # ``axis=value`` for axes declared without an id) is the short name.
        self.id_fragments: list[str] = [] if id is None else [id]

        if arg_names is None:
            arg_names = ()

        self.arg_names = tuple(arg_names)

        if tags is None:
            tags = []

        self.tags = list(tags)

    @property
    def call_spec(self) -> dict[str, Any]:
        return dict(zip(self.arg_names, self.args, strict=True))

    def __str__(self) -> str:
        if self.id:
            return self.id
        return ", ".join(f"{k}={v!r}" for k, v in self.call_spec.items())

    __repr__ = __str__
    __hash__ = None  # type: ignore[assignment]

    def copy(self) -> Param:
        param = self.__class__(
            *self.args, arg_names=self.arg_names, id=self.id, tags=self.tags
        )
        param.id_fragments = list(self.id_fragments)
        return param

    def update(self, other: Param) -> None:
        self.id = ", ".join([str(self), str(other)])
        self.id_fragments += other.id_fragments
        self.args += other.args
        self.arg_names += other.arg_names
        self.tags += other.tags

    def __eq__(self, other: object) -> bool:
        if isinstance(other, self.__class__):
            return (
                self.args == other.args
                and self.arg_names == other.arg_names
                and self.id == other.id
                and self.tags == other.tags
            )
        if isinstance(other, dict):
            return dict(zip(self.arg_names, self.args, strict=True)) == other

        return NotImplemented


def _apply_param_specs(
    param_specs: Iterable[Param],
    axis_names: Sequence[str],
    excludes: Sequence[MatrixFilter] | None,
    f: Any,
) -> Any:
    previous_param_specs = getattr(f, "parametrize", None)
    new_param_specs = update_param_specs(previous_param_specs, param_specs)
    f.parametrize = new_param_specs

    # Remember every declared axis so that exclude filters spanning several
    # stacked decorators can be validated once the whole matrix exists.
    declared_axes = list(getattr(f, "matrix_axes", ()))
    declared_axes.extend(axis_names)
    f.matrix_axes = declared_axes

    # Passing ``exclude`` (even an empty list) opts this session into stable
    # matrix identities; leaving it out keeps the legacy selection behavior.
    if excludes is not None:
        f.matrix_enabled = True
        f.matrix_excludes = [*getattr(f, "matrix_excludes", ()), *excludes]

    return f


ArgValue = Param | Iterable[Any]


def _normalize_excludes(
    exclude: MatrixFilter | Iterable[MatrixFilter] | None,
) -> list[MatrixFilter] | None:
    if exclude is None:
        return None
    if isinstance(exclude, Mapping) or callable(exclude):
        return [exclude]
    return list(exclude)


def _looks_like_session_function(value: Any) -> bool:
    """Distinguish a bare decorated session from a one-argument predicate."""
    if hasattr(value, "parametrize") or hasattr(value, "matrix_enabled"):
        return True
    try:
        parameters = tuple(inspect.signature(value).parameters.values())
    except (TypeError, ValueError):
        return False
    positional = [
        p
        for p in parameters
        if p.kind
        in (inspect.Parameter.POSITIONAL_ONLY, inspect.Parameter.POSITIONAL_OR_KEYWORD)
    ]
    return len(positional) >= 2 or bool(
        positional and positional[0].name in {"session", "s"}
    )


def parametrize_exclude_decorator(
    *excludes: MatrixFilter | Iterable[MatrixFilter],
) -> Callable[[Any], Any] | Any:
    """Exclude combinations from an already-declared parametrized matrix.

    This is equivalent to passing ``exclude=`` to :func:`parametrize_decorator`,
    but can be used as a separate decorator when that reads more clearly::

        @nox.session
        @nox.parametrize("a", [1, 2])
        @nox.parametrize("b", [1, 2])
        @nox.parametrize_exclude({"a": 1, "b": 2})
        def session(...): ...

    Multiple positional filters are combined. An iterable of filters is also
    accepted as the first positional argument. Bare ``@nox.parametrize_exclude``
    opts into stable identities without adding a filter.
    """

    # Support both ``@parametrize_exclude`` and ``@parametrize_exclude()``.
    if (
        len(excludes) == 1
        and not isinstance(excludes[0], (Mapping, str, bytes))
        and callable(excludes[0])
        and _looks_like_session_function(excludes[0])
    ):
        target = excludes[0]
        target.matrix_enabled = True
        return target

    normalized: list[MatrixFilter] = []
    for item in excludes:
        if isinstance(item, Mapping) or callable(item):
            normalized.append(item)
        else:
            normalized.extend(item)

    def decorator(f: Any) -> Any:
        # Merely opting into this explicit filter is enough to enable stable
        # matrix identities, even if the filter list is empty.
        f.matrix_enabled = True
        f.matrix_excludes = [
            *getattr(f, "matrix_excludes", ()),
            *normalized,
        ]
        return f

    return decorator


def parametrize_decorator(
    arg_names: str | Sequence[str],
    arg_values_list: Iterable[ArgValue] | ArgValue,
    ids: Iterable[str | None] | None = None,
    tags: Iterable[Sequence[str]] | None = None,
    *,
    exclude: MatrixFilter | Iterable[MatrixFilter] | None = None,
    excludes: MatrixFilter | Iterable[MatrixFilter] | None = None,
) -> Callable[[Any], Any]:
    """Parametrize a session.

    Add new invocations to the underlying session function using the list of
    ``arg_values_list`` for the given ``arg_names``. Parametrization is
    performed during session discovery and each invocation appears as a
    separate session to Nox.

    Args:
        arg_names (Sequence[str]): A list of argument names.
        arg_values_list (Iterable[Any] | Any): The list of argument
            values determines how often a session is invoked with different
            argument values. If only one argument name was specified then
            this is a simple list of values, for example ``[1, 2, 3]``. A
            list, tuple, range, or iterator (such as a generator) is expanded;
            any other value is used as a single value. If N
            argument names were specified, this must be a list of N-tuples,
            where each tuple-element specifies a value for its respective
            argument name, for example ``[(1, 'a'), (2, 'b')]``.
        ids (Sequence[str]): Optional sequence of test IDs to use for the
            parametrized arguments.
        tags (Iterable[Sequence[str]]): Optional iterable of tags to associate
            with the parametrized arguments.
        exclude: Optional combination(s) to leave out of the matrix. Each
            mapping selects a cell by naming one or more of its axes, e.g.
            ``exclude=[{"python": "3.10", "dependency": "2.0"}]``. A cell is
            removed when it matches *all* pairs of any one mapping. Supplying
            ``exclude`` (even as an empty list) also opts the session into
            stable matrix identities: each cell is then selected by the sorted
            ``name[axis=value,...]`` form, which does not change when axes are
            declared in a different order or when another cell is excluded.
    """

    # Allow args names to be specified as any of 'arg', 'arg,arg2' or ('arg', 'arg2')
    if isinstance(arg_names, str):
        arg_names = list(filter(None, [arg.strip() for arg in arg_names.split(",")]))

    # If there's only one arg_name, arg_values_list should be a single item
    # or list. Transform it so it'll work with the combine step.
    _arg_values_list: list[Param | Iterable[Any | ArgValue]] = []
    if len(arg_names) == 1:
        # In this case, the arg_values_list can also just be a single item.
        # Must be mutable for the transformation steps
        if isinstance(arg_values_list, (list, tuple, range, Iterator)):
            _arg_values_list = list[Any](arg_values_list)
        else:
            _arg_values_list = [arg_values_list]

        for n, value in enumerate(_arg_values_list):
            if not isinstance(value, Param):
                _arg_values_list[n] = [value]
    elif isinstance(arg_values_list, Param):
        _arg_values_list = [arg_values_list]
    else:
        _arg_values_list = list(arg_values_list)

    # if ids aren't specified at all, make them an empty list for zip.
    if not ids:
        ids = []

    if tags is None:
        tags = []

    # Generate params for each item in the param_args_values list.
    param_specs: list[Param] = []
    for param_arg_values, param_id, param_tags in itertools.zip_longest(
        _arg_values_list, ids, tags
    ):
        if isinstance(param_arg_values, Param):
            param_spec = param_arg_values.copy()
            param_spec.arg_names = tuple(arg_names)
        else:
            param_spec = Param(
                *param_arg_values, arg_names=arg_names, id=param_id, tags=param_tags
            )

        param_specs.append(param_spec)

    if exclude is None:
        exclude = excludes

    return functools.partial(
        _apply_param_specs, param_specs, list(arg_names), _normalize_excludes(exclude)
    )


def update_param_specs(
    param_specs: Iterable[Param] | None, new_specs: Iterable[Param]
) -> list[Param]:
    """Produces all combinations of the given sets of specs."""
    if param_specs is None:
        return list(new_specs)

    # New specs must be combined with old specs by *multiplying* them.
    combined_specs = []
    for new_spec in new_specs:
        for spec in param_specs:
            spec_copy = spec.copy()
            spec_copy.update(new_spec)
            combined_specs.append(spec_copy)
    return combined_specs
