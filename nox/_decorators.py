# Copyright 2020 Alethea Katherine Flowers
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

__lazy_modules__ = {"copy", "functools", "inspect", "types"}

import copy
import functools
import inspect
import types
from collections.abc import Callable
from typing import TYPE_CHECKING, Any, Literal, TypeVar, cast

if TYPE_CHECKING:
    from collections.abc import Iterable, Mapping, Sequence

    from . import _typing
    from ._parametrize import Param

T = TypeVar("T", bound=Callable[..., Any])

__all__ = ["Call", "DependsOn", "Func", "_copy_func", "depends_on"]


def __dir__() -> list[str]:
    return __all__


def _copy_func(src: T, name: str | None = None) -> T:
    """This function copies another function, optionally with a new name."""

    dst = types.FunctionType(
        src.__code__,
        src.__globals__,
        name=name or src.__name__,
        argdefs=src.__defaults__,
        closure=src.__closure__,
    )
    dst.__dict__.update(copy.deepcopy(src.__dict__))
    dst = functools.update_wrapper(dst, src)  # type: ignore[assignment]
    dst.__kwdefaults__ = src.__kwdefaults__
    return cast("T", dst)


class DependsOn:
    """A dependency on one specific variant of a (possibly parametrized) session.

    A ``requires=`` entry is normally a plain session name: it either names an
    exact signature or, when it names a session that was parametrized, pulls in
    *every* variant of that session. A :class:`DependsOn` (see
    :func:`nox.depends_on`) instead declares which variant is wanted, by the
    parameter values it was called with and, optionally, by its interpreter.
    Only the matching variant is scheduled, so unrelated variants -- and any
    failures of theirs -- cannot affect the requiring session.

    Args:
        session (str): The declared name of the session, e.g. ``"prepare"``.
        python (str): The interpreter the variant must use. Defaults to the
            requiring session's own interpreter when that is a single
            version; otherwise the variant must match unambiguously.
        params: The parameter values the variant must have, e.g.
            ``kind="json"``.
    """

    __slots__ = ("params", "python", "session")

    def __init__(
        self, session: str, *, python: str | None = None, **params: Any
    ) -> None:
        if not isinstance(session, str) or not session:
            msg = "depends_on() requires a non-empty session name."
            raise ValueError(msg)
        self.session = session
        self.python = python
        self.params = dict(params)

    def __repr__(self) -> str:
        parts = [repr(self.session)]
        if self.python is not None:
            parts.append(f"python={self.python!r}")
        parts.extend(f"{key}={value!r}" for key, value in self.params.items())
        return f"depends_on({', '.join(parts)})"


def depends_on(session: str, *, python: str | None = None, **params: Any) -> DependsOn:
    """Declare a dependency on exactly one variant of a session.

    Use this inside ``requires=`` to pick a single variant out of a session
    that is parametrized over one or more axes (including ``@nox.session``'s
    ``python``). Plain names in ``requires=`` keep their usual meaning.

    The variant is selected by the declared parameter values, e.g.
    ``depends_on("prepare", kind="json")``, and by the interpreter. Without an
    explicit ``python=`` the requiring session's own interpreter is used when
    it has a single version, so a test matrix session depends only on the
    ``prepare`` cell for its own Python and format.

    If no variant matches, or more than one does, resolving the dependency
    fails with a message listing the declared selectors and the available
    variants, so the declaration can be corrected.

    Args:
        session (str): The name of the session to depend on, e.g.
            ``"prepare"``.
        python (str): The interpreter the variant must use, overriding the
            requiring session's own interpreter.
        params: Parameter values the variant must have, e.g. ``kind="json"``.

    Example:

    .. code-block:: python

        @nox.session(python=["3.10", "3.11"])
        @nox.parametrize("kind", ["json", "yaml"])
        def prepare(session, kind):
            ...

        @nox.session(python=["3.10", "3.11"], requires=[nox.depends_on("prepare", kind="json")])
        def check(session):
            ...

        @nox.session(requires=[nox.depends_on("prepare", python="3.11", kind="yaml")])
        def lint(session):
            ...
    """
    return DependsOn(session, python=python, **params)


