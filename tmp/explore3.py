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
    root = build_scope(tree)
    for scope in traverse_scope(tree):
        if scope.is_udtf:
            udtf = scope.expression
            print("UDTF:", udtf.sql())
            print("  scope.sources:", {k: type(v).__name__ for k, v in scope.sources.items()})
            print("  scope.derived_tables:", [d.sql() for d in scope.derived_tables])
            print("  selects in scope expr:", [s.sql() for s in scope.expression.selects])
            if isinstance(udtf, exp.Unnest):
                print("  unnest.expressions:", [e.sql() for e in udtf.expressions])
                print("  unnest offset:", udtf.args.get("offset"))
                alias = udtf.args.get("alias")
                print("  alias columns:", [c.sql() for c in alias.columns] if alias else None)
                print("  unnest.selects:", [s.sql() for s in udtf.selects])


inspect(
    "SELECT s.id FROM tbl CROSS JOIN UNNEST(tbl.arr) AS u(s)",
    dialect="duckdb",
    schema={"tbl": {"arr": "ARRAY<STRUCT<id INT, name VARCHAR>>"}},
)

inspect(
    "SELECT a, b FROM tbl CROSS JOIN UNNEST(tbl.arr1, tbl.arr2) AS t(a, b)",
    dialect="trino",
    schema={"tbl": {"arr1": "ARRAY<VARCHAR>", "arr2": "ARRAY<INT>"}},
)

inspect(
    "WITH _data AS (SELECT [STRUCT(1 AS a, 2 AS b)] AS col) "
    "SELECT b FROM _data CROSS JOIN UNNEST(col)",
    dialect="duckdb",
)

inspect(
    "SELECT val, pos FROM tbl CROSS JOIN UNNEST(tbl.arr) WITH ORDINALITY AS t(val, pos)",
    dialect="trino",
    schema={"tbl": {"arr": "ARRAY<VARCHAR>"}},
)

inspect(
    "SELECT item, off FROM tbl, UNNEST(tbl.arr) AS item WITH OFFSET AS off",
    dialect="bigquery",
    schema={"tbl": {"arr": "ARRAY<INT64>"}},
)

inspect(
    "SELECT pe.pos, pe.col FROM tbl LATERAL VIEW POSEXPLODE(tbl.arr) pe AS pos, col",
    dialect="spark",
    schema={"tbl": {"arr": "ARRAY<INT>"}},
)
