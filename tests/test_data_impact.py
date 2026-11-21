from sqlalchemy import Column
from sqlalchemy import Integer
from sqlalchemy import MetaData
from sqlalchemy import Numeric
from sqlalchemy import String
from sqlalchemy import Table
from sqlalchemy import UniqueConstraint

from alembic import autogenerate
from alembic.migration import MigrationContext
from alembic.operations import DataImpact
from alembic.operations import DataImpactDirection
from alembic.operations import DataImpactReport
from alembic.operations import DataImpactType
from alembic.operations import inspect_data_impact
from alembic.operations import MigrateOperation
from alembic.operations import Operations
from alembic.operations import ops
from alembic.testing import assert_raises
from alembic.testing import eq_
from alembic.testing import TestBase
from alembic.testing.fixtures import op_fixture


def _script(upgrade_ops, downgrade_ops):
    return ops.MigrationScript(
        "abc123",
        ops.UpgradeOps(list(upgrade_ops)),
        ops.DowngradeOps(list(downgrade_ops)),
    )


class DataImpactBasicsTest(TestBase):
    def test_column_impact_defaults(self):
        impact = DataImpact.for_column(
            DataImpactType.DROP_COLUMN, "account", "data"
        )
        eq_(impact.table, "account")
        eq_(impact.column, "data")
        eq_(impact.loses_data, True)
        eq_(impact.identity_change, False)

    def test_rename_identity_change_retains_values(self):
        impact = DataImpact.for_column(
            DataImpactType.RENAME_COLUMN,
            "account",
            "old",
            new_name="new",
        )
        eq_(impact.new_name, "new")
        eq_(impact.identity_change, True)
        eq_(impact.loses_data, False)

    def test_explicit_unknown_loses_data(self):
        impact = DataImpact.for_column(
            DataImpactType.ADD_COLUMN,
            "account",
            "data",
            loses_data=None,
        )
        eq_(impact.loses_data, None)

    def test_type_impact_default_unknown(self):
        impact = DataImpact.for_column(
            DataImpactType.ALTER_COLUMN_TYPE, "account", "data"
        )
        eq_(impact.loses_data, None)

    def test_table_label_with_schema(self):
        impact = DataImpact.for_table(
            DataImpactType.CREATE_TABLE, "account", schema="s"
        )
        eq_(impact.table_label, "s.account")

    def test_render_text(self):
        impact = DataImpact.for_column(
            DataImpactType.DROP_COLUMN, "account", "data"
        )
        assert "'account'" in impact.render()
        assert "'data'" in impact.render()
        assert "loses original values" in impact.render()


class DataImpactReportTest(TestBase):
    def test_two_directions(self):
        script = _script(
            [
                ops.AddColumnOp("account", Column("name", String(50))),
                ops.AddColumnOp("account", Column("data", String(50))),
            ],
            [
                ops.DropColumnOp("account", "data"),
                ops.DropColumnOp("account", "name"),
            ],
        )
        report = inspect_data_impact(script)
        eq_(report.upgrade.name, "upgrade")
        eq_(report.downgrade.name, "downgrade")
        eq_(report.loses_data, {"upgrade": False, "downgrade": True})

        eq_(
            [impact.type for impact in report.upgrade.impacts],
            [DataImpactType.ADD_COLUMN, DataImpactType.ADD_COLUMN],
        )
        eq_(
            [impact.column for impact in report.downgrade.impacts],
            ["data", "name"],
        )

        eq_([d.name for d in report.losing_directions()], ["downgrade"])
        assert "upgrade: preserves original values" in report.render()
        assert "downgrade: loses original values" in report.render()

    def test_only_one_direction_marks_other_unavailable(self):
        upgrade = ops.UpgradeOps(
            [ops.DropColumnOp("account", "data")]
        )
        report = inspect_data_impact(upgrade)
        eq_(report.upgrade.name, "upgrade")
        eq_(report.upgrade.provided, True)
        eq_(report.downgrade.provided, False)
        eq_(report.downgrade.loses_data, None)
        eq_(report.loses_data, {"upgrade": True, "downgrade": None})
        assert "downgrade: not available" in report.render()

    def test_downgrade_only(self):
        downgrade = ops.DowngradeOps(
            [ops.DropTableOp("gone")]
        )
        report = inspect_data_impact(downgrade)
        eq_(report.downgrade.provided, True)
        eq_(report.upgrade.provided, False)
        eq_(report.losing_directions(), [report.downgrade])

    def test_invalid_subject(self):
        assert_raises(TypeError, inspect_data_impact, "not an op")

    def test_group_by_table(self):
        script = _script(
            [
                ops.ModifyTableOps(
                    "a",
                    [ops.AddColumnOp("a", Column("x", Integer))],
                ),
                ops.ModifyTableOps(
                    "b",
                    [ops.AddColumnOp("b", Column("y", Integer))],
                ),
            ],
            [],
        )
        report = inspect_data_impact(script)
        grouped = report.upgrade.by_table()
        eq_(list(grouped), ["a", "b"])
        eq_(len(report.upgrade.for_table("a")), 1)

    def test_report_type(self):
        report = inspect_data_impact(_script([], []))
        assert isinstance(report, DataImpactReport)
        assert isinstance(report.upgrade, DataImpactDirection)

    def test_empty_script_has_no_impact(self):
        report = inspect_data_impact(_script([], []))
        eq_(report.upgrade.impacts, ())
        eq_(report.downgrade.impacts, ())
        eq_(report.loses_data, {"upgrade": False, "downgrade": False})


