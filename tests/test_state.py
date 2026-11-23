from __future__ import annotations

import pytest

from hypercorn.state import StatePublication, StateStore


def test_acquire_returns_consistent_pair() -> None:
    store = StateStore({"primary": "p0", "secondary": "s0"})
    publication = store.acquire()
    assert isinstance(publication, StatePublication)
    assert publication.version == 0
    assert (publication["primary"], publication["secondary"]) == ("p0", "s0")


def test_publish_updates_pair_atomically() -> None:
    store = StateStore({"primary": "p0", "secondary": "s0"})
    release = store.publish({"primary": "p1", "secondary": "s1"})
    assert release.version == 1
    assert store.version == 1
    # Both values are visible together, never a torn pair
    assert (store["primary"], store["secondary"]) == ("p1", "s1")
    publication = store.acquire()
    assert (publication["primary"], publication["secondary"]) == ("p1", "s1")


def test_publish_accepts_keyword_updates() -> None:
    store = StateStore()
    store.publish(primary="p1", secondary="s1")
    assert (store["primary"], store["secondary"]) == ("p1", "s1")


def test_earlier_publication_is_retained_after_publish() -> None:
    store = StateStore({"primary": "p0", "secondary": "s0"})
    first = store.acquire()
    store.publish({"primary": "p1", "secondary": "s1"})
    store.publish({"primary": "p2", "secondary": "s2"})
    # The earlier publication still reads the release it acquired
    assert first.version == 0
    assert (first["primary"], first["secondary"]) == ("p0", "s0")
    assert store.acquire().version == 2


def test_failed_prepare_is_discarded() -> None:
    store = StateStore({"primary": "p0", "secondary": "s0"})
    store.publish({"primary": "p1", "secondary": "s1"})
    with pytest.raises(RuntimeError, match="release failed"):
        with store.prepare() as staged:
            staged["primary"] = "broken"
            staged["secondary"] = "broken"
            raise RuntimeError("release failed")
    # The failed release leaves the last successful release current
    assert store.version == 1
    assert (store["primary"], store["secondary"]) == ("p1", "s1")
    publication = store.acquire()
    assert (publication["primary"], publication["secondary"]) == ("p1", "s1")


def test_prepare_commits_on_clean_exit() -> None:
    store = StateStore({"primary": "p0", "secondary": "s0"})
    with store.prepare() as staged:
        staged["primary"] = "p1"
        staged["secondary"] = "s1"
    assert store.version == 1
    assert (store["primary"], store["secondary"]) == ("p1", "s1")


def test_publication_is_immutable() -> None:
    store = StateStore({"primary": "p0"})
    publication = store.acquire()
    with pytest.raises(TypeError):
        publication["primary"] = "p1"  # type: ignore[index]


def test_store_mapping_interface_reads_current_release() -> None:
    store = StateStore({"primary": "p0", "secondary": "s0"})
    assert len(store) == 2
    assert set(store) == {"primary", "secondary"}
    assert store.get("primary") == "p0"
    store.publish({"primary": "p1"})
    assert store["primary"] == "p1"
    assert store["secondary"] == "s0"