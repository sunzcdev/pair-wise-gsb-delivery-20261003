import unittest

from tests.test_optimizer import TestOptimizer

# Avoid the spawn-based ProcessPoolExecutor created in setUpClass (blocked in sandbox).
TestOptimizer.setUpClass = classmethod(lambda cls: setattr(cls, "pool", None))

names = [
    "test_refresh_types_matches_full_annotation",
    "test_refresh_types_reuses_unaffected_branches",
    "test_refresh_types_propagation",
    "test_refresh_types_edge_cases",
]

loader = unittest.TestLoader()
suite = unittest.TestSuite([TestOptimizer(name) for name in names])
result = unittest.TextTestRunner(verbosity=2).run(suite)
raise SystemExit(0 if result.wasSuccessful() else 1)
