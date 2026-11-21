"""Acceptance: multidb revision with per-bind custom operation renderers.

Renders one revision containing two binds (engine1, engine2), each with its
own custom operation rendering + imports, then actually runs upgrade and
downgrade against two local sqlite databases.
"""
from __future__ import annotations

import os
import sys
import types

import sqlalchemy as sa

from alembic import command
from alembic import op as alembic_op
from alembic.autogenerate import render
from alembic.operations import MigrateOperation
from alembic.operations import Operations
from alembic.testing.env import (
    _get_staging_directory,
    _multidb_testing_config,
    _sqlite_file_db,
    clear_staging_env,
    staging_env,
)

MODULE_NAME = "acceptance_multidb"


@Operations.register_operation("make_widget")
class MakeWidgetOp(MigrateOperation):
    def __init__(self, bind, label):
        self.bind = bind
        self.label = label

    @classmethod
    def make_widget(cls, operations, bind, label):
        return operations.invoke(cls(bind, label))

    def reverse(self):
        return DropWidgetOp(self.bind, self.label)


@Operations.register_operation("drop_widget")
class DropWidgetOp(MigrateOperation):
    def __init__(self, bind, label):
        self.bind = bind
        self.label = label

    @classmethod
    def drop_widget(cls, operations, bind, label):
        return operations.invoke(cls(bind, label))

    def reverse(self):
        return MakeWidgetOp(self.bind, self.label)


@Operations.implementation_for(MakeWidgetOp)
def _impl_make(operations, operation):
    operations.execute(
        "INSERT INTO widget_log (bind_name, label) VALUES ('%s', '%s')"
        % (operation.bind, operation.label)
    )


@Operations.implementation_for(DropWidgetOp)
def _impl_drop(operations, operation):
    operations.execute(
        "DELETE FROM widget_log WHERE bind_name='%s' AND label='%s'"
        % (operation.bind, operation.label)
    )


def process_revision_directives(context, rev, directives):
    script = directives[0]
    for upgrade_ops, downgrade_ops in zip(
        script.upgrade_ops_list, script.downgrade_ops_list
    ):
        token = upgrade_ops.upgrade_token
        bind = token.removesuffix("_upgrades")
        # each bind has its own AutogenContext / renderer dispatcher
        ctx = render.get_render_context(token)
        assert ctx is not None, token

        def _render_make(ac, op, _b=bind):
            ac.imports.add("from acceptance_helpers import make_%s" % _b)
            return "make_%s(%r, %r)" % (_b, op.bind, op.label)

        def _render_drop(ac, op, _b=bind):
            ac.imports.add("from acceptance_helpers import drop_%s" % _b)
            return "drop_%s(%r, %r)" % (_b, op.bind, op.label)

        # the hook runs once per bind; on later binds the earlier bind's
        # render context is already registered, so guard against
        # re-registering.  registering the same op classes on engine2's own
        # dispatcher must not silently override engine1's renderer.
        if (MakeWidgetOp, "default") not in ctx.renderers._registry:
            ctx.renderers.dispatch_for(MakeWidgetOp)(_render_make)
            ctx.renderers.dispatch_for(DropWidgetOp)(_render_drop)

        # the hook is invoked once per bind and sees every bind's stream;
        # only add this bind's operation once
        if not any(
            isinstance(existing, MakeWidgetOp)
            for existing in upgrade_ops.ops
        ):
            make_op = MakeWidgetOp(bind, "widget-%s" % bind)
            upgrade_ops.ops.append(make_op)
            downgrade_ops.ops.insert(0, make_op.reverse())


def _build_helper_module():
    helper = types.ModuleType("acceptance_helpers")
    helper.calls = {"make": [], "drop": []}

    def _insert(bind, label):
        helper.calls["make"].append((bind, label))
        alembic_op.get_bind().exec_driver_sql(
            "INSERT INTO widget_log (bind_name, label) VALUES (?, ?)",
            (bind, label),
        )

    def _delete(bind, label):
        helper.calls["drop"].append((bind, label))
        alembic_op.get_bind().exec_driver_sql(
            "DELETE FROM widget_log WHERE bind_name=? AND label=?",
            (bind, label),
        )

    for b in ("engine1", "engine2"):
        setattr(helper, "make_%s" % b,
                (lambda b=b: lambda a, c: _insert(b, c))())
        setattr(helper, "drop_%s" % b,
                (lambda b=b: lambda a, c: _delete(b, c))())
    return helper


# only register once, so a second import of this script (the generated
# env.py imports it) does not reset the recorded calls.
if "acceptance_helpers" not in sys.modules:
    sys.modules["acceptance_helpers"] = _build_helper_module()


