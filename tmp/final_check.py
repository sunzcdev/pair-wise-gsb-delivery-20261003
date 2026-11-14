from sqlglot import exp, parse_one
from sqlglot.errors import OptimizeError
from sqlglot.optimizer.annotate_types import annotate_types
from sqlglot.optimizer.qualify import qualify
from sqlglot.optimizer.refresh_types import is_stale, refresh_types

SCHEMA = {
    "t": {"a": "INT", "b": "VARCHAR"},
    "u": {"e": "BIGINT", "f": "VARCHAR"},
}


def equivalent(incr, full):
    return all(x.type == y.type for x, y in zip(incr.walk(), full.walk()))


checks = []

# 1. Parent-chain, alias, CTE, derived table, correlated subquery, set op.
for sql in (
    "SELECT a + 1 AS x FROM t ORDER BY x",
    "WITH c AS (SELECT a + 1 AS x FROM t) SELECT c.x FROM c",
    "SELECT s.x FROM (SELECT a + 1 AS x FROM t) AS s",
    "SELECT a + 1 FROM t WHERE a IN (SELECT e FROM u WHERE u.f = t.b)",
    "SELECT a + 1 FROM t UNION ALL SELECT e FROM u",
):
    tree = qualify(parse_one(sql), schema=SCHEMA, validate_qualify_columns=False)
    annotate_types(tree, schema=SCHEMA)
    old = tree.find(exp.Literal)
    new = parse_one("1.5")
    old.replace(new)
    full = tree.copy()
    annotate_types(full, schema=SCHEMA)
    result = refresh_types(tree, new, schema=SCHEMA)
    checks.append(equivalent(tree, full))
    checks.append(not any(is_stale(n) for n in tree.walk()))
    checks.append(new in result.invalidated)

# 2. Unaffected branch reused.
tree = qualify(
    parse_one("SELECT a + 1 AS changed, b AS kept FROM t"),
    schema=SCHEMA,
    validate_qualify_columns=False,
)
annotate_types(tree, schema=SCHEMA)
kept = tree.selects[1]
kept_type = kept.type
new = parse_one("1.5")
tree.selects[0].find(exp.Literal).replace(new)
result = refresh_types(tree, new, schema=SCHEMA)
checks.append(kept.type is kept_type and kept not in result.invalidated)

# 3. Detached node behavior.
tree = qualify(parse_one("SELECT a + 1 FROM t"), schema=SCHEMA, validate_qualify_columns=False)
annotate_types(tree, schema=SCHEMA)
detached = parse_one("1 + 2")
try:
    refresh_types(tree, detached, schema=SCHEMA)
    checks.append(False)
except OptimizeError:
    checks.append(True)
checks.append(refresh_types(tree, detached, schema=SCHEMA, strict=False).skipped == [detached])

# 4. Duplicate submission.
new = parse_one("2")
tree.find(exp.Literal).replace(new)
once = refresh_types(tree, new, schema=SCHEMA)
twice = refresh_types(tree, [new, new], schema=SCHEMA)
checks.append(len(twice.invalidated) == len(once.invalidated) and twice.skipped == [])

# 5. Unknown schema -> UNKNOWN, observable, no exception.
tree = qualify(parse_one("SELECT a + 1 FROM t"), schema=SCHEMA, validate_qualify_columns=False)
annotate_types(tree, schema=SCHEMA)
missing = parse_one("nope")
tree.find(exp.Column).replace(missing)
result = refresh_types(tree, missing, schema=SCHEMA)
checks.append(missing in result.unknown and tree.selects[0].is_type(exp.DType.UNKNOWN))

# 6. Full annotation interface unchanged.
tree = qualify(parse_one("SELECT a + 1 FROM t"), schema=SCHEMA, validate_qualify_columns=False)
annotated = annotate_types(tree, schema=SCHEMA, overwrite_types=True)
checks.append(annotated is tree and annotated.selects[0].is_type(exp.DType.INT))

print("PASS" if all(checks) else f"FAIL ({checks.count(False)} of {len(checks)})")
