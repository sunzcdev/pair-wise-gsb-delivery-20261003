"""Tests for the version-table recovery workflow.

The scenario throughout is a database restored from a backup whose
``alembic_version`` table holds two rows: one revision that still exists in
the current scripts, and one that belonged to a branch that has since been
replaced.  The recovery API must diagnose both without writing anything,
rehearse a single confirmed replacement, and leave the unrelated branch
alone, while the ordinary ``upgrade`` command keeps failing on the unknown
revision.
"""

from sqlalchemy import text

from alembic import command
from alembic import util
from alembic.script import ScriptDirectory
from alembic.script.revision import Revision
from alembic.script.revision import RevisionMap
from alembic.testing import assert_raises_message
from alembic.testing import eq_
from alembic.testing import is_false
from alembic.testing import is_true
from alembic.testing.env import _sqlite_file_db
from alembic.testing.env import _sqlite_testing_config
from alembic.testing.env import clear_staging_env
from alembic.testing.env import staging_env
from alembic.testing.env import write_script
from alembic.testing.fixtures import TestBase


class _RecoveryFixture(TestBase):
    """A live branch, an unrelated branch and a retired branch.

    The current scripts contain::

        main1 -> main2 -> main3     (main3 is the head)
        side1 -> side2              (side2 is an unrelated head)

    The restored database carries a row from the retired branch (``old2``),
    which no longer exists, alongside the still-valid ``side2``.
    """

    @classmethod
    def setup_class(cls):
        cls.bind = _sqlite_file_db(scope="class")
        cls.env = staging_env()
        cls.cfg = _sqlite_testing_config()

        script = ScriptDirectory.from_config(cls.cfg)

        cls.main1 = "a11ce0000001"
        cls.main2 = "a11ce0000002"
        cls.main3 = "a11ce0000003"
        cls.side1 = "51de00000001"
        cls.side2 = "51de00000002"

        cls._write(script, cls.main1, "main1", None)
        cls._write(script, cls.main2, "main2", cls.main1)
        cls._write(script, cls.main3, "main3", cls.main2)
        cls._write(script, cls.side1, "side1", None)
        cls._write(script, cls.side2, "side2", cls.side1)

        # a revision from the branch that no longer exists
        cls.old1 = "01d000000001"
        cls.old2 = "01d000000002"

    @classmethod
    def _write(cls, script, rev, name, down):
        script.generate_revision(rev, name, refresh=True, head=down or "base")
        write_script(
            script,
            rev,
            """
revision = %r
down_revision = %r


def upgrade():
    pass


def downgrade():
    pass
"""
            % (rev, down),
        )

    @classmethod
    def teardown_class(cls):
        clear_staging_env()

    def setUp(self):
        self.env = staging_env(create=False)
        self.cfg = _sqlite_testing_config()

    def _seed_version_table(self, *revisions):
        with self.bind.begin() as conn:
            conn.exec_driver_sql("drop table if exists alembic_version")
            conn.exec_driver_sql(
                "create table alembic_version "
                "(version_num varchar(32) not null primary key)"
            )
            for rev in revisions:
                conn.execute(
                    text(
                        "insert into alembic_version (version_num) "
                        "values (:rev)"
                    ),
                    {"rev": rev},
                )

    def _version_rows(self):
        with self.bind.connect() as conn:
            return tuple(
                row[0]
                for row in conn.execute(
                    text("select version_num from alembic_version")
                )
            )

    def _has_version_table(self):
        with self.bind.connect() as conn:
            return (
                conn.scalar(
                    text(
                        "select count(*) from sqlite_master "
                        "where type='table' and name='alembic_version'"
                    )
                )
                > 0
            )