class DataImpactBatchTest(TestBase):
    def test_batch_container_is_expanded(self):
        script = _script(
            [
                ops.ModifyTableOps(
                    "account",
                    [
                        ops.AddColumnOp("account", Column("x", Integer)),
                        ops.DropColumnOp("account", "old"),
                    ],
                )
            ],
            [],
        )
        report = inspect_data_impact(script)
        eq_(
            [
                (impact.type, impact.column)
                for impact in report.upgrade.impacts
            ],
            [
                (DataImpactType.ADD_COLUMN, "x"),
                (DataImpactType.DROP_COLUMN, "old"),
            ],
        )
        eq_(report.upgrade.loses_data, True)

    def test_nested_rename_inside_batch(self):
        script = _script(
            [],
            [
                ops.ModifyTableOps(
                    "account",
                    [
                        ops.AlterColumnOp(
                            "account",
                            "oldname",
                            modify_name="newname",
                        )
                    ],
                )
            ],
        )
        report = inspect_data_impact(script)
        (impact,) = report.downgrade.impacts
        eq_(impact.type, DataImpactType.RENAME_COLUMN)
        eq_(impact.column, "oldname")
        eq_(impact.new_name, "newname")
        eq_(impact.identity_change, True)
        eq_(impact.loses_data, False)

    def test_batch_without_table_name_uses_container(self):
        container = ops.ModifyTableOps(
            "account",
            [ops.AddColumnOp("account", Column("x", Integer))],
        )
        report = inspect_data_impact(container)
        eq_(report.upgrade.impacts[0].table, "account")

    def test_batch_container_schema_is_inherited(self):
        container = ops.ModifyTableOps(
            "account",
            [ops.AddColumnOp("account", Column("x", Integer))],
            schema="s",
        )
        report = inspect_data_impact(container)
        impact = report.upgrade.impacts[0]
        eq_(impact.schema, "s")
        eq_(impact.table_label, "s.account")

    def test_batch_constraint_uses_container_table(self):
        md = MetaData()
        Table(
            "account",
            md,
            Column("id", Integer, primary_key=True),
            Column("name", String(50)),
            UniqueConstraint("name", name="uq_account_name"),
        )
        uq = [
            c
            for c in md.tables["account"].constraints
            if c.name == "uq_account_name"
        ][0]
        # the batch form builds the op without a table name
        op = ops.AddConstraintOp.from_constraint(uq)
        container = ops.ModifyTableOps("account", [op], schema="s")
        report = inspect_data_impact(container)
        impact = report.upgrade.impacts[0]
        eq_(impact.table, "account")
        eq_(impact.schema, "s")


class _CreateViewOp(MigrateOperation):
    """custom operation that does not declare a data impact"""

    def __init__(self, view_name, selectable=None):
        self.view_name = view_name
        self.selectable = selectable


