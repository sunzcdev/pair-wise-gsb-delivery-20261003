from sqlglot.lineage import lineage


def show(n, i=0):
    print(" " * i + f"- {n.name!r} expr={n.expression.sql()!r}")
    for d in n.downstream:
        show(d, i + 4)


print("=== literal array")
show(lineage("e", "SELECT e FROM UNNEST(ARRAY[1,2,3]) AS t(e)", schema={}, dialect="trino"))

print("=== unnest of function result over a column")
show(
    lineage(
        "e",
        "SELECT e FROM tbl CROSS JOIN UNNEST(SPLIT(tbl.txt, ',')) AS t(e)",
        schema={"tbl": {"txt": "VARCHAR"}},
        dialect="trino",
    )
)
