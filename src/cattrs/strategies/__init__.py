"""High level strategies for converters."""

from ._class_methods import use_class_methods
from ._sets import configure_set_unstructure, make_set_unstructure_factory
from ._subclasses import include_subclasses
from ._unions import configure_tagged_union, configure_union_passthrough

__all__ = [
    "configure_set_unstructure",
    "configure_tagged_union",
    "configure_union_passthrough",
    "include_subclasses",
    "make_set_unstructure_factory",
    "use_class_methods",
]