class RevisionMapClassifyTest(TestBase):
    def _map(self):
        return RevisionMap(
            lambda: [
                Revision("a", ()),
                Revision("b", ("a",)),
                Revision("c", ("b",)),
                Revision("root2", ()),
                Revision("d2", ("root2",)),
            ]
        )

    def test_known_head(self):
        (status,) = self._map().classify_revision_ids(["c"])
        is_true(status.known)
        is_true(status.is_head)
        is_false(status.redundant)
        is_false(status.needs_mapping)
        eq_(status.reachable_heads, ("c",))

    def test_known_ancestor_duplicate(self):
        statuses = {
            s.revision: s
            for s in self._map().classify_revision_ids(["c", "b"])
        }
        is_true(statuses["b"].redundant)
        is_false(statuses["b"].is_head)
        eq_(statuses["b"].is_ancestor_of, ("c",))
        is_true(statuses["c"].is_head)

    def test_unknown_never_raises(self):
        statuses = {
            s.revision: s
            for s in self._map().classify_revision_ids(["c", "missing"])
        }
        is_false(statuses["missing"].known)
        is_true(statuses["missing"].needs_mapping)
        eq_(statuses["missing"].reachable_heads, ())

    def test_partial_identifier_is_not_matched(self):
        # a stored row is reported literally; partial resolution belongs to
        # the regular commands only
        (status,) = self._map().classify_revision_ids(["d"])
        is_false(status.known)

    def test_branch_label_is_not_matched(self):
        map_ = RevisionMap(lambda: [Revision("a", (), branch_labels="bl")])
        (status,) = map_.classify_revision_ids(["bl"])
        is_false(status.known)

    def test_duplicate_rows_collapsed(self):
        statuses = self._map().classify_revision_ids(["c", "c"])
        eq_(len(statuses), 1)

    def test_reachable_heads_excludes_other_branch(self):
        statuses = {
            s.revision: s
            for s in self._map().classify_revision_ids(["a", "root2"])
        }
        eq_(statuses["a"].reachable_heads, ("c",))
        eq_(statuses["root2"].reachable_heads, ("d2",))

    def test_get_exact_revision_ignores_partial(self):
        map_ = self._map()
        assert map_.get_exact_revision("a") is not None
        assert map_.get_exact_revision("miss") is None


class InspectVersionsTest(_RecoveryFixture):
    def test_mixed_report(self):
        self._seed_version_table(self.side2, self.old2)

        report = command.inspect_versions(self.cfg)

        eq_(report.version_table, "alembic_version")
        is_true(report.version_table_present)
        is_true(report.needs_mapping)
        eq_({row.revision for row in report.valid_rows}, {self.side2})
        eq_({row.revision for row in report.unknown_rows}, {self.old2})

    def test_valid_head_marked(self):
        self._seed_version_table(self.side2, self.old2)
        report = command.inspect_versions(self.cfg)
        valid = report.valid_rows[0]
        is_true(valid.is_head)
        eq_(valid.reachable_heads, (self.side2,))

    def test_redundant_ancestor_row(self):
        self._seed_version_table(self.main2, self.main3, self.old2)
        report = command.inspect_versions(self.cfg)
        redundant = {row.revision for row in report.redundant_rows}
        eq_(redundant, {self.main2})

    def test_report_text(self):
        self._seed_version_table(self.side2, self.old2)
        report = command.inspect_versions(self.cfg)
        text_ = report.format_report()
        assert "[valid head] %s" % self.side2 in text_
        assert "[unknown] %s" % self.old2 in text_
        assert "explicit administrator mapping" in text_

    def test_inspection_is_read_only(self):
        self._seed_version_table(self.side2, self.old2)
        before = self._version_rows()
        command.inspect_versions(self.cfg)
        eq_(set(self._version_rows()), set(before))

    def test_inspection_does_not_create_table(self):
        with self.bind.begin() as conn:
            conn.exec_driver_sql("drop table if exists alembic_version")
        is_false(self._has_version_table())
        report = command.inspect_versions(self.cfg)
        is_false(report.version_table_present)
        eq_(report.rows, ())
        is_false(self._has_version_table())

    def test_plain_upgrade_still_fails_on_unknown(self):
        self._seed_version_table(self.side2, self.old2)
        assert_raises_message(
            util.CommandError,
            "Can't locate revision identified by '%s'" % self.old2,
            command.upgrade,
            self.cfg,
            "heads",
        )
        eq_(set(self._version_rows()), {self.side2, self.old2})


