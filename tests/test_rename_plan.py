import pytest

from sqlite_utils.db import RenamePlanError, TransformError


def test_rename_plan_lists_direct_and_transitive_dependencies(fresh_db):
    fresh_db.executescript(
        """
        create table t (id integer primary key, old text, note text);
        create table other (old text, value text);
        create view direct_view as
            select t.old, note, 'old' as literal, -- old
                   other.old as other_old
            from t join other;
        create view transitive_view as select old from direct_view;
        create index idx_t_old on t(old);
        create index idx_other_old on other(old);
        create trigger trigger_old after update of old on t
        begin
            insert into other(old) values (new.old);
        end;
        """
    )
    fresh_db["t"].insert({"id": 1, "old": "one", "note": "note"})
    fresh_db["other"].insert({"old": "two", "value": "value"})

    schema_before = fresh_db.execute(
        "select type, name, sql from sqlite_master order by type, name"
    ).fetchall()
    plan = fresh_db["t"].rename_column_plan("old", "new")

    assert plan.supported
    assert plan.affected_objects == [
        "idx_t_old",
        "direct_view",
        "trigger_old",
        "transitive_view",
    ]
    assert plan.direct_dependencies == [
        "idx_t_old",
        "direct_view",
        "trigger_old",
    ]
    assert plan.transitive_dependencies == ["transitive_view"]
    assert fresh_db.execute(
        "select type, name, sql from sqlite_master order by type, name"
    ).fetchall() == schema_before

    direct_sql = next(
        action.sql_after
        for action in plan.actions
        if action.object_name == "direct_view"
    )
    assert "'old' as literal" in direct_sql
    assert "-- old" in direct_sql
    assert "other.old" in direct_sql
    assert "other_old" in direct_sql

    plan.apply()
    assert fresh_db["t"].columns_dict == {
        "id": int,
        "new": str,
        "note": str,
    }
    assert list(fresh_db.query("select * from transitive_view")) == [
        {"new": "one"}
    ]
    assert [
        (index.name, index.columns) for index in fresh_db["t"].indexes
    ] == [("idx_t_old", ["new"])]
    assert fresh_db["other"].columns_dict == {"old": str, "value": str}


def test_rename_plan_rejects_schema_change_after_plan(fresh_db):
    fresh_db["t"].insert({"old": "one"})
    plan = fresh_db["t"].rename_column_plan("old", "new")
    fresh_db.execute("create index idx_later on t(old)")

    with pytest.raises(RenamePlanError, match="Schema has changed"):
        plan.apply()

    assert fresh_db["t"].columns_dict == {"old": str}


def test_rename_plan_refuses_unsupported_index_before_changes(fresh_db):
    fresh_db["t"].insert({"old": "one"})
    fresh_db.execute("create index idx_expr on t(lower(old))")
    schema_before = fresh_db.execute(
        "select type, name, sql from sqlite_master order by type, name"
    ).fetchall()

    plan = fresh_db["t"].rename_column_plan("old", "new")

    assert not plan.supported
    assert plan.unsupported[0].object_name == "idx_expr"
    assert "expression index" in plan.unsupported[0].reason
    with pytest.raises(RenamePlanError, match="no changes have been applied"):
        plan.apply()
    assert fresh_db.execute(
        "select type, name, sql from sqlite_master order by type, name"
    ).fetchall() == schema_before


def test_rename_plan_failure_rolls_back_every_object(fresh_db, monkeypatch):
    fresh_db["t"].insert({"old": "one"})
    fresh_db.execute("create view v as select old from t")
    plan = fresh_db["t"].rename_column_plan("old", "new")
    schema_before = fresh_db.execute(
        "select type, name, sql from sqlite_master order by type, name"
    ).fetchall()

    original_execute = fresh_db.execute

    def failing_execute(sql, parameters=None):
        if sql.startswith("CREATE VIEW"):
            raise RuntimeError("simulated failure")
        return original_execute(sql, parameters)

    monkeypatch.setattr(fresh_db, "execute", failing_execute)
    with pytest.raises(RuntimeError, match="simulated failure"):
        plan.apply()

    assert fresh_db.execute(
        "select type, name, sql from sqlite_master order by type, name"
    ).fetchall() == schema_before
    assert plan.validate() is None


def test_transform_with_plan_allows_other_changes(fresh_db):
    fresh_db["t"].insert({"old": "one", "value": "1"})
    fresh_db.execute("create view v as select old, value from t")
    plan = fresh_db["t"].rename_column_plan("old", "new")

    fresh_db["t"].transform(
        types={"value": int}, rename={"old": "new"}, plan=plan
    )

    assert fresh_db["t"].columns_dict == {"new": str, "value": int}
    assert list(fresh_db.query("select * from v")) == [
        {"new": "one", "value": 1}
    ]


def test_original_transform_rename_still_works_without_plan(fresh_db):
    fresh_db["t"].insert({"old": "one"})
    fresh_db["t"].transform(rename={"old": "new"})
    assert fresh_db["t"].columns_dict == {"new": str}


def test_rename_plan_supports_swapping_column_names(fresh_db):
    fresh_db.execute("create table t (alpha text, beta text)")
    fresh_db.execute(
        "create view v as select alpha, beta from t"
    )
    fresh_db["t"].insert({"alpha": "a", "beta": "b"})
    plan = fresh_db["t"].rename_column_plan(
        {"alpha": "beta", "beta": "alpha"}
    )

    assert plan.supported
    plan.apply()

    assert fresh_db["t"].columns_dict == {"beta": str, "alpha": str}
    assert list(fresh_db.query("select * from v")) == [{"beta": "a", "alpha": "b"}]


def test_rename_plan_recreates_trigger_that_references_a_view(fresh_db):
    fresh_db.executescript(
        """
        create table t (old text, note text);
        create table log (message text);
        create view v as select old, note from t;
        create trigger tr after insert on t
        begin
            insert into log select note from v where note = new.note;
        end;
        """
    )
    fresh_db["t"].insert({"old": "one", "note": "note"})
    plan = fresh_db["t"].rename_column_plan("old", "new")

    assert [(a.object_type, a.object_name) for a in plan.actions] == [
        ("table", "t"),
        ("view", "v"),
        ("trigger", "tr"),
    ]
    plan.apply()

    fresh_db["t"].insert({"new": "two", "note": "second"})
    assert list(fresh_db["log"].rows) == [
        {"message": "note"},
        {"message": "second"},
    ]


def test_rename_plan_error_is_transform_error(fresh_db):
    fresh_db["t"].insert({"old": "one"})
    plan = fresh_db["t"].rename_column_plan("old", "new")
    fresh_db.execute("create table later (id integer)")
    with pytest.raises(TransformError):
        plan.apply()