class Func:
    """This is a function decorator that adds additional Nox-specific metadata."""

    def __new__(  # noqa: PYI034
        cls, func: Callable[..., Any], *args: Any, **kwargs: Any
    ) -> Func:
        self = super().__new__(cls)
        functools.update_wrapper(self, func)
        return self

    def __init__(
        self,
        func: Callable[..., Any],
        python: _typing.Python = None,
        reuse_venv: bool | None = None,  # noqa: FBT001
        name: str | None = None,
        venv_backend: str | None = None,
        venv_params: Sequence[str] = (),
        should_warn: Mapping[str, Any] | None = None,
        tags: Sequence[str] | None = None,
        *,
        default: bool = True,
        requires: Sequence[str | DependsOn] | None = None,
        download_python: Literal["auto", "never", "always"] | None = None,
        allow_parallel: bool | None = None,
    ) -> None:
        self.func = func
        self.python = python
        self.reuse_venv = reuse_venv
        self.name = name
        self.venv_backend = venv_backend
        self.venv_params = venv_params
        self.should_warn = dict(should_warn or {})
        self.tags = list(tags or [])
        self.default = default
        self.requires = list(requires or [])
        self.download_python = download_python
        self.allow_parallel = allow_parallel

    def __repr__(self) -> str:
        return f"{self.__class__.__name__}(name={self.name!r})"

    def __call__(self, *args: Any, **kwargs: Any) -> Any:
        return self.func(*args, **kwargs)

    def copy(self, name: str | None = None) -> Func:
        """Copy this function with a new name."""

        return Func(
            _copy_func(self.func, name),
            self.python,
            self.reuse_venv,
            name,
            self.venv_backend,
            self.venv_params,
            self.should_warn,
            self.tags,
            default=self.default,
            requires=self._requires,
            download_python=self.download_python,
            allow_parallel=self.allow_parallel,
        )

    @property
    def requires(self) -> list[str | DependsOn]:
        # Compute dynamically on lookup since ``self.python`` can be modified after
        # creation (e.g. on an instance from ``self.copy``).
        return list(map(self.format_dependency, self._requires))

    @requires.setter
    def requires(self, value: Sequence[str | DependsOn]) -> None:
        self._requires = list(value)

    def format_dependency(self, dependency: str | DependsOn) -> str | DependsOn:
        if isinstance(dependency, DependsOn):
            # Variant selectors are interpreted against the manifest when the
            # requiring session runs, so they are passed through unchanged.
            return dependency
        if isinstance(self.python, (bool, str)) or self.python is None:
            formatted = dependency.format(python=self.python, py=self.python)
            if (
                self.python is None or isinstance(self.python, bool)
            ) and formatted != dependency:
                msg = "Cannot parametrize requires with {python} when python is None or a bool."
                raise ValueError(msg)
            return formatted
        msg = "The requires of a not-yet-parametrized session cannot be parametrized."  # pragma: no cover
        raise TypeError(msg)  # pragma: no cover


class Call(Func):
    """This represents a call of a function with a particular set of arguments."""

    def __init__(self, func: Func, param_spec: Param) -> None:
        call_spec = param_spec.call_spec
        session_signature = f"({param_spec})"

        # Determine the Python interpreter for the session using either @session
        # or @parametrize. For backwards compatibility, we only use a "python"
        # parameter in @parametrize if the session function does not expect it
        # as a normal argument, and if the @session decorator does not already
        # specify `python`.

        python = func.python
        if python is None and "python" in call_spec:
            signature = inspect.signature(func.func)
            if "python" not in signature.parameters:
                python = call_spec.pop("python")

        super().__init__(
            func,
            python,
            func.reuse_venv,
            None,
            func.venv_backend,
            func.venv_params,
            func.should_warn,
            func.tags + param_spec.tags,
            default=func.default,
            requires=func.requires,
            download_python=func.download_python,
            allow_parallel=func.allow_parallel,
        )
        self.call_spec = call_spec
        self.session_signature = session_signature

    def __repr__(self) -> str:
        return f"{self.__class__.__name__}(name={self.name!r}, call_spec={self.call_spec!r}, session_signature={self.session_signature!r})"

    def __call__(self, *args: Any, **kwargs: Any) -> Any:
        kwargs.update(self.call_spec)
        return super().__call__(*args, **kwargs)

    @classmethod
    def generate_calls(
        cls: type[Call], func: Func, param_specs: Iterable[Param]
    ) -> list[Call]:
        """Generates a list of calls based on the function and parameters."""

        return [cls(func, param_spec) for param_spec in param_specs]
