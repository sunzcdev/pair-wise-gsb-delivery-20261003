import sys
sys.path.insert(0, '.')
from glom import glom, Iter, T
from glom.core import Path as P

outer_paths = []
class Check:
    def glomit(self, target, scope):
        outer_paths.append(list(scope[P]))
        return target

# Iter nested inside another spec; after full consumption, outer scope Path intact
spec = (Iter().map(T), Check())
out = list(glom([10, 20, 30], spec))
print('outer scope paths seen after/beside Iter:', outer_paths)
print('result:', out)