class _DeclaredOp(MigrateOperation):
    """custom operation that declares its own impact"""

    def __init__(self, table_name, loses):
        self.table_name = table_name
        self.loses = loses

    def data_impact(self):
        return [
            DataImpact.for_table(
                DataImpactType.DROP_COLUMN,
                self.table_name,
                detail="declared by custom op",
                loses_data=self.loses,
            )
        ]


class DataImpactCustomOpTest(TestBase):
    def test_undeclared_custom_op_is_unknown(self):
        script = _script(
            [ops.ModifyTableOps("t", [_CreateViewOp("v")])],
            [],
        )
        report = inspect_data_impact(script)
        (impact,) = report.upgrade.impacts
        eq_(impact.type, DataImpactType.UNKNOWN)
        eq_(impact.loses_data, None)
        eq_(impact.table, "t")
        eq_(report.upgrade.loses_data, None)
        eq_(report.upgrade.may_lose_data, True)
        assert "data impact unknown" in report.render()

    def test_declared_custom_op_uses_declaration(self):
        script = _script(
            [_DeclaredOp("t", loses=True)],
            [],
        )
        report = inspect_data_impact(script)
        (impact,) = report.upgrade.impacts
        eq_(impact.loses_data, True)
        eq_(impact.detail, "declared by custom op")
        eq_(report.upgrade.loses_data, True)

    def test_declared_custom_op_can_preserve(self):
        report = inspect_data_impact(_DeclaredOp("t", loses=False))
        eq_(report.upgrade.loses_data, False)

    def test_declaring_empty_sequence_means_no_impact(self):
        class _NoImpactOp(MigrateOperation):
            def data_impact(self):
                return []

        report = inspect_data_impact(_NoImpactOp())
        eq_(report.upgrade.impacts, ())
        eq_(report.upgrade.loses_data, False)

    def test_base_data_impact_is_none(self):
        eq_(MigrateOperation().data_impact(), None)


class DataImpactTypeChangeTest(TestBase):
    def _type_impact(self, existing, new):
        op = ops.AlterColumnOp(
            "t",
            "c",
            existing_type=existing,
            modify_type=new,
        )
        report = inspect_data_impact(op)
        (impact,) = report.upgrade.impacts
        eq_(impact.type, DataImpactType.ALTER_COLUMN_TYPE)
        return impact

    def test_narrowing_string_loses_data(self):
        impact = self._type_impact(String(255), String(10))
        eq_(impact.loses_data, True)
        assert "VARCHAR(255) -> VARCHAR(10)" == impact.detail

    def test_widening_string_preserves(self):
        impact = self._type_impact(String(10), String(255))
        eq_(impact.loses_data, False)

    def test_same_affinity_numeric_narrowing(self):
        from sqlalchemy import Numeric

        impact = self._type_impact(Numeric(10, 2), Numeric(5, 2))
        eq_(impact.loses_data, True)

    def test_affinity_change_is_unknown(self):
        impact = self._type_impact(String(50), Integer())
        eq_(impact.loses_data, None)

    def test_missing_existing_type_is_unknown(self):
        impact = self._type_impact(None, String(50))
        eq_(impact.loses_data, None)

    def test_no_type_change_no_impact(self):
        op = ops.AlterColumnOp(
            "t", "c", existing_type=String(50), modify_nullable=False
        )
        report = inspect_data_impact(op)
        eq_(
            [impact.type for impact in report.upgrade.impacts],
            [DataImpactType.ALTER_COLUMN_NULLABILITY],
        )


class DataImpactMirrorTest(TestBase):
    def test_upgrade_and_downgrade_differ_on_drop(self):
        """the same script reports a loss in exactly one direction"""

        script = _script(
            [ops.DropColumnOp("account", "data")],
            [ops.AddColumnOp("account", Column("data", String(50)))],
        )
        report = inspect_data_impact(script)
        eq_(report.upgrade.loses_data, True)
        eq_(report.downgrade.loses_data, False)
        eq_([d.name for d in report.losing_directions()], ["upgrade"])

    def test_reverse_of_autogenerate_script(self):
        upgrade = ops.UpgradeOps(
            [
                ops.ModifyTableOps(
                    "account",
                    [
                        ops.AddColumnOp(
                            "account", Column("x", String(50))
                        ),
                    ],
                )
            ]
        )
        downgrade = upgrade.reverse()
        script = _script(upgrade.ops, downgrade.ops)
        report = inspect_data_impact(script)
        eq_(report.upgrade.loses_data, False)
        eq_(report.downgrade.loses_data, True)

    def test_symmetric_add_drop_column(self):
        script = ops.MigrationScript(
            "abc",
            ops.UpgradeOps([ops.AddColumnOp("t", Column("c", Integer))]),
            ops.DowngradeOps([ops.DropColumnOp("t", "c")]),
        )
        report = ops.inspect_data_impact(script)
        eq_(report.upgrade.loses_data, False)
        eq_(report.downgrade.loses_data, True)


