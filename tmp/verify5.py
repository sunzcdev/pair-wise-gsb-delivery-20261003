from sqlglot.lineage import lineage


def show(n, i=0):
    print(" " * i + f"- {n.name!r} expr={n.expression.sql()!r}")
    for d in n.downstream:
        show(d, i + 4)


# A qualified table column alongside an unqualified unnest output
print("=== qualified table col + unaliased unnest")
show(
    lineage(
        "id",
        "SELECT t.id, x FROM tbl t CROSS JOIN UNNEST(t.arr)",
        schema={"tbl": {"id": "BIGINT", "arr": "ARRAY<INT>"}},
        dialect="bigquery",
    )
)

# Two unaliased unnests: an unqualified column can't be safely attributed
print("=== two unaliased unnests, unqualified output")
try:
    show(
        lineage(
            "z",
            "SELECT z FROM tbl CROSS JOIN UNNEST(tbl.arr1) CROSS JOIN UNNEST(tbl.arr2)",
            schema={"tbl": {"arr1": "ARRAY<INT>", "arr2": "ARRAY<INT>"}},
            dialect="bigquery",
        )
    )
except Exception as exc:
    print("FAILED:", type(exc).__name__, exc)
