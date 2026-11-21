# This file is a part of Dramatiq.
#
# Copyright (C) 2017,2018,2019,2020 CLEARTYPE SRL <bogdan@cleartype.io>
#
# Dramatiq is free software; you can redistribute it and/or modify it
# under the terms of the GNU Lesser General Public License as published by
# the Free Software Foundation, either version 3 of the License, or (at
# your option) any later version.
#
# Dramatiq is distributed in the hope that it will be useful, but WITHOUT
# ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
# FITNESS FOR A PARTICULAR PURPOSE. See the GNU Lesser General Public
# License for more details.
#
# You should have received a copy of the GNU Lesser General Public License
# along with this program.  If not, see <http://www.gnu.org/licenses/>.

from __future__ import annotations

import inspect
from typing import Any, Callable, Mapping, Optional, Union

from .errors import MessageContractError

#: Objects that can be coerced into a contract signature: any callable
#: (its signature is introspected) or an `inspect.Signature` instance.
SignatureSpec = Union[Callable[..., Any], inspect.Signature]

#: A single contract version or a mapping of version -> signature spec.
ContractSpec = Union[SignatureSpec, Mapping[Any, SignatureSpec], "MessageContract"]


def _as_signature(spec: SignatureSpec) -> inspect.Signature:
    if isinstance(spec, inspect.Signature):
        return spec
    if callable(spec):
        return inspect.signature(spec)
    raise TypeError(
        "Contract versions must be callables or inspect.Signature instances, got %r." % (spec,)
    )


def _binds(signature: inspect.Signature, args: tuple, kwargs: dict) -> bool:
    try:
        signature.bind(*args, **kwargs)
    except TypeError:
        return False
    return True


def _missing_parameters(signature: inspect.Signature, args: tuple, kwargs: dict) -> list:
    """Compute the required parameters of ``signature`` that are not
    covered by ``args`` and ``kwargs``, as ``(position, name)`` pairs.
    ``position`` is ``None`` for keyword-only parameters.
    """
    missing = []
    parameters = list(signature.parameters.values())
    positional = [
        p for p in parameters
        if p.kind in (inspect.Parameter.POSITIONAL_ONLY, inspect.Parameter.POSITIONAL_OR_KEYWORD)
    ]
    for position, param in enumerate(positional):
        if position < len(args) or param.name in kwargs:
            continue
        if param.default is inspect.Parameter.empty:
            missing.append((position, param.name))
    for param in parameters:
        if (
            param.kind is inspect.Parameter.KEYWORD_ONLY
            and param.name not in kwargs
            and param.default is inspect.Parameter.empty
        ):
            missing.append((None, param.name))
    return missing


class MessageContract:
    """Declares the message calling conventions an actor is compatible
    with.  During rolling deployments, old producers and new workers
    may briefly share a queue; a contract lets the broker determine,
    at enqueue time (both for regular sends and for retries), which
    declared versions a message's arguments are valid for and reject
    messages that don't satisfy any of them.

    Contracts are optional: actors that don't declare one accept any
    arguments, exactly as before.

    Examples:

      >>> def add_v1(x, y): ...
      >>> def add_v2(x, y, z=0): ...
      >>> contract = MessageContract({1: add_v1, 2: add_v2})
      >>> contract.match((1, 2), {})
      [1, 2]
      >>> contract.match((1, 2), {"z": 3})
      [2]

    Parameters:
      versions: Either a mapping of version to a callable or
        `inspect.Signature`, or a single callable/signature for
        unversioned contracts.  The last declared version is
        considered the current one.
    """

    def __init__(self, versions: ContractSpec) -> None:
        if isinstance(versions, MessageContract):
            signatures = dict(versions.signatures)
        elif isinstance(versions, Mapping):
            signatures = {version: _as_signature(spec) for version, spec in versions.items()}
        else:
            signatures = {None: _as_signature(versions)}

        if not signatures:
            raise ValueError("Message contracts must declare at least one version.")

        self.signatures: dict[Any, inspect.Signature] = signatures

    @property
    def versions(self) -> tuple:
        """The declared versions, in declaration order."""
        return tuple(self.signatures)

    @property
    def current_version(self):
        """The most recently declared version."""
        return next(reversed(self.signatures))

    def match(self, args: tuple = (), kwargs: Optional[dict] = None) -> list:
        """Determine which declared versions the given arguments are
        valid for.

        Returns:
          list: The matching versions, in declaration order.
        """
        kwargs = kwargs or {}
        return [
            version
            for version, signature in self.signatures.items()
            if _binds(signature, args, kwargs)
        ]

    def missing_parameters(self, args: tuple = (), kwargs: Optional[dict] = None, *, version=None) -> list:
        """Compute the required parameters of ``version`` (the current
        version by default) that the given arguments don't provide.

        Returns:
          list: ``(position, name)`` pairs.  ``position`` is ``None``
          for keyword-only parameters.
        """
        kwargs = kwargs or {}
        if version is None:
            version = self.current_version
        return _missing_parameters(self.signatures[version], args, kwargs)

    def validate(self, actor_name: str, args: tuple = (), kwargs: Optional[dict] = None) -> list:
        """Validate message arguments against this contract.

        Raises:
          MessageContractError: If the arguments don't satisfy any of
            the declared versions.  The error names the actor and the
            missing parameter positions of the current version.

        Returns:
          list: The versions the arguments are valid for.
        """
        matched = self.match(args, kwargs)
        if matched:
            return matched

        missing = self.missing_parameters(args, kwargs)
        details = ", ".join(
            "%r (position %d)" % (name, position) if position is not None else "%r (keyword)" % (name,)
            for position, name in missing
        )
        message = "Message arguments do not match any declared contract version of actor %s." % (actor_name,)
        if details:
            if self.current_version is None:
                message += " Missing required parameters: %s." % details
            else:
                message += " Missing required parameters for version %s: %s." % (self.current_version, details)
        raise MessageContractError(message, actor_name=actor_name, missing=missing)

    def __repr__(self) -> str:
        return "MessageContract(%r)" % (self.signatures,)