class PlanVersionsTest(_RecoveryFixture):
    def test_plan_does_not_write(self):
        self._seed_version_table(self.side2, self.old2)
        before = self._version_rows()

        plan = command.plan_versions(
            self.cfg, "%s:%s" % (self.old2, self.main1)
        )

        eq_(set(self._version_rows()), set(before))
        eq_(set(plan.heads_before), {self.side2, self.old2})
        is_true(plan.unrelated_heads_preserved)

    def test_plan_reports_upgrade_steps(self):
        self._seed_version_table(self.side2, self.old2)
        plan = command.plan_versions(
            self.cfg, "%s:%s" % (self.old2, self.main1)
        )
        eq_(set(plan.heads_after), {self.side2, self.main1})
        eq_(plan.upgrade_revisions, (self.main2, self.main3))

    def test_plan_leaves_side_branch(self):
        self._seed_version_table(self.side2, self.old2)
        plan = command.plan_versions(
            self.cfg, "%s:%s" % (self.old2, self.main1)
        )
        assert self.side2 in plan.heads_after
        assert self.side2 in plan.untouched_revisions

    def test_plan_rejects_unknown_target(self):
        self._seed_version_table(self.side2, self.old2)
        assert_raises_message(
            util.CommandError,
            "Can't map to revision '%s'" % self.old1,
            command.plan_versions,
            self.cfg,
            "%s:%s" % (self.old2, self.old1),
        )

    def test_plan_rejects_missing_source(self):
        self._seed_version_table(self.side2)
        assert_raises_message(
            util.CommandError,
            "Can't replace revision '%s'; it is not present" % self.old2,
            command.plan_versions,
            self.cfg,
            "%s:%s" % (self.old2, self.main1),
        )

    def test_plan_rejects_mapping_that_leaves_bad_state(self):
        # mapping onto an ancestor of a head that remains in the table would
        # leave a row the normal upgrade cannot reason about; the rehearsal
        # must catch it before anything is written
        self._seed_version_table(self.main3, self.old2)
        assert_raises_message(
            util.CommandError,
            "does not leave the version table in a state that can be "
            "upgraded normally",
            command.plan_versions,
            self.cfg,
            "%s:%s" % (self.old2, self.main1),
        )
        eq_(set(self._version_rows()), {self.main3, self.old2})


class RepairVersionsTest(_RecoveryFixture):
    def test_requires_confirmation(self):
        self._seed_version_table(self.side2, self.old2)
        assert_raises_message(
            util.CommandError,
            "without explicit confirmation",
            command.repair_versions,
            self.cfg,
            "%s:%s" % (self.old2, self.main1),
        )
        eq_(set(self._version_rows()), {self.side2, self.old2})

    def test_confirmed_replaces_one_row(self):
        self._seed_version_table(self.side2, self.old2)

        heads = command.repair_versions(
            self.cfg, "%s:%s" % (self.old2, self.main1), confirm=True
        )

        eq_(set(heads), {self.side2, self.main1})
        eq_(set(self._version_rows()), {self.side2, self.main1})

    def test_unrelated_branch_preserved(self):
        self._seed_version_table(self.main2, self.side1, self.old2)

        command.repair_versions(
            self.cfg, "%s:%s" % (self.old2, self.side2), confirm=True
        )

        eq_(
            set(self._version_rows()),
            {self.main2, self.side1, self.side2},
        )

    def test_bad_mapping_format(self):
        self._seed_version_table(self.side2, self.old2)
        assert_raises_message(
            util.CommandError,
            "Revision mapping must be given as '<old>:<new>'",
            command.repair_versions,
            self.cfg,
            self.old2,
            confirm=True,
        )

    def test_confirmed_rejects_unknown_target(self):
        self._seed_version_table(self.side2, self.old2)
        assert_raises_message(
            util.CommandError,
            "Can't map to revision '%s'" % self.old1,
            command.repair_versions,
            self.cfg,
            "%s:%s" % (self.old2, self.old1),
            confirm=True,
        )
        eq_(set(self._version_rows()), {self.side2, self.old2})

    def test_confirmed_rejects_missing_source(self):
        self._seed_version_table(self.side2)
        assert_raises_message(
            util.CommandError,
            "Can't replace revision '%s'; it is not present" % self.old2,
            command.repair_versions,
            self.cfg,
            "%s:%s" % (self.old2, self.main1),
            confirm=True,
        )
        eq_(set(self._version_rows()), {self.side2})

    def test_confirmed_rejects_already_present_target(self):
        self._seed_version_table(self.side2, self.old2)
        assert_raises_message(
            util.CommandError,
            "is already present in version table",
            command.repair_versions,
            self.cfg,
            "%s:%s" % (self.old2, self.side2),
            confirm=True,
        )


