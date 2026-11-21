"""Provenance for requirements expanded out of PEP 735 dependency groups.

A single ``pip install`` may draw requirements from several ``pyproject.toml``
files and from groups that include other groups.  When constraints coming from
those groups conflict during resolution, the resolver only sees the final,
flattened requirement strings.  The models here retain, for every expanded
requirement:

* the ``pyproject.toml`` and top-level group that the user selected;
* the chain of ``include-group`` references that were followed;
* the environment marker that was attached to the concrete requirement.

The data is deliberately plain and frozen so it can be consumed both by pip's
own error reporting (via :meth:`DependencyGroupProvenance.comes_from`) and by
external tooling that inspects the expansion.
"""

from __future__ import annotations

from dataclasses import dataclass


@dataclass(frozen=True, slots=True)
class DependencyGroupProvenance:
    """Where a single requirement expanded from a dependency group came from.

    :param source: The path to the ``pyproject.toml`` containing the selected
        group, as given on the command line.
    :param requested_group: The group name the user selected with ``--group``.
    :param include_chain: The groups traversed to reach the requirement. The
        first element is always ``requested_group`` and the last element is
        the group that directly listed the requirement.
    :param marker: The effective environment marker attached to the
        requirement, rendered as a string, or ``None`` when it has no marker.
    """

    source: str
    requested_group: str
    include_chain: tuple[str, ...]
    marker: str | None

    def __post_init__(self) -> None:
        if not self.include_chain:
            raise ValueError("include_chain must contain at least the requested group")
        if self.include_chain[0] != self.requested_group:
            raise ValueError(
                f"include_chain {self.include_chain!r} must start with the "
                f"requested group {self.requested_group!r}"
            )

    @property
    def direct_group(self) -> str:
        """The group that directly listed the requirement."""
        return self.include_chain[-1]

    def comes_from(self) -> str:
        """Render the provenance as a human-readable ``comes_from`` string.

        Examples:

        * ``dependency group 'test' from 'pyproject.toml'``
        * ``dependency group 'dev' from 'sub/pyproject.toml' "
          "(included via 'dev -> test')``
        * the above with ``; marker 'python_version < "3.12"'`` appended when
          the requirement carries a marker.
        """
        text = (
            f"dependency group {self.requested_group!r} from {self.source!r}"
        )
        if len(self.include_chain) > 1:
            chain = " -> ".join(self.include_chain)
            text += f" (included via {chain!r})"
        if self.marker is not None:
            text += f"; marker {self.marker!r}"
        return text


@dataclass(frozen=True, slots=True)
class ResolvedDependencyGroupRequirement:
    """A requirement produced by expanding a dependency group, with provenance."""

    requirement: str
    provenance: DependencyGroupProvenance

    def __str__(self) -> str:
        return self.requirement
