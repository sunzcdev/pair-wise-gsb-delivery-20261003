import sys
sys.path.insert(0, '.')
from glom import glom, Iter, T
from glom.core import Path as P

paths = []
class Record:
    def glomit(self, target, scope):
        paths.append((target, list(scope[P])))
        return target

target = [[10, 11], [20, 21], [30, 31]]
spec = Iter(Iter(Record()))
gen = glom(target, spec)
inner0 = next(gen)
inner1 = next(gen)
inner2 = next(gen)
print('after outer advance, before inner consumption:')
print(paths)
list(inner0)
print('after consuming inner0:')
for p in paths:
    print(p)
