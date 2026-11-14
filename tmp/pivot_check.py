from sqlglot import parse_one, exp
from sqlglot.optimizer.qualify import qualify
from sqlglot.optimizer.annotate_types import annotate_types
from sqlglot.optimizer.refresh_types import refresh_types

schema = {"t": {"a": "INT", "k": "VARCHAR", "v": "INT"}}
sql = "SELECT * FROM (SELECT a, k, v FROM t) PIVOT (sum(v) FOR k IN ('x', 'y'))"
tree = qualify(parse_one(sql), schema=schema, validate_qualify_columns=False)
annotate_types(tree, schema=schema)

target = next(tree.find_all(exp.Column))
new = parse_one("1.7")
target.replace(new)

refresh_types(tree, new, schema=schema)
full = tree.copy()
annotate_types(full, schema=schema)
mism = [
    (type(x).__name__, x.sql()[:25])
    for x, y in zip(tree.walk(), full.walk())
    if x.type != y.type
]
print("PIVOT", "OK" if not mism else "FAIL", mism)
