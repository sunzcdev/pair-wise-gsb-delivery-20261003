import sqlglot
from sqlglot import exp
from sqlglot.optimizer.qualify import qualify
from sqlglot.optimizer.scope import build_scope, traverse_scope


def inspect(sql, dialect=None, schema=None):
    print("=" * 100)
    print("SQL:", sql)
    tree = sqlglot.parse_one(sql, dialect=dialect)
    qualify(
        tree,
        dialect=dialect,
        schema=schema or {},
        validate_qualify_columns=False,
        identify=False,
    )
    for scope in traverse_scope(tree):
        if scope.is_udtf and isinstance(scope.expression, exp.Unnest):
            for u in scope.expression.find_all(exp.Unnest):
                for e in u.expressions:
                    print("  array expr:", e.sql(), "type:", e.type.sql() if e.type else None)
                    print("    repr:", repr(e))


inspect(
    "SELECT s.id FROM tbl CROSS JOIN UNNEST(tbl.arr) AS u(s)",
    dialect="duckdb",
    schema={"tbl": {"arr": "ARRAY<STRUCT<id INT, name VARCHAR>>"}},
)

inspect(
    "SELECT elem FROM tbl CROSS JOIN UNNEST(tbl.arr) AS t(elem)",
    dialect="trino",
    schema={"tbl": {"arr": "ARRAY<VARCHAR>"}},
)

inspect(
    "SELECT elem FROM tbl CROSS JOIN UNNEST(tbl.arr) AS t(elem)",
    dialect="trino",
    schema={},
)

inspect(
    "WITH _data AS (SELECT [STRUCT(1 AS a, 2 AS b)] AS col) "
    "SELECT b FROM _data CROSS JOIN UNNEST(col)",
    dialect="duckdb",
    schema={},
)

inspect(
    "SELECT x FROM tbl CROSS JOIN UNNEST(tbl.arr)",
    dialect="bigquery",
    schema={"tbl": {"arr": "ARRAY<INT64>"}},
)

inspect(
    "SELECT x FROM tbl CROSS JOIN UNNEST(tbl.arr)",
    dialect="bigquery",
    schema={},
)
