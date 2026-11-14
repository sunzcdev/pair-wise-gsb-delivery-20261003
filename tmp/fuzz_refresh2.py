import random

from sqlglot import parse_one, exp
from sqlglot.optimizer.annotate_types import annotate_types
from sqlglot.optimizer.qualify import qualify
from sqlglot.optimizer.refresh_types import is_stale, refresh_types
from fuzz_refresh import QUERIES, SCHEMA


def differential(sql, dialect, qualified, rng):
    root = parse_one(sql, dialect=dialect)
    if qualified:
        root = qualify(root, schema=SCHEMA, dialect=dialect, validate_qualify_columns=False)
    annotate_types(root, schema=SCHEMA, dialect=dialect)

    tree = root.copy()
    candidates = [n for n in tree.walk() if isinstance(n, (exp.Literal, exp.Column))]
    if len(candidates) < 2:
        return True

    k = rng.randint(1, min(3, len(candidates)))
    targets = rng.sample(candidates, k)
    new_nodes = []
    for target in targets:
        repl = parse_one(rng.choice(["1.5", "'z'", "NULL", "a + 2"]))
        target.replace(repl)
        new_nodes.append(repl)

    expected = tree.copy()
    annotate_types(expected, schema=SCHEMA, dialect=dialect)

    result = refresh_types(tree, new_nodes, schema=SCHEMA, dialect=dialect)

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
        print(f"DIFF dialect={dialect} qualified={qualified} sql={sql[:45]}")
        for d in diffs[:8]:
            print("   ", d)
        return False

    # unaffected nodes must keep their exact annotation object/value
    invalidated_ids = {id(n) for n in result.invalidated}
    for n in tree.walk():
        en = exp_map.get(id(n))
        if en and id(n) not in invalidated_ids:
            if (n.type.sql() if n.type else None) != (en.type.sql() if en.type else None):
                print("UNEXPECTED CHANGE", n.sql()[:40])
                return False
    return True


rng = random.Random(99)
fails = 0
runs = 0
for dialect in (None, "duckdb", "bigquery", "spark"):
    for qualified in (True, False):
        for sql in QUERIES:
            for _ in range(10):
                runs += 1
                try:
                    if not differential(sql, dialect, qualified, rng):
                        fails += 1
                except Exception as exc:  # noqa: BLE001
                    # Full annotation and incremental must fail in the same way; treat
                    # an exception only the incremental path hits as a failure.
                    print(f"EXC {dialect} q={qualified} {sql[:35]}: {type(exc).__name__}: {exc}")
                    fails += 1
print(f"runs={runs} fails={fails}")
