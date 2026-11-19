# -*- coding: utf-8 -*-
"""Batch rebuild column-identity / constraint-survival demo (SQLite)."""
import warnings

import sqlalchemy as sa
from sqlalchemy.exc import IntegrityError

from alembic.migration import MigrationContext
from alembic.operations import Operations

URL = "sqlite:///batch_demo.db"

DDL = """
DROP TABLE IF EXISTS child;
DROP TABLE IF EXISTS parent;
CREATE TABLE parent (
    a INTEGER NOT NULL,
    b INTEGER NOT NULL,
    CONSTRAINT pk_parent PRIMARY KEY (a, b)
);
CREATE TABLE child (
    id INTEGER NOT NULL,
    x INTEGER,
    y INTEGER,
    a INTEGER,
    b INTEGER,
    data VARCHAR(50),
    CONSTRAINT pk_child PRIMARY KEY (id),
    CONSTRAINT ck_child_x_lt_y CHECK (x < y),
    CONSTRAINT fk_child_parent FOREIGN KEY (a, b) REFERENCES parent (a, b)
);
INSERT INTO parent (a, b) VALUES (1, 1);
"""

def fresh_db():
    eng = sa.create_engine(URL)
    with eng.begin() as conn:
        for stmt in DDL.strip().split(";"):
            stmt = stmt.strip()
            if stmt:
                conn.exec_driver_sql(stmt)
    return eng

def enable_fk(conn):
    conn.exec_driver_sql("PRAGMA foreign_keys=ON")

def show_constraints(conn, label):
    insp = sa.inspect(conn)
    print("  [%s] columns: %s" % (label, ", ".join(
        "%s:%s" % (c["name"], c["type"]) for c in insp.get_columns("child"))))
    print("  [%s] check constraints: %s" % (label, insp.get_check_constraints("child")))
    print("  [%s] foreign keys: %s" % (label, insp.get_foreign_keys("child")))

def try_writes(conn):
    conn.execute(sa.text(
        "INSERT INTO child (id, x, y, a, b, data_renamed) "
        "VALUES (1, 1, 2, 1, 1, 'ok')"))
    print("  valid write (x=1<y=2, fk->parent(1,1)): OK")
    try:
        conn.execute(sa.text(
            "INSERT INTO child (id, x, y, a, b) VALUES (2, 5, 3, 1, 1)"))
        print("  CHECK-violating write (x=5>y=3): ACCEPTED (constraint lost!)")
    except IntegrityError as e:
        print("  CHECK-violating write (x=5>y=3): rejected -> %s" % e.orig)
    try:
        conn.execute(sa.text(
            "INSERT INTO child (id, x, y, a, b) VALUES (3, 1, 2, 9, 9)"))
        print("  FK-violating write (parent(9,9) missing): ACCEPTED (constraint lost!)")
    except IntegrityError as e:
        print("  FK-violating write (parent(9,9) missing): rejected -> %s" % e.orig)

def run_batch(conn, use_copy_from, extra=None):
    ctx = MigrationContext.configure(conn)
    op = Operations(ctx)
    kw = {}
    if use_copy_from:
        m = sa.MetaData()
        sa.Table("parent", m,
                 sa.Column("a", sa.Integer, primary_key=True),
                 sa.Column("b", sa.Integer, primary_key=True))
        copy_from = sa.Table(
            "child", m,
            sa.Column("id", sa.Integer, primary_key=True),
            sa.Column("x", sa.Integer),
            sa.Column("y", sa.Integer),
            sa.Column("a", sa.Integer),
            sa.Column("b", sa.Integer),
            sa.Column("data", sa.String(50)),
            sa.CheckConstraint("x < y", name="ck_child_x_lt_y"),
            sa.ForeignKeyConstraint(["a", "b"], ["parent.a", "parent.b"],
                                    name="fk_child_parent"),
        )
        kw["copy_from"] = copy_from
    with op.batch_alter_table("child", recreate="always", **kw) as bop:
        bop.alter_column("data", new_column_name="data_renamed",
                         existing_type=sa.String(50))
        bop.alter_column("y", type_=sa.BigInteger(),
                         existing_type=sa.Integer())
        if extra:
            extra(bop)

print("=" * 72)
print("Scenario A: rebuild via copy_from (offline metadata, reflected=False)")
print("=" * 72)
eng = fresh_db()
with eng.begin() as conn:
    enable_fk(conn)
    run_batch(conn, use_copy_from=True)
with eng.begin() as conn:
    enable_fk(conn)
    show_constraints(conn, "copy_from")
    try_writes(conn)

print()
print("=" * 72)
print("Scenario B: rebuild via live reflection (reflected=True)")
print("=" * 72)
eng = fresh_db()
with eng.begin() as conn:
    enable_fk(conn)
    run_batch(conn, use_copy_from=False)
with eng.begin() as conn:
    enable_fk(conn)
    show_constraints(conn, "reflect")
    try_writes(conn)

print()
print("=" * 72)
print("Scenario C: migration EXPLICITLY drops ck_child_x_lt_y (copy_from)")
print("=" * 72)
eng = fresh_db()
with eng.begin() as conn:
    enable_fk(conn)
    def drop_ck(bop):
        bop.drop_constraint("ck_child_x_lt_y", type_="check")
    run_batch(conn, use_copy_from=True, extra=drop_ck)
with eng.begin() as conn:
    enable_fk(conn)
    show_constraints(conn, "explicit-drop")
    conn.execute(sa.text(
        "INSERT INTO child (id, x, y, a, b) VALUES (2, 5, 3, 1, 1)"))
    print("  CHECK-violating write (x=5>y=3): ACCEPTED (expected: explicitly dropped)")

print()
print("=" * 72)
print("Scenario D: reflection OMITS an unnamed CHECK (legacy table)")
print("=" * 72)
eng = sa.create_engine(URL)
with eng.begin() as conn:
    conn.exec_driver_sql("DROP TABLE IF EXISTS legacy")
    conn.exec_driver_sql(
        "CREATE TABLE legacy (id INTEGER PRIMARY KEY, x INTEGER, y INTEGER, "
        "CHECK (x < y))")
with eng.begin() as conn:
    ctx = MigrationContext.configure(conn)
    op = Operations(ctx)
    with warnings.catch_warnings(record=True) as caught:
        warnings.simplefilter("always")
        with op.batch_alter_table("legacy", recreate="always") as bop:
            bop.alter_column("y", type_=sa.BigInteger(),
                             existing_type=sa.Integer())
    for w in caught:
        print("  warning: %s" % w.message)
with eng.begin() as conn:
    insp = sa.inspect(conn)
    print("  [legacy] check constraints after rebuild: %s"
          % insp.get_check_constraints("legacy"))
    conn.execute(sa.text("INSERT INTO legacy (id, x, y) VALUES (1, 9, 3)"))
    print("  CHECK-violating write (x=9>y=3): ACCEPTED (reflection omission)")
