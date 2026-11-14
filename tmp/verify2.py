from sqlglot.lineage import lineage
from sqlglot import exp


def names(node):
    return [n.name for n in node.walk()]


# Same array, two different UNNEST operations in one query
sql = "SELECT a, b FROM tbl CROSS JOIN UNNEST(tbl.arr) AS x(a) CROSS JOIN UNNEST(tbl.arr) AS y(b)"
schema = {"tbl": {"arr": "ARRAY<INT>"}}

node_a = lineage("a", sql, schema=schema, dialect="trino")
node_b = lineage("b", sql, schema=schema, dialect="trino")

print("a path:", names(node_a))
print("b path:", names(node_b))
assert "x.a" in names(node_a) and "y.a" not in names(node_a)
assert "y.b" in names(node_b) and "x.b" not in names(node_b)
assert node_a.downstream[0].source.sql().startswith("UNNEST(tbl.arr) AS x")
assert node_b.downstream[0].source.sql().startswith("UNNEST(tbl.arr) AS y")
print("DISTINCT UNNEST NODES OK")

# All-columns mode: shared cache must still keep two distinct unnest nodes
all_nodes = lineage(None, sql, schema=schema, dialect="trino")
unnest_sources = set()
for root in all_nodes.values():
    for n in root.walk():
        if n.source.sql().startswith("UNNEST"):
            unnest_sources.add(n.source.sql())
print("distinct unnest sources in all-column mode:", unnest_sources)
assert len(unnest_sources) == 2
print("ALL-COLUMN DEDUP OK")

# Correlated unnest: table column and unnest element stay separate, one unnest node
sql2 = "SELECT t.id, u.x FROM tbl t CROSS JOIN UNNEST(t.arr) AS u(x)"
schema2 = {"tbl": {"id": "BIGINT", "arr": "ARRAY<VARCHAR>"}}
n_id = lineage("id", sql2, schema=schema2, dialect="trino")
n_x = lineage("x", sql2, schema=schema2, dialect="trino")
print("id path:", names(n_id))
print("x path:", names(n_x))
assert all(not isinstance(n.expression, exp.Placeholder) for n in n_id.walk())
assert all(n.name != "u.x" for n in n_id.walk())
assert any(isinstance(n.expression, type(n_x.expression)) for n in n_x.walk())
assert "u.x" in names(n_x)
print("CORRELATED OK")
