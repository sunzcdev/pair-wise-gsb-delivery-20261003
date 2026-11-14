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
    print("QUALIFIED:", tree.sql(dialect=dialect))
    root = build_scope(tree)
    for scope in traverse_scope(tree):
        print(
            f"SCOPE {scope.scope_type.name} {type(scope.expression).__name__} "
            f"is_udtf={scope.is_udtf} parent={type(scope.parent.expression).__name__ if scope.parent else None}"
        )
        for name, src in scope.sources.items():
            expr = getattr(src, "expression", src)
            print(f"   src {name!r}: {type(src).__name__} -> {type(expr).__name__ if expr is not None else None} {expr.sql() if expr is not None else ''}")
        if scope.is_udtf:
            print("   subquery_scopes:", [type(s.expression).__name__ for s in scope.subquery_scopes])
            print("   lateral_sources:", getattr(scope, "lateral_sources", None))


# correlated unnest inside a subquery (trino)
inspect(
    "SELECT t.id, u.x FROM tbl t CROSS JOIN UNNEST(t.arr) AS u(x)",
    dialect="trino",
    schema={"tbl": {"id": "BIGINT", "arr": "ARRAY<VARCHAR>"}},
)

# same table referenced by two independent unnests in one query
inspect(
    "SELECT a, b FROM tbl CROSS JOIN UNNEST(tbl.arr) AS x(a) CROSS JOIN UNNEST(tbl.arr) AS y(b)",
    dialect="trino",
    schema={"tbl": {"arr": "ARRAY<INT>"}},
)

# lateral unnest in select? snowflake lateral flatten
inspect(
    "SELECT f.value:external_id::string AS external_id FROM tbl AS raw, LATERAL FLATTEN(raw.events) AS f",
    dialect="snowflake",
    schema={"tbl": {"events": "ARRAY"}},
)