class DataImpactReadOnlyTest(TestBase):
    def test_inspection_does_not_mutate_tree(self):
        script = _script(
            [
                ops.ModifyTableOps(
                    "account",
                    [
                        ops.AddColumnOp("account", Column("x", Integer)),
                        ops.DropColumnOp("account", "old"),
                    ],
                )
            ],
            [ops.DropTableOp("gone")],
        )
        before = [
            (type(op).__name__, getattr(op, "table_name", None))
            for op in script.upgrade_ops.ops
        ]
        before_down = [
            type(op).__name__ for op in script.downgrade_ops.ops
        ]
        inspect_data_impact(script)
        after = [
            (type(op).__name__, getattr(op, "table_name", None))
            for op in script.upgrade_ops.ops
        ]
        eq_(before, after)
        eq_(
            before_down,
            [type(op).__name__ for op in script.downgrade_ops.ops],
        )

    def test_render_sql_still_matches_original_ops(self):
        script = _script(
            [
                ops.ModifyTableOps(
                    "account",
                    [ops.DropColumnOp("account", "data")],
                )
            ],
            [ops.AddColumnOp("account", Column("data", String(50)))],
        )

        report = inspect_data_impact(script)
        eq_(report.downgrade.loses_data, False)

        # rendering the very same operations still yields the original
        # directives; the report did not consume or alter them
        rendered_upgrade = autogenerate.render_python_code(
            script.upgrade_ops
        )
        assert "op.drop_column('account', 'data')" in rendered_upgrade

        rendered_downgrade = autogenerate.render_python_code(
            script.downgrade_ops
        )
        assert "op.add_column('account'" in rendered_downgrade

    def test_report_does_not_remove_ops(self):
        upgrade = ops.UpgradeOps([ops.DropColumnOp("t", "c")])
        eq_(len(upgrade.ops), 1)
        inspect_data_impact(upgrade)
        eq_(len(upgrade.ops), 1)
        eq_(upgrade.ops[0].column_name, "c")

    def test_operations_still_invokable_after_report(self):
        context = op_fixture("postgresql")
        drop_op = ops.DropColumnOp("account", "data")
        report = inspect_data_impact(drop_op)
        eq_(report.upgrade.loses_data, True)
        Operations(context).invoke(drop_op)
        context.assert_("ALTER TABLE account DROP COLUMN data")


class DataImpactAutogenerateTest(TestBase):
    def _produce(self, metadata, conn_metadata):
        from sqlalchemy import create_engine

        engine = create_engine("sqlite://")
        with engine.begin() as conn:
            conn_metadata.create_all(conn)
        context = MigrationContext.configure(engine.connect())
        return autogenerate.api.produce_migrations(context, metadata)

    def test_produced_migration_report(self):
        conn_md = MetaData()
        Table("account", conn_md, Column("id", Integer, primary_key=True))

        new_md = MetaData()
        Table(
            "account",
            new_md,
            Column("id", Integer, primary_key=True),
            Column("name", String(50)),
        )

        script = self._produce(new_md, conn_md)
        report = inspect_data_impact(script)
        eq_(report.upgrade.loses_data, False)
        eq_(report.downgrade.loses_data, True)
        types = [impact.type for impact in report.downgrade.impacts]
        assert DataImpactType.DROP_COLUMN in types

    def test_renamed_column_identity_both_directions(self):
        """a hand-written rename pair is reported as an identity change in
        both directions, with the source name reversed"""

        upgrade = ops.UpgradeOps(
            [
                ops.ModifyTableOps(
                    "account",
                    [
                        ops.AlterColumnOp(
                            "account",
                            "old",
                            modify_name="new",
                        )
                    ],
                )
            ]
        )
        downgrade = ops.DowngradeOps(
            [
                ops.ModifyTableOps(
                    "account",
                    [
                        ops.AlterColumnOp(
                            "account",
                            "new",
                            modify_name="old",
                        )
                    ],
                )
            ]
        )
        script = _script(upgrade.ops, downgrade.ops)

        report = inspect_data_impact(script)
        up = report.upgrade.impacts[0]
        down = report.downgrade.impacts[0]
        eq_((up.table, up.column, up.new_name), ("account", "old", "new"))
        eq_(
            (down.table, down.column, down.new_name),
            ("account", "new", "old"),
        )
        eq_(report.upgrade.loses_data, False)
        eq_(report.downgrade.loses_data, False)


