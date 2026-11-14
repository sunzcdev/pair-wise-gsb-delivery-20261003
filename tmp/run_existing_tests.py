import unittest

from tests.test_optimizer import TestOptimizer

TestOptimizer.setUpClass = classmethod(lambda cls: setattr(cls, "pool", None))

names = [
    "test_annotate_types",
    "test_annotate_types_reuse",
    "test_annotate_struct_subquery_values",
    "test_reannotate_struct_subquery_values",
    "test_annotate_row_types",
    "test_annotate_untyped_binary_operands",
    "test_multiple_pivots_annotate_types",
    "test_unpivot_cte_annotate_types",
    "test_annotate_json_dot_parts_preserve_meta",
]

loader = unittest.TestLoader()
suite = unittest.TestSuite([TestOptimizer(name) for name in names])
result = unittest.TextTestRunner(verbosity=1).run(suite)
raise SystemExit(0 if result.wasSuccessful() else 1)
