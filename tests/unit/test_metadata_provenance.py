"""Unit tests for pip._internal.metadata.provenance."""

from __future__ import annotations

from email.parser import Parser
from typing import Any

import pytest

from pip._internal.exceptions import BuiltMetadataChanged
from pip._internal.metadata._json import msg_to_json
from pip._internal.metadata.provenance import (
    BUILT_WHEEL_METADATA_SOURCE,
    PREPARED_METADATA_SOURCE,
    MetadataSnapshot,
    build_metadata_change_report,
    metadata_digest,
    raise_for_blocking_changes,
)


def _metadata(*lines: str, name: str = "pkg", version: str = "1.0") -> dict[str, Any]:
    text = "\n".join(
        ["Metadata-Version: 2.1", f"Name: {name}", f"Version: {version}", *lines]
    )
    return msg_to_json(Parser().parsestr(text + "\n"))


def _snapshot(
    metadata: dict[str, Any], *, source: str = PREPARED_METADATA_SOURCE
) -> MetadataSnapshot:
    return MetadataSnapshot(
        source=source,
        filename="pkg-1.0-py3-none-any.whl",
        digest=metadata_digest(metadata),
        metadata=metadata,
    )


def test_digest_ignores_equivalent_spellings() -> None:
    a = _metadata(
        "Requires-Dist: Requests >= 2.0",
        "Provides-Extra: a",
        "Provides-Extra: b",
    )
    b = _metadata(
        "Requires-Dist: requests>=2.0",
        "Provides-Extra: b",
        "Provides-Extra: a",
    )
    assert metadata_digest(a) == metadata_digest(b)


def test_digest_changes_with_dependencies() -> None:
    assert metadata_digest(_metadata()) != metadata_digest(
        _metadata("Requires-Dist: extra")
    )


def test_descriptive_change_does_not_block() -> None:
    report = build_metadata_change_report(
        _snapshot(_metadata("Summary: one")),
        _snapshot(_metadata("Summary: two"), source=BUILT_WHEEL_METADATA_SOURCE),
    )
    assert report.blocking is False
    assert [difference.field for difference in report.differences] == ["summary"]
    # No exception is raised for descriptive differences.
    raise_for_blocking_changes("Package: pkg 1.0", report)


@pytest.mark.parametrize(
    "line",
    [
        "Requires-Dist: new-dependency",
        "Requires-Python: >=3.9",
        "Provides-Extra: new-extra",
    ],
)
def test_resolution_affecting_change_blocks(line: str) -> None:
    report = build_metadata_change_report(
        _snapshot(_metadata()),
        _snapshot(_metadata(line), source=BUILT_WHEEL_METADATA_SOURCE),
    )
    assert report.blocking is True
    with pytest.raises(BuiltMetadataChanged) as excinfo:
        raise_for_blocking_changes("Package: pkg 1.0", report)
    error = excinfo.value
    assert error.resolution_digest == report.resolution.digest
    assert error.built_digest == report.built.digest


def test_version_change_blocks() -> None:
    report = build_metadata_change_report(
        _snapshot(_metadata(version="1.0")),
        _snapshot(_metadata(version="2.0"), source=BUILT_WHEEL_METADATA_SOURCE),
    )
    assert report.blocking is True


def test_report_dict_records_both_sources() -> None:
    report = build_metadata_change_report(
        _snapshot(_metadata()),
        _snapshot(
            _metadata("Requires-Dist: extra"), source=BUILT_WHEEL_METADATA_SOURCE
        ),
    )
    data = report.to_dict()
    assert data["resolution"]["source"] == PREPARED_METADATA_SOURCE
    assert data["built"]["source"] == BUILT_WHEEL_METADATA_SOURCE
    assert data["blocking"] is True
    assert data["differences"][0]["field"] == "requires_dist"
    assert data["differences"][0]["affects_resolution"] is True
