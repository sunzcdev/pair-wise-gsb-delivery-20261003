"""Types for patch (incremental) loading.

Patch loading distinguishes three states for every declared field:

- the field is absent from the input: it does not appear in the result at all;
- the field holds the `marshmallow.constants.DELETE` marker: the result
  contains a `PatchValue` with `PatchOp.DELETE`;
- any other value (including an explicit ``None``): the value is deserialized
  and validated as usual, and the result contains a `PatchValue` with
  `PatchOp.SET`.
"""

from __future__ import annotations

import enum
import typing


class PatchOp(enum.Enum):
    """The operation implied by a field in a patch result."""

    #: The field is assigned a new value.
    SET = "set"
    #: The field's value is deleted.
    DELETE = "delete"


class PatchValue(typing.NamedTuple):
    """The result of loading a single field in patch mode.

    Carries the operation implied by the input together with the
    deserialized value for `PatchOp.SET` operations.
    """

    op: PatchOp
    value: typing.Any = None

    @classmethod
    def set(cls, value: typing.Any) -> PatchValue:
        """Build a `PatchValue` representing an assignment of ``value``."""
        return cls(PatchOp.SET, value)

    @classmethod
    def delete(cls) -> PatchValue:
        """Build a `PatchValue` representing a deletion."""
        return cls(PatchOp.DELETE)
