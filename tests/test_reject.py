import pytest

from sqlite_utils import RejectedRow
from sqlite_utils.utils import sqlite3


def _table_with_constraints(db):
    db.execute(
        "create table t ("
        "id integer primary key, "
        "value text not null"
        ")"
    )
    return db.table("t")


def test_reject_records_details_and_continues(fresh_db):
    table = _table_with_constraints(fresh_db)

    table.insert_all(
        [
            {"id": 1, "value": "one"},
            {"id": 2, "value": None},
            {"id": 3, "value": "three"},
        ],
        reject=True,
    )

    assert list(table.rows) == [
        {"id": 1, "value": "one"},
        {"id": 3, "value": "three"},
    ]
    assert len(table.rejections) == 1
    rejection = table.rejections[0]
    assert isinstance(rejection, RejectedRow)
    assert rejection.index == 1
    assert rejection.row_index == 1
    assert rejection.row_number == 2
    assert rejection.pk == 2
    assert rejection["primary_key"] == 2
    assert rejection.error_type == "IntegrityError"
    assert rejection.error_category == "SQLITE_CONSTRAINT_NOTNULL"
    assert rejection.constraint_type == "NOTNULL"
    assert rejection.message == "NOT NULL constraint failed: t.value"
    assert table.rejected is table.rejections
    assert table.rejected_rows is table.rejections


def test_reject_is_opt_in_and_default_rolls_back_chunk(fresh_db):
    table = _table_with_constraints(fresh_db)

    with pytest.raises(sqlite3.IntegrityError):
        table.insert_all(
            [
                {"id": 1, "value": "one"},
                {"id": 2, "value": None},
            ]
        )

    assert table.count == 0
    assert table.rejections == []


def test_reject_does_not_swallow_operational_errors(fresh_db):
    table = _table_with_constraints(fresh_db)

    with pytest.raises(sqlite3.OperationalError):
        table.insert_all(
            [{"id": 1, "value": "one"}],
            conversions={"value": "not valid sql(?)"},
            reject=True,
        )

    assert table.count == 0
    assert table.rejections == []


def test_reject_does_not_rerun_conversions(fresh_db):
    table = _table_with_constraints(fresh_db)
    calls = []

    def record_conversion(value):
        calls.append(value)
        return value

    fresh_db.register_function(record_conversion, name="record_conversion")
    table.insert_all(
        [
            {"id": 1, "value": "one"},
            {"id": 2, "value": None},
            {"id": 3, "value": "three"},
        ],
        conversions={"value": "record_conversion(?)"},
        reject=True,
    )

    assert calls == ["one", None, "three"]
    assert [(row["id"], row["value"]) for row in table.rows] == [
        (1, "one"),
        (3, "three"),
    ]


def test_reject_compound_primary_key_and_same_batch_duplicate(fresh_db):
    fresh_db.execute(
        "create table t ("
        "a integer, "
        "b text, "
        "value text, "
        "primary key (a, b)"
        ")"
    )
    table = fresh_db.table("t")

    table.insert_all(
        [
            {"a": 1, "b": "x", "value": "first"},
            {"a": 2, "b": "y", "value": "second"},
            {"a": 1, "b": "x", "value": "duplicate"},
        ],
        pk=("a", "b"),
        reject=True,
    )

    assert list(table.rows) == [
        {"a": 1, "b": "x", "value": "first"},
        {"a": 2, "b": "y", "value": "second"},
    ]
    assert len(table.rejections) == 1
    assert table.rejections[0].index == 2
    assert table.rejections[0].pk == (1, "x")
    assert table.rejections[0].constraint_type in {"PRIMARYKEY", "UNIQUE"}


def test_reject_list_mode_uses_data_row_index(fresh_db):
    table = _table_with_constraints(fresh_db)

    table.insert_all(
        [
            ["id", "value"],
            [1, "one"],
            [2, None],
            [3, "three"],
        ],
        reject=True,
    )

    assert table.rejections[0].index == 1
    assert table.rejections[0].input_index == 2
    assert table.rejections[0].pk == 2


def test_reject_honors_outer_transaction_rollback(fresh_db):
    table = _table_with_constraints(fresh_db)

    with pytest.raises(RuntimeError), fresh_db.atomic():
        table.insert_all(
            [
                {"id": 1, "value": "one"},
                {"id": 2, "value": None},
            ],
            reject=True,
        )
        assert table.count == 1
        assert len(table.rejections) == 1
        raise RuntimeError("roll back everything")

    assert table.count == 0
    assert not fresh_db.conn.in_transaction


def test_reject_rolls_back_failed_row_savepoint(fresh_db):
    table = _table_with_constraints(fresh_db)
    fresh_db.execute(
        """
        create trigger reject_bad_value after insert on t
        when new.value = 'bad'
        begin
            select raise(abort, 'bad value');
        end;
        """
    )

    table.insert_all(
        [
            {"id": 1, "value": "one"},
            {"id": 2, "value": "bad"},
            {"id": 3, "value": "three"},
        ],
        reject=True,
    )

    assert list(table.rows) == [
        {"id": 1, "value": "one"},
        {"id": 3, "value": "three"},
    ]
    assert table.rejections[0].pk == 2
