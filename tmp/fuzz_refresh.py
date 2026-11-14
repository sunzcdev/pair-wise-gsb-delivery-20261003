import random

from sqlglot import parse_one, exp
from sqlglot.optimizer.annotate_types import annotate_types
from sqlglot.optimizer.qualify import qualify
from sqlglot.optimizer.refresh_types import is_stale, refresh_types

SCHEMA = {
    "t": {"a": "INT", "b": "VARCHAR", "c": "DOUBLE", "d": "DATE"},
    "u": {"b": "INT", "e": "BIGINT", "f": "VARCHAR"},
    "v": {"x": "INT", "y": "INT"},
}

QUERIES = [
    "SELECT a + 1 AS z, b FROM t WHERE a > 2",
    "SELECT a, count(*) FROM t GROUP BY a HAVING count(*) > 1 ORDER BY a",
    "SELECT a + 1 AS x FROM t ORDER BY x + 2",
    "WITH c AS (SELECT a + 1 AS x, b FROM t) SELECT c.x, c.b FROM c JOIN t ON c.b = t.b",
    "WITH c AS (SELECT a, b FROM t), d AS (SELECT b, e FROM u) SELECT c.a, d.e FROM c JOIN d ON c.b = d.b",
    "SELECT s.x FROM (SELECT a + 1 AS x FROM t) AS s",
    "SELECT a FROM t AS t1 WHERE a IN (SELECT e FROM u AS u1 WHERE u1.f = t1.b)",
    "SELECT (SELECT max(e) FROM u WHERE u.f = t.b) AS m FROM t",
    "SELECT a + 1 AS x FROM t UNION ALL SELECT e FROM u",
    "SELECT a FROM t UNION ALL SELECT e FROM u UNION ALL SELECT x FROM v",
    "SELECT a, row_number() OVER (PARTITION BY b ORDER BY a) FROM t",
    "SELECT CASE WHEN a > 1 THEN a * 2 ELSE 0 END FROM t",
    "SELECT t.a, u.e FROM t LEFT JOIN u ON t.b = u.f",
    "WITH cte AS (SELECT a + 1 AS x FROM t) SELECT (SELECT max(x) FROM cte) FROM t",
    "SELECT * FROM (SELECT a, b, c FROM t) WHERE a IN (SELECT x FROM v WHERE y > a)",
    "SELECT coalesce(a, 0) + 1.5 FROM t",
    "SELECT a FROM t WHERE exists(SELECT 1 FROM u WHERE u.b = t.a)",
    "WITH c AS (SELECT a, b FROM t) SELECT c.b, (SELECT count(*) FROM c) FROM c",
    "SELECT a FROM t WHERE a IN (SELECT e FROM u WHERE u.f IN (SELECT f FROM u AS u2 WHERE u2.e = u.e))",
    "WITH c AS (SELECT a + 1 AS x, b FROM t) SELECT c1.x + c2.x FROM c AS c1 JOIN c AS c2 ON c1.b = c2.b",
    "WITH c AS (SELECT a, b FROM t) SELECT c.a FROM c WHERE c.b IN (SELECT f FROM u WHERE u.e = c.a)",
    "SELECT t.a, (SELECT min(e) FROM u WHERE u.f = t.b), (SELECT max(x) FROM v WHERE v.y = t.a) FROM t",
]

REPLACEMENTS = ["1.5", "'z'", "CAST(a AS DOUBLE)", "a + 2", "NULL", "a * 3.0"]


def snapshot(root):
    out = []
    for n in root.walk():
        out.append((id(n), n.type.sql() if n.type else None, n.meta_get("nonnull"), n.meta_get("query_type") is not None))
    return out


def run():
    rng = random.Random(1234)
    failures = 0
    for qi, sql in enumerate(QUERIES):
        for trial in range(20):
            base = qualify(parse_one(sql), schema=SCHEMA, validate_qualify_columns=False)
            annotate_types(base, schema=SCHEMA)

            tree = base.copy()
            # pick a random non-identifier, non-column leaf-ish node to replace
            candidates = [n for n in tree.walk() if isinstance(n, (exp.Literal, exp.Column))]
            if not candidates:
                continue
            target = rng.choice(candidates)
            repl = parse_one(rng.choice(REPLACEMENTS))
            target.replace(repl)

            expected = tree.copy()
            annotate_types(expected, schema=SCHEMA)

            try:
                result = refresh_types(tree, repl, schema=SCHEMA)
            except Exception as exc:  # noqa: BLE001
                failures += 1
                print(f"[EXC] q{qi} trial{trial} {sql[:40]}: {type(exc).__name__}: {exc}")
                continue

            exp_map = {id(n): n for n in expected.walk()}
            diffs = []
            for n in tree.walk():
                en = exp_map.get(id(n))
                if en is None:
                    continue
                et = en.type.sql() if en.type else None
                ot = n.type.sql() if n.type else None
                if et != ot:
                    diffs.append((n.sql()[:40], et, ot))
            stale = [n for n in tree.walk() if is_stale(n)]
            if diffs or stale:
                failures += 1
                print(f"[DIFF] q{qi} trial{trial} replaced={repl.sql()} sql={sql[:50]}")
                for d in diffs[:8]:
                    print("   ", d)
                if stale:
                    print("    stale left:", [n.sql()[:30] for n in stale])
    print("DONE, failures =", failures)


run()
