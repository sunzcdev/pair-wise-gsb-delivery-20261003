import sqlglot
from sqlglot import exp
from sqlglot.optimizer.qualify import qualify
from sqlglot.optimizer.scope import build_scope, traverse_scope


def dump(sql, dialect=None, schema=None):
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
    print("QUALIFIED:", tree.sql(dialect=dialect))
    for scope in traverse_scope(tree):
        if scope.is_udtf:
            print("UDTF SCOPE expr:")
            print(repr(scope.expression))
            print("sources:")
            for name, src in scope.sources.items():
                expr = src.expression if hasattr(src, "expression") else src
                print(f"  {name!r}: {type(expr).__name__}: {expr.sql() if expr is not None else None}")
    root = build_scope(tree)
    print("ROOT sources:")
    for name, src in root.sources.items():
        expr = src.expression if hasattr(src, "expression") else src
        print(f"  {name!r}: {type(src).__name__} -> {expr.sql() if expr is not None else None}")

    # Trace what lineage currently does with the columns
    from sqlglot.lineage import lineage

    for col in tree.named_selects:
        try:
            node = lineage(col, sql, schema=schema or {}, dialect=dialect)

            def show(n, indent=2):
                print(" " * indent + f"- {n.name!r} expr={n.expression.sql()!r}")
                for d in n.downstream:
                    show(d, indent + 4)

            print(f"LINEAGE for {col}:")
            show(node)
        except Exception as exc:
            print(f"LINEAGE for {col} FAILED: {type(exc).__name__}: {exc}")


dump(
    "WITH _data AS (SELECT [STRUCT(1 AS a, 2 AS b)] AS col) "
    "SELECT b FROM _data CROSS JOIN UNNEST(col)",
    dialect="duckdb",
)

dump(
    "SELECT s.id, s.name FROM tbl CROSS JOIN UNNEST(tbl.arr) AS u(s)",
    dialect="duckdb",
    schema={"tbl": {"arr": "ARRAY<STRUCT<id INT, name VARCHAR>>"}},
)

dump(
    "SELECT a, b FROM tbl CROSS JOIN UNNEST(tbl.arr1, tbl.arr2) AS t(a, b)",
    dialect="trino",
    schema={"tbl": {"arr1": "ARRAY<VARCHAR>", "arr2": "ARRAY<INT>"}},
)

dump(
    "SELECT x FROM tbl CROSS JOIN UNNEST(tbl.arr)",
    dialect="bigquery",
    schema={"tbl": {"arr": "ARRAY<INT64>"}},
)

dump(
    "SELECT id, x FROM tbl t CROSS JOIN UNNEST(t.arr) AS u(x)",
    dialect="trino",
    schema={"tbl": {"id": "BIGINT", "arr": "ARRAY<VARCHAR>"}},
)

dump(
    "SELECT elem FROM (SELECT arr FROM tbl) CROSS JOIN UNNEST(arr) AS t(elem)",
    dialect="trino",
    schema={"tbl": {"arr": "ARRAY<VARCHAR>"}},
)

dump(
    "SELECT val FROM tbl CROSS JOIN UNNEST(tbl.arr) WITH ORDINALITY AS t(val, pos)",
    dialect="trino",
    schema={"tbl": {"arr": "ARRAY<VARCHAR>"}},
)
