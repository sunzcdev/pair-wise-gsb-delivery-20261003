import sys
sys.path.insert(0, '.')
from glom import glom, Iter, T, S, A, Vars, SKIP, Val

# record path seen by each element evaluation
seen = []
def record(t):
    from glom.core import Path as P
    # scope not accessible here; use Inspect-like trick instead
    return t

# Use S to expose path: map each element to (index via enumerate?) 
# Better: check scope[Path] through a spec function with S? S can't read Path easily.
# Use Inspect
from glom import Inspect
import io, contextlib

# Nested iter: outer yields inner generators; consume them interleaved
target = [[10, 11], [20, 21], [30, 31]]
spec = Iter(Iter(lambda t: t * 10))
gen = glom(target, spec)
inner0 = next(gen)   # outer at index 0, paused
inner1 = next(gen)   # outer advanced to index 1
inner2 = next(gen)   # outer at index 2
print('consume inner0 after outer advanced:')
print(list(inner0))  # expect [100, 110]
print(list(inner1))  # expect [200, 210]
print(list(inner2))  # expect [300, 310]
