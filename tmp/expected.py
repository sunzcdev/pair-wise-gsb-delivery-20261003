from sqlglot.lineage import lineage


def chain(node):
    rows = []

    def walk(n, depth=0):
        rows.append((depth, n.name, n.expression.sql(), n.source.sql()))
        for d in n.downstream:
            walk(d, depth + 1)

    walk(node)
    return rows


scenarios = [
    (
        "duckdb struct no alias",
        "b",
        "with _data as (select [struct(1 as a, 2 as b)] as col) select b from _data cross join unnest(col)",
        "duckdb",
        {},
    ),
    (
        "trino alias element",
        "elem",
        "SELECT elem FROM tbl CROSS JOIN UNNEST(tbl.arr) AS t(elem)",
        "trino",
        {"tbl": {"arr": "ARRAY<VARCHAR>"}},
    ),
    (
        "trino ordinality element",
        "val",
        "SELECT val, pos FROM tbl CROSS JOIN UNNEST(tbl.arr) WITH ORDINALITY AS t(val, pos)",
        "trino",
        {"tbl": {"arr": "ARRAY<VARCHAR>"}},
    ),
    (
        "trino ordinality position",
        "pos",
        "SELECT val, pos FROM tbl CROSS JOIN UNNEST(tbl.arr) WITH ORDINALITY AS t(val, pos)",
        "trino",
        {"tbl": {"arr": "ARRAY<VARCHAR>"}},
    ),
    (
        "bigquery offset",
        "off",
        "SELECT item, off FROM tbl, UNNEST(tbl.arr) AS item WITH OFFSET AS off",
        "bigquery",
        {"tbl": {"arr": "ARRAY<INT64>"}},
    ),
    (
        "bigquery offset element",
        "item",
        "SELECT item, off FROM tbl, UNNEST(tbl.arr) AS item WITH OFFSET AS off",
        "bigquery",
        {"tbl": {"arr": "ARRAY<INT64>"}},
    ),
    (
        "duckdb nested struct field",
        "id",
        "SELECT s.id FROM tbl CROSS JOIN UNNEST(tbl.arr) AS u(s)",
        "duckdb",
        {"tbl": {"arr": "ARRAY<STRUCT<id INT, name VARCHAR>>"}},
    ),
    (
        "trino two arrays a",
        "a",
        "SELECT a, b FROM tbl CROSS JOIN UNNEST(tbl.arr1, tbl.arr2) AS t(a, b)",
        "trino",
        {"tbl": {"arr1": "ARRAY<VARCHAR>", "arr2": "ARRAY<INT>"}},
    ),
    (
        "trino two arrays b",
        "b",
        "SELECT a, b FROM tbl CROSS JOIN UNNEST(tbl.arr1, tbl.arr2) AS t(a, b)",
        "trino",
        {"tbl": {"arr1": "ARRAY<VARCHAR>", "arr2": "ARRAY<INT>"}},
    ),
    (
        "trino two unnests a",
        "a",
        "SELECT a, b FROM tbl CROSS JOIN UNNEST(tbl.arr) AS x(a) CROSS JOIN UNNEST(tbl.arr) AS y(b)",
        "trino",
        {"tbl": {"arr": "ARRAY<INT>"}},
    ),
    (
        "trino two unnests b",
        "b",
        "SELECT a, b FROM tbl CROSS JOIN UNNEST(tbl.arr) AS x(a) CROSS JOIN UNNEST(tbl.arr) AS y(b)",
        "trino",
        {"tbl": {"arr": "ARRAY<INT>"}},
    ),
    (
        "trino unknown type",
        "elem",
        "SELECT elem FROM tbl CROSS JOIN UNNEST(tbl.arr) AS t(elem)",
        "trino",
        {},
    ),
]

for title, col, sql, dialect, schema in scenarios:
    print("=" * 90)
    print(title)
    for depth, name, expr, src in chain(lineage(col, sql, schema=schema, dialect=dialect)):
        print(" " * (depth * 2), f"name={name!r} expr={expr!r}")