def main():
    e1 = _sqlite_file_db(tempname="eng1.db")
    e2 = _sqlite_file_db(tempname="eng2.db")

    staging_env(template="multidb")
    try:
        cfg = _multidb_testing_config({"engine1": e1, "engine2": e2})

        path = os.path.join(_get_staging_directory(), "scripts", "env.py")
        with open(path) as f:
            src = f.read()
        src = src.replace(
            "target_metadata = {}",
            "import sqlalchemy as sa\n"
            "%s\n"
            "from %s import process_revision_directives\n"
            "m1 = sa.MetaData()\n"
            "m2 = sa.MetaData()\n"
            "sa.Table('widget_log', m1,\n"
            "         sa.Column('id', sa.Integer, primary_key=True),\n"
            "         sa.Column('bind_name', sa.String(20)),\n"
            "         sa.Column('label', sa.String(40)))\n"
            "sa.Table('e1_extra', m1, sa.Column('id', sa.Integer,"
            " primary_key=True))\n"
            "sa.Table('widget_log', m2,\n"
            "         sa.Column('id', sa.Integer, primary_key=True),\n"
            "         sa.Column('bind_name', sa.String(20)),\n"
            "         sa.Column('label', sa.String(40)))\n"
            "sa.Table('e2_extra', m2, sa.Column('id', sa.Integer,"
            " primary_key=True))\n"
            "target_metadata = {'engine1': m1, 'engine2': m2}\n"
            % (_setup_sys_path_code(), MODULE_NAME),
        )
        src = src.replace(
            '            context.configure(\n'
            '                connection=rec["connection"],',
            '            context.configure(\n'
            '                connection=rec["connection"],\n'
            "                process_revision_directives="
            "process_revision_directives,",
        )
        with open(path, "w") as f:
            f.write(src)

        rev = command.revision(cfg, message="two bind custom ops",
                               autogenerate=True)
        with open(rev.path) as f:
            contents = f.read()

        print("=" * 70)
        print(contents)
        print("=" * 70)

        assert "make_engine1('engine1', 'widget-engine1')" in contents
        assert "make_engine2('engine2', 'widget-engine2')" in contents
        assert "drop_engine1('engine1', 'widget-engine1')" in contents
        assert "drop_engine2('engine2', 'widget-engine2')" in contents

        s1 = contents[contents.index("def upgrade_engine1"):
                      contents.index("def upgrade_engine2")]
        s2 = contents[contents.index("def upgrade_engine2"):]
        assert "from acceptance_helpers import make_engine1" in s1
        assert "from acceptance_helpers import make_engine2" not in s1
        assert "from acceptance_helpers import make_engine2" in s2
        assert "from acceptance_helpers import make_engine1" not in s2
        # default rendering still applies to the plain generated tables
        assert "op.create_table('e1_extra'" in s1
        assert "op.create_table('e2_extra'" in s2
        # no per-bind imports leaked to module level
        header = contents[: contents.index("def upgrade(")]
        assert "acceptance_helpers" not in header

        command.upgrade(cfg, "head")
        insp1 = sa.inspect(e1)
        insp2 = sa.inspect(e2)
        assert "widget_log" in insp1.get_table_names()
        assert "widget_log" in insp2.get_table_names()

        calls = sys.modules["acceptance_helpers"].calls
        with e1.begin() as conn:
            rows = conn.execute(sa.text(
                "SELECT bind_name, label FROM widget_log")).fetchall()
            assert rows == [("engine1", "widget-engine1")], rows
        with e2.begin() as conn:
            rows = conn.execute(sa.text(
                "SELECT bind_name, label FROM widget_log")).fetchall()
            assert rows == [("engine2", "widget-engine2")], rows

        assert calls["make"] == [
            ("engine1", "widget-engine1"),
            ("engine2", "widget-engine2"),
        ], calls["make"]

        command.downgrade(cfg, "base")
        assert calls["drop"] == [
            ("engine1", "widget-engine1"),
            ("engine2", "widget-engine2"),
        ], calls["drop"]

        # registry cleared once rendering completes
        assert render._render_contexts == {}

        print("ALL ACCEPTANCE CHECKS PASSED")
    finally:
        clear_staging_env()


def _setup_sys_path_code():
    # the generated env.py imports this module by name; make sure the
    # workspace root (this file's directory) is importable.
    here = os.path.dirname(os.path.abspath(__file__))
    return "import sys as _sys; _sys.path.insert(0, %r)" % here


if __name__ == "__main__":
    main()
