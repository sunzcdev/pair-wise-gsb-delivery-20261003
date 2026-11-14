from sqlglot import parse_one, exp
from sqlglot.errors import OptimizeError
from sqlglot.optimizer.annotate_types import annotate_types
from sqlglot.optimizer.qualify import qualify
from sqlglot.optimizer.refresh_types import is_stale, refresh_types


def build(sql, schema=None, dialect=None):
    tree = qualify(
        parse_one(sql, dialect=dialect),
        schema=schema or {},
        dialect=dialect,
        validate_qualify_columns=False,
    )
    return annotate_types(tree, schema=schema, dialect=dialect)


def type_map(root):
    return {id(n): (n.sql()[:40], n.type.sql() if n.type else None) for n in root.walk()}


def replace_and_target(tree, old_node, replacement_sql):
    new_node = parse_one(replacement_sql)
    old_node.replace(new_node)
    return new_node


def check(name, sql, find_changed, schema=None, dialect=None):
    schema = schema or {"t": {"a": "INT", "b": "VARCHAR"}, "u": {"b": "INT"}}
    tree = build(sql, schema, dialect)

    target = find_changed(tree)
    target = replace_and_target(tree, target, "1.5")

    baseline = tree.copy()
    annotate_types(baseline, schema=schema, dialect=dialect)

    result = refresh_types(tree, target, schema=schema, dialect=dialect)

    diffs = []
    for fresh, old in zip(baseline.walk(), tree.walk()):
        ft = fresh.type.sql() if fresh.type else None
        ot = old.type.sql() if old.type else None
        if ft != ot:
            diffs.append((fresh.sql()[:50], ft, ot))

    stale_left = [n.sql()[:30] for n in tree.walk() if is_stale(n)]
    print(f"== {name}: invalidated={len(result.invalidated)} scopes={len(result.scopes)} "
          f"unknown={len(result.unknown)} diffs={len(diffs)} stale_left={stale_left}")
    if diffs:
        for d in diffs[:10]:
            print("   DIFF", d)
    return not diffs and not stale_left


ok = True

ok &= check(
    "simple parent chain",
    "SELECT a + 1 FROM t",
    lambda tr: tr.find(exp.Literal),
)

ok &= check(
    "alias ref in order by",
    "SELECT a + 1 AS x FROM t ORDER BY x + 2",
    lambda tr: tr.find(exp.Literal),
)

ok &= check(
    "cte output",
    "WITH c AS (SELECT a + 1 AS x FROM t) SELECT c.x FROM c",
    lambda tr: tr.find(exp.CTE).this.find(exp.Literal),
)

ok &= check(
    "correlated subquery",
    "SELECT a + 1 FROM t AS t1 WHERE a IN (SELECT b FROM u AS u1 WHERE u1.b = t1.a)",
    lambda tr: tr.find(exp.Literal),
)

ok &= check(
    "derived table output",
    "SELECT s.x FROM (SELECT a + 1 AS x FROM t) AS s",
    lambda tr: tr.find(exp.Subquery).find(exp.Literal),
)

ok &= check(
    "set operation",
    "SELECT a + 1 AS x FROM t UNION ALL SELECT a FROM t",
    lambda tr: tr.find(exp.Literal),
)

ok &= check(
    "unknown schema col",
    "SELECT a + 1 FROM t",
    lambda tr: tr.find(exp.Literal),
    schema={"t": {"a": "INT"}},
)

print("ALL OK" if ok else "FAILURES")

# Edge cases
tree = build("SELECT a + 1 FROM t", {"t": {"a": "INT"}})
old_lit = tree.find(exp.Literal)
lit = replace_and_target(tree, old_lit, "2")
result = refresh_types(tree, lit, schema={"t": {"a": "INT"}})
print("single node arg works, invalidated:", len(result.invalidated))

# duplicate submission
result2 = refresh_types(tree, [lit, lit], schema={"t": {"a": "INT"}})
print("duplicate skipped/handled; invalidated:", len(result2.invalidated))

# detached node
detached = parse_one("1 + 2")
try:
    refresh_types(tree, detached, schema={"t": {"a": "INT"}})
    print("detached strict: NO ERROR (bad)")
except OptimizeError:
    print("detached strict raises OptimizeError: OK")
res = refresh_types(tree, detached, schema={"t": {"a": "INT"}}, strict=False)
print("detached non-strict skipped:", len(res.skipped))
