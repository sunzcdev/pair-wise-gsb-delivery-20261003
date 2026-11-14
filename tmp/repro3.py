import sys
sys.path.insert(0, '.')
from glom import glom, Iter, T, S, A, Vars, SKIP, Val, GlomError, Path

# Scenario A: explicit shared scope passed into one glom call, two Iters
print('=== A: shared scope, two Iters, interleaved ===')
target = [[1, 2, 3], [10, 20, 30]]
spec = (Iter().map((A.v, S.v)), Iter().map((A.w, S.w)))
try:
    out = glom([1,2,3], spec, scope={})
    print(out)
except Exception as e:
    print(type(e).__name__, e)
