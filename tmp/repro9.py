import sys
sys.path.insert(0, '.')
from glom import glom, Iter, T, S, A
from glom.core import Path as P

paths = []
class Record:
    def glomit(self, target, scope):
        paths.append((self.tag, target, list(scope[P])))
        return target
r1 = Record(); r1.tag = 'iter1'
r2 = Record(); r2.tag = 'iter2'

target = {'a': [1, 2, 3], 'b': [4, 5, 6]}
spec = {'x': (T['a'], Iter(r1)), 'y': (T['b'], Iter(r2))}
res = glom(target, spec)
i1, i2 = res['x'], res['y']
next(i1)
next(i2)
next(i1)
for p in paths:
    print(p)
