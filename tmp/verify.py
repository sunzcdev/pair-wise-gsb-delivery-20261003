from sqlglot.lineage import lineage


def show(node, indent=0):
    print(
        " " * indent
        + f"- {node.name!r} expr={node.expression.sql()!r} source={node.source.sql()[:70]!r}"
    )
    for d in node.downstream:
        show(d, indent + 4)


cases = [
    (
        "nested struct field",
        "SELECT s.id FROM tbl CROSS JOIN UNNEST(tbl.arr) AS u(s)",
        "duckdb",
        {"tbl": {"arr": "ARRAY<STRUCT<id INT, name VARCHAR>>"}},
        "id",
    ),
    (
        "two arrays in one unnest",
        "SELECT a, b FROM tbl CROSS JOIN UNNEST(tbl.arr1, tbl.arr2) AS t(a, b)",
        "trino",
        {"tbl": {"arr1": "ARRAY<VARCHAR>", "arr2": "ARRAY<INT>"}},
        "a",
    ),
    (
        "two arrays in one unnest (b)",
        "SELECT a, b FROM tbl CROSS JOIN UNNEST(tbl.arr1, tbl.arr2) AS t(a, b)",
        "trino",
        {"tbl": {"arr1": "ARRAY<VARCHAR>", "arr2": "ARRAY<INT>"}},
        "b",
    ),
    (
        "correlated unnest + normal column",
        "SELECT t.id, u.x FROM tbl t CROSS JOIN UNNEST(t.arr) AS u(x)",
        "trino",
        {"tbl": {"id": "BIGINT", "arr": "ARRAY<VARCHAR>"}},
        "x",
    ),
    (
        "correlated unnest normal col id",
        "SELECT t.id, u.x FROM tbl t CROSS JOIN UNNEST(t.arr) AS u(x)",
        "trino",
        {"tbl": {"id": "BIGINT", "arr": "ARRAY<VARCHAR>"}},
        "id",
    ),
    (
        "subquery source unnest",
        "SELECT elem FROM (SELECT arr FROM tbl) CROSS JOIN UNNEST(arr) AS t(elem)",
        "trino",
        {"tbl": {"arr": "ARRAY<VARCHAR>"}},
        "elem",
    ),
    (
        "ordinality",
        "SELECT val, pos FROM tbl CROSS JOIN UNNEST(tbl.arr) WITH ORDINALITY AS t(val, pos)",
        "trino",
        {"tbl": {"arr": "ARRAY<VARCHAR>"}},
        "pos",
    ),
    (
        "unknown array type no schema",
        "SELECT elem FROM tbl CROSS JOIN UNNEST(tbl.arr) AS t(elem)",
        "trino",
        {},
        "elem",
    ),
]

for title, sql, dialect, schema, col in cases:
    print("=" * 90)
    print(title, "->", col)
    try:
        show(lineage(col, sql, schema=schema, dialect=dialect))
    except Exception as exc:
        print("FAILED:", type(exc).__name__, exc)
