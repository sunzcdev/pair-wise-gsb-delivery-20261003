"""Metadata provenance tracking for prepared and built distributions.

When pip prepares metadata for a source distribution (through
``prepare_metadata_for_build_wheel``) that metadata drives dependency
resolution.  The build backend is then asked to build a wheel, and the wheel's
``METADATA`` file is what actually gets installed.  A misbehaving backend can
report different information in the two places.

This module captures a fingerprint ("snapshot") of the metadata used at
resolution time and of the metadata in the built wheel, so that pip can
explain what changed after the build.  Changes to fields that affect
resolution must be rejected before installation; purely descriptive changes
are reported but do not block the installation.
"""

from __future__ import annotations

import hashlib
import json
from collections.abc import Iterable, Mapping
from dataclasses import dataclass
from typing import Any

from pip._vendor.packaging.requirements import InvalidRequirement
from pip._vendor.packaging.utils import canonicalize_name, canonicalize_version

from pip._internal.metadata._json import METADATA_FIELDS
from pip._internal.metadata.base import BaseDistribution
from pip._internal.utils.packaging import get_requirement

#: Source labels, used to tell the two metadata views apart in reports.
PREPARED_METADATA_SOURCE = "prepare-metadata"
BUILT_WHEEL_METADATA_SOURCE = "built-wheel"

#: Metadata keys, in ``msg_to_json`` form, whose changes affect resolution.
#: ``Name`` and ``Version`` are included because a different distribution
#: would no longer match what was resolved.
RESOLUTION_AFFECTING_FIELDS = frozenset(
    {
        "name",
        "version",
        "requires_dist",
        "requires_python",
        "provides_extra",
    }
)

#: Metadata keys that hold repeated headers.
_MULTI_FIELDS = frozenset(
    field.lower().replace("-", "_") for field, multi in METADATA_FIELDS if multi
)


def _json_name(field: str) -> str:
    return field.lower().replace("-", "_")


def _canonical_requirement(text: str) -> str:
    """Return a canonical string for a ``Requires-Dist`` entry.

    ``Requirement`` compares equal across equivalent spellings (name case,
    extra order, whitespace) but ``str()`` preserves the original spelling, so
    build a stable form explicitly.
    """
    requirement = get_requirement(text)
    parts: list[str] = [canonicalize_name(requirement.name)]
    if requirement.extras:
        parts.append("[" + ",".join(sorted(requirement.extras)) + "]")
    if requirement.specifier:
        parts.append(str(requirement.specifier))
    if requirement.url:
        parts.append("@ " + requirement.url)
    if requirement.marker:
        parts.append("; " + str(requirement.marker))
    return "".join(parts)


def _normalize_item(field: str, value: str) -> str:
    """Normalize a single header value so equivalent spellings compare equal."""
    text = value.strip() if isinstance(value, str) else value
    if field == "requires_dist":
        try:
            return _canonical_requirement(text)
        except InvalidRequirement:
            # Leave invalid entries untouched; they are reported as-is and
            # validation happens elsewhere.
            return text
    if field in ("name", "provides_extra"):
        return canonicalize_name(text)
    if field == "version":
        return canonicalize_version(text)
    return text


def _normalize_value(field: str, value: Any) -> Any:
    if isinstance(value, list):
        return sorted(_normalize_item(field, item) for item in value)
    if value is None:
        return None
    return _normalize_item(field, value)


def _normalized_metadata(metadata: Mapping[str, Any]) -> dict[str, Any]:
    normalized: dict[str, Any] = {}
    for field in set(metadata) | _MULTI_FIELDS:
        value = metadata.get(field)
        if value is None:
            if field in _MULTI_FIELDS:
                normalized[field] = []
            continue
        normalized[field] = _normalize_value(field, value)
    return normalized


def metadata_digest(metadata: Mapping[str, Any]) -> str:
    """Return a stable ``sha256:`` digest of normalized metadata."""
    payload = json.dumps(
        _normalized_metadata(metadata),
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
    )
    return "sha256:" + hashlib.sha256(payload.encode("utf-8")).hexdigest()


