import sys
sys.path.insert(0, '.')
from glom import glom, Iter, GlomError
from glom.core import Path as P

seen = []
class Fail:
    def glomit(self, target, scope):
        seen.append(list(scope[P]))
        if scope[P][-1] == 1 and scope[P][-2] == 1:
            raise KeyError('boom')
        return target

target = [[10, 11], [20, 21]]
spec = Iter(Iter(Fail()))
gen = glom(target, spec)
inner0 = next(gen)  # outer paused at 0
inner1 = next(gen)  # outer advanced to 1
print('before resume:', seen)
list(inner0)        # resumes outer-0 inner, paths [0,0],[0,1]
print('after inner0:', seen)
try:
    list(inner1)    # resumes outer-1 inner, fails at [1,1]
except GlomError as e:
    print('after inner1:', seen)
    print('finalized path:', list(e._scope[P]))
