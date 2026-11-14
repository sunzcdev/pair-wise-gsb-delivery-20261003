from sqlglot.lineage import lineage


def show(node, indent=0):
    print(" " * indent + f"- {node.name!r} expr={node.expression.sql()!r} source={node.source.sql()[:70]!r}")
    for d in node.downstream:
        show(d, indent + 4)


# array expression that is not a plain column (subscript / function result)
cases = [
    (
        "unnest of a struct field access",
        "SELECT e FROM tbl CROSS JOIN UNNEST(tbl.data.nums) AS t(e)",
        "trino",
        {"tbl": {"data": "STRUCT<nums ARRAY<INT>>"}},
        "e",
    ),
    (
        "lateral wrapped unnest with ordinality (trino)",
        "SELECT v, p FROM tbl CROSS JOIN LATERAL (SELECT * FROM UNNEST(tbl.arr) WITH ORDINALITY AS x(v, p))",
        "trino",
        {"tbl": {"arr": "ARRAY<INT>"}},
        "p",
    ),
    (
        "duckdb unnest ordinality two outputs",
        "SELECT val, pos FROM tbl CROSS JOIN UNNEST(tbl.arr) WITH ORDINALITY AS t(val, pos)",
        "duckdb",
        {"tbl": {"arr": "ARRAY<INT>"}},
        "val",
    ),
    (
        "repeated lineage calls share cache correctly",
        "SELECT a FROM tbl CROSS JOIN UNNEST(tbl.arr) AS x(a)",
        "trino",
        {"tbl": {"arr": "ARRAY<INT>"}},
        "a",
    ),
]

for title, sql, dialect, schema, col in cases:
    print("=" * 90)
    print(title)
    try:
        show(lineage(col, sql, schema=schema, dialect=dialect))
    except Exception as exc:
        import traceback

        traceback.print_exc()
