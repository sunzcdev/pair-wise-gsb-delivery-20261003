"""High level strategies for converters."""

from ._class_methods import use_class_methods
from ._deterministic_sets import configure_deterministic_sets
from ._subclasses import include_subclasses
from ._unions import configure_tagged_union, configure_union_passthrough

__all__ = [
    "configure_deterministic_sets",
    "configure_tagged_union",
    "configure_union_passthrough",
    "include_subclasses",
    "use_class_methods",
]