class DataImpactRenameTableTest(TestBase):
    def test_rename_table_shows_identity_change(self):
        script = _script(
            [ops.RenameTableOp("old_name", "new_name")],
            [ops.RenameTableOp("new_name", "old_name")],
        )
        report = inspect_data_impact(script)
        eq_(report.upgrade.impacts[0].type, DataImpactType.RENAME_TABLE)
        eq_(report.upgrade.impacts[0].new_name, "new_name")
        eq_(report.downgrade.impacts[0].new_name, "old_name")
        eq_(report.loses_data, {"upgrade": False, "downgrade": False})

    def test_rename_table_not_lossy(self):
        report = inspect_data_impact(ops.RenameTableOp("a", "b"))
        eq_(report.upgrade.loses_data, False)


class DataImpactCategoryTest(TestBase):
    def test_constraint_ops_are_categorized(self):
        md = MetaData()
        Table(
            "account",
            md,
            Column("id", Integer, primary_key=True),
            Column("name", String(50)),
            UniqueConstraint("name", name="uq_account_name"),
        )
        constraint = md.tables["account"].constraints
        uq = [c for c in constraint if c.name == "uq_account_name"][0]

        add = ops.AddConstraintOp.from_constraint(uq)
        report = inspect_data_impact(add)
        (impact,) = report.upgrade.impacts
        eq_(impact.type, DataImpactType.ADD_CONSTRAINT)
        eq_(impact.table, "account")
        eq_(impact.loses_data, False)

        drop = ops.DropConstraintOp.from_constraint(uq)
        report = inspect_data_impact(drop)
        (impact,) = report.upgrade.impacts
        eq_(impact.type, DataImpactType.DROP_CONSTRAINT)

    def test_index_ops_are_categorized(self):
        create = ops.CreateIndexOp("ix", "account", ["a"])
        report = inspect_data_impact(create)
        eq_(report.upgrade.impacts[0].type, DataImpactType.CREATE_INDEX)
        eq_(report.upgrade.loses_data, False)

        drop = ops.DropIndexOp("ix", "account")
        report = inspect_data_impact(drop)
        eq_(report.upgrade.impacts[0].type, DataImpactType.DROP_INDEX)

    def test_create_and_drop_table(self):
        create = ops.CreateTableOp(
            "account", [Column("id", Integer)]
        )
        report = inspect_data_impact(create)
        eq_(report.upgrade.impacts[0].type, DataImpactType.CREATE_TABLE)
        eq_(report.upgrade.loses_data, False)

        drop = ops.DropTableOp("account")
        report = inspect_data_impact(drop)
        eq_(report.upgrade.impacts[0].type, DataImpactType.DROP_TABLE)
        eq_(report.upgrade.loses_data, True)

    def test_execute_sql_is_unknown(self):
        execute = ops.ExecuteSQLOp("UPDATE account SET x = 1")
        report = inspect_data_impact(execute)
        (impact,) = report.upgrade.impacts
        eq_(impact.type, DataImpactType.UNKNOWN)
        eq_(impact.loses_data, None)
        assert "UPDATE account" in impact.detail

    def test_bulk_insert_counts_rows(self):
        from sqlalchemy.sql import table

        t = table("account", Column("id", Integer))
        insert = ops.BulkInsertOp(t, [{"id": 1}, {"id": 2}])
        report = inspect_data_impact(insert)
        (impact,) = report.upgrade.impacts
        eq_(impact.type, DataImpactType.INSERT_ROWS)
        eq_(impact.table, "account")
        assert "2 row" in impact.detail

