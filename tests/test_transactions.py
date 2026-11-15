#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

import pytest

from sqlparse import transactions
from sqlparse.transactions import (
    TransactionEventType as E,
    TransactionScanConfig,
    check_savepoint_order,
    scan_transactions,
)


def types(source, config=None):
    return [event.type for event in scan_transactions(source, config).events]


def test_basic_transaction_events():
    report = scan_transactions(
        "BEGIN;\n"
        "SAVEPOINT sp1;\n"
        "RELEASE SAVEPOINT sp1;\n"
        "SAVEPOINT sp2;\n"
        "ROLLBACK TO SAVEPOINT sp2;\n"
        "COMMIT;\n"
        "ROLLBACK;\n"
    )
    assert [event.type for event in report.events] == [
        E.BEGIN,
        E.SAVEPOINT,
        E.RELEASE,
        E.SAVEPOINT,
        E.ROLLBACK_TO_SAVEPOINT,
        E.COMMIT,
        E.ROLLBACK,
    ]
    assert report.issues == []


def test_savepoint_names():
    report = scan_transactions(
        'SAVEPOINT sp1; RELEASE sp1; SAVEPOINT "my sp"; '
        "ROLLBACK TO SAVEPOINT sp1;"
    )
    assert [event.name for event in report.events] == [
        "sp1",
        "sp1",
        "my sp",
        "sp1",
    ]


def test_rollback_to_without_savepoint_keyword():
    (event,) = scan_transactions("ROLLBACK TO sp1;").events
    assert event.type == E.ROLLBACK_TO_SAVEPOINT
    assert event.name == "sp1"


def test_rollback_work_to_savepoint():
    (event,) = scan_transactions("ROLLBACK WORK TO SAVEPOINT sp2;").events
    assert event.type == E.ROLLBACK_TO_SAVEPOINT
    assert event.name == "sp2"


def test_release_without_savepoint_keyword():
    (event,) = scan_transactions("RELEASE sp1;").events
    assert event.type == E.RELEASE
    assert event.name == "sp1"


def test_unknown_statements():
    assert types("SELECT 1; INSERT INTO t VALUES (1); \n") == [
        E.UNKNOWN,
        E.UNKNOWN,
    ]


def test_strings_are_not_events():
    (event,) = scan_transactions(
        "INSERT INTO t VALUES ('SAVEPOINT fake; ROLLBACK');"
    ).events
    assert event.type == E.UNKNOWN


def test_comments_are_not_events():
    report = scan_transactions(
        "-- SAVEPOINT nope;\n/* ROLLBACK TO nope; */\nSAVEPOINT real_one;"
    )
    assert [event.type for event in report.events] == [E.SAVEPOINT]
    assert report.events[0].name == "real_one"


def test_procedure_body_is_not_event():
    report = scan_transactions(
        "CREATE PROCEDURE p() BEGIN SAVEPOINT inner_sp; END;"
    )
    assert [event.type for event in report.events] == [E.UNKNOWN]
    assert report.issues == []


def test_positions():
    source = "SELECT 1;\n  SAVEPOINT sp1;\nCOMMIT;"
    report = scan_transactions(source)
    savepoint, commit = report.events[1], report.events[2]
    assert savepoint.statement_index == 1
    assert source[savepoint.offset:].startswith("SAVEPOINT")
    assert (savepoint.line, savepoint.column) == (2, 3)
    assert source[commit.offset:].startswith("COMMIT")
    assert (commit.line, commit.column) == (3, 1)


def test_start_transaction_requires_config():
    assert types("START TRANSACTION;") == [E.UNKNOWN]
    assert types(
        "START TRANSACTION;", TransactionScanConfig.postgresql()
    ) == [E.BEGIN]
    assert types("START TRANSACTION;", TransactionScanConfig.mysql()) == [
        E.BEGIN
    ]


def test_begin_variants():
    assert types("BEGIN; BEGIN WORK; BEGIN TRANSACTION;") == [E.BEGIN] * 3


def test_sqlserver_config_excludes_bare_begin():
    config = TransactionScanConfig.sqlserver()
    assert types("BEGIN TRAN;", config) == [E.BEGIN]
    assert types("BEGIN TRANSACTION;", config) == [E.BEGIN]
    assert types("BEGIN;", config) == [E.UNKNOWN]


def test_custom_starter_config():
    config = TransactionScanConfig(transaction_starters=(("START", "TRAN"),))
    assert types("START TRAN;", config) == [E.BEGIN]
    assert types("BEGIN;", config) == [E.UNKNOWN]


def test_unmatched_release_and_rollback_to():
    report = scan_transactions(
        "BEGIN; SAVEPOINT a; RELEASE b; ROLLBACK TO c; COMMIT;"
    )
    assert [(issue.name, issue.event.type) for issue in report.issues] == [
        ("b", E.RELEASE),
        ("c", E.ROLLBACK_TO_SAVEPOINT),
    ]
    assert "b" in report.issues[0].message


def test_savepoint_not_active_after_release():
    report = scan_transactions(
        "SAVEPOINT a; SAVEPOINT b; RELEASE a; ROLLBACK TO b;"
    )
    # Releasing "a" also discards "b" which was defined after it.
    assert [issue.name for issue in report.issues] == ["b"]


def test_savepoint_not_active_after_rollback_to():
    report = scan_transactions(
        "SAVEPOINT a; SAVEPOINT b; ROLLBACK TO a; RELEASE b;"
    )
    # Rolling back to "a" discards "b", but "a" itself stays active.
    assert [issue.name for issue in report.issues] == ["b"]


def test_savepoints_cleared_by_commit_and_rollback():
    report = scan_transactions(
        "SAVEPOINT a; COMMIT; RELEASE a; SAVEPOINT b; ROLLBACK; RELEASE b;"
    )
    assert [issue.name for issue in report.issues] == ["a", "b"]


def test_savepoint_name_matching_is_case_insensitive():
    report = scan_transactions("SAVEPOINT Sp1; RELEASE SP1;")
    assert report.issues == []


def test_savepoint_redefinition():
    report = scan_transactions(
        "SAVEPOINT a; SAVEPOINT b; SAVEPOINT a; RELEASE a; RELEASE b;"
    )
    # Redefining "a" moves it after "b", so releasing "a" keeps "b".
    assert report.issues == []


def test_check_savepoint_order_ignores_unknown_and_unnamed():
    report = scan_transactions("SELECT 1; SAVEPOINT; RELEASE;")
    assert report.issues == []


def test_check_savepoint_order_accepts_iterable():
    report = scan_transactions("RELEASE x;")
    assert check_savepoint_order(iter(report.events))


def test_empty_source():
    report = scan_transactions("")
    assert report.events == []
    assert report.issues == []


def test_module_is_exposed():
    assert transactions.scan_transactions is scan_transactions


@pytest.mark.parametrize("keyword", ["COMMIT", "ROLLBACK"])
def test_commit_rollback_with_optional_work(keyword):
    (event,) = scan_transactions(f"{keyword} WORK;").events
    assert event.type == E(keyword)
