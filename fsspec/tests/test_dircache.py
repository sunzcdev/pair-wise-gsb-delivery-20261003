import pickle
import time

import pytest

from fsspec.dircache import DirCache


def test_dircache_evicts_oldest_when_capacity_exceeded():
    dc = DirCache(max_paths=2)
    dc["a"] = 1
    dc["b"] = 2

    dc["c"] = 3

    assert "a" not in dc
    assert dc["b"] == 2
    assert dc["c"] == 3
    assert len(dc) == 2


def test_dircache_read_promotes_key_to_most_recent():
    dc = DirCache(max_paths=2)
    dc["a"] = 1
    dc["b"] = 2

    _ = dc["a"]
    dc["c"] = 3

    assert "b" not in dc
    assert dc["a"] == 1
    assert dc["c"] == 3


@pytest.mark.parametrize("max_paths", [-1, 0, None])
def test_dircache_unlimited_paths_retains_all_items(max_paths):
    dc = DirCache(max_paths=max_paths)
    for i in range(10):
        dc[f"k{i}"] = i

    assert len(dc) == 10
    assert dc["k0"] == 0
    assert dc["k9"] == 9
    assert len(list(dc)) == 10


@pytest.mark.parametrize("operation", ["get", "contains", "iter"])
def test_dircache_expired_entry(operation):
    dc = DirCache(listings_expiry_time=0.05)
    dc["a"] = 100
    dc["b"] = 200
    time.sleep(0.08)

    # Use a fresh cache for each operation: checking expiry can evict entries.
    if operation == "get":
        with pytest.raises(KeyError):
            dc["a"]
    elif operation == "contains":
        assert "a" not in dc
    else:
        assert list(dc) == []
        assert len(dc) == 0


def test_dircache_missing_key_not_in_cache_with_expiry():
    dc = DirCache(listings_expiry_time=10)

    is_present = "missing" in dc

    assert is_present is False


def test_dircache_disabled():
    dc = DirCache(use_listings_cache=False)
    dc["a"] = 1
    assert "a" not in dc
    assert len(dc) == 0
    assert list(dc) == []
    with pytest.raises(KeyError):
        dc["a"]


def test_dircache_del_removes_entry():
    dc = DirCache(listings_expiry_time=60, max_paths=10)
    dc["a"] = 1

    del dc["a"]

    assert "a" not in dc
    assert len(dc) == 0


def test_dircache_clear_removes_all_entries():
    dc = DirCache(listings_expiry_time=60, max_paths=10)
    dc["a"] = 1
    dc["b"] = 2

    dc.clear()

    assert len(dc) == 0
    assert "a" not in dc
    assert "b" not in dc


def test_dircache_pickle_roundtrip():
    dc = DirCache(use_listings_cache=True, listings_expiry_time=10, max_paths=5)

    dc2 = pickle.loads(pickle.dumps(dc))

    assert dc2.use_listings_cache is True
    assert dc2.listings_expiry_time == 10
    assert dc2.max_paths == 5


def test_dircache_invalidate_directory_removes_only_that_listing():
    dc = DirCache()
    dc["/a"] = ["/a/f"]
    dc["/a/b"] = ["/a/b/f"]
    dc["/a/b/c"] = []

    dc.invalidate_directory("/a/b")

    assert list(dc) == ["/a", "/a/b/c"]


def test_dircache_invalidate_subtree_respects_name_boundaries():
    dc = DirCache()
    for key in ["/a", "/a/b", "/a/b/c", "/a/bc", "/a/bc/d", "/other"]:
        dc[key] = []

    dc.invalidate_subtree("/a/b")

    # "/a/bc" shares a string prefix with "/a/b" but is not beneath it
    assert sorted(dc) == ["/a", "/a/bc", "/a/bc/d", "/other"]


@pytest.mark.parametrize("root", ["", "/"])
def test_dircache_invalidate_subtree_root_clears_everything(root):
    dc = DirCache()
    dc[""] = []
    dc["/a"] = []
    dc["a/b"] = []

    dc.invalidate_subtree(root)

    assert len(dc) == 0


def test_dircache_invalidate_subtree_trailing_separator():
    dc = DirCache()
    dc["/a"] = []
    dc["/a/b"] = []

    dc.invalidate_subtree("/a/")

    assert len(dc) == 0


def test_dircache_invalidate_with_backend_canonicalize():
    dc = DirCache()
    dc["bucket/a"] = []
    dc["bucket/a/b"] = []
    dc["bucket/ab"] = []

    def canonicalize(path):
        # backend-specific identity: protocol and trailing separators
        return path.removeprefix("s3://").rstrip("/")

    dc.invalidate_subtree("s3://bucket/a/", canonicalize=canonicalize)

    assert list(dc) == ["bucket/ab"]


def test_dircache_invalidate_ancestors_walks_chain_to_root():
    dc = DirCache()
    for key in ["", "a", "a/b", "a/b/c", "a/b/c/d", "a/other"]:
        dc[key] = []

    dc.invalidate_ancestors("a/b/c/d")

    # the whole containing chain is gone; the path's own listing and
    # unrelated listings stay
    assert sorted(dc) == ["a/b/c/d", "a/other"]


def test_dircache_invalidate_ancestors_custom_parent_stops_at_fixed_point():
    dc = DirCache()
    dc["/"] = []
    dc["/a"] = []
    dc["/a/b"] = []

    def parent(path):
        # like AbstractFileSystem._parent with root_marker="/": "/" is
        # its own parent, so the walk must terminate rather than spin
        return path.rsplit("/", 1)[0] or "/"

    dc.invalidate_ancestors("/a/b/f", parent=parent)

    assert len(dc) == 0


def test_dircache_invalidate_purges_expired_entries():
    dc = DirCache(listings_expiry_time=0.05)
    dc["old"] = []
    time.sleep(0.08)
    dc["new"] = []

    dc.invalidate_subtree("unrelated")

    # the expired entry was dropped by the invalidation pass, not kept alive
    assert "old" not in dc._cache
    assert "old" not in dc._times
    assert "new" in dc


def test_dircache_read_does_not_mutate_caller_list():
    dc = DirCache()
    original = [{"name": "a/f"}]
    dc["a"] = original

    # a reader mutating what it got back must not change the caller's list
    out = dc["a"]
    out.append({"name": "a/g"})
    out.reverse()

    assert original == [{"name": "a/f"}]


def test_dircache_caller_mutation_does_not_corrupt_cache():
    dc = DirCache()
    original = [{"name": "a/f"}]
    dc["a"] = original

    # the caller mutating its own list later must not change the cache
    original.append({"name": "a/h"})

    assert dc["a"] == [{"name": "a/f"}]
