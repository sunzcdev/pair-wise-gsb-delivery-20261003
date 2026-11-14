# Copyright 2010-2025 Kurt McKee <contactme@kurtmckee.org>
# Copyright 2002-2008 Mark Pilgrim
# All rights reserved.
#
# This file is a part of feedparser.
#
# Redistribution and use in source and binary forms, with or without modification,
# are permitted provided that the following conditions are met:
#
# * Redistributions of source code must retain the above copyright notice,
#   this list of conditions and the following disclaimer.
# * Redistributions in binary form must reproduce the above copyright notice,
#   this list of conditions and the following disclaimer in the documentation
#   and/or other materials provided with the distribution.
#
# THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS 'AS IS'
# AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
# IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE
# ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT OWNER OR CONTRIBUTORS BE
# LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR
# CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF
# SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
# INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN
# CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE)
# ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
# POSSIBILITY OF SUCH DAMAGE.

"""Build a deduplicated view of parsed feed entries.

Entries are considered the same only when they share an explicit
``id``/``guid``; titles or other content are never used to guess identity.
Entries without an identity are kept as-is. The input entries are never
modified, and the view never merges fields from different versions into
an entry that never existed: every entry in the view is one of the
original entries.
"""

from .util import FeedParserDict

# Timestamp keys are used for ordering, not for detecting content conflicts.
_TIMESTAMP_KEYS = frozenset({"updated", "updated_parsed"})


def _entry_identity(entry: FeedParserDict) -> str | None:
    """Return the explicit identity of an entry, or None."""

    identity = entry.get("id")
    if identity is None:
        # FeedParserDict maps "guid" to "id" on access, but a raw "guid"
        # key may still exist in hand-built entries.
        identity = dict.get(entry, "guid")
    if isinstance(identity, str) and identity:
        return identity
    return None


def _content_signature(entry: FeedParserDict) -> dict:
    """Return the entry content, excluding ordering timestamps."""

    return {
        key: value for key, value in dict(entry).items() if key not in _TIMESTAMP_KEYS
    }


def _resolve_group(identity: str, group: list) -> tuple:
    """Choose which entry of a same-identity group the view keeps.

    :return: A ``(kept_index, conflict)`` tuple. ``conflict`` is None
        unless the entries cannot be ordered by update time and their
        contents differ.
    """

    if len(group) == 1:
        return group[0][0], None

    # Prefer the newest entry when update times are comparable.
    if all(entry.get("updated_parsed") is not None for _, entry in group):
        newest = max(entry.get("updated_parsed") for _, entry in group)
        winners = [
            (index, entry)
            for index, entry in group
            if entry.get("updated_parsed") == newest
        ]
        if len(winners) == 1:
            return winners[0][0], None

    # Times are missing or tied: keep the first occurrence, and record a
    # conflict if the candidates' contents differ.
    kept_index = group[0][0]
    signature = _content_signature(group[0][1])
    if all(_content_signature(entry) == signature for _, entry in group[1:]):
        return kept_index, None

    conflict = FeedParserDict(
        id=identity,
        kept_index=kept_index,
        indices=[index for index, _ in group],
    )
    return kept_index, conflict


def deduplicate_entries(entries: list) -> FeedParserDict:
    """Build a deduplicated view of parsed entries.

    :param entries: The parsed entries, typically ``result["entries"]``.
    :return: A :class:`FeedParserDict` with two keys:

        ``entries``
            The deduplicated entries. Each identity appears once, at the
            position of its first occurrence. Entries without an identity
            are kept as-is. Every entry is one of the original entries.
        ``conflicts``
            A list of conflicts between same-identity entries that could
            not be ordered by update time. Each conflict has the entry
            ``id``, the ``kept_index``, and the ``indices`` of all
            candidates, referring to positions in the original
            ``entries`` list.
    """

    groups: dict[str, list] = {}
    view = []
    for index, entry in enumerate(entries):
        identity = _entry_identity(entry)
        if identity is None:
            view.append(("entry", entry))
            continue
        if identity not in groups:
            groups[identity] = []
            view.append(("group", identity))
        groups[identity].append((index, entry))

    kept: dict[str, int] = {}
    conflicts = []
    for identity, group in groups.items():
        kept_index, conflict = _resolve_group(identity, group)
        kept[identity] = kept_index
        if conflict is not None:
            conflicts.append(conflict)

    return FeedParserDict(
        entries=[
            entry if kind == "entry" else entries[kept[entry]]
            for kind, entry in view
        ],
        conflicts=conflicts,
    )
