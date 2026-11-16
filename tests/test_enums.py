# tests/test_enums.py
from __future__ import annotations

import enum

from chardet.enums import DeclarationPolicy, DeclarationStatus, EncodingEra


def test_encoding_era_is_int_flag():
    assert issubclass(EncodingEra, enum.IntFlag)


def test_encoding_era_members_exist():
    expected = {
        "MODERN_WEB",
        "LEGACY_ISO",
        "LEGACY_MAC",
        "LEGACY_REGIONAL",
        "DOS",
        "MAINFRAME",
        "ALL",
    }
    assert set(EncodingEra.__members__.keys()) == expected


def test_encoding_era_bitwise_or():
    combined = EncodingEra.MODERN_WEB | EncodingEra.LEGACY_ISO
    assert EncodingEra.MODERN_WEB in combined
    assert EncodingEra.LEGACY_ISO in combined
    assert EncodingEra.DOS not in combined


def test_encoding_era_all_contains_every_member():
    for member in EncodingEra:
        if member is not EncodingEra.ALL:
            assert member in EncodingEra.ALL


def test_encoding_era_values_are_powers_of_two():
    for member in EncodingEra:
        if member is not EncodingEra.ALL:
            assert member.value & (member.value - 1) == 0, (
                f"{member.name} is not a power of two"
            )

def test_declaration_policy_members_and_values():
    # The values are a public contract: resolve() and the CLI accept them.
    assert {m.name: m.value for m in DeclarationPolicy} == {
        "STRICT": "strict",
        "PREFER": "prefer",
        "HINT": "hint",
    }


def test_declaration_status_members():
    assert set(DeclarationStatus.__members__) == {
        "ACCEPTED",
        "FILTERED",
        "DECODE_MISMATCH",
        "INVALID_LABEL",
    }
