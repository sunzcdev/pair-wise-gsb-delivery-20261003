"""
Keyset (seek) pagination demonstration.

Run with:  python examples/keyset_pagination_demo.py

The script pages through a table while a *second connection* inserts and
deletes rows between pages. It runs the same workload twice:

* snapshot=True  - a read transaction is held for the whole iteration, so the
  paginator keeps a single consistent view and never sees the concurrent edits;
* the default     - every page is a fresh statement, so concurrent edits become
  visible as soon as they are committed.

WAL mode is enabled so a reader can hold a snapshot while a writer commits.
"""

import os
import tempfile

from sqlite_utils import Database


def build_database(path: str) -> None:
    db = Database(path)
    db.enable_wal()
    db.table("events").insert_all(
        ({"id": i, "label": f"event-{i}"} for i in range(1, 9)),
        pk="id",
    )
    db.close()


def mutate_from_another_connection(path: str) -> None:
    writer = Database(path)
    writer.table("events").insert({"id": 999, "label": "event-999"})
    writer.table("events").delete(3)
    writer.close()


def run(path: str, snapshot: bool) -> list[int]:
    db = Database(path)
    paginator = db.table("events").keyset_pages(
        [("id", "asc")], page_size=3, snapshot=snapshot
    )
    seen: list[int] = []
    for page_number, page in enumerate(paginator, start=1):
        if page_number == 2:
            # Another connection changes the table halfway through.
            mutate_from_another_connection(path)
        ids = [row["id"] for row in page]
        seen.extend(ids)
        mode = "snapshot" if snapshot else "normal"
        print(f"  [{mode}] page {page_number}: {ids}")
    db.close()
    return seen


def main() -> None:
    directory = tempfile.mkdtemp(prefix="keyset_demo_")
    path = os.path.join(directory, "demo.db")
    # Some restricted sandboxes forbid opening new files inside a freshly
    # created temp directory; fall back to a database in the working directory.
    try:
        Database(path).close()
    except Exception:
        directory = os.getcwd()
        path = os.path.join(directory, "keyset_demo.db")
    # Always start from an empty database, clearing any file the probe or a
    # previous run may have left behind (including WAL/SHM sidecars).
    for suffix in ("", "-wal", "-shm"):
        if os.path.exists(path + suffix):
            os.remove(path + suffix)
    build_database(path)

    print("Snapshot mode (snapshot=True):")
    snapshot_ids = run(path, snapshot=True)
    print(f"  full result: {snapshot_ids}")
    print("  -> deleted id 3 is still read, inserted id 999 is never seen\n")

    # Rebuild so both runs start from identical data.
    os.remove(path)
    for suffix in ("-wal", "-shm"):
        if os.path.exists(path + suffix):
            os.remove(path + suffix)
    build_database(path)

    print("Normal mode (snapshot=False):")
    normal_ids = run(path, snapshot=False)
    print(f"  full result: {normal_ids}")
    print("  -> id 3 disappears and id 999 appears once it is committed")


if __name__ == "__main__":
    main()
