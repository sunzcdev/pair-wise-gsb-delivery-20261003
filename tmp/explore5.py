from sqlglot.lineage import lineage


def show(node, indent=0):
    print(" " * indent + f"- {node.name!r} expr={node.expression.sql()!r} source={node.source.sql()!r} ref={node.reference_node_name!r}")
    for d in node.downstream:
        show(d, indent + 4)


# two unnests of the same array in one query
sql = "SELECT a, b FROM tbl CROSS JOIN UNNEST(tbl.arr) AS x(a) CROSS JOIN UNNEST(tbl.arr) AS y(b)"
schema = {"tbl": {"arr": "ARRAY<INT>"}}
for col in ("a", "b"):
    print("=" * 80)
    print("column", col)
    show(lineage(col, sql, schema=schema, dialect="trino"))

# all-column dict result to see the shared cache
print("=" * 80)
print("lineage(None)")
nodes = lineage(None, sql, schema=schema, dialect="trino")
for name, node in nodes.items():
    print("OUTPUT:", name)
    show(node, 2)

# unnest without alias list, plain element column
print("=" * 80)
sql2 = "SELECT x FROM tbl CROSS JOIN UNNEST(tbl.arr)"
try:
    show(lineage("x", sql2, schema={"tbl": {"arr": "ARRAY<INT64>"}}, dialect="bigquery"))
except Exception as exc:
    print("FAILED:", type(exc).__name__, exc)

# offset column lineage
print("=" * 80)
sql3 = "SELECT item, off FROM tbl, UNNEST(tbl.arr) AS item WITH OFFSET AS off"
for col in ("item", "off"):
    print("column", col)
    show(lineage(col, sql3, schema={"tbl": {"arr": "ARRAY<INT64>"}}, dialect="bigquery"))
