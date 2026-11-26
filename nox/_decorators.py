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


def _as_action_list(value: Any) -> list[Any]:
    if value is None:
        return []
    if isinstance(value, (list, tuple, set)):
        return list(value)
    return [value]

__all__ = ["Call", "Func", "_copy_func"]


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
        requires: Sequence[str] | None = None,
        download_python: Literal["auto", "never", "always"] | None = None,
        allow_parallel: bool | None = None,
        host_diagnostics: Sequence[Any] | None = None,
        diagnostics: Sequence[Any] | None = None,
        host_diagnostic: Any = None,
        diagnostic: Any = None,
        host_actions: Sequence[Any] | None = None,
        diagnostic_actions: Sequence[Any] | None = None,
        host_diagnostic_actions: Sequence[Any] | None = None,
        actions: Sequence[Any] | None = None,
        diagnostic_action: Any = None,
        host_diagnostic_action: Any = None,
        host_checks: Sequence[Any] | None = None,
        host_check: Any = None,
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
        # ``diagnostics`` is the short alias for the public host-diagnostic
        # registry. Keep one canonical list so reports cannot count the same
        # registered action twice.
        self.host_diagnostics = [
            *_as_action_list(host_diagnostics),
            *_as_action_list(diagnostics),
            *_as_action_list(host_diagnostic),
            *_as_action_list(diagnostic),
            *_as_action_list(host_actions),
            *_as_action_list(diagnostic_actions),
            *_as_action_list(host_diagnostic_actions),
            *_as_action_list(actions),
            *_as_action_list(diagnostic_action),
            *_as_action_list(host_diagnostic_action),
            *_as_action_list(host_checks),
            *_as_action_list(host_check),
        ]
        self.diagnostics = self.host_diagnostics

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
            host_diagnostics=self.host_diagnostics,
        )

    def add_host_diagnostic(
        self, target: Any = None, /, *args: Any, **options: Any
    ) -> Any:
        """Explicitly register a host-only diagnostic action."""

        from nox.diagnostics import HostDiagnostic  # noqa: PLC0415

        if isinstance(target, HostDiagnostic) and not args and not options:
            action = target
        else:
            action = HostDiagnostic(target, *args, **options)
        self.host_diagnostics.append(action)
        # ``__init__`` aliases the same list; keep the alias meaningful for
        # older Func objects created before this attribute existed.
        self.diagnostics = self.host_diagnostics
        return action

    def host_diagnostic(
        self, target: Any = None, /, *args: Any, **options: Any
    ) -> Any:
        """Register a host diagnostic as a decorator or command call."""

        if target is not None and callable(target) and not args and not options:
            self.add_host_diagnostic(target)
            return target

        def decorate(func: RawFunc) -> RawFunc:
            self.add_host_diagnostic(func, *args, **options)
            return func

        if target is None:
            return decorate
        return self.add_host_diagnostic(target, *args, **options)

    # Short and domain-language aliases used by noxfiles and integrations.
    add_diagnostic = add_host_diagnostic

    def register_host_diagnostic(
        self, target: Any = None, /, *args: Any, **options: Any
    ) -> Any:
        """Register or decorate a host diagnostic with decorator semantics."""
        return self.host_diagnostic(target, *args, **options)

    register_diagnostic = register_host_diagnostic

    def register_action(
        self,
        kind: Any = "host_diagnostic",
        target: Any = None,
        /,
        *args: Any,
        **options: Any,
    ) -> Any:
        """Register either ``(target)`` or ``(kind, target)``."""

        if target is None and not isinstance(kind, str):
            target = kind
            kind = "host_diagnostic"
        if target is None:
            def decorate(func: RawFunc) -> RawFunc:
                self.host_diagnostic(
                    func,
                    *args,
                    kind=kind if isinstance(kind, str) else "host_diagnostic",
                    **options,
                )
                return func

            return decorate
        options.setdefault("kind", kind if isinstance(kind, str) else "host_diagnostic")
        return self.add_host_diagnostic(target, *args, **options)

    add_action = register_action
    diagnostic = host_diagnostic
    host_diagnose = host_diagnostic
    diagnose_host = host_diagnostic
    diagnostic_action = host_diagnostic
    host_diagnostic_action = host_diagnostic
    host_action = host_diagnostic
    host_check = host_diagnostic
    add_diagnostic_action = add_host_diagnostic
    register_diagnostic_action = register_host_diagnostic
    add_host_diagnostic_action = add_host_diagnostic
    register_host_diagnostic_action = register_host_diagnostic
    add_host_action = add_host_diagnostic
    register_host_action = register_host_diagnostic
    add_host_check = add_host_diagnostic
    register_host_check = register_host_diagnostic

    @property
    def requires(self) -> list[str]:
        # Compute dynamically on lookup since ``self.python`` can be modified after
        # creation (e.g. on an instance from ``self.copy``).
        return list(map(self.format_dependency, self._requires))

    @requires.setter
    def requires(self, value: Sequence[str]) -> None:
        self._requires = list(value)

    def format_dependency(self, dependency: str) -> str:
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
            host_diagnostics=getattr(func, "host_diagnostics", []),
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
