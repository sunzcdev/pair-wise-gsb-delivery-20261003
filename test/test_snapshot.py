import pytest

import dataset
from dataset import SnapshotError


def rows(source, skus):
    return [
        {"source": source, "sku": sku, "price": i * 10} for i, sku in enumerate(skus)
    ]


@pytest.fixture(scope="function")
def catalog(db):
    tbl = db["catalog"]
    tbl.insert_many(rows("shop-a", ["a1", "a2", "a3"]))
    tbl.insert_many(rows("shop-b", ["b1", "b2"]))
    return tbl


def test_sync_snapshot_basic(db):
    tbl = db["items"]
    count = tbl.sync_snapshot(
        [{"sku": "x1", "price": 5}, {"sku": "x2", "price": 7}],
        ["sku"],
        scope={"source": "shop-a"},
    )
    assert count == 2
    assert tbl.count(source="shop-a") == 2
    assert tbl.find_one(sku="x1")["price"] == 5


def test_sync_snapshot_removes_stale_rows(catalog):
    catalog.sync_snapshot(
        rows("shop-a", ["a1", "a9"]), ["sku"], scope={"source": "shop-a"}
    )
    skus = {r["sku"] for r in catalog.find(source="shop-a")}
    assert skus == {"a1", "a9"}


def test_sync_snapshot_leaves_other_scopes_untouched(catalog):
    before = list(catalog.find(source="shop-b"))
    catalog.sync_snapshot(rows("shop-a", ["a1"]), ["sku"], scope={"source": "shop-a"})
    after = list(catalog.find(source="shop-b"))
    assert before == after
    assert catalog.count(source="shop-b") == 2


def test_sync_snapshot_preserves_identity(catalog):
    before = {r["sku"]: r["id"] for r in catalog.find(source="shop-a")}
    catalog.sync_snapshot(
        [{"sku": "a1", "price": 999}, {"sku": "a2", "price": 888}],
        ["sku"],
        scope={"source": "shop-a"},
    )
    after = {r["sku"]: r["id"] for r in catalog.find(source="shop-a")}
    assert after["a1"] == before["a1"]
    assert after["a2"] == before["a2"]
    assert catalog.find_one(sku="a1")["price"] == 999


def test_sync_snapshot_repeat_is_idempotent(catalog):
    snapshot = rows("shop-a", ["a1", "a2"])
    catalog.sync_snapshot(snapshot, ["sku"], scope={"source": "shop-a"})
    first = list(catalog.find(source="shop-a", order_by="id"))
    catalog.sync_snapshot(snapshot, ["sku"], scope={"source": "shop-a"})
    second = list(catalog.find(source="shop-a", order_by="id"))
    assert catalog.count(source="shop-a") == 2
    assert first == second


def test_sync_snapshot_duplicate_key_aborts(catalog):
    before = list(catalog.find(source="shop-a", order_by="id"))
    dupe = [{"sku": "a1", "price": 1}, {"sku": "a1", "price": 2}]
    with pytest.raises(SnapshotError):
        catalog.sync_snapshot(dupe, ["sku"], scope={"source": "shop-a"})
    after = list(catalog.find(source="shop-a", order_by="id"))
    assert before == after


def test_sync_snapshot_missing_key_aborts(catalog):
    before = list(catalog.find(source="shop-a", order_by="id"))
    with pytest.raises(SnapshotError):
        catalog.sync_snapshot([{"price": 1}], ["sku"], scope={"source": "shop-a"})
    after = list(catalog.find(source="shop-a", order_by="id"))
    assert before == after


def test_sync_snapshot_source_failure_keeps_old_data(catalog):
    before = list(catalog.find(source="shop-a", order_by="id"))

    def broken_source():
        yield {"sku": "a1", "price": 1}
        raise RuntimeError("source blew up midway")

    with pytest.raises(RuntimeError):
        catalog.sync_snapshot(broken_source(), ["sku"], scope={"source": "shop-a"})
    after = list(catalog.find(source="shop-a", order_by="id"))
    assert before == after


def test_sync_snapshot_chunked_staging(db):
    tbl = db["bulk"]
    data = [{"sku": f"s{i}", "price": i} for i in range(250)]
    count = tbl.sync_snapshot(data, ["sku"], scope={"source": "s"}, chunk_size=10)
    assert count == 250
    assert tbl.count(source="s") == 250
    # Re-sync a subset in chunks: stale rows are removed, no duplicates.
    tbl.sync_snapshot(data[:100], ["sku"], scope={"source": "s"}, chunk_size=10)
    assert tbl.count(source="s") == 100


def test_sync_snapshot_empty_clears_scope(catalog):
    catalog.sync_snapshot([], ["sku"], scope={"source": "shop-a"})
    assert catalog.count(source="shop-a") == 0
    assert catalog.count(source="shop-b") == 2


def test_sync_snapshot_composite_keys(db):
    tbl = db["composite"]
    data = [
        {"region": "eu", "sku": "x1", "price": 1},
        {"region": "us", "sku": "x1", "price": 2},
    ]
    tbl.sync_snapshot(data, ["region", "sku"], scope={"source": "s"})
    assert tbl.count(source="s") == 2
    tbl.sync_snapshot(data[:1], ["region", "sku"], scope={"source": "s"})
    assert tbl.count(source="s") == 1
    assert tbl.find_one(source="s")["region"] == "eu"


def test_snapshot_sync_context_manager(catalog):
    with catalog.snapshot_sync(
        ["sku"], scope={"source": "shop-a"}, chunk_size=2
    ) as sync:
        for sku in ["a2", "a4", "a5"]:
            sync.add({"sku": sku, "price": 3})
    assert sync.count == 3
    skus = {r["sku"] for r in catalog.find(source="shop-a")}
    assert skus == {"a2", "a4", "a5"}


def test_snapshot_sync_requires_keys(db):
    tbl = db["nokeys"]
    with pytest.raises(SnapshotError):
        tbl.snapshot_sync([], scope={"source": "s"})


def test_sync_snapshot_exported():
    assert dataset.SnapshotSync is not None
    assert issubclass(dataset.SnapshotError, dataset.DatasetError)
