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
    print("QUALIFIED:")
    print(tree.sql(dialect=dialect, pretty=True))
    root = build_scope(tree)
    for scope in traverse_scope(tree):
        print(
            f"  SCOPE {scope.scope_type.name} expr={type(scope.expression).__name__} "
            f"is_udtf={scope.is_udtf}"
        )
        for name, src in scope.sources.items():
            expr = src.expression if isinstance(src, type(root)) else src
            print(f"    source {name!r}: {type(src).__name__} -> {type(expr).__name__}: {expr.sql()}")
        if scope.is_udtf:
            print("    udtf expression repr:", repr(scope.expression))


# 1. basic cross join unnest of struct array, no alias list (duckdb)
dump(
    "WITH _data AS (SELECT [STRUCT(1 AS a, 2 AS b)] AS col) "
    "SELECT b FROM _data CROSS JOIN UNNEST(col)",
    dialect="duckdb",
)

# 2. unnest with alias column list
dump(
    "SELECT elem, idx FROM tbl CROSS JOIN UNNEST(tbl.arr) WITH ORDINALITY AS t(elem, idx)",
    dialect="trino",
    schema={"tbl": {"arr": "ARRAY<INT>"}},
)

# 3. bigquery unnest with offset
dump(
    "SELECT item, off FROM tbl, UNNEST(tbl.arr) AS item WITH OFFSET AS off",
    dialect="bigquery",
    schema={"tbl": {"arr": "ARRAY<INT64>"}},
)

# 4. lateral correlated unnest (presto/trino)
dump(
    "SELECT t.id, u.x FROM tbl t CROSS JOIN UNNEST(t.arr) AS u(x)",
    dialect="trino",
    schema={"tbl": {"id": "BIGINT", "arr": "ARRAY(VARCHAR)"}},
)

# 5. two unnests in same query
dump(
    "SELECT a, b FROM tbl CROSS JOIN UNNEST(tbl.arr1) AS x(a) CROSS JOIN UNNEST(tbl.arr2) AS y(b)",
    dialect="duckdb",
    schema={"tbl": {"arr1": "ARRAY<INT>", "arr2": "ARRAY<INT>"}},
)

# 6. nested struct field inside unnest element
dump(
    "SELECT s.id FROM tbl CROSS JOIN UNNEST(tbl.arr) AS u(s)",
    dialect="duckdb",
    schema={"tbl": {"arr": "ARRAY<STRUCT<id INT, name VARCHAR>>"}},
)

# 7. posexplode (spark)
dump(
    "SELECT pe.pos, pe.col FROM tbl LATERAL VIEW POSEXPLODE(tbl.arr) pe AS pos, col",
    dialect="spark",
    schema={"tbl": {"arr": "ARRAY<INT>"}},
)