@dataclass(frozen=True)
class MetadataSnapshot:
    """A fingerprint of one view of a distribution's metadata."""

    source: str
    filename: str
    digest: str
    metadata: dict[str, Any]

    @classmethod
    def from_distribution(
        cls,
        distribution: BaseDistribution,
        *,
        source: str,
        filename: str,
    ) -> MetadataSnapshot:
        metadata = distribution.metadata_dict
        return cls(
            source=source,
            filename=filename,
            digest=metadata_digest(metadata),
            metadata=metadata,
        )

    def to_source_dict(self) -> dict[str, str]:
        return {
            "source": self.source,
            "filename": self.filename,
            "digest": self.digest,
        }


@dataclass(frozen=True)
class MetadataDifference:
    """A single field that differs between two metadata snapshots."""

    field: str
    resolution_value: Any
    built_value: Any
    affects_resolution: bool

    def to_dict(self) -> dict[str, Any]:
        return {
            "field": self.field,
            "resolution": self.resolution_value,
            "built": self.built_value,
            "affects_resolution": self.affects_resolution,
        }

    def describe(self) -> str:
        return (
            f"{_display_name(self.field)}: prepared {self.resolution_value!r}, "
            f"built {self.built_value!r}"
        )


def _display_name(field: str) -> str:
    for name, _multi in METADATA_FIELDS:
        if _json_name(name) == field:
            return name
    return field


def compare_metadata(
    resolution: MetadataSnapshot,
    built: MetadataSnapshot,
) -> list[MetadataDifference]:
    """Return the fields that differ between the two snapshots.

    Fields are compared using normalized values so that equivalent spellings
    (for example ``requests >= 2.0`` and ``Requests>=2.0``) are not flagged.
    """
    resolution_metadata = _normalized_metadata(resolution.metadata)
    built_metadata = _normalized_metadata(built.metadata)

    differences: list[MetadataDifference] = []
    for field in sorted(set(resolution_metadata) | set(built_metadata)):
        resolution_value = resolution_metadata.get(field)
        built_value = built_metadata.get(field)
        if resolution_value == built_value:
            continue
        differences.append(
            MetadataDifference(
                field=field,
                resolution_value=resolution_value,
                built_value=built_value,
                affects_resolution=field in RESOLUTION_AFFECTING_FIELDS,
            )
        )
    return differences


def affects_resolution(
    differences: Iterable[MetadataDifference],
) -> list[MetadataDifference]:
    """Filter ``differences`` down to the ones that affect resolution."""
    return [difference for difference in differences if difference.affects_resolution]


@dataclass(frozen=True)
class MetadataChangeReport:
    """The outcome of comparing resolution metadata with built-wheel metadata."""

    resolution: MetadataSnapshot
    built: MetadataSnapshot
    differences: list[MetadataDifference]

    @property
    def blocking_differences(self) -> list[MetadataDifference]:
        return affects_resolution(self.differences)

    @property
    def blocking(self) -> bool:
        return bool(self.blocking_differences)

    def format_differences(self) -> str:
        """Render the blocking differences for use in an error message."""
        return "\n".join(
            f"  {difference.describe()}" for difference in self.blocking_differences
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "resolution": self.resolution.to_source_dict(),
            "built": self.built.to_source_dict(),
            "differences": [difference.to_dict() for difference in self.differences],
            "blocking": self.blocking,
        }


def build_metadata_change_report(
    resolution: MetadataSnapshot,
    built: MetadataSnapshot,
) -> MetadataChangeReport:
    return MetadataChangeReport(
        resolution=resolution,
        built=built,
        differences=compare_metadata(resolution, built),
    )


def snapshot_built_wheel(
    distribution: BaseDistribution,
    filename: str,
) -> MetadataSnapshot:
    """Snapshot the metadata read from a built wheel."""
    return MetadataSnapshot.from_distribution(
        distribution,
        source=BUILT_WHEEL_METADATA_SOURCE,
        filename=filename,
    )


def raise_for_blocking_changes(
    package_details: str, report: MetadataChangeReport
) -> None:
    """Raise if ``report`` contains resolver-affecting differences.

    Descriptive differences are intentionally allowed through; only changes
    that would make the installed distribution differ from what was resolved
    are rejected.
    """
    if not report.blocking:
        return

    # Imported lazily to keep this module free of import-time coupling.
    from pip._internal.exceptions import BuiltMetadataChanged

    raise BuiltMetadataChanged(
        package_details=package_details,
        resolution_source=report.resolution.source,
        resolution_digest=report.resolution.digest,
        built_source=report.built.source,
        built_digest=report.built.digest,
        differences=report.format_differences(),
    )
