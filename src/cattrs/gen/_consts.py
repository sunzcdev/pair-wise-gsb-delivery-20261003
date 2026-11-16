from __future__ import annotations

from threading import local
from typing import Any, Callable

from attrs import NOTHING, field, frozen


@frozen
class AttributeOverride:
    omit_if_default: bool | None = None
    rename: str | None = None
    omit: bool | None = None  # Omit the field completely.
    struct_hook: Callable[[Any, Any], Any] | None = None  # Structure hook to use.
    unstruct_hook: Callable[[Any], Any] | None = None  # Structure hook to use.
    # An explicit baseline to compare against when omitting defaults.
    # `attrs.NOTHING` (via a factory, so it works as a default) means no
    # baseline. Excluded from the hash so unhashable baselines (like lists)
    # keep `Annotated[...]` overrides hashable.
    default_baseline: Any = field(factory=lambda: NOTHING, hash=False)
    # A custom equality function for the default comparison.
    default_eq: Callable[[Any, Any], bool] | None = None


neutral = AttributeOverride()
already_generating = local()
