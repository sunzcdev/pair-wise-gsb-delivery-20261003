from __future__ import annotations

from collections.abc import Sequence
from typing import TYPE_CHECKING, Any, Optional, Union

from typing_extensions import Self

from cattrs._compat import ExceptionGroup

if TYPE_CHECKING:
    from .dispatch import HookCandidate


class CattrsError(Exception):
    """Base ``cattrs`` exception."""


class StructureHandlerNotFoundError(CattrsError):
    """
    Error raised when structuring cannot find a handler for converting inputs into
    :attr:`type_`.
    """

    def __init__(self, message: str, type_: type) -> None:
        super().__init__(message, type_)
        self.message = message
        self.type_ = type_

    def __str__(self) -> str:
        return self.message


class BaseValidationError(ExceptionGroup, CattrsError):
    cl: type

    def __new__(cls, message: str, excs: Sequence[Exception], cl: type) -> Self:
        obj = super().__new__(cls, message, excs)
        obj.cl = cl
        return obj

    def derive(self, excs: Sequence[Exception]) -> Self:
        return self.__class__(self.message, excs, self.cl)


class IterableValidationNote(str):
    """Attached as a note to an exception when an iterable element fails structuring."""

    index: Union[int, str]  # Ints for list indices, strs for dict keys
    type: Any

    def __new__(cls, string: str, index: Union[int, str], type: Any) -> Self:
        instance = str.__new__(cls, string)
        instance.index = index
        instance.type = type
        return instance

    def __getnewargs__(self) -> tuple[str, Union[int, str], Any]:
        return (str(self), self.index, self.type)


class IterableValidationError(BaseValidationError):
    """Raised when structuring an iterable."""

    def group_exceptions(
        self,
    ) -> tuple[list[tuple[Exception, IterableValidationNote]], list[Exception]]:
        """Split the exceptions into two groups: with and without validation notes."""
        excs_with_notes = []
        other_excs = []
        for subexc in self.exceptions:
            if hasattr(subexc, "__notes__"):
                for note in subexc.__notes__:
                    if note.__class__ is IterableValidationNote:
                        excs_with_notes.append((subexc, note))
                        break
                else:
                    other_excs.append(subexc)
            else:
                other_excs.append(subexc)

        return excs_with_notes, other_excs


class AttributeValidationNote(str):
    """Attached as a note to an exception when an attribute fails structuring."""

    name: str
    type: Any

    def __new__(cls, string: str, name: str, type: Any) -> Self:
        instance = str.__new__(cls, string)
        instance.name = name
        instance.type = type
        return instance

    def __getnewargs__(self) -> tuple[str, str, Any]:
        return (str(self), self.name, self.type)


class ClassValidationError(BaseValidationError):
    """Raised when validating a class if any attributes are invalid."""

    def group_exceptions(
        self,
    ) -> tuple[list[tuple[Exception, AttributeValidationNote]], list[Exception]]:
        """Split the exceptions into two groups: with and without validation notes."""
        excs_with_notes = []
        other_excs = []
        for subexc in self.exceptions:
            if hasattr(subexc, "__notes__"):
                for note in subexc.__notes__:
                    if note.__class__ is AttributeValidationNote:
                        excs_with_notes.append((subexc, note))
                        break
                else:
                    other_excs.append(subexc)
            else:
                other_excs.append(subexc)

        return excs_with_notes, other_excs


class ForbiddenExtraKeysError(CattrsError):
    """
    Raised when `forbid_extra_keys` is activated and such extra keys are detected
    during structuring.

    The attribute `extra_fields` is a sequence of those extra keys, which were the
    cause of this error, and `cl` is the class which was structured with those extra
    keys.
    """

    def __init__(
        self, message: Optional[str], cl: type, extra_fields: set[str]
    ) -> None:
        self.message = message
        self.cl = cl
        self.extra_fields = extra_fields

        super().__init__(message, cl, extra_fields)

    def __str__(self) -> str:
        return (
            self.message
            or f"Extra fields in constructor for {self.cl.__name__}: "
            f"{', '.join(sorted(self.extra_fields))}"
        )


class AmbiguousHookError(CattrsError):
    """Raised by diagnostic hook resolution when multiple rules of the same
    priority match a type and ambiguity is not allowed.

    Only predicate (function-dispatch) rules can be ambiguous: exact-match and
    ``singledispatch`` rules have at most one winner per type. The colliding
    candidates are retained on :attr:`candidates` for inspection.

    :param type_: The type being resolved.
    :param candidates: The equally-prioritized candidates that all matched.

    .. versionadded:: NEXT
    """

    def __init__(
        self, message: str, type_: Any, candidates: Sequence[HookCandidate]
    ) -> None:
        super().__init__(message, type_)
        self.message = message
        self.type_ = type_
        self.candidates = tuple(candidates)

    def __str__(self) -> str:
        return self.message