class UpgradeAfterMappingTest(_RecoveryFixture):
    def test_upgrade_only_advances_mapped_branch(self):
        self._seed_version_table(self.side2, self.old2)

        command.repair_versions(
            self.cfg, "%s:%s" % (self.old2, self.main1), confirm=True
        )
        eq_(set(self._version_rows()), {self.side2, self.main1})

        command.upgrade(self.cfg, "heads")
        eq_(set(self._version_rows()), {self.side2, self.main3})

    def test_upgrade_leaves_unrelated_branch_alone(self):
        self._seed_version_table(self.side2, self.old2)

        command.repair_versions(
            self.cfg, "%s:%s" % (self.old2, self.main1), confirm=True
        )
        command.upgrade(self.cfg, "heads")

        rows = set(self._version_rows())
        eq_(rows, {self.side2, self.main3})

    def test_mapping_only_replaces_one_row(self):
        self._seed_version_table(self.side2, self.old2)

        command.repair_versions(
            self.cfg, "%s:%s" % (self.old2, self.main1), confirm=True
        )

        rows = set(self._version_rows())
        assert self.old2 not in rows
        assert self.side2 in rows
        eq_(len(rows), 2)

    def test_full_flow_inspect_plan_repair_upgrade(self):
        self._seed_version_table(self.side2, self.old2)

        report = command.inspect_versions(self.cfg)
        eq_({r.revision for r in report.unknown_rows}, {self.old2})

        plan = command.plan_versions(
            self.cfg, "%s:%s" % (self.old2, self.main1)
        )
        eq_(plan.upgrade_revisions, (self.main2, self.main3))

        command.repair_versions(
            self.cfg, "%s:%s" % (self.old2, self.main1), confirm=True
        )
        command.upgrade(self.cfg, "heads")

        eq_(set(self._version_rows()), {self.side2, self.main3})
        report = command.inspect_versions(self.cfg)
        is_false(report.needs_mapping)


class ContextDirectAPITest(_RecoveryFixture):
    def setUp(self):
        self.env = staging_env(create=False)
        self.cfg = _sqlite_testing_config()
        self.connection = self.bind.connect()

    def tearDown(self):
        self.connection.close()

    def _context(self):
        from alembic.runtime.migration import MigrationContext

        script = ScriptDirectory.from_config(self.cfg)
        return MigrationContext.configure(
            connection=self.connection, opts={"script": script}
        )

    def test_get_recovery_report(self):
        self._seed_version_table(self.side2, self.old2)
        report = self._context().get_recovery_report()
        eq_({r.revision for r in report.unknown_rows}, {self.old2})

    def test_report_requires_script(self):
        from alembic.runtime.migration import MigrationContext

        self._seed_version_table(self.side2)
        with self.bind.connect() as conn:
            context = MigrationContext.configure(connection=conn)
            assert_raises_message(
                util.CommandError,
                "requires a ScriptDirectory",
                context.get_recovery_report,
            )

    def test_report_rejected_offline(self):
        from alembic.runtime.migration import MigrationContext

        context = MigrationContext.configure(
            url="sqlite://", opts={"as_sql": True}
        )
        assert_raises_message(
            util.CommandError,
            "not available in --sql / offline mode",
            context.get_recovery_report,
        )

    def test_apply_revision_mapping(self):
        self._seed_version_table(self.side2, self.old2)
        heads = self._context().apply_revision_mapping(
            self.old2, self.main1
        )
        eq_(set(heads), {self.side2, self.main1})
        eq_(set(self._version_rows()), {self.side2, self.main1})

    def test_apply_revision_mapping_in_external_transaction(self):
        # the connection-sharing recipe: the caller owns the transaction and
        # decides when (and whether) the replacement becomes permanent
        self._seed_version_table(self.side2, self.old2)

        with self.connection.begin():
            context = self._context()
            heads = context.apply_revision_mapping(self.old2, self.main1)
            eq_(set(heads), {self.side2, self.main1})

        eq_(set(self._version_rows()), {self.side2, self.main1})

    def test_apply_revision_mapping_external_rollback(self):
        self._seed_version_table(self.side2, self.old2)

        transaction = self.connection.begin()
        context = self._context()
        context.apply_revision_mapping(self.old2, self.main1)
        transaction.rollback()

        eq_(set(self._version_rows()), {self.side2, self.old2})