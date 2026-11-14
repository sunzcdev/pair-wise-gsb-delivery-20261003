import sys
sys.path.insert(0, '.')
from glom import glom, Iter, T
from glom.core import Spec

class Dump:
    def glomit(self, target, scope):
        for i, m in enumerate(scope.maps):
            print(i, [getattr(k, "__name__", str(k)) for k in m.keys()])
        print('---')
        return target

gen = glom([[1]], Iter(Iter(Dump())))
for chunk in gen:
    list(chunk)
