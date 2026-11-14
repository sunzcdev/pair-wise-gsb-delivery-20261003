import sys
sys.path.insert(0, '.')
from glom import glom, Iter, GlomError

class Fail:
    def glomit(self, target, scope):
        raise KeyError('boom')

# single-layer Iter raising non-GlomError
gen = glom([1, 2], Iter(Fail()))
try:
    list(gen)
except Exception as e:
    print('single layer FINAL:', type(e).__name__)
